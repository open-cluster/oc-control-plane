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
	"github.com/open-cluster/oc-control-plane/internal/authz"
	"github.com/open-cluster/oc-control-plane/internal/integrations"
	"github.com/open-cluster/oc-control-plane/internal/tenancy"
)

// The integrations capability owns its vocabulary; this file is its persistence. The
// contract is asserted here so a drifted method signature is a compile error.
var _ integrations.Store = (*Placements)(nil)

// integrationColumns is every column an Integration is read from, named once. One list
// rather than five copies, because a column added to one query and forgotten in another is
// a field that is silently always zero.
const integrationColumns = `integration_id, integration_type_id, name, configuration,
	       webhook_secret_digest, webhook_secret_fingerprint, webhook_secret_created_at,
	       webhook_secret_rotated_at, credential_sealed, credential_fingerprint,
	       credential_created_at, credential_rotated_at, labels, relay_id, status,
	       last_verified_at, verify_note, verify_grants, disabled_at, created_by,
	       created_at, updated_at`

// CreateIntegration records one configured installation.
//
// The Relay is not read first and then written against: the composite foreign key means
// the insert itself fails when it belongs to another organization, so a request naming
// another tenant's Relay is refused by the database rather than by a check that has to be
// remembered at every call site.
func (p *Placements) CreateIntegration(
	ctx context.Context, principal authz.Principal, organization tenancy.Organization,
	wanted integrations.NewIntegration,
) (integrations.Integration, error) {
	return audited(ctx, p, principal, organization, audit.ActionIntegrationCreated,
		func(ctx context.Context, transaction pgx.Tx) (
			integrations.Integration, audit.Target, audit.Detail, error,
		) {
			labels, err := json.Marshal(orEmptyLabels(wanted.Labels))
			if err != nil {
				return integrations.Integration{}, audit.Target{}, nil,
					fmt.Errorf("encoding labels: %w", err)
			}
			configuration, err := json.Marshal(orEmptyConfiguration(wanted.Configuration))
			if err != nil {
				return integrations.Integration{}, audit.Target{}, nil,
					fmt.Errorf("encoding configuration: %w", err)
			}

			// A pre-creation probe's judgement lands with the row itself, so a
			// credential-bearing Integration is born verified in one transaction: there
			// is no moment where it exists with a checked credential and an unchecked
			// status.
			var (
				status   = int16(integrations.StatusConfigured)
				verified = false
				note     string
				grants   []byte
			)
			if wanted.Verification != nil {
				status = int16(wanted.Verification.Status)
				verified = true
				note = wanted.Verification.Note
				if grants, err = encodedGrants(wanted.Verification.Grants); err != nil {
					return integrations.Integration{}, audit.Target{}, nil, err
				}
			}

			row := transaction.QueryRow(ctx, `
				INSERT INTO integration (integration_id, org_id, integration_type_id, name,
				                         configuration, labels, relay_id,
				                         webhook_secret_digest, webhook_secret_fingerprint,
				                         webhook_secret_created_at,
				                         credential_sealed, credential_fingerprint,
				                         credential_created_at,
				                         status, last_verified_at, verify_note,
				                         verify_grants, created_by)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9,
				        CASE WHEN $8::BYTEA IS NULL THEN NULL ELSE now() END,
				        $10, $11,
				        CASE WHEN $10::BYTEA IS NULL THEN NULL ELSE now() END,
				        $12, CASE WHEN $13 THEN now() END, $14, $15, $16)
				RETURNING `+integrationColumns,
				identityOrNew(wanted.ID), organization.String(), int16(wanted.Type), wanted.Name,
				configuration, labels, nullableUUID(wanted.RelayID),
				wanted.WebhookSecretDigest, nullableText(wanted.WebhookSecretFingerprint),
				wanted.CredentialSealed, nullableText(wanted.CredentialFingerprint),
				status, verified, note, grants, wanted.CreatedBy)

			created, err := scanIntegration(row, organization.String())
			switch {
			case isUniqueViolation(err, "integration_name_is_unique_per_org"):
				return integrations.Integration{}, audit.Target{}, nil, integrations.ErrNameTaken
			case isForeignKeyViolation(err):
				return integrations.Integration{}, audit.Target{}, nil, integrations.ErrCrossTenant
			case err != nil:
				return integrations.Integration{}, audit.Target{}, nil,
					fmt.Errorf("creating an integration: %w", err)
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

// IntegrationByID resolves an Integration from its opaque identifier alone, across every
// placement this deployment serves, and returns the organization it belongs to.
//
// This is the ONE integration read that does not take an organization, and it is
// deliberate. An inbound delivery names its Integration and nothing else, because a path
// is chosen by the caller and a caller who could name a tenant could try every tenant.
// The row that is found is itself the authority for the organization: it discovers a
// tenant rather than trusting one.
func (p *Placements) IntegrationByID(
	ctx context.Context, id uuid.UUID,
) (integrations.Integration, error) {
	for _, name := range p.names() {
		var organization string
		row := p.pools[name].QueryRow(ctx, `
			SELECT org_id, `+integrationColumns+`
			  FROM integration
			 WHERE integration_id = $1`, id)
		found, err := scanIntegrationWithOrganization(row, &organization)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			// A placement that cannot be read is reported rather than skipped. Continuing
			// would turn one database's outage into "this integration does not exist".
			return integrations.Integration{},
				fmt.Errorf("resolving an integration in placement %q: %w", name, err)
		}
		found.OrgID = organization
		return found, nil
	}
	return integrations.Integration{}, integrations.ErrUnknown
}

// Integration reads one, scoped to the tenant.
func (p *Placements) Integration(
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

// QueryIntegrations returns an organization's Integrations, narrowed and paged by the
// database, newest first.
func (p *Placements) QueryIntegrations(
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
	limit := pageLimit(query.Page.Limit)
	cursorAt, cursorID, err := decodeCursor(query.Page.After)
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
			where = append(where, "disabled_at IS NOT NULL")
		} else {
			where = append(where, "disabled_at IS NULL")
		}
	}
	if cursorID != nil {
		arguments = append(arguments, *cursorAt, *cursorID)
		where = append(where, fmt.Sprintf(
			"(created_at, integration_id) < ($%d, $%d)", len(arguments)-1, len(arguments)))
	}
	arguments = append(arguments, limit+1)

	rows, err := pool.Query(ctx, fmt.Sprintf(`
		SELECT %s
		  FROM integration
		 WHERE %s
		 ORDER BY created_at DESC, integration_id DESC
		 LIMIT $%d`,
		integrationColumns, strings.Join(where, "\n   AND "), len(arguments)), arguments...)
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
			list.Next = encodeCursor(last.CreatedAt, last.ID)
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
func (p *Placements) CountIntegrationsByType(
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

// ReviseIntegration changes what a PATCH may change: the name, the configuration, the
// labels. Identity, type, relay binding and secrets are left alone by construction.
func (p *Placements) ReviseIntegration(
	ctx context.Context, principal authz.Principal, organization tenancy.Organization,
	id uuid.UUID, revision integrations.Revision,
) (integrations.Integration, error) {
	return audited(ctx, p, principal, organization, audit.ActionIntegrationRevised,
		func(ctx context.Context, transaction pgx.Tx) (
			integrations.Integration, audit.Target, audit.Detail, error,
		) {
			var (
				configuration []byte
				labels        []byte
				err           error
			)
			if revision.Configuration != nil {
				configuration, err = json.Marshal(revision.Configuration)
				if err != nil {
					return integrations.Integration{}, audit.Target{}, nil,
						fmt.Errorf("encoding configuration: %w", err)
				}
			}
			if revision.Labels != nil {
				labels, err = json.Marshal(revision.Labels)
				if err != nil {
					return integrations.Integration{}, audit.Target{}, nil,
						fmt.Errorf("encoding labels: %w", err)
				}
			}

			row := transaction.QueryRow(ctx, `
				UPDATE integration
				   SET name          = coalesce($3, name),
				       configuration = coalesce($4, configuration),
				       labels        = coalesce($5, labels),
				       updated_at    = now()
				 WHERE integration_id = $1 AND org_id = $2
				RETURNING `+integrationColumns,
				id, organization.String(), revision.Name, configuration, labels)

			revised, err := scanIntegration(row, organization.String())
			switch {
			case errors.Is(err, pgx.ErrNoRows):
				return integrations.Integration{}, audit.Target{}, nil, integrations.ErrUnknown
			case isUniqueViolation(err, "integration_name_is_unique_per_org"):
				return integrations.Integration{}, audit.Target{}, nil, integrations.ErrNameTaken
			case err != nil:
				return integrations.Integration{}, audit.Target{}, nil,
					fmt.Errorf("revising an integration: %w", err)
			}
			return revised,
				audit.Target{Kind: audit.TargetIntegration, ID: id.String()},
				audit.Detail{
					"nameChanged":          revision.Name != nil,
					"configurationChanged": revision.Configuration != nil,
					"labelsChanged":        revision.Labels != nil,
				}, nil
		})
}

// SetIntegrationDisabled turns an Integration off or back on without deleting it, so an
// operator can stop using a source without losing the record of what it produced.
func (p *Placements) SetIntegrationDisabled(
	ctx context.Context, principal authz.Principal, organization tenancy.Organization,
	id uuid.UUID, disabled bool,
) error {
	_, err := audited(ctx, p, principal, organization, audit.ActionIntegrationEnabled,
		func(ctx context.Context, transaction pgx.Tx) (struct{}, audit.Target, audit.Detail, error) {
			var wasDisabled *time.Time
			err := transaction.QueryRow(ctx, `
				UPDATE integration
				   SET disabled_at = CASE WHEN $3 THEN coalesce(disabled_at, now()) END,
				       updated_at  = now()
				 WHERE integration_id = $1 AND org_id = $2
				RETURNING (SELECT disabled_at FROM integration previous
				            WHERE previous.integration_id = integration.integration_id)`,
				id, organization.String(), disabled).Scan(&wasDisabled)
			if errors.Is(err, pgx.ErrNoRows) {
				return struct{}{}, audit.Target{}, nil, integrations.ErrUnknown
			}
			if err != nil {
				return struct{}{}, audit.Target{}, nil,
					fmt.Errorf("changing an integration's disabled state: %w", err)
			}
			return struct{}{},
				audit.Target{Kind: audit.TargetIntegration, ID: id.String()},
				audit.Detail{"before": wasDisabled == nil, "after": !disabled}, nil
		})
	return err
}

// DeleteIntegration removes an Integration nothing depends on.
//
// The dependents are counted inside the deleting transaction, so a signal arriving between
// the check and the delete serialises on the row rather than racing it. Deliveries are
// deliberately NOT a dependent: they cascade with the Integration, because a delivery
// record's whole subject is the Integration it belongs to.
func (p *Placements) DeleteIntegration(
	ctx context.Context, principal authz.Principal, organization tenancy.Organization,
	id uuid.UUID,
) error {
	_, err := audited(ctx, p, principal, organization, audit.ActionIntegrationDeleted,
		func(ctx context.Context, transaction pgx.Tx) (struct{}, audit.Target, audit.Detail, error) {
			var signals, jobs, ledger, investigations int
			err := transaction.QueryRow(ctx, `
				SELECT (SELECT count(*) FROM signal
				         WHERE org_id = $2 AND integration_id = $1),
				       (SELECT count(*) FROM relay_job
				         WHERE org_id = $2 AND integration_id = $1),
				       (SELECT count(*) FROM change_ledger
				         WHERE org_id = $2 AND integration_id = $1),
				       (SELECT count(*) FROM investigation
				         WHERE org_id = $2 AND integration_id = $1)
				       + (SELECT count(*) FROM investigation_source
				           WHERE org_id = $2 AND integration_id = $1)
				       + (SELECT count(*) FROM investigation_tool_run
				           WHERE org_id = $2 AND integration_id = $1)`,
				id, organization.String()).Scan(&signals, &jobs, &ledger, &investigations)
			if err != nil {
				return struct{}{}, audit.Target{}, nil,
					fmt.Errorf("counting an integration's dependents: %w", err)
			}
			if signals+jobs+ledger+investigations > 0 {
				return struct{}{}, audit.Target{}, nil, fmt.Errorf(
					"%w: %d signals, %d jobs, %d change-ledger entries, %d investigation records",
					integrations.ErrInUse, signals, jobs, ledger, investigations)
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
func (p *Placements) RotateIntegrationWebhookSecret(
	ctx context.Context, principal authz.Principal, organization tenancy.Organization,
	id uuid.UUID, digest []byte, fingerprint string,
) error {
	_, err := audited(ctx, p, principal, organization, audit.ActionIntegrationSecretRotated,
		func(ctx context.Context, transaction pgx.Tx) (struct{}, audit.Target, audit.Detail, error) {
			// Guarded on the Integration already carrying one: rotating a secret onto a
			// type that receives no webhooks would create a credential with no user.
			tag, err := transaction.Exec(ctx, `
				UPDATE integration
				   SET webhook_secret_digest      = $3,
				       webhook_secret_fingerprint = $4,
				       webhook_secret_rotated_at  = now(),
				       updated_at                 = now()
				 WHERE integration_id = $1
				   AND org_id = $2
				   AND webhook_secret_digest IS NOT NULL`,
				id, organization.String(), digest, fingerprint)
			if err != nil {
				return struct{}{}, audit.Target{}, nil,
					fmt.Errorf("rotating a webhook secret: %w", err)
			}
			if tag.RowsAffected() == 0 {
				return struct{}{}, audit.Target{}, nil, integrations.ErrUnknown
			}
			return struct{}{},
				audit.Target{Kind: audit.TargetIntegration, ID: id.String()},
				audit.Detail{
					// The fingerprint is an identity rather than a secret, so it is on
					// the record: "which secret is live now" is exactly what somebody
					// reading this event later needs. The key carries no credential word,
					// because Detail.Safe drops those on the way in.
					"fingerprint": fingerprint,
					"effect": "the previous webhook secret stopped working; " +
						"there is no overlap window",
				}, nil
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
func (p *Placements) ReplaceIntegrationCredential(
	ctx context.Context, principal authz.Principal, organization tenancy.Organization,
	id uuid.UUID, revision integrations.Revision, sealed []byte, fingerprint string,
	verification integrations.Verification,
) (integrations.Integration, error) {
	return audited(ctx, p, principal, organization, audit.ActionIntegrationCredentialReplaced,
		func(ctx context.Context, transaction pgx.Tx) (
			integrations.Integration, audit.Target, audit.Detail, error,
		) {
			var (
				configuration []byte
				labels        []byte
				err           error
			)
			if revision.Configuration != nil {
				if configuration, err = json.Marshal(revision.Configuration); err != nil {
					return integrations.Integration{}, audit.Target{}, nil,
						fmt.Errorf("encoding configuration: %w", err)
				}
			}
			if revision.Labels != nil {
				if labels, err = json.Marshal(revision.Labels); err != nil {
					return integrations.Integration{}, audit.Target{}, nil,
						fmt.Errorf("encoding labels: %w", err)
				}
			}
			grants, err := encodedGrants(verification.Grants)
			if err != nil {
				return integrations.Integration{}, audit.Target{}, nil, err
			}

			row := transaction.QueryRow(ctx, `
				UPDATE integration
				   SET name                   = coalesce($3, name),
				       configuration          = coalesce($4, configuration),
				       labels                 = coalesce($5, labels),
				       credential_sealed      = $6,
				       credential_fingerprint = $7,
				       credential_rotated_at  = now(),
				       status                 = $8,
				       last_verified_at       = now(),
				       verify_note            = $9,
				       verify_grants          = $10,
				       updated_at             = now()
				 WHERE integration_id = $1
				   AND org_id = $2
				   AND credential_sealed IS NOT NULL
				RETURNING `+integrationColumns,
				id, organization.String(), revision.Name, configuration, labels,
				sealed, fingerprint, int16(verification.Status), verification.Note,
				grants)

			replaced, err := scanIntegration(row, organization.String())
			switch {
			case errors.Is(err, pgx.ErrNoRows):
				return integrations.Integration{}, audit.Target{}, nil, integrations.ErrUnknown
			case isUniqueViolation(err, "integration_name_is_unique_per_org"):
				return integrations.Integration{}, audit.Target{}, nil, integrations.ErrNameTaken
			case err != nil:
				return integrations.Integration{}, audit.Target{}, nil,
					fmt.Errorf("replacing a credential: %w", err)
			}
			return replaced,
				audit.Target{Kind: audit.TargetIntegration, ID: id.String()},
				audit.Detail{
					"fingerprint": fingerprint,
					"status":      replaced.Status.String(),
				}, nil
		})
}

// RecordIntegrationVerification writes what a verify run established onto the record.
func (p *Placements) RecordIntegrationVerification(
	ctx context.Context, principal authz.Principal, organization tenancy.Organization,
	id uuid.UUID, verification integrations.Verification,
) (integrations.Integration, error) {
	return audited(ctx, p, principal, organization, audit.ActionIntegrationVerified,
		func(ctx context.Context, transaction pgx.Tx) (
			integrations.Integration, audit.Target, audit.Detail, error,
		) {
			grants, err := encodedGrants(verification.Grants)
			if err != nil {
				return integrations.Integration{}, audit.Target{}, nil, err
			}
			row := transaction.QueryRow(ctx, `
				UPDATE integration
				   SET status           = $3,
				       last_verified_at = now(),
				       verify_note      = $4,
				       verify_grants    = $5,
				       updated_at       = now()
				 WHERE integration_id = $1 AND org_id = $2
				RETURNING `+integrationColumns,
				id, organization.String(), int16(verification.Status), verification.Note,
				grants)

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
func (p *Placements) IntegrationRelayStatus(
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
		ID string `json:"capability_id"`
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
func (p *Placements) LastAcceptedDelivery(
	ctx context.Context, organization tenancy.Organization, id uuid.UUID,
) (time.Time, error) {
	pool, err := p.Pool(organization)
	if err != nil {
		return time.Time{}, err
	}

	var last time.Time
	err = pool.QueryRow(ctx, `
		SELECT received_at
		  FROM integration_delivery
		 WHERE integration_id = $1 AND org_id = $2 AND outcome = 1
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
	configuration         []byte
	webhookFingerprint    *string
	webhookCreatedAt      *time.Time
	webhookRotatedAt      *time.Time
	credentialFingerprint *string
	credentialCreatedAt   *time.Time
	credentialRotatedAt   *time.Time
	labels                []byte
	relay                 *uuid.UUID
	lastVerifiedAt        *time.Time
	verifyGrants          []byte
	disabledAt            *time.Time
}

// destinations is the scan target list, in the order integrationColumns names them. A
// method rather than an inline list so the two scanners cannot drift apart.
func (n *nullableIntegration) destinations(found *integrations.Integration) []any {
	return []any{
		&found.ID, &found.Type, &found.Name, &n.configuration,
		&found.WebhookSecretDigest, &n.webhookFingerprint, &n.webhookCreatedAt,
		&n.webhookRotatedAt, &found.CredentialSealed, &n.credentialFingerprint,
		&n.credentialCreatedAt, &n.credentialRotatedAt, &n.labels, &n.relay,
		&found.Status, &n.lastVerifiedAt,
		&found.VerifyNote, &n.verifyGrants, &n.disabledAt, &found.CreatedBy,
		&found.CreatedAt, &found.UpdatedAt,
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
	if nullable.disabledAt != nil {
		found.DisabledAt = *nullable.disabledAt
	}
	if nullable.lastVerifiedAt != nil {
		found.LastVerifiedAt = *nullable.lastVerifiedAt
	}
	if nullable.webhookFingerprint != nil {
		found.WebhookSecret.Fingerprint = *nullable.webhookFingerprint
	}
	if nullable.webhookCreatedAt != nil {
		found.WebhookSecret.CreatedAt = *nullable.webhookCreatedAt
	}
	if nullable.webhookRotatedAt != nil {
		found.WebhookSecret.RotatedAt = *nullable.webhookRotatedAt
	}
	if nullable.credentialFingerprint != nil {
		found.Credential.Fingerprint = *nullable.credentialFingerprint
	}
	if nullable.credentialCreatedAt != nil {
		found.Credential.CreatedAt = *nullable.credentialCreatedAt
	}
	if nullable.credentialRotatedAt != nil {
		found.Credential.RotatedAt = *nullable.credentialRotatedAt
	}
	if len(nullable.verifyGrants) > 0 {
		if err := json.Unmarshal(nullable.verifyGrants, &found.VerifyGrants); err != nil {
			return integrations.Integration{}, fmt.Errorf("decoding verification grants: %w", err)
		}
	}
	if len(nullable.labels) > 0 {
		if err := json.Unmarshal(nullable.labels, &found.Labels); err != nil {
			return integrations.Integration{}, fmt.Errorf("decoding integration labels: %w", err)
		}
	}
	if found.Labels == nil {
		found.Labels = map[string]string{}
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

func orEmptyLabels(labels map[string]string) map[string]string {
	if labels == nil {
		return map[string]string{}
	}
	return labels
}

func orEmptyConfiguration(configuration map[string]any) map[string]any {
	if configuration == nil {
		return map[string]any{}
	}
	return configuration
}

// encodedGrants renders a verification's grants for the JSONB column: SQL NULL when
// none were recorded, so "nothing recorded" and "recorded as empty" stay two facts.
func encodedGrants(grants []string) ([]byte, error) {
	if grants == nil {
		return nil, nil
	}
	encoded, err := json.Marshal(grants)
	if err != nil {
		return nil, fmt.Errorf("encoding verification grants: %w", err)
	}
	return encoded, nil
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
func (p *Placements) RecordCredentialUnseal(
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
