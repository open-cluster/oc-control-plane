package relay

import (
	"context"
	"log/slog"
	"time"
)

// Durable presence: the record of which relays are holding a session right now, written by the
// session that holds one.
//
// It exists because the live session registry is PER PROCESS by construction, and an operator
// asking "how many of my hundred relays are connected" would otherwise be answered by whichever
// instance served the request — a fraction of the fleet reported as the fleet. That is not a
// small inaccuracy: the number an operator reads before deciding whether something is wrong
// would be wrong in the direction of alarming them.
//
// Every write is guarded on the session identifier, so a session that has already been displaced
// cannot clear or refresh the presence of the registration its successor now holds. That is the
// same rule liveSessions.remove follows in memory, made durable.

// presenceRefresh is how often a held session re-stamps its presence.
//
// It is the heartbeat interval, because that is the rhythm at which the relay proves it is alive
// and there is nothing to record between two proofs. One row update per relay per fifteen
// seconds is the cost, and it buys an answer that is the same from every instance.
const presenceRefresh = heartbeatInterval

// presenceTimeout bounds one presence write. It is short: presence is a convenience for an
// operator's summary, and a slow database must never be able to hold up a relay session that is
// otherwise healthy.
const presenceTimeout = 5 * time.Second

// recordPresence marks this session as the live one for its registration.
//
// A failure is logged and does not end the session. Presence is a read model; a relay that is
// genuinely connected and merely unrecorded is a worse thing to disconnect than to under-report,
// and the liveness window means an unrecorded session ages out of "connected" on its own rather
// than being reported as connected forever.
func (s *SessionService) recordPresence(session *sessionState) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(session.ctx), presenceTimeout)
	defer cancel()

	if err := s.placements.RelaySessionOpened(ctx, session.organization,
		session.registrationID, session.id, session.peer); err != nil {
		session.logger.ErrorContext(ctx, "a relay session could not be recorded as present",
			slog.String("error", err.Error()))
	}
}

// watchPresence re-stamps this session's presence while it is held.
//
// It runs on its own goroutine for the same reason watchLiveness does: the session loop can be
// blocked inside a send to a relay that has stopped reading, and a presence refresh that shared
// that goroutine would stop exactly when the relay was becoming interesting. It ends when the
// session does, and the last stamp ages out of the liveness window on its own.
func (s *SessionService) watchPresence(session *sessionState) {
	ticker := time.NewTicker(presenceRefresh)
	defer ticker.Stop()

	for {
		select {
		case <-session.ctx.Done():
			return
		case <-ticker.C:
			// Only while the relay is actually proving it is alive. Re-stamping presence for a
			// session that has gone silent would keep a dead relay looking connected for as long
			// as this process stayed up, which is the precise failure durable presence exists to
			// avoid.
			if session.silentFor() >= sessionIdleTimeout {
				continue
			}
			ctx, cancel := context.WithTimeout(
				context.WithoutCancel(session.ctx), presenceTimeout)
			err := s.placements.RelaySessionHeard(ctx, session.organization,
				session.registrationID, session.id)
			cancel()
			if err != nil {
				session.logger.ErrorContext(session.ctx, "a relay's presence could not be refreshed",
					slog.String("error", err.Error()))
			}
		}
	}
}

// releasePresence records that this session has ended.
//
// It is detached from the session's own context deliberately: the session is being torn down,
// and a write under a cancelled context would fail every time — leaving every clean disconnect
// looking like a relay that vanished, which is the one case an operator should be able to tell
// apart from the others.
func (s *SessionService) releasePresence(session *sessionState) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(session.ctx), presenceTimeout)
	defer cancel()

	if err := s.placements.RelaySessionClosed(ctx, session.organization,
		session.registrationID, session.id); err != nil {
		session.logger.ErrorContext(ctx, "the end of a relay session could not be recorded",
			slog.String("error", err.Error()))
	}
}
