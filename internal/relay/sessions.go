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

	"github.com/open-cluster/oc-control-plane/internal/auth/tenancy"
)

// sessionState is one established session: the identity every message is validated against,
// the write queue and logger scoped to it, and the switch that ends it.
type sessionState struct {
	organization   tenancy.Organization
	registrationID uuid.UUID
	id             uuid.UUID
	outbound       chan *relayv1.ControlToRelay
	logger         *slog.Logger
	ctx            context.Context
	stop           context.CancelFunc
	// peer is where this connection came from, kept only to tell one host apart from another
	// when a registration's session keeps being taken over.
	peer string

	// delivering guards the start of delivery. A relay may say hello more than once — the
	// protocol refreshes attestations that way — and only the first one starts anything.
	delivering sync.Once
	// policies guards the start of the inventory policy refresh, for the same reason.
	policies sync.Once
	// lastHeard is when the relay was last known to be alive, held as an integer so the
	// liveness watch can read it without a lock the session loop would have to wait on.
	lastHeard atomic.Int64
	// capacity is the most work this relay may hold at once, from its own account of what it
	// can run. Nothing is dispatched beyond it.
	capacity atomic.Int64
	// draining reports that the session has been told to stand down, so no new work is taken.
	draining atomic.Bool
	// contested reports that this registration's session is being taken over faster, or by more
	// parties, than reconnection explains.
	contested atomic.Bool
	// claimAfter is how long this session waits before claiming new work, held as a duration in
	// nanoseconds. It is set when the session displaces another and throttles nothing else:
	// work the relay is already running is adopted the moment it says hello.
	claimAfter atomic.Int64
	// ended is why the session was stood down, when it was stood down deliberately.
	ended atomic.Pointer[sessionEnd]
}

// sessionEnd is a deliberate ending and the status the relay should be told about it.
type sessionEnd struct {
	code    codes.Code
	message string
}

func (e *sessionEnd) status() error {
	return status.Error(e.code, e.message)
}

// heard records that the relay is alive now. Any message counts, not only a heartbeat: a relay
// streaming results is evidently alive. What a heartbeat adds is proof from a relay with
// nothing to say.
func (x *sessionState) heard() {
	x.lastHeard.Store(time.Now().UnixNano())
}

// silentFor reports how long the relay has said nothing.
func (x *sessionState) silentFor() time.Duration {
	return time.Since(time.Unix(0, x.lastHeard.Load()))
}

// end stands the session down for a stated reason. The first reason wins: what ends a session
// is the thing that decided to, not whatever failed afterwards as a consequence.
func (x *sessionState) end(code codes.Code, message string) {
	x.ended.CompareAndSwap(nil, &sessionEnd{code: code, message: message})
	x.stop()
}

// ending reports what the relay should be told the session ended for.
//
// A deliberate ending outranks whatever surfaced first, because what surfaces first is usually
// a consequence: a send abandoned mid-queue, or a read that failed because the stream was
// already being torn down. Reporting that would tell the relay the transport broke when in
// fact it was stood down, and the two call for different behaviour on its side.
func (x *sessionState) ending(err error) error {
	if reason := x.ended.Load(); reason != nil {
		return reason.status()
	}
	return err
}

// watchLiveness ends a session that has stopped proving it is alive.
//
// It watches from its own goroutine deliberately. The session loop cannot be trusted to notice
// its own silence: acknowledgements queue on the same bounded channel as assignments, so a
// relay that has stopped reading blocks that loop inside a send — and a relay that has stopped
// reading is precisely the one this check exists to catch. A guarantee that fails in the case
// it was written for is not a guarantee.
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
			// Silence is not proof of death, only the end of proof of life. The session ends
			// and its leases are left to expire on their own clock, because a relay behind a
			// half-open connection may still be executing everything it holds.
			session.logger.Warn("relay session went silent",
				slog.Duration("silent_for", silent),
				slog.Duration("allowance", sessionIdleTimeout))
			session.end(codes.DeadlineExceeded, "session idle")
			return
		}
	}
}

// liveSessions is the one live session per registration.
//
// It is per process, so it cannot see a session held by another control-plane instance. That
// is what the idle allowance is for: supersession makes the common case immediate, and the
// allowance bounds every case it cannot see.
type liveSessions struct {
	mutex          sync.Mutex
	byRegistration map[uuid.UUID]*sessionState
}

func newLiveSessions() *liveSessions {
	return &liveSessions{byRegistration: map[uuid.UUID]*sessionState{}}
}

// install makes a session the live one and returns the session it displaced, if any.
func (l *liveSessions) install(session *sessionState) *sessionState {
	l.mutex.Lock()
	defer l.mutex.Unlock()

	displaced := l.byRegistration[session.registrationID]
	l.byRegistration[session.registrationID] = session
	return displaced
}

// remove drops a session unless it has already been displaced: a replaced session must not
// evict its successor on the way out.
func (l *liveSessions) remove(session *sessionState) {
	l.mutex.Lock()
	defer l.mutex.Unlock()

	if l.byRegistration[session.registrationID] == session {
		delete(l.byRegistration, session.registrationID)
	}
}

// all snapshots the live sessions, so a caller never holds the lock while working with them.
func (l *liveSessions) all() []*sessionState {
	l.mutex.Lock()
	defer l.mutex.Unlock()

	sessions := make([]*sessionState, 0, len(l.byRegistration))
	for _, session := range l.byRegistration {
		sessions = append(sessions, session)
	}
	return sessions
}

// Drain tells every live session to stand down within the given window: stop taking new work,
// flush what it holds, and disconnect. Nothing durable changes — anything left unfinished is
// recovered by lease expiry, which is why cutting a session short is safe.
//
// Each session stops taking new work immediately, whether or not the instruction reaches the
// relay. Work claimed now would be leased to something that is disconnecting and would then
// sit unexecuted until its lease ran out.
//
// It never blocks. A relay whose queue is already full is not reading, and waiting on it would
// let one unresponsive relay decide how long a shutdown takes, which is the failure the whole
// drain path exists to bound.
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
