package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-cluster/oc-control-plane/internal/audit"
	"github.com/open-cluster/oc-control-plane/internal/auth/authz"
	"github.com/open-cluster/oc-control-plane/internal/auth/tenancy"
	"github.com/open-cluster/oc-control-plane/internal/integrations"
	"github.com/open-cluster/oc-control-plane/internal/secrets"
)

// The integrations capability owns its vocabulary; this file is its persistence. The
// contract is asserted here so a drifted method signature is a compile error.
var _ integrations.Store = (*Database)(nil)

func validateCredentialEnvelope(sealed []byte) error {
	if len(sealed) == 0 {
		return nil
	}
	if sealed[0] == seal.LegacyKeyVersion {
		return nil
	}
	_, err := seal.EnvelopeKeyID(sealed)
	if err != nil {
		return errors.New("storing an integration credential: invalid sealed envelope")
	}
	return nil
}

// integrationColumns is every column an Integration is read from, named once. One list
// rather than five copies, because a column added to one query and forgotten in another is
// a field that is silently always zero.
const integrationColumns = `integration_id, integration_type_id, name, configuration,
	       webhook_secret_digest, credential_sealed, relay_id, verification_status,
	       verified_at, verification_grants, disabled, created_at,
	       (SELECT jsonb_build_object(
	           'application', installed.application,
	           'enterprise', installed.enterprise,
	           'workspace', installed.workspace,
	           'enterpriseWide', installed.enterprise_wide,
	           'agent', installed.agent,
	           'authorizer', installed.authorizer)
	          FROM integration_installation installed
	         WHERE installed.integration_id = integration.integration_id
	           AND installed.org_id = integration.org_id)`

// CreateIntegration records one configured installation.
//
// The Relay is not read first and then written against: the composite foreign key means
// the insert itself fails when it belongs to another organization, so a request naming
// another tenant's Relay is refused by the database rather than by a check that has to be
// remembered at every call site.
func (p *Database) CreateIntegration(
	ctx context.Context, principal authz.Principal, organization tenancy.Organization,
	wanted integrations.NewIntegration,
) (integrations.Integration, error) {
	return audited(ctx, p, principal, organization, audit.ActionIntegrationCreated,
		func(ctx context.Context, transaction pgx.Tx) (
			integrations.Integration, audit.Target, audit.Detail, error,
		) {
			configuration, err := json.Marshal(orEmptyConfiguration(wanted.Configuration))
			if err != nil {
				return integrations.Integration{}, audit.Target{}, nil,
					fmt.Errorf("encoding configuration: %w", err)
			}

			// A pre-creation probe's judgement lands with the row itself, so a
			// credential-bearing Integration is born verified in one transaction: there
			// is no moment where it exists with a checked credential and an unchecked
			// status.
			var status *string
			verified := false
			grants := []string{}
			if wanted.Verification != nil {
				value := wanted.Verification.Status.String()
				status = &value
				verified = wanted.Verification.Status == integrations.StatusVerified
				if verified {
					grants = orEmptyGrants(wanted.Verification.Grants)
				}
			}

			if err := validateCredentialEnvelope(wanted.CredentialSealed); err != nil {
				return integrations.Integration{}, audit.Target{}, nil, err
			}
			row := transaction.QueryRow(ctx, `
				INSERT INTO integration (integration_id, org_id, integration_type_id, name,
				                         configuration, relay_id, webhook_secret_digest,
				                         credential_sealed, verification_status, verified_at,
				                         verification_grants)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9,
				        CASE WHEN $10 THEN now() END, $11)
				RETURNING `+integrationColumns,
				identityOrNew(wanted.ID), organization.String(), int16(wanted.Type), wanted.Name,
				configuration, nullableUUID(wanted.RelayID), wanted.WebhookSecretDigest,
				wanted.CredentialSealed, status, verified, grants)

			created, err := scanIntegration(row, organization.String())
			switch {
			case isForeignKeyViolation(err):
				return integrations.Integration{}, audit.Target{}, nil, integrations.ErrCrossTenant
			case err != nil:
				return integrations.Integration{}, audit.Target{}, nil,
					fmt.Errorf("creating an integration: %w", err)
			}
			// The routing record lands in the SAME transaction, so an Integration that
			// exists is one an inbound event can reach. A workspace another Integration
			// already holds refuses the whole creation rather than leaving a connected
			// integration whose events resolve somewhere else.
			if wanted.Installation != nil {
				if err := recordInstallation(ctx, transaction, organization, created.ID,
					created.Type, *wanted.Installation); err != nil {
					return integrations.Integration{}, audit.Target{}, nil, err
				}
				installed := *wanted.Installation
				created.Installation = &installed
			}

			// The webhook secret is nowhere in the detail and could not be: audit.Detail
			// drops anything named like a credential on the way in.
			return created,
				audit.Target{Kind: audit.TargetIntegration, ID: created.ID.String()},
				audit.Detail{
					"name": created.Name,
					"type": int(created.Type),
				}, nil
		})
}

