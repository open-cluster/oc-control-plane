package relay

import (
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
			verdict = watch.record(registration, "10.0.0.1:"+portOf(port))
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

func portOf(index int) string {
	return string(rune('0'+index)) + "4001"
}
