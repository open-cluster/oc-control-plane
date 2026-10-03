package relay

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	relayv1 "github.com/open-cluster/oc-relay/gen/go/opencluster/relay/v1"
)

type sessionState struct {
	organization   uuid.UUID
	registrationID uuid.UUID
	id             uuid.UUID
	outbound       chan *relayv1.ControlToRelay
	logger         *slog.Logger
	ctx            context.Context
	stop           context.CancelFunc
	peer           string

	delivering sync.Once
	policies   sync.Once
	lastHeard  atomic.Int64
	capacity   atomic.Int64
	draining   atomic.Bool
	contested  atomic.Bool
	claimAfter atomic.Int64
	ended      atomic.Pointer[sessionEnd]
}

type sessionEnd struct {
	code    codes.Code
	message string
}

func (e *sessionEnd) status() error {
	return status.Error(e.code, e.message)
}

func (x *sessionState) heard() {
	x.lastHeard.Store(time.Now().UnixNano())
}

func (x *sessionState) silentFor() time.Duration {
	return time.Since(time.Unix(0, x.lastHeard.Load()))
}

func (x *sessionState) end(code codes.Code, message string) {
	x.ended.CompareAndSwap(nil, &sessionEnd{code: code, message: message})
	x.stop()
}

func (x *sessionState) ending(err error) error {
	if reason := x.ended.Load(); reason != nil {
		return reason.status()
	}
	return err
}

func watchLiveness(session *sessionState) {
	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()

	for {
		select {
		case <-session.ctx.Done():
			return
		case <-ticker.C:
			silent := session.silentFor()
			if silent < sessionIdleTimeout {
				continue
			}
			session.logger.Warn("relay session went silent",
				slog.Duration("silent_for", silent),
				slog.Duration("allowance", sessionIdleTimeout))
			session.end(codes.DeadlineExceeded, "session idle")
			return
		}
	}
}

type liveSessions struct {
	mutex          sync.Mutex
	byRegistration map[uuid.UUID]*sessionState
}

func newLiveSessions() *liveSessions {
	return &liveSessions{byRegistration: map[uuid.UUID]*sessionState{}}
}

func (l *liveSessions) install(session *sessionState) *sessionState {
	l.mutex.Lock()
	defer l.mutex.Unlock()

	displaced := l.byRegistration[session.registrationID]
	l.byRegistration[session.registrationID] = session
	return displaced
}

func (l *liveSessions) remove(session *sessionState) {
	l.mutex.Lock()
	defer l.mutex.Unlock()

	if l.byRegistration[session.registrationID] == session {
		delete(l.byRegistration, session.registrationID)
	}
}

func (l *liveSessions) all() []*sessionState {
	l.mutex.Lock()
	defer l.mutex.Unlock()

	sessions := make([]*sessionState, 0, len(l.byRegistration))
	for _, session := range l.byRegistration {
		sessions = append(sessions, session)
	}
	return sessions
}

func (s *SessionService) Drain(within time.Duration) {
	for _, session := range s.live.all() {
		session.draining.Store(true)
		select {
		case session.outbound <- draining(within):
		default:
			session.logger.Warn("a drain instruction could not be queued")
		}
	}
}
