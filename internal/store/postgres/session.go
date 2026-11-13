package storage

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-cluster/oc-control-plane/internal/audit"
	"github.com/open-cluster/oc-control-plane/internal/auth/authz"
	"github.com/open-cluster/oc-control-plane/internal/auth/session"
)

type SignedIn struct {
	Session    session.Session
	User       User
	Membership authz.Membership
}

func (p *Database) IssueSession(
	ctx context.Context, organization uuid.UUID,
	userID uuid.UUID, digest []byte, actor audit.Actor, sourceAddress string, detail audit.Detail,
) (session.Session, error) {
	pool, err := p.poolForOrganization(organization)
	if err != nil {
		return session.Session{}, err
	}

	transaction, err := pool.Begin(ctx)
	if err != nil {
		return session.Session{}, fmt.Errorf("begin: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = transaction.Rollback(ctx)
		}
	}()

	issued, err := issueSessionIn(
		ctx, transaction, organization, userID, digest, actor, sourceAddress, detail)
	if err != nil {
		return session.Session{}, err
	}
	if err := transaction.Commit(ctx); err != nil {
		return session.Session{}, fmt.Errorf("commit: %w", err)
	}
	committed = true
	return issued, nil
}

func (p *Database) IssueLocalSession(
	ctx context.Context,
	organization uuid.UUID,
	userID uuid.UUID,
	digest []byte,
	actor audit.Actor,
	sourceAddress string,
	detail audit.Detail,
	previous string,
) (session.Session, error) {
	pool, err := p.poolForOrganization(organization)
	if err != nil {
		return session.Session{}, err
	}
	transaction, err := pool.Begin(ctx)
	if err != nil {
		return session.Session{}, fmt.Errorf("begin local sign-in: %w", err)
	}
	defer func() { _ = transaction.Rollback(ctx) }()
	var current string
	err = transaction.QueryRow(ctx, `SELECT c.password_hash FROM local_password c JOIN app_user u USING (user_id)
		WHERE c.user_id = $1 AND u.issuer = $2 AND u.disabled_at IS NULL FOR SHARE OF c`, userID, LocalIssuer).Scan(&current)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && current != previous) {
		return session.Session{}, ErrLocalCredentialUnknown
	}
	if err != nil {
		return session.Session{}, fmt.Errorf("checking local sign-in: %w", err)
	}
	issued, err := issueSessionIn(ctx, transaction, organization, userID, digest, actor, sourceAddress, detail)
	if err != nil {
		return session.Session{}, err
	}
	if err = transaction.Commit(ctx); err != nil {
		return session.Session{}, fmt.Errorf("commit local sign-in: %w", err)
	}
	return issued, nil
}

func issueSessionIn(
	ctx context.Context, transaction pgx.Tx, organization uuid.UUID,
	userID uuid.UUID, digest []byte, actor audit.Actor, sourceAddress string, detail audit.Detail,
) (session.Session, error) {
	issued, err := insertSession(ctx, transaction, userID, digest)
	if err != nil {
		return session.Session{}, err
	}
	detail = maps.Clone(detail)
	if detail == nil {
		detail = audit.Detail{}
	}
	detail["expiresAt"] = issued.ExpiresAt.UTC().Format(time.RFC3339)
	if err := writeEvent(ctx, transaction, audit.Event{
		Organization:  organization.String(),
		Actor:         actor,
		Action:        audit.ActionSignInCompleted,
		Target:        audit.Target{Kind: audit.TargetSession, ID: issued.ID.String()},
		Outcome:       audit.OutcomeAllowed,
		SourceAddress: sourceAddress,
		Detail:        detail,
	}); err != nil {
		return session.Session{}, err
	}
	return issued, nil
}

