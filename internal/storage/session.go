package storage

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-cluster/oc-control-plane/internal/audit"
	"github.com/open-cluster/oc-control-plane/internal/authz"
	"github.com/open-cluster/oc-control-plane/internal/session"
	"github.com/open-cluster/oc-control-plane/internal/tenancy"
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
	}, p.forwardingAudit.Load()); err != nil {
		return err
	}
	if err := transaction.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	committed = true
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
	// The last-seen stamp is refreshed in the same statement that reads the row, and only when
	// it has gone stale, so an authenticated read is a write at most once a minute per session
	// rather than once per request.
	err := on.QueryRow(ctx, `
		UPDATE operator_session
		   SET last_seen_at = CASE WHEN last_seen_at < now() - $2::INTERVAL
		                           THEN now() ELSE last_seen_at END
		 WHERE token_digest = $1
		RETURNING session_id, user_id, org_id, issued_at, expires_at, last_seen_at,
		          revoked_at, user_agent, address,
		          (SELECT email FROM app_user WHERE app_user.user_id = operator_session.user_id),
		          (SELECT display_name FROM app_user WHERE app_user.user_id = operator_session.user_id),
		          (SELECT disabled_at FROM app_user WHERE app_user.user_id = operator_session.user_id)`,
		digest, lastSeenResolution).Scan(&found.Session.ID, &found.Session.UserID,
		&found.Session.Organization, &found.Session.IssuedAt, &found.Session.ExpiresAt,
		&found.Session.LastSeenAt, &revoked, &found.Session.UserAgent, &found.Session.Address,
		&found.User.Email, &found.User.DisplayName, &disabled)
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

// DeleteSession ends the caller's own session. It deletes the row rather than marking it, so
// the credential is gone before the response is written — a row that still exists is a row a
// bug can read.
func (p *Database) DeleteSession(
	ctx context.Context, principal authz.Principal, organization tenancy.Organization,
	id uuid.UUID,
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

	if _, err := transaction.Exec(ctx,
		`DELETE FROM operator_session WHERE session_id = $1`, id); err != nil {
		return fmt.Errorf("signing out: %w", err)
	}
	// Recorded in the same transaction as the deletion, exactly as a state change is: a session
	// that ended with nothing saying so is a gap in the trail an offboarding review reads.
	if err := writeEvent(ctx, transaction, audit.Event{
		Organization:  organization.String(),
		Actor:         principal.Actor(),
		Action:        audit.ActionSignedOut,
		Target:        audit.Target{Kind: audit.TargetSession, ID: id.String()},
		Outcome:       audit.OutcomeAllowed,
		SourceAddress: principal.SourceAddress(),
		RequestID:     principal.RequestID(),
	}, p.forwardingAudit.Load()); err != nil {
		return err
	}
	if err := transaction.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	committed = true
	return nil
}

// RevokeSessionsOf ends every live session one person holds in one organization.
//
// Story 10: offboarding takes effect before the next token refresh. It marks rather than
// deletes, because an administrator who ended somebody's session should be able to see that
// they did, and because the person deserves to be told they were signed out rather than that
// their session timed out.
func (p *Database) RevokeSessionsOf(
	ctx context.Context, principal authz.Principal, organization tenancy.Organization,
	user uuid.UUID,
) (int64, error) {
	return audited(ctx, p, principal, organization, audit.ActionSessionRevoked,
		func(ctx context.Context, transaction pgx.Tx) (int64, audit.Target, audit.Detail, error) {
			tag, err := transaction.Exec(ctx, `
				UPDATE operator_session
				   SET revoked_at = now(), revoked_by = $3
				 WHERE user_id = $1 AND org_id = $2 AND revoked_at IS NULL`,
				user, organization.String(), principal.ID())
			if err != nil {
				return 0, audit.Target{}, nil, fmt.Errorf("revoking sessions: %w", err)
			}
			return tag.RowsAffected(),
				audit.Target{Kind: audit.TargetSession, ID: user.String()},
				audit.Detail{"sessions": tag.RowsAffected(), "userId": user.String()}, nil
		})
}

// ListSessions reports the live sessions in an organization, so an administrator can see what
// they would be ending before they end it.
func (p *Database) ListSessions(
	ctx context.Context, principal authz.Principal, organization tenancy.Organization,
) ([]session.Session, error) {
	if !principal.MemberOf(organization) {
		return nil, ErrNotAMember
	}
	pool, err := p.Pool(organization)
	if err != nil {
		return nil, err
	}

	rows, err := pool.Query(ctx, `
		SELECT session_id, user_id, issued_at, expires_at, last_seen_at, revoked_at,
		       user_agent, address
		  FROM operator_session
		 WHERE org_id = $1 AND revoked_at IS NULL AND expires_at > now()
		 ORDER BY last_seen_at DESC
		 LIMIT $2`, organization.String(), maxPageSize)
	if err != nil {
		return nil, fmt.Errorf("reading sessions: %w", err)
	}
	defer rows.Close()

	sessions := make([]session.Session, 0, 8)
	for rows.Next() {
		var (
			live    session.Session
			revoked *time.Time
		)
		if err := rows.Scan(&live.ID, &live.UserID, &live.IssuedAt, &live.ExpiresAt,
			&live.LastSeenAt, &revoked, &live.UserAgent, &live.Address); err != nil {
			return nil, fmt.Errorf("scanning a session: %w", err)
		}
		if revoked != nil {
			live.RevokedAt = *revoked
		}
		live.Organization = organization.String()
		sessions = append(sessions, live)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading sessions: %w", err)
	}
	return sessions, nil
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
