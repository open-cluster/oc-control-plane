package storage

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-cluster/oc-control-plane/internal/audit"
	"github.com/open-cluster/oc-control-plane/internal/auth/authz"
	"github.com/open-cluster/oc-control-plane/internal/auth/session"
	"github.com/open-cluster/oc-control-plane/internal/auth/tenancy"
)

var (
	ErrLocalBootstrapComplete = errors.New("a local administrator already exists")
	ErrLocalCredentialUnknown = errors.New("local credential unknown")
	ErrLocalAccountExists     = errors.New("local account already exists")
)

// LocalIssuer identifies identities whose credential is managed by this deployment.
const LocalIssuer = "opencluster:local"

type LocalIdentity struct {
	User         User
	Memberships  []authz.Membership
	PasswordHash string
}

func (p *Database) BootstrapLocalAdmin(
	ctx context.Context, organization tenancy.Organization,
	email, displayName, passwordHash string, issued session.Session, digest []byte,
	detail audit.Detail,
) (User, []authz.Membership, error) {
	pool, err := p.Pool(organization)
	if err != nil {
		return User{}, nil, err
	}
	transaction, err := pool.Begin(ctx)
	if err != nil {
		return User{}, nil, fmt.Errorf("beginning local bootstrap: %w", err)
	}
	defer func() { _ = transaction.Rollback(ctx) }()

	if _, err := transaction.Exec(ctx,
		`LOCK TABLE organization_membership IN SHARE ROW EXCLUSIVE MODE`); err != nil {
		return User{}, nil, fmt.Errorf("locking local bootstrap: %w", err)
	}
	var admins int
	if err := transaction.QueryRow(ctx, `
		SELECT count(*) FROM organization_membership membership
		 JOIN local_password credential ON credential.user_id=membership.user_id
		 WHERE membership.active AND membership.role = $1`, string(authz.Admin)).Scan(&admins); err != nil {
		return User{}, nil, fmt.Errorf("checking local bootstrap: %w", err)
	}
	if admins != 0 {
		return User{}, nil, ErrLocalBootstrapComplete
	}
	if _, err := transaction.Exec(ctx,
		`INSERT INTO organization (org_id, created_by) VALUES ($1, 'local bootstrap')`,
		organization.String()); err != nil {
		if isUniqueViolation(err, "organization_pkey") {
			return User{}, nil, ErrLocalBootstrapComplete
		}
		return User{}, nil, fmt.Errorf("creating the first organization: %w", err)
	}

	normalized := strings.ToLower(strings.TrimSpace(email))
	var user User
	if err := transaction.QueryRow(ctx, `
		INSERT INTO app_user
			(user_id, issuer, subject, email, email_verified, display_name, last_sign_in)
		VALUES ($1, $2, $3, $3, TRUE, $4, now())
		RETURNING user_id, issuer, subject, email, email_verified, display_name, created_at`,
		uuid.New(), LocalIssuer, normalized, displayName).Scan(
		&user.ID, &user.Issuer, &user.Subject, &user.Email, &user.EmailVerified,
		&user.DisplayName, &user.CreatedAt); err != nil {
		return User{}, nil, fmt.Errorf("creating the first local administrator: %w", err)
	}
	if _, err := transaction.Exec(ctx, `
		INSERT INTO local_password (user_id, password_hash) VALUES ($1, $2)`,
		user.ID, passwordHash); err != nil {
		return User{}, nil, fmt.Errorf("storing the first local password: %w", err)
	}
	membershipID := uuid.New()
	if _, err := transaction.Exec(ctx, `
		INSERT INTO organization_membership
			(membership_id, org_id, user_id, role, source, granted_by)
		VALUES ($1, $2, $3, $4, $5, 'local bootstrap')`,
		membershipID, organization.String(), user.ID, string(authz.Admin),
		int16(SourceManual)); err != nil {
		return User{}, nil, fmt.Errorf("granting the first local administrator: %w", err)
	}
	if err := writeEvent(ctx, transaction, audit.Event{
		Organization: organization.String(),
		Actor:        audit.System("local bootstrap"),
		Action:       audit.ActionUserProvisioned,
		Target:       audit.Target{Kind: audit.TargetMembership, ID: membershipID.String()},
		Outcome:      audit.OutcomeAllowed,
		Detail: audit.Detail{
			"role": string(authz.Admin), "source": SourceManual.String(), "email": normalized,
		},
	}); err != nil {
		return User{}, nil, err
	}
	memberships, err := membershipsOf(ctx, transaction, user.ID)
	if err != nil {
		return User{}, nil, err
	}
	issued.UserID = user.ID
	if err = issueSessionIn(ctx, transaction, organization, issued, digest, audit.Actor{
		Kind: audit.ActorUser, ID: user.ID.String(), DisplayName: user.DisplayName,
	}, detail); err != nil {
		return User{}, nil, err
	}
	if err := transaction.Commit(ctx); err != nil {
		return User{}, nil, fmt.Errorf("committing local bootstrap: %w", err)
	}
	return user, memberships, nil
}