func insertSession(ctx context.Context, tx pgx.Tx, userID uuid.UUID, digest []byte) (session.Session, error) {
	idle, _ := session.Lifetimes()
	issued := session.Session{UserID: userID}
	err := tx.QueryRow(ctx, `
		INSERT INTO session (credential_digest, user_id, created_at, expires_at)
		SELECT $1, $2, started, started + $3::interval
		FROM (SELECT date_trunc('second', clock_timestamp()) AS started) AS ts
		RETURNING session_id, created_at, expires_at`, digest, userID, idle).
		Scan(&issued.ID, &issued.CreatedAt, &issued.ExpiresAt)
	if err != nil {
		return session.Session{}, fmt.Errorf("issuing a session: %w", err)
	}
	return issued, nil
}

func (p *Database) SessionByToken(ctx context.Context, digest []byte) (SignedIn, error) {
	var (
		found    SignedIn
		disabled *time.Time
		expired  bool
	)
	_, maximum := session.Lifetimes()
	err := p.pool.QueryRow(ctx, `
		SELECT s.session_id, s.user_id, s.created_at, s.expires_at,
		       LEAST(s.expires_at, s.created_at + $2::interval) <= clock_timestamp(),
		       u.email, u.display_name, u.disabled_at
		FROM session s JOIN app_user u ON u.user_id = s.user_id
		WHERE s.credential_digest = $1`,
		digest, maximum).Scan(&found.Session.ID, &found.Session.UserID, &found.Session.CreatedAt, &found.Session.ExpiresAt, &expired,
		&found.User.Email, &found.User.DisplayName, &disabled)
	if errors.Is(err, pgx.ErrNoRows) {
		return SignedIn{}, session.ErrUnknown
	}
	if err != nil {
		return SignedIn{}, fmt.Errorf("reading a session: %w", err)
	}
	found.User.ID = found.Session.UserID
	if disabled != nil {
		found.User.DisabledAt = *disabled
	}

	if expired {
		return found, session.ErrExpired
	}
	if found.User.Disabled() {
		return found, session.ErrUnknown
	}

	membership, err := membershipOf(ctx, p.pool, found.Session.UserID)
	if errors.Is(err, ErrMembershipUnknown) {
		return SignedIn{}, session.ErrUnknown
	}
	if err != nil {
		return SignedIn{}, err
	}
	found.Membership = membership
	return found, nil
}

func (p *Database) RefreshSession(ctx context.Context, principal authz.Principal, digest []byte) (time.Time, bool, error) {
	const renewalThreshold = 30 * time.Minute
	idle, maximum := session.Lifetimes()
	var expires time.Time
	// Sample the clock again after acquiring the lock. A session can expire
	// while a competing writer holds the row.
	err := p.pool.QueryRow(ctx, `
		WITH candidate AS MATERIALIZED (
			SELECT s.session_id
			FROM session s
			JOIN app_user u USING (user_id)
			JOIN organization_membership m USING (user_id)
			WHERE s.session_id = $1 AND s.user_id = $2 AND s.credential_digest = $3
			  AND m.org_id = $4 AND u.disabled_at IS NULL
			  AND s.expires_at <= clock_timestamp() + $5::interval
			  AND s.expires_at < s.created_at + $6::interval
			  AND s.expires_at > clock_timestamp()
			FOR NO KEY UPDATE OF s FOR SHARE OF u, m
		), renewal AS MATERIALIZED (
			SELECT session_id, clock_timestamp() AS checked_at FROM candidate
		)
		UPDATE session s
		SET expires_at = LEAST(date_trunc('second', r.checked_at) + $7::interval, s.created_at + $6::interval)
		FROM renewal r
		WHERE s.session_id = r.session_id
		  AND s.expires_at > r.checked_at
		  AND s.expires_at < LEAST(date_trunc('second', r.checked_at) + $7::interval, s.created_at + $6::interval)
		RETURNING s.expires_at`,
		principal.SessionID(), principal.UserID(), digest, principal.Organization(),
		idle-renewalThreshold, maximum, idle).Scan(&expires)
	if err == nil {
		return expires, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, false, fmt.Errorf("refreshing a session: %w", err)
	}
	// A concurrent renewal or revocation may have committed since the update
	// started. Resolve against a new snapshot.
	current, err := p.SessionByToken(ctx, digest)
	if err != nil {
		return time.Time{}, false, err
	}
	if current.Session.ID != principal.SessionID() || current.User.ID != principal.UserID() ||
		current.Membership.Organization != principal.Organization() {
		return time.Time{}, false, session.ErrUnknown
	}
	return current.Session.ExpiresAt, false, nil
}

