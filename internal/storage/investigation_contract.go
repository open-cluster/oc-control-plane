package storage

import "github.com/open-cluster/oc-control-plane/internal/investigation"

// The persistence package implements the investigation capability's interfaces, and says so here.
//
// It is an assertion rather than a comment because the direction of dependency is the point of
// ADR-017: the capability declares what it needs and this package satisfies it, so a signature that
// drifts has to fail somewhere. Failing here makes it a compile error in the package that moved,
// rather than a nil interface discovered when the composition root is wired.
var (
	// The read side, which the routes take. It is separate from the write side because a handler
	// given the writing interface is one typo away from mutating what it was asked to display.
	_ investigation.Reader = (*Placements)(nil)
	// The write side, which only a fenced round holds.
	_ investigation.Store = (*Placements)(nil)
	// The worker's discovery of which tenants have a claimable round. It is the one investigation
	// read that takes no organization, because finding out which organizations there are work for
	// IS the question.
	_ investigation.Pending = (*Placements)(nil)
)
