package storage

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/open-cluster/oc-control-plane/internal/auth/tenancy"
	"github.com/open-cluster/oc-control-plane/internal/integrations"
)

// StartConnectFlow removes expired state opportunistically before storing a new digest.
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
		 WHERE expires_at <= now()`); err != nil {
		return fmt.Errorf("clearing spent connect flows: %w", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO integration_connect_flow (org_id, provider, principal,
		                                      state_digest, return_to, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		organization.String(), flow.Provider, flow.Principal, digest[:],
		flow.ReturnTo, flow.ExpiresAt); err != nil {
		return fmt.Errorf("starting a connect flow: %w", err)
	}
	return nil
}

// RedeemConnectFlow atomically deletes live state and supplies its Organization boundary.
func (p *Database) RedeemConnectFlow(
	ctx context.Context, state string,
) (integrations.ConnectFlow, error) {
	digest := sha256.Sum256([]byte(state))

	var flow integrations.ConnectFlow
	err := p.pool.QueryRow(ctx, `
			DELETE FROM integration_connect_flow
			 WHERE state_digest = $1 AND expires_at > now()
			RETURNING org_id, provider, principal, return_to, expires_at`,
		digest[:]).Scan(&flow.Organization, &flow.Provider, &flow.Principal,
		&flow.ReturnTo, &flow.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return integrations.ConnectFlow{}, integrations.ErrConnectFlowUnknown
	}
	if err != nil {
		return integrations.ConnectFlow{}, fmt.Errorf("redeeming a connect flow: %w", err)
	}
	return flow, nil
}