// IntegrationByID resolves an Integration from its opaque identifier alone and returns
// the organization it belongs to.
//
// This is the ONE integration read that does not take an organization, and it is
// deliberate. An inbound delivery names its Integration and nothing else, because a path
// is chosen by the caller and a caller who could name a tenant could try every tenant.
// The row that is found is itself the authority for the organization: it discovers a
// tenant rather than trusting one.
func (p *Database) IntegrationByID(
	ctx context.Context, id uuid.UUID,
) (integrations.Integration, error) {
	var organization string
	row := p.pool.QueryRow(ctx, `
			SELECT org_id, `+integrationColumns+`
			  FROM integration
			 WHERE integration_id = $1`, id)
	found, err := scanIntegrationWithOrganization(row, &organization)
	if errors.Is(err, pgx.ErrNoRows) {
		return integrations.Integration{}, integrations.ErrUnknown
	}
	if err != nil {
		return integrations.Integration{}, fmt.Errorf("resolving an integration: %w", err)
	}
	found.OrgID = organization
	return found, nil
}

// Integration reads one, scoped to the tenant.
func (p *Database) Integration(
	ctx context.Context, organization tenancy.Organization, id uuid.UUID,
) (integrations.Integration, error) {
	pool, err := p.Pool(organization)
	if err != nil {
		return integrations.Integration{}, err
	}

	row := pool.QueryRow(ctx, `
		SELECT `+integrationColumns+`
		  FROM integration
		 WHERE integration_id = $1 AND org_id = $2`,
		id, organization.String())
	found, err := scanIntegration(row, organization.String())
	if errors.Is(err, pgx.ErrNoRows) {
		return integrations.Integration{}, integrations.ErrUnknown
	}
	if err != nil {
		return integrations.Integration{}, fmt.Errorf("reading an integration: %w", err)
	}
	return found, nil
}

