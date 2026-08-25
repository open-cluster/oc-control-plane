package webhooks

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

// The limiter bounds what one Integration can spend on every other tenant's behalf. What it
// must never do is let one Integration's traffic shed another's, so the isolation is asserted
// as carefully as the bound itself.
//
// Time is injected rather than waited on. A test that slept for the refill would be slow and
// would assert on the scheduler; this asserts on the arithmetic.

func atTime(start time.Time, elapsed *time.Duration) func() time.Time {
	return func() time.Time { return start.Add(*elapsed) }
}

func TestLimiter_AllowsABurstAndThenSheds(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	var elapsed time.Duration
	limiter := newLimiter(atTime(start, &elapsed))
	connection := uuid.New()

	// A restart that flushes a queued backlog is a real pattern and must not be shed.
	for attempt := range burst {
		if !limiter.allow(connection) {
			t.Fatalf("delivery %d of the burst was shed; a source flushing a queue has done "+
				"nothing wrong", attempt+1)
		}
	}
	if limiter.allow(connection) {
		t.Fatal("the burst is a bound, and a delivery past it must be shed")
	}
}

func TestLimiter_HeadroomReturnsOverTime(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	var elapsed time.Duration
	limiter := newLimiter(atTime(start, &elapsed))
	connection := uuid.New()

	for range burst {
		limiter.allow(connection)
	}
	if limiter.allow(connection) {
		t.Fatal("the bucket should be empty")
	}

	// One refill interval returns exactly one delivery of headroom.
	elapsed = refillInterval
	if !limiter.allow(connection) {
		t.Fatal("headroom must return with time; a source shed once must not be shed forever")
	}
	if limiter.allow(connection) {
		t.Fatal("one interval returns one delivery, not the whole burst")
	}
}

// The property that matters most: shedding is per Integration. One compromised or
// misconfigured source must not be able to spend another tenant's intake capacity.
func TestLimiter_OneIntegrationExhaustedLeavesAnotherUntouched(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	var elapsed time.Duration
	limiter := newLimiter(atTime(start, &elapsed))
	noisy, quiet := uuid.New(), uuid.New()

	for range burst + 10 {
		limiter.allow(noisy)
	}
	if limiter.allow(noisy) {
		t.Fatal("the noisy connection should be shed")
	}

	for attempt := range burst {
		if !limiter.allow(quiet) {
			t.Fatalf("delivery %d from a quiet connection was shed because another was noisy; "+
				"one source must never spend another's capacity", attempt+1)
		}
	}
}

// The map is keyed by whatever arrives, so it is a memory-exhaustion primitive unless it is
// bounded. It is bounded by eviction rather than by refusal: refusing at the bound would let
// anyone with many identifiers deny service to every real Integration.
func TestLimiter_IsBoundedInMemoryAndStillAdmitsRealTraffic(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	var elapsed time.Duration
	limiter := newLimiter(atTime(start, &elapsed))

	// Far more identifiers than the bound, each delivering once — what a flood of guesses at
	// connection identifiers would look like.
	for range tracked * 2 {
		limiter.allow(uuid.New())
	}
	if len(limiter.buckets) > tracked {
		t.Fatalf("the limiter is tracking %d connections, above its bound of %d",
			len(limiter.buckets), tracked)
	}

	// And a real Integration arriving after that flood is still served rather than refused to
	// protect a memory bound.
	if !limiter.allow(uuid.New()) {
		t.Fatal("shedding real traffic to protect a memory bound would be the limiter causing " +
			"the outage it exists to prevent")
	}
}

func TestLimiter_IdleIntegrationsAreForgotten(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	var elapsed time.Duration
	limiter := newLimiter(atTime(start, &elapsed))

	for range tracked {
		limiter.allow(uuid.New())
	}
	// Everything above has now been idle long enough that its bucket is full again, so
	// forgetting it costs nothing.
	elapsed = idleEviction
	limiter.allow(uuid.New())

	if len(limiter.buckets) >= tracked {
		t.Fatalf("idle connections were not evicted: %d still tracked", len(limiter.buckets))
	}
}
