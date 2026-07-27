package relay

import (
	"sync"
	"time"
)

// floodLimiter bounds how many enrolments may be attempted in a window, globally and per
// claimed organization.
//
// It is checked before the presented token is examined. That ordering is the point: a shed
// carries no information about whether a token was valid, so an attacker cannot use the
// difference between "refused" and "shed" to tell a real token from an invented one. It also
// means the expensive path — a database transaction — is never reached by a flood.
//
// Counting is by fixed window rather than a sliding one. A fixed window admits a burst at a
// boundary, which is acceptable here because the cap exists to bound cost and probing rate,
// not to smooth traffic; a sliding window would cost per-key history for no security gain.
type floodLimiter struct {
	window         time.Duration
	perOrgLimit    int
	globalLimit    int
	maxTrackedOrgs int
	now            func() time.Time

	mu          sync.Mutex
	windowStart time.Time
	globalCount int
	perOrgCount map[string]int
}

// newFloodLimiter returns a limiter over the given window. now is injected so the window's
// behaviour can be driven deterministically rather than by sleeping.
func newFloodLimiter(limits FloodLimits, now func() time.Time) *floodLimiter {
	return &floodLimiter{
		window:         limits.Window,
		perOrgLimit:    limits.PerOrganization,
		globalLimit:    limits.Global,
		maxTrackedOrgs: limits.MaxTrackedOrganizations,
		now:            now,
		perOrgCount:    make(map[string]int),
	}
}

// FloodLimits bounds enrolment attempts. The per-organization cap stops one tenant's leaked
// installation manifest becoming everyone's outage; the global cap stops an attacker
// spreading attempts across invented organizations to evade it.
type FloodLimits struct {
	Window                  time.Duration
	PerOrganization         int
	Global                  int
	MaxTrackedOrganizations int
}

// allow reports whether an attempt claiming this organization may proceed.
func (l *floodLimiter) allow(organization string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	if now.Sub(l.windowStart) >= l.window {
		l.windowStart = now
		l.globalCount = 0
		clear(l.perOrgCount)
	}

	if l.globalCount >= l.globalLimit {
		return false
	}
	// An unbounded key map is itself a denial of service: attempts claiming a fresh
	// organization each time would grow it without limit. Past the cap, an unseen
	// organization is shed rather than tracked, while organizations already counted in this
	// window keep being counted accurately.
	count, seen := l.perOrgCount[organization]
	if !seen && len(l.perOrgCount) >= l.maxTrackedOrgs {
		return false
	}
	if count >= l.perOrgLimit {
		return false
	}

	l.perOrgCount[organization] = count + 1
	l.globalCount++
	return true
}
