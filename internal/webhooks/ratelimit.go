package webhooks

import (
	"sync"
	"time"
)

const (
	requestBurst          = 600
	requestRefillInterval = 100 * time.Millisecond
)

type limiter struct {
	mu       sync.Mutex
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
	return &limiter{
		now:      now,
		requests: bucket{tokens: requestBurst, last: now()},
	}
}

func (l *limiter) allowRequest() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.requests.take(l.now(), requestBurst, requestRefillInterval)
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