func (p *Database) QueryIntegrations(
	ctx context.Context, principal authz.Principal, organization tenancy.Organization,
	query integrations.Query,
) (integrations.List, error) {
	if !principal.MemberOf(organization) {
		return integrations.List{}, ErrNotAMember
	}
	pool, err := p.Pool(organization)
	if err != nil {
		return integrations.List{}, err
	}
	sortField := query.Sort
	descending := query.Descending
	if sortField == "" {
		sortField = "createdAt"
		descending = true
	}
	if sortField != "createdAt" {
		return integrations.List{}, fmt.Errorf("integration listing cannot order by %q", sortField)
	}
	scope := sortScope(sortField, descending)
	limit := pageLimit(query.Page.Limit)
	cursorAt, cursorID, err := decodeTimeSortCursor(query.Page.After, scope)
	if err != nil {
		return integrations.List{}, integrations.ErrBadCursor
	}

	arguments := []any{organization.String()}
	where := []string{"org_id = $1"}
	add := func(clause string, value any) {
		arguments = append(arguments, value)
		where = append(where, fmt.Sprintf(clause, len(arguments)))
	}

	if query.Type != 0 {
		add("integration_type_id = $%d", int16(query.Type))
	}
	if query.Relay != uuid.Nil {
		add("relay_id = $%d", query.Relay)
	}
	if query.Search != "" {
		add("name ILIKE '%%' || $%d || '%%'", query.Search)
	}
	if query.Disabled != nil {
		if *query.Disabled {
			where = append(where, "disabled")
		} else {
			where = append(where, "NOT disabled")
		}
	}
	if cursorID != nil {
		arguments = append(arguments, *cursorAt, *cursorID)
		comparison := ">"
		if descending {
			comparison = "<"
		}
		where = append(where, fmt.Sprintf(
			"(created_at, integration_id) %s ($%d, $%d)", comparison,
			len(arguments)-1, len(arguments)))
	}
	arguments = append(arguments, limit+1)
	direction := "ASC"
	if descending {
		direction = "DESC"
	}

	rows, err := pool.Query(ctx, fmt.Sprintf(`
		SELECT %s
		  FROM integration
		 WHERE %s
		 ORDER BY created_at %s, integration_id %s
		 LIMIT $%d`,
		integrationColumns, strings.Join(where, "\n   AND "), direction, direction,
		len(arguments)), arguments...)
	if err != nil {
		return integrations.List{}, fmt.Errorf("listing integrations: %w", err)
	}
	defer rows.Close()

	list := integrations.List{Integrations: make([]integrations.Integration, 0, limit)}
	for rows.Next() {
		found, scanErr := scanIntegration(rows, organization.String())
		if scanErr != nil {
			return integrations.List{}, scanErr
		}
		if len(list.Integrations) == limit {
			last := list.Integrations[limit-1]
			list.Next = encodeTimeSortCursor(scope, last.CreatedAt, last.ID)
			break
		}
		list.Integrations = append(list.Integrations, found)
	}
	if err = rows.Err(); err != nil {
		return integrations.List{}, fmt.Errorf("listing integrations: %w", err)
	}
	return list, nil
}

// CountIntegrationsByType reports how many Integrations of each type a tenant has, for
// the catalog's "3 configured" column. Counted by the database rather than by walking a
// bounded page, so the number cannot be silently short.
func (p *Database) CountIntegrationsByType(
	ctx context.Context, principal authz.Principal, organization tenancy.Organization,
) ([]integrations.TypeCount, error) {
	if !principal.MemberOf(organization) {
		return nil, ErrNotAMember
	}
	pool, err := p.Pool(organization)
	if err != nil {
		return nil, err
	}

	rows, err := pool.Query(ctx, `
		SELECT integration_type_id, count(*)
		  FROM integration
		 WHERE org_id = $1
		 GROUP BY integration_type_id`, organization.String())
	if err != nil {
		return nil, fmt.Errorf("counting integrations: %w", err)
	}
	defer rows.Close()

	counts := make([]integrations.TypeCount, 0, 4)
	for rows.Next() {
		var (
			typeID int16
			count  int
		)
		if err := rows.Scan(&typeID, &count); err != nil {
			return nil, fmt.Errorf("scanning an integration count: %w", err)
		}
		counts = append(counts, integrations.TypeCount{
			Type: integrations.TypeID(typeID), Count: count})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("counting integrations: %w", err)
	}
	return counts, nil
}

