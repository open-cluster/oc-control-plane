package relay

import (
	"context"
	"net"
	"sync"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/peer"
)

const (
	churnWindow = 2 * sessionIdleTimeout

	churnThreshold = 3

	contendingHosts = 2

	minimumSessionLifetime = sessionIdleTimeout

	churnMemory = 8

	maxTrackedRegistrations = 4096
	churnSweepInterval      = churnWindow
)

type churnWatch struct {
	now func() time.Time

	mutex          sync.Mutex
	byRegistration map[uuid.UUID]*takeovers
	lastSwept      time.Time
}

func newChurnWatch(now func() time.Time) *churnWatch {
	return &churnWatch{now: now, byRegistration: map[uuid.UUID]*takeovers{}}
}

type takeover struct {
	at   time.Time
	host string
}

type takeovers struct {
	recent []takeover
}

type churnVerdict struct {
	contested     bool
	takeovers     int
	distinctHosts int
	backoff       time.Duration
	untracked     bool
}

func (c *churnWatch) record(registration uuid.UUID, peer string) churnVerdict {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	now := c.now()
	c.sweep(now)

	history, tracked := c.byRegistration[registration]
	if !tracked {
		if len(c.byRegistration) >= maxTrackedRegistrations {
			return churnVerdict{untracked: true}
		}
		history = &takeovers{}
		c.byRegistration[registration] = history
	}
	history.add(now, hostOf(peer))
	return history.verdict()
}

func (c *churnWatch) sweep(now time.Time) {
	if now.Sub(c.lastSwept) < churnSweepInterval {
		return
	}
	c.lastSwept = now

	for registration, history := range c.byRegistration {
		history.forgetBefore(now.Add(-churnWindow))
		if len(history.recent) == 0 {
			delete(c.byRegistration, registration)
		}
	}
}

func (t *takeovers) add(now time.Time, host string) {
	t.forgetBefore(now.Add(-churnWindow))

	if len(t.recent) == churnMemory {
		t.recent = t.recent[1:]
	}
	t.recent = append(t.recent, takeover{at: now, host: host})
}

func (t *takeovers) forgetBefore(cutoff time.Time) {
	keep := 0
	for keep < len(t.recent) && !t.recent[keep].at.After(cutoff) {
		keep++
	}
	t.recent = t.recent[keep:]
}

func (t *takeovers) verdict() churnVerdict {
	hosts := t.hosts()
	verdict := churnVerdict{
		takeovers:     len(t.recent),
		distinctHosts: hosts,
		contested: len(t.recent) >= churnThreshold ||
			(hosts >= contendingHosts && len(t.recent) >= contendingHosts),
	}
	if len(t.recent) >= contendingHosts {
		verdict.backoff = minimumSessionLifetime
	}
	return verdict
}

func (t *takeovers) hosts() int {
	seen := make(map[string]struct{}, len(t.recent))
	for _, each := range t.recent {
		seen[each.host] = struct{}{}
	}
	return len(seen)
}

func peerAddress(ctx context.Context) string {
	caller, ok := peer.FromContext(ctx)
	if !ok || caller.Addr == nil {
		return ""
	}
	return caller.Addr.String()
}

func hostOf(peer string) string {
	host, _, err := net.SplitHostPort(peer)
	if err != nil {
		return peer
	}
	return host
}
