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
	Membership   authz.Membership
	PasswordHash string
}

// BootstrapLocalUser creates the first Organization, Admin User, password, and session atomically.
func (p *Database) BootstrapLocalUser(
	ctx context.Context, organizationName, email, displayName, passwordHash string, issued session.Session,
	digest []byte,
) (User, session.Session, error) {
	transaction, err := p.pool.Begin(ctx)
	if err != nil {
		return User{}, session.Session{}, fmt.Errorf("beginning local bootstrap: %w", err)
	}
	defer func() { _ = transaction.Rollback(ctx) }()

	if _, err = transaction.Exec(ctx, `LOCK TABLE app_user IN SHARE ROW EXCLUSIVE MODE`); err != nil {
		return User{}, session.Session{}, fmt.Errorf("locking local bootstrap: %w", err)
	}
	var users int
	if err = transaction.QueryRow(ctx, `SELECT count(*) FROM app_user`).Scan(&users); err != nil {
		return User{}, session.Session{}, fmt.Errorf("checking local bootstrap: %w", err)
	}
	if users != 0 {
		return User{}, session.Session{}, ErrLocalBootstrapComplete
	}
	normalized := strings.ToLower(strings.TrimSpace(email))
	var user User
	if err = transaction.QueryRow(ctx, `
		INSERT INTO app_user (issuer, subject, email, display_name)
		VALUES ($1, $2, $2, $3)
		RETURNING user_id, issuer, subject, email, display_name, created_at`,
		LocalIssuer, normalized, displayName).Scan(
		&user.ID, &user.Issuer, &user.Subject, &user.Email,
		&user.DisplayName, &user.CreatedAt); err != nil {
		return User{}, session.Session{}, fmt.Errorf("creating the first local user: %w", err)
	}
	var organization tenancy.Organization
	var organizationID string
	if err = transaction.QueryRow(ctx, `
		INSERT INTO organization (display_name, created_by)
		VALUES ($1, $2) RETURNING org_id`, organizationName, user.ID.String()).Scan(&organizationID); err != nil {
		return User{}, session.Session{}, fmt.Errorf("creating the first organization: %w", err)
	}
	organization, err = tenancy.NewOrganization(organizationID)
	if err != nil {
		return User{}, session.Session{}, fmt.Errorf("reading the first organization: %w", err)
	}
	if _, err = transaction.Exec(ctx, `
		INSERT INTO organization_membership (org_id, user_id, role)
		VALUES ($1, $2, $3)`, organization.String(), user.ID, string(authz.Admin)); err != nil {
		return User{}, session.Session{}, fmt.Errorf("granting the first membership: %w", err)
	}
	if _, err = transaction.Exec(ctx,
		`INSERT INTO local_password (user_id, password_hash) VALUES ($1, $2)`,
		user.ID, passwordHash); err != nil {
		return User{}, session.Session{}, fmt.Errorf("storing the first local password: %w", err)
	}
	issued.UserID = user.ID
	if err = transaction.QueryRow(ctx, `
		INSERT INTO session (credential_digest, user_id, issued_at, expires_at, last_seen_at,
		                     client_user_agent, remote_addr)
		VALUES ($1, $2, $3, $4, $3, $5, $6) RETURNING session_id`,
		digest, issued.UserID, issued.IssuedAt, issued.ExpiresAt,
		nullableText(truncateTo(issued.ClientUserAgent, session.MaxClientUserAgentLength)),
		nullableText(truncateTo(issued.RemoteAddr, session.MaxRemoteAddrLength))).Scan(&issued.ID); err != nil {
		return User{}, session.Session{}, fmt.Errorf("issuing the bootstrap session: %w", err)
	}
	if err = writeEvent(ctx, transaction, audit.Event{
		Actor: audit.System("deployment bootstrap"), Action: audit.ActionLocalBootstrapCompleted,
		Target: audit.Target{Kind: audit.TargetUser, ID: user.ID.String()}, Outcome: audit.OutcomeAllowed,
		SourceAddress: issued.RemoteAddr,
	}); err != nil {
		return User{}, session.Session{}, err
	}
	if err = transaction.Commit(ctx); err != nil {
		return User{}, session.Session{}, fmt.Errorf("committing local bootstrap: %w", err)
	}
	return user, issued, nil
}

func (p *Database) LocalIdentityByEmail(
	ctx context.Context, email string,
) (LocalIdentity, error) {
	var found LocalIdentity
	var disabled *time.Time
	err := p.pool.QueryRow(ctx, `
		SELECT person.user_id, person.issuer, person.subject, person.email,
		       person.display_name, person.disabled_at,
		       person.created_at, credential.password_hash
		  FROM app_user person
		  JOIN local_password credential ON credential.user_id = person.user_id
		  JOIN organization_membership membership ON membership.user_id = person.user_id
		 WHERE person.issuer = $1 AND lower(person.email) = lower($2)`,
		LocalIssuer, strings.TrimSpace(email)).Scan(
		&found.User.ID, &found.User.Issuer, &found.User.Subject, &found.User.Email,
		&found.User.DisplayName, &disabled,
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
	found.Membership, err = membershipOf(ctx, p.pool, found.User.ID)
	if errors.Is(err, ErrMembershipUnknown) {
		return LocalIdentity{}, ErrLocalCredentialUnknown
	}
	if err != nil {
		return LocalIdentity{}, err
	}
	return found, nil
}

func (p *Database) RehashLocalPassword(
	ctx context.Context, user uuid.UUID,
	previous, replacement string,
) error {
	_, err := p.pool.Exec(ctx, `
		UPDATE local_password SET password_hash = $1, changed_at = now()
		 WHERE user_id = $2 AND password_hash = $3
		   AND EXISTS (SELECT 1 FROM organization_membership WHERE user_id = $2)`,
		replacement, user, previous)
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
			var userID uuid.UUID
			if err := transaction.QueryRow(ctx, `
				INSERT INTO app_user (issuer, subject, email, display_name)
				VALUES ($1, $2, $2, $3) RETURNING user_id`,
				LocalIssuer, normalized, displayName).Scan(&userID); err != nil {
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
					(org_id, user_id, role)
				VALUES ($1, $2, $3)
				RETURNING user_id, role, created_at`,
				organization.String(), userID, string(role)).Scan(&member.UserID, &member.Role,
				&member.CreatedAt); err != nil {
				return Member{}, audit.Target{}, nil, fmt.Errorf("granting a local membership: %w", err)
			}
			member.Email = normalized
			member.DisplayName = displayName
			return member,
				audit.Target{Kind: audit.TargetUser, ID: userID.String()},
				audit.Detail{"email": normalized, "role": string(role)}, nil
		})
}