// ReviseIntegration changes what a PATCH may change.
func (p *Database) ReviseIntegration(
	ctx context.Context, principal authz.Principal, organization tenancy.Organization,
	id uuid.UUID, revision integrations.Revision,
) (integrations.Integration, error) {
	return audited(ctx, p, principal, organization, audit.ActionIntegrationRevised,
		func(ctx context.Context, transaction pgx.Tx) (
			integrations.Integration, audit.Target, audit.Detail, error,
		) {
			var configuration []byte
			var err error
			if revision.Configuration != nil {
				configuration, err = json.Marshal(revision.Configuration)
				if err != nil {
					return integrations.Integration{}, audit.Target{}, nil,
						fmt.Errorf("encoding configuration: %w", err)
				}
			}
			row := transaction.QueryRow(ctx, `
				UPDATE integration
				   SET name          = coalesce($3, name),
				       configuration = coalesce($4, configuration)
				 WHERE integration_id = $1 AND org_id = $2
				RETURNING `+integrationColumns,
				id, organization.String(), revision.Name, configuration)

			revised, err := scanIntegration(row, organization.String())
			switch {
			case errors.Is(err, pgx.ErrNoRows):
				return integrations.Integration{}, audit.Target{}, nil, integrations.ErrUnknown
			case err != nil:
				return integrations.Integration{}, audit.Target{}, nil,
					fmt.Errorf("revising an integration: %w", err)
			}
			return revised,
				audit.Target{Kind: audit.TargetIntegration, ID: id.String()},
				audit.Detail{
					"nameChanged":          revision.Name != nil,
					"configurationChanged": revision.Configuration != nil,
				}, nil
		})
}

// SetIntegrationDisabled turns an Integration off or back on without deleting it, so an
// operator can stop using a source without losing the record of what it produced.
func (p *Database) SetIntegrationDisabled(
	ctx context.Context, principal authz.Principal, organization tenancy.Organization,
	id uuid.UUID, disabled bool,
) error {
	_, err := audited(ctx, p, principal, organization, audit.ActionIntegrationEnabled,
		func(ctx context.Context, transaction pgx.Tx) (struct{}, audit.Target, audit.Detail, error) {
			var wasDisabled bool
			err := transaction.QueryRow(ctx, `
				SELECT disabled FROM integration
				 WHERE integration_id = $1 AND org_id = $2
				 FOR UPDATE`, id, organization.String()).Scan(&wasDisabled)
			if errors.Is(err, pgx.ErrNoRows) {
				return struct{}{}, audit.Target{}, nil, integrations.ErrUnknown
			}
			if err != nil {
				return struct{}{}, audit.Target{}, nil,
					fmt.Errorf("changing an integration's disabled state: %w", err)
			}
			if _, err := transaction.Exec(ctx, `
				UPDATE integration SET disabled = $3
				 WHERE integration_id = $1 AND org_id = $2`,
				id, organization.String(), disabled); err != nil {
				return struct{}{}, audit.Target{}, nil,
					fmt.Errorf("changing an integration's disabled state: %w", err)
			}
			return struct{}{},
				audit.Target{Kind: audit.TargetIntegration, ID: id.String()},
				audit.Detail{"before": !wasDisabled, "after": !disabled}, nil
		})
	return err
}

