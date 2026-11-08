package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-cluster/oc-control-plane/internal/audit"
	"github.com/open-cluster/oc-control-plane/internal/auth/authz"
	"github.com/open-cluster/oc-control-plane/internal/integrations"
)

var _ integrations.Store = (*Database)(nil)

const integrationColumns = `integration_id, provider, name, configuration,
	       webhook_secret_digest, credential_sealed, relay_id, verification_status,
	       verified_at, verification_grants, disconnected, created_at,
	       (SELECT jsonb_build_object(
	           'key', installed.installation_key,
	           'providerActorId', installed.provider_actor_id)
	          FROM integration_installation installed
	         WHERE installed.integration_id = integration.integration_id
	           AND installed.org_id = integration.org_id)`

func (p *Database) CreateIntegration(
	ctx context.Context, principal authz.Principal, organization uuid.UUID,
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

			row := transaction.QueryRow(ctx, `
				INSERT INTO integration (integration_id, org_id, provider, name,
				                         configuration, relay_id, webhook_secret_digest,
				                         credential_sealed, verification_status, verified_at,
				                         verification_grants)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9,
				        CASE WHEN $10 THEN now() END, $11)
				RETURNING `+integrationColumns,
				identityOrNew(wanted.ID), organization, wanted.Provider, wanted.Name,
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
			if wanted.Installation != nil {
				if err := recordInstallation(ctx, transaction, organization, created.ID,
					created.Provider, *wanted.Installation); err != nil {
					return integrations.Integration{}, audit.Target{}, nil, err
				}
				installed := *wanted.Installation
				created.Installation = &installed
			}

			return created,
				audit.Target{Kind: audit.TargetIntegration, ID: created.ID.String()},
				audit.Detail{
					"name":     created.Name,
					"provider": created.Provider,
				}, nil
		})
}

func (p *Database) IntegrationByID(ctx context.Context, id uuid.UUID,
) (integrations.Integration, error) {

	var organization uuid.UUID
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
	found.OrgID = organization.String()
	return found, nil
}

