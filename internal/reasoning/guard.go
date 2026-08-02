package reasoning

import (
	"context"
	"sync"
	"time"
)

// The bounds a deployment runs under, and the reason each of them is here rather than left to the
// provider.
//
// All three exist because a model provider is a shared dependency that fails in ways a database
// does not. It gets slow rather than unavailable, it charges for the attempt, and it stays broken
// for minutes at a time. A caller with no bounds turns each of those into an outage of this whole
// service instead of a failed round.

// Ceiling is a spending limit across rounds.
//
// It is a CEILING rather than a budget, and the word is the glossary's: a budget is something you
// spend down and a limit is something you may not cross, and this is the second. The round's own
// cost ceiling already exists in the investigation's execution limits; this is the one above it,
// which a single runaway case cannot cross by opening more rounds.
type Ceiling struct {
	mutex sync.Mutex
	limit int64
	spent int64
}

// NewCeiling bounds total spend in micro-cents. Zero means no ceiling, which is not the same as a
// ceiling of zero — it is an operator fact rather than a currency.
func NewCeiling(microCents int64) *Ceiling {
	return &Ceiling{limit: microCents}
}

// Allows reports whether there is room to send another request.
//
// It is a CHECK rather than a reservation, and the difference is worth stating: nothing is held
// between this answering and the cost being recorded, so calls already in flight can carry the
// total past the limit by up to what they collectively cost. Concurrency to one deployment is
// bounded separately, which bounds the overshoot; what this guarantees is that no request is sent
// once the limit is known to have been reached, which is what stops a runaway rather than what
// makes the figure exact.
func (c *Ceiling) Allows() bool {
	if c == nil {
		return true
	}
	c.mutex.Lock()
	defer c.mutex.Unlock()
	return c.limit <= 0 || c.spent < c.limit
}

// Record adds what a call actually cost. It is called for failed calls too: a refused request that
// consumed input tokens spent real money, and a ceiling that only counted successes would be one
// an unlucky day could walk straight through.
func (c *Ceiling) Record(microCents int64) {
	if c == nil {
		return
	}
	c.mutex.Lock()
	defer c.mutex.Unlock()
	c.spent += microCents
}

// Spent reports what has been consumed so far.
func (c *Ceiling) Spent() int64 {
	if c == nil {
		return 0
	}
	c.mutex.Lock()
	defer c.mutex.Unlock()
	return c.spent
}

// breaker fails fast while a provider is known to be broken.
//
// Without one, every round queues behind a dead dependency for its full timeout and the failure
// takes as long as the deadline allows. An open breaker is recorded and logged rather than silent,
// because a silently open breaker looks exactly like a vendor that stopped being called.
type breaker struct {
	mutex sync.Mutex
	// consecutive counts failures since the last success. A single failure is a bad minute; a run
	// of them is a dependency.
	consecutive int
	openUntil   time.Time
	threshold   int
	cooldown    time.Duration
	now         func() time.Time
}

// Breaker bounds are deliberately forgiving. A model provider that fails one request in ten is
// still worth asking; one that has failed several in a row is not, until it has had a moment.
const (
	defaultBreakerThreshold = 4
	defaultBreakerCooldown  = 30 * time.Second
)

func newBreaker(now func() time.Time) *breaker {
	return &breaker{
		threshold: defaultBreakerThreshold,
		cooldown:  defaultBreakerCooldown,
		now:       now,
	}
}

// Closed reports whether requests may be sent.
func (b *breaker) Closed() bool {
	b.mutex.Lock()
	defer b.mutex.Unlock()
	return !b.now().Before(b.openUntil)
}

// Succeeded closes the breaker. One good answer is enough: the cooldown probe is the request that
// just worked, so there is nothing further to prove.
func (b *breaker) Succeeded() {
	b.mutex.Lock()
	defer b.mutex.Unlock()
	b.consecutive = 0
	b.openUntil = time.Time{}
}

// Failed counts a failure and opens the breaker once there have been enough of them.
//
// Only outcomes that say something about the PROVIDER count. A refused request and a malformed
// answer are things the model did while working perfectly, and letting them trip the breaker would
// take a healthy vendor out of service for asking it a hard question.
func (b *breaker) Failed(outcome Outcome) {
	if outcome != OutcomeOutage && outcome != OutcomeTimeout {
		return
	}
	b.mutex.Lock()
	defer b.mutex.Unlock()
	b.consecutive++
	if b.consecutive >= b.threshold {
		b.openUntil = b.now().Add(b.cooldown)
	}
}

// limiter bounds concurrent in-flight requests to one deployment, so one investigation cannot
// consume the capacity every other investigation is waiting for.
type limiter struct {
	slots chan struct{}
}

func newLimiter(concurrent int) *limiter {
	if concurrent <= 0 {
		concurrent = defaultMaxConcurrent
	}
	return &limiter{slots: make(chan struct{}, concurrent)}
}

// Acquire waits for a slot or for the round's deadline, whichever comes first. Waiting is bounded
// by the caller's context rather than by a timer of its own: a limiter that outlived the round it
// was admitting work for would hold a slot nothing is using.
func (l *limiter) Acquire(ctx context.Context) error {
	select {
	case l.slots <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (l *limiter) Release() {
	select {
	case <-l.slots:
	default:
	}
}
