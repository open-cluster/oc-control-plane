package relay

import (
	"context"
	"net"
	"sync"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/peer"
)

// A relay has one identity and should have one session, so being superseded is ordinary once —
// that is a reconnection — and not ordinary repeatedly. Repeatedly means two parties are
// holding one relay's credential and taking the session from each other in turn.
//
// The reason this cannot be left to whoever reads a log is that nobody involved can see it. The
// real relay's view is only "connected, then immediately superseded", over and over, which is
// exactly what its own bad network would look like. The pattern is visible from here.
//
// What separates the two cases is where the connections come from. A relay with an unstable
// link reconnects from the same host; a stolen credential is used from another one. Source
// ports change on every reconnection and mean nothing, so hosts are counted, not addresses.
//
// TWO LIMITS ARE WORTH STATING PLAINLY, because a detection whose blind spots are undocumented
// is worse than none — it is trusted where it cannot see.
//
// The first is the network path. Where relays reach the control plane through a load balancer
// or a terminating proxy, every connection appears to come from that middlebox, and the
// distinct-host signal is dead: everything looks like one host. The rate signal still works,
// so a takeover storm is still caught, but the discriminator between a bad network and a
// stolen credential is not. Recovering it means trusting a forwarded-for header, which is
// only safe with a configured trusted proxy — and forging that header would otherwise let an
// attacker both hide and fabricate the signature.
//
// The second is that this is per process, like the session registry it follows. Two parties
// connected to different replicas never supersede each other here, so nothing is seen. The
// architecture records that v1 runs one instance; this is one more thing that assumption is
// holding up.
const (
	// How far back takeovers are counted. Two idle allowances: if a registration is taken over
	// repeatedly inside the time it takes to notice a dead session twice over, nothing about
	// that is an ordinary reconnection.
	churnWindow = 2 * sessionIdleTimeout

	// How many takeovers inside that window stop being a sequence of unrelated events. One is
	// a reconnection, two can be a redeploy, three is something taking turns.
	churnThreshold = 3

	// How many hosts taking turns is a signature on its own. Two takeovers from two hosts is
	// not a relay whose address changed — that is one takeover from a new host — and catching
	// it at two is what stops an attacker simply staying under the rate.
	contendingHosts = 2

	// How soon after a takeover a new session may start claiming work. Below this a session is
	// arriving faster than a displaced relay was told to come back, and letting each arrival
	// re-run catch-up turns a flap loop into dispatch churn. It delays new work only: what the
	// relay is already running is adopted the moment it says hello, either way.
	minimumSessionLifetime = sessionIdleTimeout

	// How many takeovers are remembered per registration. Only the threshold and the set of
	// hosts matter, so remembering more would cost memory to learn nothing.
	churnMemory = 8

	// How many registrations are tracked at once, and how often the quiet ones are swept out.
	// Sweeping is O(tracked) under the lock on the connect path, so it is amortised rather
	// than run on every takeover.
	maxTrackedRegistrations = 4096
	churnSweepInterval      = churnWindow
)

// churnWatch counts how often each registration's session is taken over, and by how many
// distinct hosts.
type churnWatch struct {
	now func() time.Time

	mutex          sync.Mutex
	byRegistration map[uuid.UUID]*takeovers
	lastSwept      time.Time
}

// newChurnWatch returns a watch. now is injected so the window's behaviour can be driven
// deterministically rather than by sleeping.
func newChurnWatch(now func() time.Time) *churnWatch {
	return &churnWatch{now: now, byRegistration: map[uuid.UUID]*takeovers{}}
}

// takeover is one session being taken from another: when, and from where.
type takeover struct {
	at   time.Time
	host string
}

// takeovers is one registration's recent history, oldest first.
type takeovers struct {
	recent []takeover
}

// churnVerdict is what the history says about a registration right now.
type churnVerdict struct {
	// contested reports that the registration is being taken over faster, or by more parties,
	// than reconnection explains.
	contested bool
	// takeovers and distinctHosts are the evidence. More than one host is the credential-theft
	// signature; one host is a relay that cannot hold a connection.
	takeovers     int
	distinctHosts int
	// backoff is how long this session should wait before claiming new work, so a flap loop
	// cannot re-run catch-up on every arrival.
	backoff time.Duration
	// untracked reports that the watch was full and this registration was not counted, so a
	// verdict of "not contested" here means "not looked at" rather than "looked at and clear".
	untracked bool
}

// record notes that this registration's session was taken over by a connection from peer, and
// reports what the recent history amounts to.
func (c *churnWatch) record(registration uuid.UUID, peer string) churnVerdict {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	now := c.now()
	c.sweep(now)

	history, tracked := c.byRegistration[registration]
	if !tracked {
		// An unbounded key map is itself a denial of service. Past the cap an unseen
		// registration is left uncounted rather than tracked, while registrations already being
		// counted keep being counted accurately — and the caller is told, so a gap in detection
		// is never mistaken for an absence of churn.
		if len(c.byRegistration) >= maxTrackedRegistrations {
			return churnVerdict{untracked: true}
		}
		history = &takeovers{}
		c.byRegistration[registration] = history
	}
	history.add(now, hostOf(peer))
	return history.verdict()
}

// sweep drops registrations with nothing recent to say, at most once a window. Scanning every
// tracked registration on every takeover would put the cost of the whole fleet on each
// connection.
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

// add records a takeover, keeping only what is recent and bounding what is kept.
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

// verdict reads the history.
//
// Either signal is enough on its own, because each catches what the other misses. Rate alone
// misses an attacker patient enough to stay under it. Hosts alone would flag a relay whose
// address changed, which happens for ordinary reasons — but two hosts taking the session from
// each other is not an address that changed, it is two parties.
func (t *takeovers) verdict() churnVerdict {
	hosts := t.hosts()
	verdict := churnVerdict{
		takeovers:     len(t.recent),
		distinctHosts: hosts,
		contested: len(t.recent) >= churnThreshold ||
			(hosts >= contendingHosts && len(t.recent) >= contendingHosts),
	}
	// A single reconnection gets no backoff at all: work stranded by a blip must come back at
	// once, which is the whole point of reconciling on hello. It is repetition that is throttled.
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

// peerAddress reports where a call came from, or an empty string when the transport did not
// say. Connections of unknown origin then share one bucket and are counted as a single host,
// which understates rather than invents a second party; grpc-go always reports a peer, so this
// is a fallback rather than a path.
func peerAddress(ctx context.Context) string {
	caller, ok := peer.FromContext(ctx)
	if !ok || caller.Addr == nil {
		return ""
	}
	return caller.Addr.String()
}

// hostOf reduces a peer address to the host that connected. The port changes on every
// connection, so counting addresses whole would make a relay that simply reconnects look
// exactly like a second party holding its credential — which is the one distinction this is
// here to make.
func hostOf(peer string) string {
	host, _, err := net.SplitHostPort(peer)
	if err != nil {
		return peer
	}
	return host
}
