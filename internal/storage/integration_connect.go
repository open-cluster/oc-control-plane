package storage

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/open-cluster/oc-control-plane/internal/integrations"
	"github.com/open-cluster/oc-control-plane/internal/tenancy"
)

// Persistence for the provider installation flow. It is the sign-in flow's shape for the
// same reason: only a digest is stored, redemption is a conditional update rather than a
// read followed by a write, and the row that is found is itself the authority for the
// tenant.

// IntegrationConfiguredAs resolves the Integration of one type whose configuration is
// exactly this one. JSONB equality is the comparison rather than a key-by-key check in Go:
// Postgres normalises both sides, so "the same installation" is one predicate the database
// can answer instead of a page walk this process would have to bound.
func (p *Database) IntegrationConfiguredAs(
	ctx context.Context, organization tenancy.Organization, typeID integrations.TypeID,
	configuration map[string]any,
) (integrations.Integration, error) {
	pool, err := p.Pool(organization)
	if err != nil {
		return integrations.Integration{}, err
	}
	encoded, err := json.Marshal(orEmptyConfiguration(configuration))
	if err != nil {
		return integrations.Integration{}, fmt.Errorf("encoding configuration: %w", err)
	}

	row := pool.QueryRow(ctx, `
		SELECT `+integrationColumns+`
		  FROM integration
		 WHERE org_id = $1 AND integration_type_id = $2 AND configuration = $3::JSONB
		 ORDER BY created_at
		 LIMIT 1`,
		organization.String(), int16(typeID), encoded)
	found, err := scanIntegration(row, organization.String())
	if errors.Is(err, pgx.ErrNoRows) {
		return integrations.Integration{}, integrations.ErrUnknown
	}
	if err != nil {
		return integrations.Integration{}, fmt.Errorf("resolving an integration by its "+
			"configuration: %w", err)
	}
	return found, nil
}

// StartConnectFlow records an installation flow so its state can be checked when the
// browser comes back.
//
// It also clears the spent rows first. A started connect that nobody finishes is the
// ordinary case — somebody closed the tab — and without this the table grows by a row per
// abandoned attempt forever. It is done here rather than by a worker because the cheapest
// honest moment to clear a table is while writing to it, and a background sweeper for a
// handful of rows would be a process to operate for no gain.
func (p *Database) StartConnectFlow(
	ctx context.Context, organization tenancy.Organization, flow integrations.ConnectFlow,
	state string,
) error {
	pool, err := p.Pool(organization)
	if err != nil {
		return err
	}
	digest := sha256.Sum256([]byte(state))

	if _, err := pool.Exec(ctx, `
		DELETE FROM integration_connect_flow
		 WHERE expires_at <= now() OR consumed_at IS NOT NULL`); err != nil {
		return fmt.Errorf("clearing spent connect flows: %w", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO integration_connect_flow (flow_id, org_id, integration_type_id,
		                                      principal, state_digest, return_to, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		flow.ID, organization.String(), int16(flow.Type), flow.Principal, digest[:],
		flow.ReturnTo, flow.ExpiresAt); err != nil {
		return fmt.Errorf("starting a connect flow: %w", err)
	}
	return nil
}

// RedeemConnectFlow consumes an installation flow exactly once and returns what it
// recorded.
//
// The consumption is a conditional UPDATE, so two presentations of the same callback cannot
// both win. An unknown state, an expired one and one already redeemed are the same refusal:
// telling them apart is how a caller learns which half of a guess landed.
//
// The callback carries state and nothing that names a tenant. The consumed row supplies
// the organization boundary for the remaining work.
func (p *Database) RedeemConnectFlow(
	ctx context.Context, state string,
) (integrations.ConnectFlow, error) {
	digest := sha256.Sum256([]byte(state))

	var (
		flow   integrations.ConnectFlow
		typeID int16
	)
	err := p.pool.QueryRow(ctx, `
			UPDATE integration_connect_flow
			   SET consumed_at = now()
			 WHERE state_digest = $1 AND consumed_at IS NULL AND expires_at > now()
			RETURNING flow_id, org_id, integration_type_id, principal, return_to, expires_at`,
		digest[:]).Scan(&flow.ID, &flow.Organization, &typeID, &flow.Principal,
		&flow.ReturnTo, &flow.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return integrations.ConnectFlow{}, integrations.ErrConnectFlowUnknown
	}
	if err != nil {
		return integrations.ConnectFlow{}, fmt.Errorf("redeeming a connect flow: %w", err)
	}
	flow.Type = integrations.TypeID(typeID)
	return flow, nil
}
