package storage

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-cluster/oc-control-plane/internal/audit"
	"github.com/open-cluster/oc-control-plane/internal/auth/authz"
	"github.com/open-cluster/oc-control-plane/internal/auth/session"
	"github.com/open-cluster/oc-control-plane/internal/auth/tenancy"
)

// lastSeenResolution is how stale a session's last-seen stamp may get before a read refreshes
// it. Writing it on every request would turn every authenticated read into a write, on the
// hottest path the surface has, for a column an administrator reads by eye.
const lastSeenResolution = time.Minute

// SignedIn is everything one cookie resolves to: the session, who holds it, and what they may
// reach. The three come back together because they are read in one round trip and because a
// caller that could get the session without the memberships would be a caller who could
// authenticate somebody and then authorize them from a stale copy.
type SignedIn struct {
	Session     session.Session
	User        User
	Memberships []authz.Membership
}

// IssueSession records a signed-in operator, and the event saying so, in ONE transaction.
//
// The event is in the transaction rather than written after it for the reason every other
// state change is: a live session nobody can attribute is worse than a sign-in that failed.
// This is the one path where the actor is established for the first time, so it cannot go
// through audited — there is no principal yet to check a membership for. The actor is
// therefore passed explicitly, and it is the person the identity provider just asserted.
func (p *Database) IssueSession(
	ctx context.Context, organization tenancy.Organization,
	issued session.Session, digest []byte, actor audit.Actor, detail audit.Detail,
) error {
	pool, err := p.Pool(organization)
	if err != nil {
		return err
	}

	transaction, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = transaction.Rollback(ctx)
		}
	}()

	if err = issueSessionIn(ctx, transaction, organization, issued, digest, actor, detail); err != nil {
		return err
	}
	if err := transaction.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	committed = true
	return nil
}

// IssueLocalSession holds the verifier lock through issuance so password replacement revokes concurrent sign-ins.
func (p *Database) IssueLocalSession(
	ctx context.Context, organization tenancy.Organization,
	issued session.Session, digest []byte, actor audit.Actor, detail audit.Detail, previous string,
) error {
	pool, err := p.Pool(organization)
	if err != nil {
		return err
	}
	transaction, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin local sign-in: %w", err)
	}
	defer func() { _ = transaction.Rollback(ctx) }()
	var current string
	err = transaction.QueryRow(ctx, `SELECT c.password_hash FROM local_password c JOIN app_user u USING (user_id)
		WHERE c.user_id = $1 AND u.issuer = $2 AND u.disabled_at IS NULL FOR SHARE OF c`, issued.UserID, LocalIssuer).Scan(&current)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && current != previous) {
		return ErrLocalCredentialUnknown
	}
	if err != nil {
		return fmt.Errorf("checking local sign-in: %w", err)
	}
	if err = issueSessionIn(ctx, transaction, organization, issued, digest, actor, detail); err != nil {
		return err
	}
	if err = transaction.Commit(ctx); err != nil {
		return fmt.Errorf("commit local sign-in: %w", err)
	}
	return nil
}

func issueSessionIn(
	ctx context.Context, transaction pgx.Tx, organization tenancy.Organization,
	issued session.Session, digest []byte, actor audit.Actor, detail audit.Detail,
) error {
	if _, err := transaction.Exec(ctx, `
		INSERT INTO operator_session (session_id, token_digest, user_id, org_id,
		                              issued_at, expires_at, last_seen_at, user_agent, address)
		VALUES ($1, $2, $3, $4, $5, $6, $5, $7, $8)`,
		issued.ID, digest, issued.UserID, organization.String(), issued.IssuedAt,
		issued.ExpiresAt, truncateTo(issued.UserAgent, session.MaxUserAgentLength),
		truncateTo(issued.Address, session.MaxAddressLength)); err != nil {
		return fmt.Errorf("issuing a session: %w", err)
	}
	if err := writeEvent(ctx, transaction, audit.Event{
		Organization:  organization.String(),
		Actor:         actor,
		Action:        audit.ActionSignInCompleted,
		Target:        audit.Target{Kind: audit.TargetSession, ID: issued.ID.String()},
		Outcome:       audit.OutcomeAllowed,
		SourceAddress: issued.Address,
		Detail:        detail,
	}); err != nil {
		return err
	}
	return nil
}

// SessionByToken resolves the cookie a request presented.
//
// A session cookie names no tenant. The row that is found is itself the authority for the
// organization.
//
// The refusal says WHY — unknown, expired, revoked — because story 5 asks that a session which
// has run out returns the operator to sign-in with an explanation rather than a screen of
// error states. The distinction is safe here in a way it is not for a credential guess: the
// three answers are all "you are not signed in", and none of them says anything about a
// session the caller does not already hold.
func (p *Database) SessionByToken(ctx context.Context, digest []byte) (SignedIn, error) {
	return signedInFrom(ctx, p.pool, digest)
}