func (p *Database) LocalBootstrapComplete(ctx context.Context, organization tenancy.Organization) (bool, error) {
	pool, err := p.Pool(organization)
	if err != nil {
		return false, err
	}
	var complete bool
	err = pool.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM organization_membership membership
		JOIN local_password credential ON credential.user_id=membership.user_id
		WHERE membership.org_id=$1 AND membership.active AND membership.role=$2)`,
		organization.String(), string(authz.Admin)).Scan(&complete)
	if err != nil {
		return false, fmt.Errorf("checking local bootstrap: %w", err)
	}
	return complete, nil
}

func (p *Database) LocalIdentityByEmail(
	ctx context.Context, organization tenancy.Organization, email string,
) (LocalIdentity, error) {
	pool, err := p.Pool(organization)
	if err != nil {
		return LocalIdentity{}, err
	}
	var found LocalIdentity
	var disabled *time.Time
	err = pool.QueryRow(ctx, `
		SELECT person.user_id, person.issuer, person.subject, person.email,
		       person.email_verified, person.display_name, person.disabled_at,
		       person.created_at, credential.password_hash
		  FROM app_user person
		  JOIN local_password credential ON credential.user_id = person.user_id
		  JOIN organization_membership membership ON membership.user_id = person.user_id
		 WHERE person.issuer = $1 AND lower(person.email) = lower($2)
		   AND membership.org_id = $3 AND membership.active AND membership.role IS NOT NULL`,
		LocalIssuer, strings.TrimSpace(email), organization.String()).Scan(
		&found.User.ID, &found.User.Issuer, &found.User.Subject, &found.User.Email,
		&found.User.EmailVerified, &found.User.DisplayName, &disabled,
		&found.User.CreatedAt, &found.PasswordHash)
	if errors.Is(err, pgx.ErrNoRows) {
		return LocalIdentity{}, ErrLocalCredentialUnknown
	}
	if err != nil {
		return LocalIdentity{}, fmt.Errorf("reading a local identity: %w", err)
	}
	if disabled != nil {
		found.User.DisabledAt = *disabled
		return LocalIdentity{}, ErrUserDisabled
	}
	found.Memberships, err = membershipsOf(ctx, pool, found.User.ID)
	if err != nil {
		return LocalIdentity{}, err
	}
	return found, nil
}

func (p *Database) RehashLocalPassword(
	ctx context.Context, organization tenancy.Organization, user uuid.UUID,
	previous, replacement string,
) error {
	pool, err := p.Pool(organization)
	if err != nil {
		return err
	}
	_, err = pool.Exec(ctx, `
		UPDATE local_password SET password_hash = $1, updated_at = now()
		 WHERE user_id = $2 AND password_hash = $3
		   AND EXISTS (SELECT 1 FROM organization_membership
		                WHERE org_id = $4 AND user_id = $2 AND active)`,
		replacement, user, previous, organization.String())
	if err != nil {
		return fmt.Errorf("rehashing a local password: %w", err)
	}
	return nil
}

func (p *Database) CreateLocalMember(
	ctx context.Context, principal authz.Principal, organization tenancy.Organization,
	email, displayName, passwordHash string, role authz.Role,
) (Member, error) {
	return audited(ctx, p, principal, organization, audit.ActionUserProvisioned,
		func(ctx context.Context, transaction pgx.Tx) (Member, audit.Target, audit.Detail, error) {
			normalized := strings.ToLower(strings.TrimSpace(email))
			userID := uuid.New()
			if _, err := transaction.Exec(ctx, `
				INSERT INTO app_user
					(user_id, issuer, subject, email, email_verified, display_name)
				VALUES ($1, $2, $3, $3, TRUE, $4)`,
				userID, LocalIssuer, normalized, displayName); err != nil {
				if isUniqueViolation(err, "app_user_identity_is_the_issuer_and_subject") {
					return Member{}, audit.Target{}, nil, ErrLocalAccountExists
				}
				return Member{}, audit.Target{}, nil, fmt.Errorf("creating a local member: %w", err)
			}
			if _, err := transaction.Exec(ctx, `
				INSERT INTO local_password (user_id, password_hash) VALUES ($1, $2)`,
				userID, passwordHash); err != nil {
				return Member{}, audit.Target{}, nil, fmt.Errorf("storing a local password: %w", err)
			}
			var member Member
			if err := transaction.QueryRow(ctx, `
				INSERT INTO organization_membership
					(membership_id, org_id, user_id, role, source, granted_by)
				VALUES ($1, $2, $3, $4, $5, $6)
				RETURNING membership_id, user_id, role, source, created_at`,
				uuid.New(), organization.String(), userID, string(role), int16(SourceManual),
				principal.ID()).Scan(&member.MembershipID, &member.UserID, &member.Role,
				&member.Source, &member.CreatedAt); err != nil {
				return Member{}, audit.Target{}, nil, fmt.Errorf("granting a local membership: %w", err)
			}
			member.Email = normalized
			member.DisplayName = displayName
			member.Active = true
			return member,
				audit.Target{Kind: audit.TargetMembership, ID: member.MembershipID.String()},
				audit.Detail{"userId": userID.String(), "email": normalized,
					"role": string(role), "source": SourceManual.String()}, nil
		})
}

func (p *Database) ResetLocalPassword(
	ctx context.Context, principal authz.Principal, organization tenancy.Organization,
	user uuid.UUID, passwordHash string,
) error {
	_, err := audited(ctx, p, principal, organization, audit.ActionLocalPasswordReset,
		func(ctx context.Context, transaction pgx.Tx) (struct{}, audit.Target, audit.Detail, error) {
			tag, err := transaction.Exec(ctx, `
				UPDATE local_password credential
				   SET password_hash = $1, password_changed_at = now(), updated_at = now()
				  FROM app_user person, organization_membership membership
				 WHERE credential.user_id = $2
				   AND person.user_id = credential.user_id AND person.issuer = $3
				   AND membership.user_id = credential.user_id
				   AND membership.org_id = $4 AND membership.active`,
				passwordHash, user, LocalIssuer, organization.String())
			if err != nil {
				return struct{}{}, audit.Target{}, nil, fmt.Errorf("resetting a local password: %w", err)
			}
			if tag.RowsAffected() != 1 {
				return struct{}{}, audit.Target{}, nil, ErrLocalCredentialUnknown
			}
			if _, err := transaction.Exec(ctx, `
				UPDATE operator_session
				   SET revoked_at = now(), revoked_by = $3
				 WHERE user_id = $1 AND revoked_at IS NULL
				   AND EXISTS (SELECT 1 FROM organization_membership
				               WHERE org_id=$2 AND user_id=$1 AND active)`,
				user, organization.String(), principal.ID()); err != nil {
				return struct{}{}, audit.Target{}, nil, fmt.Errorf("revoking reset sessions: %w", err)
			}
			return struct{}{}, audit.Target{Kind: audit.TargetMembership, ID: user.String()},
				audit.Detail{"userId": user.String(), "sessionsRevoked": true}, nil
		})
	return err
}