// DeleteIntegration removes an Integration nothing depends on.
//
// The dependents are counted inside the deleting transaction, so an Alert Event arriving between
// the check and the delete serialises on the row rather than racing it. Historical deliveries
// are dependents too; deletion must not erase accepted external evidence.
func (p *Database) DeleteIntegration(
	ctx context.Context, principal authz.Principal, organization tenancy.Organization,
	id uuid.UUID,
) error {
	_, err := audited(ctx, p, principal, organization, audit.ActionIntegrationDeleted,
		func(ctx context.Context, transaction pgx.Tx) (struct{}, audit.Target, audit.Detail, error) {
			var alertEvents, deliveries, jobs, changeEvents, investigations int
			err := transaction.QueryRow(ctx, `
				SELECT (SELECT count(*) FROM alert_event
				         WHERE org_id = $2 AND integration_id = $1),
				       (SELECT count(*) FROM webhook_delivery
				         WHERE org_id = $2 AND integration_id = $1),
				       (SELECT count(*) FROM relay_job
				         WHERE org_id = $2 AND integration_id = $1),
				       (SELECT count(*) FROM change_event
				         WHERE org_id = $2 AND integration_id = $1),
				       (SELECT count(*) FROM investigation_tool_run
				           WHERE org_id = $2 AND integration_id = $1)`,
				id, organization.String()).Scan(
				&alertEvents, &deliveries, &jobs, &changeEvents, &investigations)
			if err != nil {
				return struct{}{}, audit.Target{}, nil,
					fmt.Errorf("counting an integration's dependents: %w", err)
			}
			if alertEvents+deliveries+jobs+changeEvents+investigations > 0 {
				return struct{}{}, audit.Target{}, nil, fmt.Errorf(
					"%w: %d Alert Events, %d deliveries, %d jobs, %d change events, %d Investigation records",
					integrations.ErrInUse, alertEvents, deliveries, jobs, changeEvents, investigations)
			}

			// Removing the Integration retires its reply obligations atomically; a
			// mapping cannot be removed independently while a reply still uses it.
			if _, err := transaction.Exec(ctx, `
				DELETE FROM slack_reply r USING slack_conversation s
				WHERE r.org_id = $2 AND s.org_id = r.org_id
				  AND s.conversation_id = r.conversation_id AND s.integration_id = $1`,
				id, organization.String()); err != nil {
				return struct{}{}, audit.Target{}, nil, fmt.Errorf("retiring integration replies: %w", err)
			}
			if _, err := transaction.Exec(ctx, `
				DELETE FROM slack_conversation WHERE org_id = $2 AND integration_id = $1`,
				id, organization.String()); err != nil {
				return struct{}{}, audit.Target{}, nil, fmt.Errorf("retiring integration conversations: %w", err)
			}
			if _, err := transaction.Exec(ctx, `
				DELETE FROM integration_installation WHERE org_id = $2 AND integration_id = $1`,
				id, organization.String()); err != nil {
				return struct{}{}, audit.Target{}, nil, fmt.Errorf("retiring integration installation: %w", err)
			}
			tag, err := transaction.Exec(ctx, `
				DELETE FROM integration
				 WHERE integration_id = $1 AND org_id = $2`,
				id, organization.String())
			if err != nil {
				// A dependent created between the count and the delete surfaces as a
				// foreign-key refusal, which is the race answered by the database.
				if isForeignKeyViolation(err) {
					return struct{}{}, audit.Target{}, nil, integrations.ErrInUse
				}
				return struct{}{}, audit.Target{}, nil,
					fmt.Errorf("deleting an integration: %w", err)
			}
			if tag.RowsAffected() == 0 {
				return struct{}{}, audit.Target{}, nil, integrations.ErrUnknown
			}
			return struct{}{},
				audit.Target{Kind: audit.TargetIntegration, ID: id.String()}, nil, nil
		})
	return err
}

// RotateIntegrationWebhookSecret replaces the digest without disturbing identity, so a
// suspected disclosure does not mean recreating the Integration and reconfiguring the
// source. One digest is live at a time; a rotation is a brief outage the operator
// schedules.
func (p *Database) RotateIntegrationWebhookSecret(
	ctx context.Context, principal authz.Principal, organization tenancy.Organization,
	id uuid.UUID, digest []byte,
) error {
	_, err := audited(ctx, p, principal, organization, audit.ActionIntegrationSecretRotated,
		func(ctx context.Context, transaction pgx.Tx) (struct{}, audit.Target, audit.Detail, error) {
			// Guarded on the Integration already carrying one: rotating a secret onto a
			// type that receives no webhooks would create a credential with no user.
			tag, err := transaction.Exec(ctx, `
				UPDATE integration
				   SET webhook_secret_digest = $3
				 WHERE integration_id = $1
				   AND org_id = $2
				   AND webhook_secret_digest IS NOT NULL`,
				id, organization.String(), digest)
			if err != nil {
				return struct{}{}, audit.Target{}, nil,
					fmt.Errorf("rotating a webhook secret: %w", err)
			}
			if tag.RowsAffected() == 0 {
				return struct{}{}, audit.Target{}, nil, integrations.ErrUnknown
			}
			return struct{}{},
				audit.Target{Kind: audit.TargetIntegration, ID: id.String()},
				audit.Detail{"effect": "the previous webhook secret stopped working; there is no overlap window"}, nil
		})
	return err
}