func signedInFrom(ctx context.Context, on querier, digest []byte) (SignedIn, error) {
	var (
		found    SignedIn
		revoked  *time.Time
		disabled *time.Time
	)
	err := on.QueryRow(ctx, `
		WITH touched AS (
			UPDATE operator_session SET last_seen_at = now()
			WHERE token_digest = $1 AND last_seen_at < now() - $2::interval
			  AND revoked_at IS NULL AND expires_at > now()
			RETURNING last_seen_at
		)
		SELECT s.session_id, s.user_id, COALESCE(s.org_id, ''), s.issued_at, s.expires_at,
		       COALESCE((SELECT last_seen_at FROM touched), s.last_seen_at),
		       s.revoked_at, s.user_agent, s.address, u.email, u.issuer, u.display_name, u.disabled_at
		FROM operator_session s JOIN app_user u ON u.user_id = s.user_id
		WHERE s.token_digest = $1`,
		digest, lastSeenResolution).Scan(&found.Session.ID, &found.Session.UserID,
		&found.Session.Organization, &found.Session.IssuedAt, &found.Session.ExpiresAt,
		&found.Session.LastSeenAt, &revoked, &found.Session.UserAgent, &found.Session.Address,
		&found.User.Email, &found.User.Issuer, &found.User.DisplayName, &disabled)
	if errors.Is(err, pgx.ErrNoRows) {
		return SignedIn{}, session.ErrUnknown
	}
	if err != nil {
		return SignedIn{}, fmt.Errorf("reading a session: %w", err)
	}
	if revoked != nil {
		found.Session.RevokedAt = *revoked
	}
	found.User.ID = found.Session.UserID
	if disabled != nil {
		found.User.DisabledAt = *disabled
	}

	if refusal := found.Session.Refusal(time.Now()); refusal != nil {
		return found, refusal
	}
	// A disabled person's live session stops working now rather than at its expiry. Story 10
	// is about the moment access ends, and a session that outlives the account is exactly the
	// gap it names.
	if found.User.Disabled() {
		return found, session.ErrRevoked
	}

	memberships, err := membershipsOf(ctx, on, found.Session.UserID)
	if err != nil {
		return SignedIn{}, err
	}
	found.Memberships = memberships
	return found, nil
}

// RevokeCurrentSession revokes the caller's current session and audits it in deployment scope.
func (p *Database) RevokeCurrentSession(ctx context.Context, principal authz.Principal, id uuid.UUID) error {
	if principal.CredentialID() != id.String() {
		return session.ErrUnknown
	}
	return p.endOwnedSession(ctx, principal, id, audit.ActionSignedOut)
}

// RevokeSession revokes only a session belonging to the authenticated User.
func (p *Database) RevokeSession(ctx context.Context, principal authz.Principal, id uuid.UUID) error {
	return p.endOwnedSession(ctx, principal, id, audit.ActionSessionRevoked)
}

