package storage

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-cluster/oc-control-plane/internal/audit"
	"github.com/open-cluster/oc-control-plane/internal/auth/authz"
	"github.com/open-cluster/oc-control-plane/internal/auth/tenancy"
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

func (p *Database) CreateOIDCMember(ctx context.Context, principal authz.Principal, organization tenancy.Organization, identity Identity, role authz.Role) (Member, error) {
	return audited(ctx, p, principal, organization, audit.ActionUserProvisioned,
		func(ctx context.Context, tx pgx.Tx) (Member, audit.Target, audit.Detail, error) {
			var userID uuid.UUID
			err := tx.QueryRow(ctx, `INSERT INTO app_user
				(issuer,subject,email,display_name)
				VALUES ($1,$2,$3,$4)
				ON CONFLICT (issuer,subject) DO UPDATE SET email=EXCLUDED.email,
					display_name=EXCLUDED.display_name
				RETURNING user_id`, identity.Issuer, identity.Subject, identity.Email, identity.DisplayName).Scan(&userID)
			if err != nil {
				return Member{}, audit.Target{}, nil, fmt.Errorf("creating an OIDC member: %w", err)
			}
			var member Member
			err = tx.QueryRow(ctx, `INSERT INTO organization_membership
				(org_id,user_id,role)
				VALUES ($1,$2,$3) RETURNING user_id,role,created_at`,
				organization.String(), userID, string(role)).Scan(&member.UserID, &member.Role, &member.CreatedAt)
			if err != nil {
				return Member{}, audit.Target{}, nil, fmt.Errorf("granting an OIDC membership: %w", err)
			}
			member.Email, member.DisplayName = identity.Email, identity.DisplayName
			return member, audit.Target{Kind: audit.TargetUser, ID: userID.String()}, audit.Detail{"role": string(role)}, nil
		})
}

func (p *Database) OIDCIdentity(ctx context.Context, identity Identity) (User, []authz.Membership, error) {
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
		return User{}, nil, ErrLocalCredentialUnknown
	}
	if err != nil {
		return User{}, nil, fmt.Errorf("resolving a deployment OIDC identity: %w", err)
	}
	if disabled != nil {
		return User{}, nil, ErrUserDisabled
	}
	memberships, err := membershipsOf(ctx, p.pool, user.ID)
	if err == nil && len(memberships) != 1 {
		return User{}, nil, ErrLocalCredentialUnknown
	}
	return user, memberships, err
}
