package relay

import (
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
)

// This is the one piece of the session that is tested here rather than through the composition
// root, and the reason is that the composition root cannot reach it. Every connection a test
// makes comes from the loopback host, so the distinction that matters most — one host
// reconnecting badly against two hosts holding one credential — can only be exercised where
// the peer address is an input.
//
// Time is an input for the same reason: what separates a contested identity from an ordinary
// reconnection is how close together the takeovers are, and a test that established that by
// sleeping would be a test nobody runs.

func TestChurnWatch(t *testing.T) {
	t.Parallel()

	t.Run("a single takeover is a reconnection", func(t *testing.T) {
		t.Parallel()
		clock := &testClock{at: time.Unix(1_700_000_000, 0)}
		watch := newChurnWatch(clock.now)

		verdict := watch.record(uuid.New(), "10.0.0.1:44001")

		if verdict.contested {
			t.Error("one takeover was reported as contested; every relay that reconnects " +
				"would raise a credential-theft alert")
		}
	})

	t.Run("repeated takeovers close together are contested", func(t *testing.T) {
		t.Parallel()
		clock := &testClock{at: time.Unix(1_700_000_000, 0)}
		watch := newChurnWatch(clock.now)
		registration := uuid.New()

		var verdict churnVerdict
		for range churnThreshold {
			verdict = watch.record(registration, "10.0.0.1:44001")
			clock.advance(time.Second)
		}

		if !verdict.contested {
			t.Fatalf("%d takeovers in a few seconds were not reported as contested",
				verdict.takeovers)
		}
		if verdict.distinctHosts != 1 {
			t.Errorf("counted %d hosts, want 1 — one host reconnecting is a relay that "+
				"cannot hold a connection, not a stolen credential", verdict.distinctHosts)
		}
	})

	t.Run("takeovers spread beyond the window are not", func(t *testing.T) {
		t.Parallel()
		clock := &testClock{at: time.Unix(1_700_000_000, 0)}
		watch := newChurnWatch(clock.now)
		registration := uuid.New()

		var verdict churnVerdict
		for range churnThreshold * 2 {
			verdict = watch.record(registration, "10.0.0.1:44001")
			clock.advance(churnWindow + time.Second)
		}

		if verdict.contested {
			t.Errorf("takeovers a window apart were reported as contested; a relay on a bad "+
				"link would alert forever (%d counted)", verdict.takeovers)
		}
	})

	t.Run("a reconnecting relay is one host however its port changes", func(t *testing.T) {
		t.Parallel()
		clock := &testClock{at: time.Unix(1_700_000_000, 0)}
		watch := newChurnWatch(clock.now)
		registration := uuid.New()

		// The source port is new on every connection. Counting addresses whole would make this
		// indistinguishable from the case below, which is the distinction that matters.
		var verdict churnVerdict
		for port := range churnThreshold {
			verdict = watch.record(registration, "10.0.0.1:"+strconv.Itoa(44001+port))
			clock.advance(time.Second)
		}

		if verdict.distinctHosts != 1 {
			t.Errorf("counted %d hosts for one relay reconnecting, want 1",
				verdict.distinctHosts)
		}
	})

	t.Run("two hosts holding one identity are counted apart", func(t *testing.T) {
		t.Parallel()
		clock := &testClock{at: time.Unix(1_700_000_000, 0)}
		watch := newChurnWatch(clock.now)
		registration := uuid.New()

		// The credential-theft signature: one relay identity, two places using it, taking the
		// session from each other.
		hosts := []string{"10.0.0.1:44001", "203.0.113.9:51002", "10.0.0.1:44002"}
		var verdict churnVerdict
		for _, host := range hosts {
			verdict = watch.record(registration, host)
			clock.advance(time.Second)
		}

		if !verdict.contested {
			t.Fatal("alternating takeovers were not reported as contested")
		}
		if verdict.distinctHosts != 2 {
			t.Errorf("counted %d hosts, want 2 — more than one host holding one relay's "+
				"credential is the whole signal", verdict.distinctHosts)
		}
	})

	// An attacker who knows the rate can simply stay under it. Two hosts trading the session is
	// a signature that does not depend on how fast they do it, which is what closes that door.
	t.Run("two hosts trading the session are contested below the rate", func(t *testing.T) {
		t.Parallel()
		clock := &testClock{at: time.Unix(1_700_000_000, 0)}
		watch := newChurnWatch(clock.now)
		registration := uuid.New()

		watch.record(registration, "10.0.0.1:44001")
		clock.advance(time.Second)
		verdict := watch.record(registration, "203.0.113.9:51002")

		if verdict.takeovers >= churnThreshold {
			t.Fatalf("this test no longer stays under the rate: %d takeovers", verdict.takeovers)
		}
		if !verdict.contested {
			t.Error("two hosts trading one identity under the rate went unreported; staying " +
				"under the threshold is the first thing anyone would try")
		}
	})

	// The other half: one relay whose address changes is not two parties. That is one takeover
	// from a new host, not two hosts taking turns.
	t.Run("a relay that moves is not contested", func(t *testing.T) {
		t.Parallel()
		clock := &testClock{at: time.Unix(1_700_000_000, 0)}
		watch := newChurnWatch(clock.now)

		verdict := watch.record(uuid.New(), "10.0.0.7:44001")

		if verdict.contested {
			t.Error("a relay reconnecting from a new address was reported as contested; " +
				"rescheduling a pod would raise a credential-theft alert")
		}
	})

	t.Run("a first connection claims work at once and a flapping one waits", func(t *testing.T) {
		t.Parallel()
		clock := &testClock{at: time.Unix(1_700_000_000, 0)}
		watch := newChurnWatch(clock.now)
		registration := uuid.New()

		// Work stranded by a single blip has to come back immediately; that is the whole point
		// of reconciling on hello. It is repetition that gets held back.
		if backoff := watch.record(registration, "10.0.0.1:44001").backoff; backoff != 0 {
			t.Errorf("one reconnection was held back for %v; work stranded by a blip would "+
				"wait for no reason", backoff)
		}
		clock.advance(time.Second)
		if backoff := watch.record(registration, "10.0.0.1:44002").backoff; backoff <= 0 {
			t.Error("a session replacing another moments later was not held back, so a flap " +
				"loop re-runs catch-up on every arrival")
		}
	})

	t.Run("a full watch says so rather than reporting nothing wrong", func(t *testing.T) {
		t.Parallel()
		clock := &testClock{at: time.Unix(1_700_000_000, 0)}
		watch := newChurnWatch(clock.now)

		for range maxTrackedRegistrations {
			watch.record(uuid.New(), "10.0.0.1:44001")
		}
		verdict := watch.record(uuid.New(), "203.0.113.9:51002")

		if !verdict.untracked {
			t.Fatal("a registration past the cap was tracked anyway; the map grows without " +
				"limit and every takeover pays for the whole fleet")
		}
		if verdict.contested {
			t.Error("an uncounted registration was reported as contested")
		}
	})

	t.Run("one registration's history is not another's", func(t *testing.T) {
		t.Parallel()
		clock := &testClock{at: time.Unix(1_700_000_000, 0)}
		watch := newChurnWatch(clock.now)
		busy := uuid.New()

		for range churnThreshold {
			watch.record(busy, "10.0.0.1:44001")
			clock.advance(time.Second)
		}
		verdict := watch.record(uuid.New(), "10.0.0.2:44001")

		if verdict.contested {
			t.Error("a quiet registration inherited a busy one's history; every relay in an " +
				"organization would alert because one of them flaps")
		}
	})
}

// testClock is time as an input. Advancing it is how the window is crossed, because a suite
// that waited out real windows would be a suite that gets disabled.
type testClock struct {
	at time.Time
}

func (c *testClock) now() time.Time { return c.at }

func (c *testClock) advance(by time.Duration) { c.at = c.at.Add(by) }
