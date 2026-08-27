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
	ID           uuid.UUID
	Organization string
	CodeVerifier string
	Nonce        string
	ReturnTo     string
	ExpiresAt    time.Time
}

var ErrFlowUnknown = errors.New("sign-in flow unknown")

func (p *Database) StartDeploymentSignIn(ctx context.Context, organization tenancy.Organization, flow DeploymentSignInFlow, state string) error {
	digest := sha256.Sum256([]byte(state))
	pool, err := p.Pool(organization)
	if err != nil {
		return err
	}
	if _, err = pool.Exec(ctx, `DELETE FROM deployment_sign_in_flow
		WHERE org_id=$1 AND (expires_at<=now() OR consumed_at IS NOT NULL)`, organization.String()); err != nil {
		return fmt.Errorf("expiring deployment sign-ins: %w", err)
	}
	_, err = pool.Exec(ctx, `INSERT INTO deployment_sign_in_flow
		(flow_id, org_id, state_digest, code_verifier, nonce, return_to, expires_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7)`, flow.ID, organization.String(), digest[:], nullableText(flow.CodeVerifier), nullableText(flow.Nonce), flow.ReturnTo, flow.ExpiresAt)
	if err != nil {
		return fmt.Errorf("starting deployment sign-in: %w", err)
	}
	return nil
}

func (p *Database) RedeemDeploymentSignIn(ctx context.Context, state string) (DeploymentSignInFlow, error) {
	digest := sha256.Sum256([]byte(state))
	var flow DeploymentSignInFlow
	var verifier, nonce *string
	err := p.pool.QueryRow(ctx, `DELETE FROM deployment_sign_in_flow
		WHERE state_digest=$1 AND consumed_at IS NULL AND expires_at>now()
		RETURNING flow_id,org_id,code_verifier,nonce,return_to,expires_at`, digest[:]).Scan(&flow.ID, &flow.Organization, &verifier, &nonce, &flow.ReturnTo, &flow.ExpiresAt)
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
				(user_id,issuer,subject,email,email_verified,display_name)
				VALUES ($1,$2,$3,$4,FALSE,$5)
				ON CONFLICT (issuer,subject) DO UPDATE SET email=EXCLUDED.email,
					display_name=EXCLUDED.display_name,updated_at=now()
				RETURNING user_id`, uuid.New(), identity.Issuer, identity.Subject, identity.Email, identity.DisplayName).Scan(&userID)
			if err != nil {
				return Member{}, audit.Target{}, nil, fmt.Errorf("creating an OIDC member: %w", err)
			}
			var member Member
			err = tx.QueryRow(ctx, `INSERT INTO organization_membership
				(membership_id,org_id,user_id,role,source,granted_by)
				VALUES ($1,$2,$3,$4,$5,$6) RETURNING membership_id,user_id,role,source,created_at`,
				uuid.New(), organization.String(), userID, string(role), int16(SourceManual), principal.ID()).Scan(&member.MembershipID, &member.UserID, &member.Role, &member.Source, &member.CreatedAt)
			if err != nil {
				return Member{}, audit.Target{}, nil, fmt.Errorf("granting an OIDC membership: %w", err)
			}
			member.Email, member.DisplayName, member.Active = identity.Email, identity.DisplayName, true
			return member, audit.Target{Kind: audit.TargetMembership, ID: member.MembershipID.String()}, audit.Detail{"userId": userID.String(), "role": string(role), "source": SourceManual.String()}, nil
		})
}

func (p *Database) OIDCIdentity(ctx context.Context, organization tenancy.Organization, identity Identity) (User, []authz.Membership, error) {
	pool, err := p.Pool(organization)
	if err != nil {
		return User{}, nil, err
	}
	var user User
	var disabled *time.Time
	err = pool.QueryRow(ctx, `UPDATE app_user person SET email=$1,email_verified=$2,
		display_name=$3,last_sign_in=now(),updated_at=now()
		WHERE issuer=$4 AND subject=$5
		  AND EXISTS (SELECT 1 FROM organization_membership membership
		              WHERE membership.user_id=person.user_id AND membership.org_id=$6
		                AND membership.active AND membership.role IS NOT NULL)
		RETURNING user_id,issuer,subject,email,email_verified,display_name,disabled_at,created_at`,
		identity.Email, identity.EmailVerified, identity.DisplayName, identity.Issuer, identity.Subject,
		organization.String()).Scan(&user.ID, &user.Issuer, &user.Subject, &user.Email, &user.EmailVerified, &user.DisplayName, &disabled, &user.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, nil, ErrLocalCredentialUnknown
	}
	if err != nil {
		return User{}, nil, fmt.Errorf("resolving a deployment OIDC identity: %w", err)
	}
	if disabled != nil {
		return User{}, nil, ErrUserDisabled
	}
	memberships, err := membershipsOf(ctx, pool, user.ID)
	return user, memberships, err
}