// ReplaceIntegrationCredential swaps the sealed outbound credential, applies the revision
// it travelled with, and records what the probe of the new one established — one
// transaction, so there is no moment where the revision holds without the credential, or
// the new credential sits beside the old one's verification.
//
// Guarded on the Integration already holding one: a credential can be replaced, never
// acquired, because a type that takes one requires it at creation.
func (p *Database) ReplaceIntegrationCredential(
	ctx context.Context, principal authz.Principal, organization tenancy.Organization,
	id uuid.UUID, revision integrations.Revision, sealed []byte,
	verification integrations.Verification, installed *integrations.Installation,
) (integrations.Integration, error) {
	return audited(ctx, p, principal, organization, audit.ActionIntegrationCredentialReplaced,
		func(ctx context.Context, transaction pgx.Tx) (
			integrations.Integration, audit.Target, audit.Detail, error,
		) {
			var configuration []byte
			var err error
			if revision.Configuration != nil {
				if configuration, err = json.Marshal(revision.Configuration); err != nil {
					return integrations.Integration{}, audit.Target{}, nil,
						fmt.Errorf("encoding configuration: %w", err)
				}
			}
			grants := verificationGrants(verification)
			if err := validateCredentialEnvelope(sealed); err != nil {
				return integrations.Integration{}, audit.Target{}, nil, err
			}

			row := transaction.QueryRow(ctx, `
				UPDATE integration
				   SET name                = coalesce($3, name),
				       configuration       = coalesce($4, configuration),
				       credential_sealed   = $5,
				       verification_status = $6,
				       verified_at         = CASE WHEN $6 = 'verified' THEN now() ELSE verified_at END,
				       verification_grants = $7
				 WHERE integration_id = $1
				   AND org_id = $2
				   AND credential_sealed IS NOT NULL
				RETURNING `+integrationColumns,
				id, organization.String(), revision.Name, configuration, sealed,
				verification.Status.String(), grants)

			replaced, err := scanIntegration(row, organization.String())
			switch {
			case errors.Is(err, pgx.ErrNoRows):
				return integrations.Integration{}, audit.Target{}, nil, integrations.ErrUnknown
			case err != nil:
				return integrations.Integration{}, audit.Target{}, nil,
					fmt.Errorf("replacing a credential: %w", err)
			}

			// The routing record moves WITH the credential, in this transaction.
			// Authorizing again can issue a new agent identity, and a credential replaced
			// without its routing refreshed is a live credential answering as an identity
			// it no longer holds — which is how an agent starts replying to itself.
			if installed != nil {
				if err := recordInstallationIn(ctx, transaction, organization, id,
					replaced.Type, *installed); err != nil {
					return integrations.Integration{}, audit.Target{}, nil, err
				}
				copy := *installed
				replaced.Installation = &copy
			}
			return replaced,
				audit.Target{Kind: audit.TargetIntegration, ID: id.String()},
				audit.Detail{"status": replaced.Status.String(), "note": verification.Note}, nil
		})
}

// RecordIntegrationVerification writes what a verify run established onto the record.
func (p *Database) RecordIntegrationVerification(
	ctx context.Context, principal authz.Principal, organization tenancy.Organization,
	id uuid.UUID, verification integrations.Verification,
) (integrations.Integration, error) {
	return audited(ctx, p, principal, organization, audit.ActionIntegrationVerified,
		func(ctx context.Context, transaction pgx.Tx) (
			integrations.Integration, audit.Target, audit.Detail, error,
		) {
			grants := verificationGrants(verification)
			row := transaction.QueryRow(ctx, `
				UPDATE integration
				   SET verification_status = $3,
				       verified_at = CASE WHEN $3 = 'verified' THEN now() ELSE verified_at END,
				       verification_grants = $4
				 WHERE integration_id = $1 AND org_id = $2
				RETURNING `+integrationColumns,
				id, organization.String(), verification.Status.String(), grants)

			verified, err := scanIntegration(row, organization.String())
			if errors.Is(err, pgx.ErrNoRows) {
				return integrations.Integration{}, audit.Target{}, nil, integrations.ErrUnknown
			}
			if err != nil {
				return integrations.Integration{}, audit.Target{}, nil,
					fmt.Errorf("recording a verification: %w", err)
			}
			return verified,
				audit.Target{Kind: audit.TargetIntegration, ID: id.String()},
				audit.Detail{"status": verified.Status.String(), "note": verification.Note}, nil
		})
}