func (p *Database) Integration(
	ctx context.Context, organization uuid.UUID, id uuid.UUID,
) (integrations.Integration, error) {
	pool, err := p.poolForOrganization(organization)
	if err != nil {
		return integrations.Integration{}, err
	}

	row := pool.QueryRow(ctx, `
		SELECT `+integrationColumns+`
		  FROM integration
		 WHERE integration_id = $1 AND org_id = $2`,
		id, organization)
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
	ctx context.Context, principal authz.Principal, organization uuid.UUID,
	query integrations.Query,
) (integrations.List, error) {
	if principal.Organization() != organization {
		return integrations.List{}, ErrNotAMember
	}
	pool, err := p.poolForOrganization(organization)
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

	arguments := []any{organization}
	predicates := []string{"org_id = $1", "NOT disconnected"}
	add := func(clause string, value any) {
		arguments = append(arguments, value)
		predicates = append(predicates, fmt.Sprintf(clause, len(arguments)))
	}

	if query.Provider != "" {
		add("provider = $%d", query.Provider)
	}
	if query.Relay != uuid.Nil {
		add("relay_id = $%d", query.Relay)
	}
	if query.Search != "" {
		add("name ILIKE '%%' || $%d || '%%'", query.Search)
	}
	if cursorID != nil {
		arguments = append(arguments, *cursorAt, *cursorID)
		comparison := ">"
		if descending {
			comparison = "<"
		}
		predicates = append(predicates, fmt.Sprintf(
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
		integrationColumns, strings.Join(predicates, "\n   AND "), direction, direction,
		len(arguments)), arguments...)
	if err != nil {
		return integrations.List{}, fmt.Errorf("listing integrations: %w", err)
	}
	defer rows.Close()

	list := integrations.List{Integrations: make([]integrations.Integration, 0, limit)}
	for rows.Next() {
		integration, scanErr := scanIntegration(rows, organization.String())
		if scanErr != nil {
			return integrations.List{}, scanErr
		}
		if len(list.Integrations) == limit {
			last := list.Integrations[limit-1]
			list.Next = encodeTimeSortCursor(scope, last.CreatedAt, last.ID)
			break
		}
		list.Integrations = append(list.Integrations, integration)
	}
	if err = rows.Err(); err != nil {
		return integrations.List{}, fmt.Errorf("listing integrations: %w", err)
	}
	return list, nil
}

func (p *Database) CountIntegrationsByProvider(
	ctx context.Context, principal authz.Principal, organization uuid.UUID,
) ([]integrations.ProviderCount, error) {
	if principal.Organization() != organization {
		return nil, ErrNotAMember
	}
	pool, err := p.poolForOrganization(organization)
	if err != nil {
		return nil, err
	}

	rows, err := pool.Query(ctx, `
		SELECT provider, count(*)
		  FROM integration
		 WHERE org_id = $1 AND NOT disconnected
		 GROUP BY provider`, organization)
	if err != nil {
		return nil, fmt.Errorf("counting integrations: %w", err)
	}
	defer rows.Close()

	counts := make([]integrations.ProviderCount, 0, 4)
	for rows.Next() {
		var (
			provider integrations.Provider
			count    int
		)
		if err := rows.Scan(&provider, &count); err != nil {
			return nil, fmt.Errorf("scanning an integration count: %w", err)
		}
		counts = append(counts, integrations.ProviderCount{
			Provider: provider, Count: count})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("counting integrations: %w", err)
	}
	return counts, nil
}

func (p *Database) UpdateIntegration(
	ctx context.Context, principal authz.Principal, organization uuid.UUID,
	id uuid.UUID, update integrations.IntegrationUpdate,
) (integrations.Integration, error) {
	return audited(ctx, p, principal, organization, audit.ActionIntegrationRevised,
		func(ctx context.Context, transaction pgx.Tx) (
			integrations.Integration, audit.Target, audit.Detail, error,
		) {
			integration, err := scanIntegration(transaction.QueryRow(ctx,
				`SELECT `+integrationColumns+` FROM integration WHERE integration_id = $1 AND org_id = $2 FOR UPDATE`, id, organization), organization.String())
			if errors.Is(err, pgx.ErrNoRows) {
				return integrations.Integration{}, audit.Target{}, nil, integrations.ErrUnknown
			}
			if err != nil {
				return integrations.Integration{}, audit.Target{}, nil, err
			}
			if integration.Disconnected {
				return integrations.Integration{}, audit.Target{}, nil, integrations.ErrDisconnected
			}
			configurationChanged := update.Configuration != nil && !reflect.DeepEqual(integration.Configuration, update.Configuration)
			if configurationChanged && len(integration.CredentialSealed) > 0 {
				return integrations.Integration{}, audit.Target{}, nil, integrations.ErrCredentialRequired
			}
			var configuration []byte
			if update.Configuration != nil {
				configuration, err = json.Marshal(update.Configuration)
				if err != nil {
					return integrations.Integration{}, audit.Target{}, nil,
						fmt.Errorf("encoding configuration: %w", err)
				}
			}
			row := transaction.QueryRow(ctx, `
				UPDATE integration
				   SET name          = coalesce($3, name),
				       configuration = coalesce($4, configuration),
				       verification_status = CASE WHEN $5 THEN NULL ELSE verification_status END,
				       verified_at = CASE WHEN $5 THEN NULL ELSE verified_at END,
				       verification_grants = CASE WHEN $5 THEN '{}'::text[] ELSE verification_grants END
				 WHERE integration_id = $1 AND org_id = $2
				RETURNING `+integrationColumns,
				id, organization, update.Name, configuration, configurationChanged)

			updated, err := scanIntegration(row, organization.String())
			switch {
			case errors.Is(err, pgx.ErrNoRows):
				return integrations.Integration{}, audit.Target{}, nil, integrations.ErrUnknown
			case err != nil:
				return integrations.Integration{}, audit.Target{}, nil,
					fmt.Errorf("updating an integration: %w", err)
			}
			return updated,
				audit.Target{Kind: audit.TargetIntegration, ID: id.String()},
				audit.Detail{
					"nameChanged":          integration.Name != updated.Name,
					"configurationChanged": !reflect.DeepEqual(integration.Configuration, updated.Configuration),
				}, nil
		})
}

func (p *Database) DisconnectIntegration(
	ctx context.Context, principal authz.Principal, organization uuid.UUID, id uuid.UUID,
) error {
	_, err := audited(ctx, p, principal, organization, audit.ActionIntegrationDeleted,
		func(ctx context.Context, transaction pgx.Tx) (struct{}, audit.Target, audit.Detail, error) {
			var disconnected bool
			err := transaction.QueryRow(ctx, `
				SELECT disconnected FROM integration
				WHERE integration_id = $1 AND org_id = $2 FOR UPDATE`, id, organization).Scan(&disconnected)
			if errors.Is(err, pgx.ErrNoRows) {
				return struct{}{}, audit.Target{}, nil, integrations.ErrUnknown
			}
			if err != nil {
				return struct{}{}, audit.Target{}, nil, err
			}
			target := audit.Target{Kind: audit.TargetIntegration, ID: id.String()}
			if disconnected {
				return struct{}{}, target, audit.Detail{"alreadyDisconnected": true}, nil
			}
			if _, err = transaction.Exec(ctx, `
				UPDATE integration SET disconnected = true,
				credential_sealed = NULL, webhook_secret_digest = NULL, verification_status = NULL,
				verified_at = NULL, verification_grants = '{}'::text[]
				WHERE integration_id = $1 AND org_id = $2`, id, organization); err != nil {
				return struct{}{}, audit.Target{}, nil, err
			}
			if _, err = transaction.Exec(ctx, `
				DELETE FROM integration_installation
				WHERE integration_id = $1 AND org_id = $2`, id, organization); err != nil {
				return struct{}{}, audit.Target{}, nil, err
			}
			if _, err = transaction.Exec(ctx, `
				UPDATE relay_job
				SET status = CASE WHEN status = 0 THEN 4 ELSE status END,
				terminal_at = CASE WHEN status = 0 THEN now() ELSE terminal_at END,
				cancel_requested_at = coalesce(cancel_requested_at, now())
				WHERE integration_id = $1 AND org_id = $2 AND status IN (0, 1)`, id, organization); err != nil {
				return struct{}{}, audit.Target{}, nil, err
			}
			if _, err = transaction.Exec(ctx, `
				UPDATE slack_message_work
				SET status = 4, lease_owner = '', lease_expires_at = NULL,
				failure_class = 'integration-disconnected', failure_message = 'the integration is disconnected', updated_at = now()
				WHERE integration_id = $1 AND org_id = $2 AND status IN (1, 2, 3)`, id, organization); err != nil {
				return struct{}{}, audit.Target{}, nil, err
			}
			if _, err = transaction.Exec(ctx, `
				UPDATE slack_reply reply
				SET status = 4, lease_owner = NULL, leased_until = NULL, note = 'the integration is disconnected', updated_at = now()
				FROM slack_conversation conversation WHERE reply.org_id = $2 AND conversation.org_id = reply.org_id
				AND conversation.conversation_id = reply.conversation_id AND conversation.integration_id = $1 AND reply.status IN (1, 2)`, id, organization); err != nil {
				return struct{}{}, audit.Target{}, nil, err
			}
			return struct{}{}, target, audit.Detail{"disconnected": true}, nil
		})
	return err
}

func (p *Database) RotateIntegrationWebhookSecret(
	ctx context.Context, principal authz.Principal, organization uuid.UUID,
	id uuid.UUID, digest []byte,
) error {
	_, err := audited(ctx, p, principal, organization, audit.ActionIntegrationSecretRotated,
		func(ctx context.Context, transaction pgx.Tx) (struct{}, audit.Target, audit.Detail, error) {
			tag, err := transaction.Exec(ctx, `
				UPDATE integration
				   SET webhook_secret_digest = $3
				 WHERE integration_id = $1
				   AND org_id = $2
				   AND NOT disconnected
				   AND webhook_secret_digest IS NOT NULL`,
				id, organization, digest)
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

func (p *Database) ReplaceIntegrationCredential(
	ctx context.Context, principal authz.Principal, organization uuid.UUID,
	expectedIntegration integrations.Integration, update integrations.IntegrationUpdate, sealed []byte,
	verification integrations.Verification, installed *integrations.Installation,
) (integrations.Integration, error) {
	id := expectedIntegration.ID
	return audited(ctx, p, principal, organization, audit.ActionIntegrationCredentialReplaced,
		func(ctx context.Context, transaction pgx.Tx) (
			integrations.Integration, audit.Target, audit.Detail, error,
		) {
			var configuration []byte
			var err error
			if update.Configuration != nil {
				if configuration, err = json.Marshal(update.Configuration); err != nil {
					return integrations.Integration{}, audit.Target{}, nil,
						fmt.Errorf("encoding configuration: %w", err)
				}
			}
			if err := checkExpectedIntegration(ctx, transaction, organization, expectedIntegration); err != nil {
				return integrations.Integration{}, audit.Target{}, nil, err
			}
			grants := verificationGrants(verification)
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
				RETURNING `+integrationColumns,
				id, organization, update.Name, configuration, sealed,
				verification.Status.String(), grants)

			replaced, err := scanIntegration(row, organization.String())
			switch {
			case errors.Is(err, pgx.ErrNoRows):
				return integrations.Integration{}, audit.Target{}, nil, integrations.ErrUnknown
			case err != nil:
				return integrations.Integration{}, audit.Target{}, nil,
					fmt.Errorf("replacing a credential: %w", err)
			}

			if installed != nil {
				if err := recordInstallationIn(ctx, transaction, organization, id,
					replaced.Provider, *installed); err != nil {
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

func (p *Database) RecordIntegrationVerification(
	ctx context.Context, principal authz.Principal, organization uuid.UUID,
	expectedIntegration integrations.Integration, verification integrations.Verification,
) (integrations.Integration, error) {
	id := expectedIntegration.ID
	return audited(ctx, p, principal, organization, audit.ActionIntegrationVerified,
		func(ctx context.Context, transaction pgx.Tx) (
			integrations.Integration, audit.Target, audit.Detail, error,
		) {
			if err := checkExpectedIntegration(ctx, transaction, organization, expectedIntegration); err != nil {
				return integrations.Integration{}, audit.Target{}, nil, err
			}
			grants := verificationGrants(verification)
			row := transaction.QueryRow(ctx, `
				UPDATE integration
				   SET verification_status = $3,
				       verified_at = CASE WHEN $3 = 'verified' THEN now() ELSE verified_at END,
				       verification_grants = $4
				 WHERE integration_id = $1 AND org_id = $2
				RETURNING `+integrationColumns,
				id, organization, verification.Status.String(), grants)

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

func (p *Database) IntegrationRelayStatus(
	ctx context.Context, organization uuid.UUID, relayID uuid.UUID,
) (integrations.RelayStatus, error) {
	if relayID == uuid.Nil {
		return integrations.RelayStatus{}, nil
	}
	pool, err := p.poolForOrganization(organization)
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
		organization, relayID).
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
	status.Connected = revoked == nil && sessionEnded == nil && lastSeen != nil &&
		time.Since(*lastSeen) <= LivenessAllowance
	return status, nil
}

const LivenessAllowance = 45 * time.Second

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

func (p *Database) LastAcceptedDelivery(
	ctx context.Context, organization uuid.UUID, id uuid.UUID,
) (time.Time, error) {
	pool, err := p.poolForOrganization(organization)
	if err != nil {
		return time.Time{}, err
	}

	var last time.Time
	err = pool.QueryRow(ctx, `
		SELECT received_at
		  FROM webhook_delivery
		 WHERE integration_id = $1 AND org_id = $2
		 ORDER BY received_at DESC
		 LIMIT 1`, id, organization).Scan(&last)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("reading the last accepted delivery: %w", err)
	}
	return last, nil
}

type nullableIntegration struct {
	configuration []byte
	relay         *uuid.UUID
	status        *string
	verifiedAt    *time.Time
	installation  []byte
}

func (n *nullableIntegration) destinations(found *integrations.Integration) []any {
	return []any{
		&found.ID, &found.Provider, &found.Name, &n.configuration,
		&found.WebhookSecretDigest, &found.CredentialSealed, &n.relay, &n.status,
		&n.verifiedAt, &found.VerificationGrants, &found.Disconnected, &found.CreatedAt,
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

func scanIntegrationWithOrganization(row scanned, organization *uuid.UUID,
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

func identityOrNew(id uuid.UUID) uuid.UUID {
	if id == uuid.Nil {
		return uuid.New()
	}
	return id
}

func nullableUUID(id uuid.UUID) *uuid.UUID {
	if id == uuid.Nil {
		return nil
	}
	return &id
}

func (p *Database) RecordCredentialUnseal(
	ctx context.Context, organization uuid.UUID, id uuid.UUID, purpose string,
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

// Lock the checked access settings until the mutation and its audit record commit.
func checkExpectedIntegration(ctx context.Context, transaction pgx.Tx, organization uuid.UUID, expectedIntegration integrations.Integration) error {
	integration, err := scanIntegration(transaction.QueryRow(ctx,
		`SELECT `+integrationColumns+` FROM integration WHERE integration_id = $1 AND org_id = $2 FOR UPDATE`, expectedIntegration.ID, organization), organization.String())
	if errors.Is(err, pgx.ErrNoRows) {
		return integrations.ErrUnknown
	}
	if err != nil {
		return err
	}
	if integration.Disconnected {
		return integrations.ErrDisconnected
	}
	if integration.Status != expectedIntegration.Status || !integration.VerifiedAt.Equal(expectedIntegration.VerifiedAt) ||
		!reflect.DeepEqual(integration.VerificationGrants, expectedIntegration.VerificationGrants) ||
		!bytes.Equal(integration.WebhookSecretDigest, expectedIntegration.WebhookSecretDigest) ||
		integration.Disconnected != expectedIntegration.Disconnected || integration.RelayID != expectedIntegration.RelayID ||
		!bytes.Equal(integration.CredentialSealed, expectedIntegration.CredentialSealed) ||
		!reflect.DeepEqual(integration.Configuration, expectedIntegration.Configuration) ||
		!reflect.DeepEqual(integration.Installation, expectedIntegration.Installation) {
		return integrations.ErrConflict
	}
	return nil
}

// Serialize database admission with Disconnect without retaining locks during provider calls.
func lockActiveIntegration(ctx context.Context, transaction pgx.Tx, organization, id uuid.UUID) error {
	var disconnected bool
	err := transaction.QueryRow(ctx, `SELECT disconnected FROM integration
		WHERE org_id = $1 AND integration_id = $2 FOR SHARE`, organization, id).Scan(&disconnected)
	if errors.Is(err, pgx.ErrNoRows) {
		return integrations.ErrUnknown
	}
	if err != nil {
		return err
	}
	if disconnected {
		return integrations.ErrDisconnected
	}
	return nil
}
