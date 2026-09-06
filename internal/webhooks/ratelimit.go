package webhooks

import (
	"sync"
	"time"

	"github.com/google/uuid"
)

// Per-Integration delivery limits. Alertmanager's default group interval is five minutes and
// its repeat interval is hours, so a healthy source delivers a handful of times a minute at
// most even during a storm. These are generous against that and still bound what one
// compromised or misconfigured Integration can spend on every other tenant's behalf.
const (
	// burst is how many deliveries an Integration may make back to back. A restart that flushes
	// a queue is a real pattern and must not be shed.
	burst = 60
	// refillInterval is how often one delivery of headroom returns, so the sustained rate is
	// one per second.
	refillInterval = time.Second
	// tracked bounds authenticated Integration buckets; new entries are refused at capacity.
	tracked = 4096
	// idleEviction is how long an Integration is remembered after its last delivery. A bucket
	// that has been idle this long is full, so forgetting it loses nothing.
	idleEviction          = 10 * time.Minute
	requestBurst          = 600
	requestRefillInterval = 100 * time.Millisecond
)

// limiter sheds deliveries from an Integration that is sending faster than any real alerting
// source does.
//
// It is per Integration rather than per caller address, deliberately: the address is whatever
// proxy sits in front, and the thing worth bounding is the credential, because that is what a
// compromise gets you. It is in-process, so a deployment of several instances allows several
// times this rate — which is stated rather than hidden, and is the right trade until there is
// a shared limiter worth its coordination cost.
type limiter struct {
	mu       sync.Mutex
	buckets  map[uuid.UUID]*bucket
	now      func() time.Time
	requests bucket
}

type bucket struct {
	tokens float64
	last   time.Time
}

func newLimiter(now func() time.Time) *limiter {
	if now == nil {
		now = time.Now
	}
	return &limiter{buckets: make(map[uuid.UUID]*bucket), now: now,
		requests: bucket{tokens: requestBurst, last: now()}}
}

// allowRequest bounds preauthentication work without trusting caller-selected identifiers.
func (l *limiter) allowRequest() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.requests.take(l.now(), requestBurst, requestRefillInterval)
}

// allow reports whether this Integration may deliver now, spending one unit of headroom if so.
func (l *limiter) allow(integration uuid.UUID) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	at := l.now()
	held, known := l.buckets[integration]
	if !known {
		if len(l.buckets) >= tracked {
			l.evictIdle(at)
		}
		if len(l.buckets) >= tracked {
			return false
		}
		l.buckets[integration] = &bucket{tokens: burst - 1, last: at}
		return true
	}

	return held.take(at, burst, refillInterval)
}

func (held *bucket) take(at time.Time, capacity float64, interval time.Duration) bool {
	held.tokens += float64(at.Sub(held.last)) / float64(interval)
	if held.tokens > capacity {
		held.tokens = capacity
	}
	held.last = at

	if held.tokens < 1 {
		return false
	}
	held.tokens--
	return true
}

// evictIdle forgets Integrations that have not delivered recently. Their buckets are full by
// now, so recreating one costs nothing.
func (l *limiter) evictIdle(at time.Time) {
	for connection, held := range l.buckets {
		if at.Sub(held.last) >= idleEviction {
			delete(l.buckets, connection)
		}
	}
}