// IntegrationRelayStatus reports a Relay's presence and advertised capabilities, for
// verification. A relay that does not exist in this tenant answers as unbound rather than
// as an error: the verify run's job is to report, and "the relay this names is gone" is a
// report.
func (p *Database) IntegrationRelayStatus(
	ctx context.Context, organization tenancy.Organization, relayID uuid.UUID,
) (integrations.RelayStatus, error) {
	if relayID == uuid.Nil {
		return integrations.RelayStatus{}, nil
	}
	pool, err := p.Pool(organization)
	if err != nil {
		return integrations.RelayStatus{}, err
	}

	var (
		capabilities []byte
		lastSeen     *time.Time
		sessionEnded *time.Time
		revoked      *time.Time
	)
	err = pool.QueryRow(ctx, `
		SELECT capabilities, last_seen_at, session_ended_at, revoked_at
		  FROM relay_registration
		 WHERE org_id = $1 AND registration_id = $2`,
		organization.String(), relayID).
		Scan(&capabilities, &lastSeen, &sessionEnded, &revoked)
	if errors.Is(err, pgx.ErrNoRows) {
		return integrations.RelayStatus{}, nil
	}
	if err != nil {
		return integrations.RelayStatus{}, fmt.Errorf("reading a relay's status: %w", err)
	}

	status := integrations.RelayStatus{Bound: true}
	if names, decodeErr := decodeCapabilityNames(capabilities); decodeErr == nil {
		status.Capabilities = names
	}
	// Connected is derived, never stored: a recent heartbeat, no recorded ending for the
	// current session, and an identity that has not been revoked.
	status.Connected = revoked == nil && sessionEnded == nil && lastSeen != nil &&
		time.Since(*lastSeen) <= LivenessAllowance
	return status, nil
}

// LivenessAllowance is how stale a relay's heartbeat may be before "connected" stops
// being an honest answer. It must equal relay.LivenessAllowance — the session's own idle
// timeout — and a composition-root test asserts the two agree, because this package cannot
// import the one that owns the protocol cadence.
const LivenessAllowance = 45 * time.Second

// decodeCapabilityNames flattens the enrolment attestation's capability list to names.
// The attestation stores [{id, version}] objects; verification compares names.
func decodeCapabilityNames(raw []byte) ([]string, error) {
	if len(raw) == 0 {
		return []string{}, nil
	}
	var attested []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &attested); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(attested))
	for _, one := range attested {
		if one.ID != "" {
			names = append(names, one.ID)
		}
	}
	return names, nil
}

// LastAcceptedDelivery reports when an integration last accepted a webhook delivery, zero
// when it never has.
func (p *Database) LastAcceptedDelivery(
	ctx context.Context, organization tenancy.Organization, id uuid.UUID,
) (time.Time, error) {
	pool, err := p.Pool(organization)
	if err != nil {
		return time.Time{}, err
	}

	var last time.Time
	err = pool.QueryRow(ctx, `
		SELECT received_at
		  FROM webhook_delivery
		 WHERE integration_id = $1 AND org_id = $2
		 ORDER BY received_at DESC
		 LIMIT 1`, id, organization.String()).Scan(&last)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("reading the last accepted delivery: %w", err)
	}
	return last, nil
}

// nullableIntegration holds the columns that may be SQL NULL, so one struct is threaded
// through both scanners rather than pointers being declared twice and getting out of order
// once.
type nullableIntegration struct {
	configuration []byte
	relay         *uuid.UUID
	status        *string
	verifiedAt    *time.Time
	installation  []byte
}

