package intake

import (
	"sync"
	"time"

	"github.com/google/uuid"
)

// Per-Connection delivery limits. Alertmanager's default group interval is five minutes and
// its repeat interval is hours, so a healthy source delivers a handful of times a minute at
// most even during a storm. These are generous against that and still bound what one
// compromised or misconfigured Connection can spend on every other tenant's behalf.
const (
	// burst is how many deliveries a Connection may make back to back. A restart that flushes
	// a queue is a real pattern and must not be shed.
	burst = 60
	// refillInterval is how often one delivery of headroom returns, so the sustained rate is
	// one per second.
	refillInterval = time.Second
	// tracked bounds how many Connections are remembered at once. The limiter is reachable by
	// anything that can guess an identifier, so a map keyed by whatever arrives is a memory
	// exhaustion primitive unless it is bounded.
	tracked = 4096
	// idleEviction is how long a Connection is remembered after its last delivery. A bucket
	// that has been idle this long is full, so forgetting it loses nothing.
	idleEviction = 10 * time.Minute
)

// limiter sheds deliveries from a Connection that is sending faster than any real alerting
// source does.
//
// It is per Connection rather than per caller address, deliberately: the address is whatever
// proxy sits in front, and the thing worth bounding is the credential, because that is what a
// compromise gets you. It is in-process, so a deployment of several instances allows several
// times this rate — which is stated rather than hidden, and is the right trade until there is
// a shared limiter worth its coordination cost.
type limiter struct {
	mu      sync.Mutex
	buckets map[uuid.UUID]*bucket
	now     func() time.Time
}

type bucket struct {
	tokens float64
	last   time.Time
}

func newLimiter(now func() time.Time) *limiter {
	if now == nil {
		now = time.Now
	}
	return &limiter{buckets: make(map[uuid.UUID]*bucket), now: now}
}

// allow reports whether this Connection may deliver now, spending one unit of headroom if so.
func (l *limiter) allow(connection uuid.UUID) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	at := l.now()
	held, known := l.buckets[connection]
	if !known {
		// A Connection arriving when the map is full does not get an unbounded pass and does
		// not get refused either: sweeping first is what keeps the bound honest, and a bound
		// that turned into a refusal would let an attacker with many identifiers deny service
		// to every real one.
		if len(l.buckets) >= tracked {
			l.evictIdle(at)
		}
		if len(l.buckets) >= tracked {
			// Still full after a sweep, which means this many Connections are genuinely active.
			// Admitting the delivery is the right failure: shedding real traffic to protect a
			// memory bound would be the limiter causing the outage it exists to prevent.
			return true
		}
		l.buckets[connection] = &bucket{tokens: burst - 1, last: at}
		return true
	}

	held.tokens += at.Sub(held.last).Seconds() * (float64(time.Second) / float64(refillInterval))
	if held.tokens > burst {
		held.tokens = burst
	}
	held.last = at

	if held.tokens < 1 {
		return false
	}
	held.tokens--
	return true
}

// evictIdle forgets Connections that have not delivered recently. Their buckets are full by
// now, so recreating one costs nothing.
func (l *limiter) evictIdle(at time.Time) {
	for connection, held := range l.buckets {
		if at.Sub(held.last) >= idleEviction {
			delete(l.buckets, connection)
		}
	}
}
