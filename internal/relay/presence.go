package relay

import (
	"context"
	"log/slog"
	"time"
)

const presenceRefresh = heartbeatInterval

const presenceTimeout = 5 * time.Second

func (s *SessionService) recordPresence(session *sessionState) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(session.ctx), presenceTimeout)
	defer cancel()

	if err := s.database.RelaySessionOpened(ctx, session.organization,
		session.registrationID, session.id, session.peer); err != nil {
		session.logger.ErrorContext(ctx, "a relay session could not be recorded as present",
			slog.String("error", err.Error()))
	}
}

func (s *SessionService) watchPresence(session *sessionState) {
	ticker := time.NewTicker(presenceRefresh)
	defer ticker.Stop()

	for {
		select {
		case <-session.ctx.Done():
			return
		case <-ticker.C:
			if session.silentFor() >= sessionIdleTimeout {
				continue
			}
			ctx, cancel := context.WithTimeout(
				context.WithoutCancel(session.ctx), presenceTimeout)
			err := s.database.RelaySessionHeard(ctx, session.organization,
				session.registrationID, session.id)
			cancel()
			if err != nil {
				session.logger.ErrorContext(session.ctx, "a relay's presence could not be refreshed",
					slog.String("error", err.Error()))
			}
		}
	}
}

func (s *SessionService) releasePresence(session *sessionState) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(session.ctx), presenceTimeout)
	defer cancel()

	if err := s.database.RelaySessionClosed(ctx, session.organization,
		session.registrationID, session.id); err != nil {
		session.logger.ErrorContext(ctx, "the end of a relay session could not be recorded",
			slog.String("error", err.Error()))
	}
}
