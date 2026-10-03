package relay

import (
	"sync"
	"time"
)

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

type FloodLimits struct {
	Window                  time.Duration
	PerOrganization         int
	Global                  int
	MaxTrackedOrganizations int
}

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
