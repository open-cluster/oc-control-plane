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
// The reason this cannot be left to whoever reads a log is that the victim cannot see it. The
// real relay's view is only "connected, then immediately superseded", over and over, which is
// indistinguishable from its own network being bad. The pattern is only visible from here.
//
// What separates the two is where the connections come from. A relay with an unstable network
// reconnects from the same host; a stolen credential is used from another one. Ports differ on
// every reconnection and mean nothing, so hosts are what is counted.
const (
	// How far back supersessions are counted. Two idle allowances: if a registration is taken
	// over repeatedly inside the time it takes to notice a dead session twice over, nothing
	// about that is an ordinary reconnection.
	churnWindow = 2 * sessionIdleTimeout

	// How many supersessions inside that window stop being a sequence of unrelated events. One
	// is a reconnection, two can be a redeploy, three is something taking turns.
	churnThreshold = 3

	// How many supersessions are remembered per registration. Only the threshold and the set of
	// hosts matter, so remembering more would cost memory to learn nothing.
	churnMemory = 8

	// How many registrations are tracked at once. A fleet reconnecting through one bad network
	// event must not be able to grow this without limit; past the cap the oldest are forgotten,
	// which loses detection for those rather than the process.
	maxTrackedRegistrations = 4096
)

// churnWatch counts how often each registration's session is taken over, and by how many
// distinct hosts.
type churnWatch struct {
	now func() time.Time

	mutex          sync.Mutex
	byRegistration map[uuid.UUID]*takeovers
}

// newChurnWatch returns a watch. now is injected so the window's behaviour can be driven
// deterministically rather than by sleeping.
func newChurnWatch(now func() time.Time) *churnWatch {
	return &churnWatch{now: now, byRegistration: map[uuid.UUID]*takeovers{}}
}

// takeovers is one registration's recent history: when it was taken over, and from where.
type takeovers struct {
	at   []time.Time
	from []string
}

// churnVerdict is what the history says about a registration right now.
type churnVerdict struct {
	// contested reports that the registration is being taken over faster than reconnection
	// explains.
	contested bool
	// takeovers and distinctHosts are the evidence. More than one host is the credential-theft
	// signature; one host is a relay that cannot hold a connection.
	takeovers     int
	distinctHosts int
}

// record notes that this registration's session was taken over by a connection from peer, and
// reports what the recent history amounts to.
func (c *churnWatch) record(registration uuid.UUID, peer string) churnVerdict {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	now := c.now()
	if len(c.byRegistration) >= maxTrackedRegistrations {
		c.forgetQuiet(now)
	}

	history := c.byRegistration[registration]
	if history == nil {
		history = &takeovers{}
		c.byRegistration[registration] = history
	}
	history.add(now, hostOf(peer))

	return churnVerdict{
		contested:     len(history.at) >= churnThreshold,
		takeovers:     len(history.at),
		distinctHosts: history.hosts(),
	}
}

// forgetQuiet drops registrations with nothing recent to say. It runs only when the map has
// reached its cap, so the common path never pays for it.
func (c *churnWatch) forgetQuiet(now time.Time) {
	for registration, history := range c.byRegistration {
		history.forgetBefore(now.Add(-churnWindow))
		if len(history.at) == 0 {
			delete(c.byRegistration, registration)
		}
	}
}

// add records a takeover, keeping only what is recent and bounding what is kept.
func (t *takeovers) add(now time.Time, host string) {
	t.forgetBefore(now.Add(-churnWindow))

	if len(t.at) == churnMemory {
		t.at = t.at[1:]
		t.from = t.from[1:]
	}
	t.at = append(t.at, now)
	t.from = append(t.from, host)
}

func (t *takeovers) forgetBefore(cutoff time.Time) {
	keep := 0
	for keep < len(t.at) && !t.at[keep].After(cutoff) {
		keep++
	}
	t.at = t.at[keep:]
	t.from = t.from[keep:]
}

func (t *takeovers) hosts() int {
	seen := make(map[string]struct{}, len(t.from))
	for _, host := range t.from {
		seen[host] = struct{}{}
	}
	return len(seen)
}

// peerAddress reports where a call came from, or an empty string when the transport did not
// say. An unknown origin is counted as its own host rather than guessed at: pretending it
// matches something would make two parties look like one, which is the wrong way to be wrong.
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
