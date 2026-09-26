package storage

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/open-cluster/oc-control-plane/internal/auth/authz"
)

type DeploymentSignInFlow struct {
	CodeVerifier string
	Nonce        string
	ReturnTo     string
	ExpiresAt    time.Time
}

var ErrFlowUnknown = errors.New("sign-in flow unknown")

func (p *Database) StartDeploymentSignIn(ctx context.Context, flow DeploymentSignInFlow, state string) error {
	digest := sha256.Sum256([]byte(state))
	if _, err := p.pool.Exec(ctx, `DELETE FROM oidc_sign_in_flow WHERE expires_at<=now()`); err != nil {
		return fmt.Errorf("expiring deployment sign-ins: %w", err)
	}
	_, err := p.pool.Exec(ctx, `INSERT INTO oidc_sign_in_flow
		(state_digest, code_verifier, nonce, return_to, expires_at)
		VALUES ($1,$2,$3,$4,$5)`, digest[:], nullableText(flow.CodeVerifier), nullableText(flow.Nonce), flow.ReturnTo, flow.ExpiresAt)
	if err != nil {
		return fmt.Errorf("starting deployment sign-in: %w", err)
	}
	return nil
}

func (p *Database) RedeemDeploymentSignIn(ctx context.Context, state string) (DeploymentSignInFlow, error) {
	digest := sha256.Sum256([]byte(state))
	var flow DeploymentSignInFlow
	var verifier, nonce *string
	err := p.pool.QueryRow(ctx, `DELETE FROM oidc_sign_in_flow
		WHERE state_digest=$1 AND expires_at>now()
		RETURNING code_verifier,nonce,return_to,expires_at`, digest[:]).Scan(&verifier, &nonce, &flow.ReturnTo, &flow.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return flow, ErrFlowUnknown
	}
	if err != nil {
		return flow, fmt.Errorf("redeeming deployment sign-in: %w", err)
	}
	flow.CodeVerifier, flow.Nonce = orEmptyText(verifier), orEmptyText(nonce)
	return flow, nil
}

func (p *Database) OIDCIdentity(ctx context.Context, identity Identity) (User, authz.Membership, error) {
	var user User
	var disabled *time.Time
	err := p.pool.QueryRow(ctx, `UPDATE app_user person SET email=$1,display_name=$2
		WHERE issuer=$3 AND subject=$4
		  AND EXISTS (SELECT 1 FROM organization_membership membership
		              WHERE membership.user_id=person.user_id)
		RETURNING user_id,issuer,subject,email,display_name,disabled_at,created_at`,
		identity.Email, identity.DisplayName, identity.Issuer, identity.Subject).Scan(&user.ID, &user.Issuer, &user.Subject, &user.Email,
		&user.DisplayName, &disabled, &user.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, authz.Membership{}, ErrLocalCredentialUnknown
	}
	if err != nil {
		return User{}, authz.Membership{}, fmt.Errorf("resolving a deployment OIDC identity: %w", err)
	}
	if disabled != nil {
		return User{}, authz.Membership{}, ErrUserDisabled
	}
	membership, err := membershipOf(ctx, p.pool, user.ID)
	if errors.Is(err, ErrMembershipUnknown) {
		return User{}, authz.Membership{}, ErrLocalCredentialUnknown
	}
	return user, membership, err
}