func (p *Database) DeleteCurrentSession(ctx context.Context, principal authz.Principal) error {
	userID := principal.UserID()
	id := principal.SessionID()
	if userID == uuid.Nil || id == uuid.Nil {
		return session.ErrUnknown
	}
	transaction, err := p.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = transaction.Rollback(ctx) }()
	tag, err := transaction.Exec(ctx, `
		DELETE FROM session
		WHERE session_id = $1 AND user_id = $2::uuid`, id, userID.String())
	if err != nil {
		return fmt.Errorf("deleting session: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return session.ErrUnknown
	}
	if err = writeEvent(ctx, transaction, audit.Event{
		Actor: principal.Actor(), Action: audit.ActionSignedOut,
		Target:  audit.Target{Kind: audit.TargetSession, ID: id.String()},
		Outcome: audit.OutcomeAllowed, SourceAddress: principal.SourceAddress(), RequestID: principal.RequestID(),
	}); err != nil {
		return err
	}
	if err = transaction.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

func (p *Database) PruneSessions(ctx context.Context) (int64, error) {
	tag, err := p.pool.Exec(ctx, `
		DELETE FROM session
		 WHERE session_id IN (
		   SELECT session_id FROM session
		    WHERE expires_at <= now()
		    ORDER BY expires_at, session_id
		    LIMIT 1000 FOR UPDATE SKIP LOCKED
		 )`)
	if err != nil {
		return 0, fmt.Errorf("sweeping sessions: %w", err)
	}
	return tag.RowsAffected(), nil
}

func (p *Database) OrganizationAuditRetention(
	ctx context.Context, organization uuid.UUID,
) (int, error) {
	pool, err := p.poolForOrganization(organization)
	if err != nil {
		return 0, err
	}
	var retention int
	err = pool.QueryRow(ctx, `
		SELECT audit_retention_days
		  FROM organization WHERE org_id = $1`,
		organization).Scan(&retention)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("reading audit retention: %w", err)
	}
	return retention, nil
}

func (p *Database) SetOrganizationAuditRetention(
	ctx context.Context, principal authz.Principal, organization uuid.UUID,
	retentionDays int,
) error {
	_, err := audited(ctx, p, principal, organization, audit.ActionPolicyChanged,
		func(ctx context.Context, transaction pgx.Tx) (struct{}, audit.Target, audit.Detail, error) {
			var beforeRetention int
			err := transaction.QueryRow(ctx, `
				SELECT audit_retention_days
				  FROM organization WHERE org_id = $1 FOR UPDATE`,
				organization).Scan(&beforeRetention)
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return struct{}{}, audit.Target{}, nil, fmt.Errorf("reading the policy: %w", err)
			}

			if _, err := transaction.Exec(ctx, `
				UPDATE organization SET audit_retention_days = $2
				 WHERE org_id = $1`,
				organization, retentionDays); err != nil {
				return struct{}{}, audit.Target{}, nil, fmt.Errorf("writing the policy: %w", err)
			}
			return struct{}{},
				audit.Target{Kind: audit.TargetOrganization, ID: organization.String()},
				audit.Detail{
					"before": map[string]any{
						"auditRetentionDays": beforeRetention,
					},
					"after": map[string]any{
						"auditRetentionDays": retentionDays,
					},
				}, nil
		})
	return err
}