func (p *Database) endOwnedSession(ctx context.Context, principal authz.Principal, id uuid.UUID, action audit.Action) error {
	userID, err := uuid.Parse(principal.ID())
	if err != nil || principal.Kind() != authz.KindUser {
		return session.ErrUnknown
	}
	transaction, err := p.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = transaction.Rollback(ctx) }()
	tag, err := transaction.Exec(ctx, `
		UPDATE operator_session SET revoked_at = now(), revoked_by = $2
		WHERE session_id = $1 AND user_id = $2::uuid AND revoked_at IS NULL`, id, userID.String())
	if err != nil {
		return fmt.Errorf("revoking session: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return session.ErrUnknown
	}
	if err = writeEvent(ctx, transaction, audit.Event{
		Actor: principal.Actor(), Action: action,
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

type SessionList struct {
	Sessions []session.Session
	Next     string
}

func (p *Database) ListSessions(
	ctx context.Context, principal authz.Principal, page Page,
) (SessionList, error) {
	userID, err := uuid.Parse(principal.ID())
	if err != nil || principal.Kind() != authz.KindUser {
		return SessionList{}, session.ErrUnknown
	}
	after, afterID, err := decodeCursor(page.After, "-lastSeenAt")
	if err != nil {
		return SessionList{}, err
	}
	limit := pageLimit(page.Limit)

	rows, err := p.pool.Query(ctx, `
		SELECT session_id, user_id, issued_at, expires_at, last_seen_at, revoked_at,
		       user_agent, address
		  FROM operator_session
		 WHERE user_id = $1 AND revoked_at IS NULL AND expires_at > now()
		   AND ($2::timestamptz IS NULL OR (last_seen_at, session_id) < ($2, $3))
		 ORDER BY last_seen_at DESC, session_id DESC
		 LIMIT $4`, userID, after, afterID, limit+1)
	if err != nil {
		return SessionList{}, fmt.Errorf("reading sessions: %w", err)
	}
	defer rows.Close()

	list := SessionList{Sessions: make([]session.Session, 0, limit)}
	for rows.Next() {
		var (
			live    session.Session
			revoked *time.Time
		)
		if err := rows.Scan(&live.ID, &live.UserID, &live.IssuedAt, &live.ExpiresAt,
			&live.LastSeenAt, &revoked, &live.UserAgent, &live.Address); err != nil {
			return SessionList{}, fmt.Errorf("scanning a session: %w", err)
		}
		if revoked != nil {
			live.RevokedAt = *revoked
		}
		if len(list.Sessions) == limit {
			last := list.Sessions[limit-1]
			list.Next = encodeCursor("-lastSeenAt", last.LastSeenAt, last.ID)
			break
		}
		list.Sessions = append(list.Sessions, live)
	}
	if err := rows.Err(); err != nil {
		return SessionList{}, fmt.Errorf("reading sessions: %w", err)
	}
	return list, nil
}

// SweepExpiredSessions removes sessions nobody can use. Expired rows authenticate nothing, so
// keeping them buys an administrator a longer list and nothing else.
func (p *Database) SweepExpiredSessions(
	ctx context.Context, organization tenancy.Organization, keepRevokedFor time.Duration,
) (int64, error) {
	pool, err := p.Pool(organization)
	if err != nil {
		return 0, err
	}
	// A revoked session survives for a while on purpose: an administrator who has just ended
	// somebody's access should still see it in the list they ended it from.
	tag, err := pool.Exec(ctx, `
		DELETE FROM operator_session
		 WHERE expires_at <= now()
		    OR (revoked_at IS NOT NULL AND revoked_at <= now() - $1::INTERVAL)`, keepRevokedFor)
	if err != nil {
		return 0, fmt.Errorf("sweeping sessions: %w", err)
	}
	return tag.RowsAffected(), nil
}

// SessionPolicy reports how long a tenant's sessions live and how long it says it keeps its
// record. Both are the organization's own settings; the application holds the lifetime inside
// the bounds this build serves.
func (p *Database) SessionPolicy(
	ctx context.Context, organization tenancy.Organization,
) (time.Duration, int, error) {
	pool, err := p.Pool(organization)
	if err != nil {
		return 0, 0, err
	}
	var seconds, retention int
	err = pool.QueryRow(ctx, `
		SELECT session_lifetime_seconds, audit_retention_days
		  FROM organization_policy WHERE org_id = $1`,
		organization.String()).Scan(&seconds, &retention)
	if errors.Is(err, pgx.ErrNoRows) {
		// A tenant that has configured nothing takes the product's defaults rather than
		// failing. There is no row to create here: writing one on a read would mean every
		// sign-in mutates.
		return 0, 0, nil
	}
	if err != nil {
		return 0, 0, fmt.Errorf("reading the session policy: %w", err)
	}
	return time.Duration(seconds) * time.Second, retention, nil
}

// SetSessionPolicy records a tenant's own security policy, and what it was before.
func (p *Database) SetSessionPolicy(
	ctx context.Context, principal authz.Principal, organization tenancy.Organization,
	lifetime time.Duration, retentionDays int,
) error {
	_, err := audited(ctx, p, principal, organization, audit.ActionPolicyChanged,
		func(ctx context.Context, transaction pgx.Tx) (struct{}, audit.Target, audit.Detail, error) {
			var beforeSeconds, beforeRetention int
			err := transaction.QueryRow(ctx, `
				SELECT session_lifetime_seconds, audit_retention_days
				  FROM organization_policy WHERE org_id = $1 FOR UPDATE`,
				organization.String()).Scan(&beforeSeconds, &beforeRetention)
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return struct{}{}, audit.Target{}, nil, fmt.Errorf("reading the policy: %w", err)
			}

			if _, err := transaction.Exec(ctx, `
				INSERT INTO organization_policy (org_id, session_lifetime_seconds,
				                                 audit_retention_days, updated_by)
				VALUES ($1, $2, $3, $4)
				ON CONFLICT (org_id) DO UPDATE
				    SET session_lifetime_seconds = EXCLUDED.session_lifetime_seconds,
				        audit_retention_days     = EXCLUDED.audit_retention_days,
				        updated_at               = now(),
				        updated_by               = EXCLUDED.updated_by`,
				organization.String(), int(lifetime.Seconds()), retentionDays,
				principal.ID()); err != nil {
				return struct{}{}, audit.Target{}, nil, fmt.Errorf("writing the policy: %w", err)
			}
			// Story 23 applies to every identity setting, not only to the provider: a weakened
			// policy is discoverable because both values are on the record.
			return struct{}{},
				audit.Target{Kind: audit.TargetOrganization, ID: organization.String()},
				audit.Detail{
					"before": map[string]any{
						"sessionLifetimeSeconds": beforeSeconds,
						"auditRetentionDays":     beforeRetention,
					},
					"after": map[string]any{
						"sessionLifetimeSeconds": int(lifetime.Seconds()),
						"auditRetentionDays":     retentionDays,
					},
				}, nil
		})
	return err
}

func truncateTo(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit]
}