// destinations is the scan target list, in the order integrationColumns names them. A
// method rather than an inline list so the two scanners cannot drift apart.
func (n *nullableIntegration) destinations(found *integrations.Integration) []any {
	return []any{
		&found.ID, &found.Type, &found.Name, &n.configuration,
		&found.WebhookSecretDigest, &found.CredentialSealed, &n.relay, &n.status,
		&n.verifiedAt, &found.VerificationGrants, &found.Disabled, &found.CreatedAt,
		&n.installation,
	}
}

func scanIntegration(row scanned, organization string) (integrations.Integration, error) {
	found := integrations.Integration{OrgID: organization}
	var nullable nullableIntegration
	if err := row.Scan(nullable.destinations(&found)...); err != nil {
		return integrations.Integration{}, err
	}
	return finishIntegration(found, nullable)
}

func scanIntegrationWithOrganization(
	row scanned, organization *string,
) (integrations.Integration, error) {
	var (
		found    integrations.Integration
		nullable nullableIntegration
	)
	if err := row.Scan(append([]any{organization}, nullable.destinations(&found)...)...); err != nil {
		return integrations.Integration{}, err
	}
	return finishIntegration(found, nullable)
}

func finishIntegration(
	found integrations.Integration, nullable nullableIntegration,
) (integrations.Integration, error) {
	if nullable.relay != nil {
		found.RelayID = *nullable.relay
	}
	if nullable.status != nil {
		found.Status = integrations.Status(*nullable.status)
	}
	if nullable.verifiedAt != nil {
		found.VerifiedAt = *nullable.verifiedAt
	}
	if len(nullable.installation) > 0 {
		var installed integrations.Installation
		if err := json.Unmarshal(nullable.installation, &installed); err != nil {
			return integrations.Integration{}, fmt.Errorf("decoding integration installation: %w", err)
		}
		found.Installation = &installed
	}
	if len(nullable.configuration) > 0 {
		if err := json.Unmarshal(nullable.configuration, &found.Configuration); err != nil {
			return integrations.Integration{},
				fmt.Errorf("decoding integration configuration: %w", err)
		}
	}
	if found.Configuration == nil {
		found.Configuration = map[string]any{}
	}
	return found, nil
}

func orEmptyConfiguration(configuration map[string]any) map[string]any {
	if configuration == nil {
		return map[string]any{}
	}
	return configuration
}

func verificationGrants(verification integrations.Verification) []string {
	if verification.Status != integrations.StatusVerified {
		return []string{}
	}
	return orEmptyGrants(verification.Grants)
}

// identityOrNew honors an identity the handler minted before the insert — the sealed
// credential is bound to it — and mints one only for a caller that supplied none.
func identityOrNew(id uuid.UUID) uuid.UUID {
	if id == uuid.Nil {
		return uuid.New()
	}
	return id
}

// nullableUUID renders the zero UUID as SQL NULL, which is what "no Relay serves this"
// means in the column.
func nullableUUID(id uuid.UUID) *uuid.UUID {
	if id == uuid.Nil {
		return nil
	}
	return &id
}

// RecordCredentialUnseal writes the audit event for one credential unseal: a system act
// naming the integration whose credential was opened and the path that opened it. It is
// recorded BEFORE the credential is used — a use that cannot be recorded does not
// happen, for the same reason audited operations roll back with their record.
func (p *Database) RecordCredentialUnseal(
	ctx context.Context, organization tenancy.Organization, id uuid.UUID, purpose string,
) error {
	return p.RecordEvent(ctx, organization, audit.Event{
		Organization: organization.String(),
		Actor:        audit.System("control-plane"),
		Action:       audit.ActionIntegrationCredentialUnsealed,
		Target:       audit.Target{Kind: audit.TargetIntegration, ID: id.String()},
		Outcome:      audit.OutcomeAllowed,
		Detail:       audit.Detail{"purpose": purpose},
	})
}
