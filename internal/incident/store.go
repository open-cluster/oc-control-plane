package incident

import (
	"context"

	"github.com/google/uuid"

	"github.com/open-cluster/oc-control-plane/internal/authz"
	"github.com/open-cluster/oc-control-plane/internal/tenancy"
)

// Store is everything this capability needs from durable state.
//
// It is declared here rather than in the persistence package because the capability owns its
// vocabulary and persistence depends on it (ADR-017). The one write takes the principal as well as
// the organization: the middleware has already checked the membership by the time it is called, so
// the value of taking it here is that the check also covers a call made from a path nobody routed
// through the middleware, and that the actor reaches the audit row the write commits alongside
// itself.
//
// GROUPING IS NOT HERE. An episode is created by the delivery that produced its first Signal, in
// the transaction that writes the Signals, because an episode assigned afterwards is a history that
// changed. What this interface holds is what an operator does with episodes once they exist.
type Store interface {
	// QueryEpisodes reports a page of a tenant's episodes.
	QueryEpisodes(ctx context.Context, org tenancy.Organization, query Query) (Page, error)
	// Episode reads one, scoped to the tenant.
	Episode(ctx context.Context, org tenancy.Organization, id uuid.UUID) (Episode, error)
	// EpisodeSignals reports the Signals grouped into one episode, oldest first, so a reader
	// follows the failure in the order it was reported.
	EpisodeSignals(ctx context.Context, org tenancy.Organization,
		id uuid.UUID, page SignalPage) (SignalList, error)
	// MergeEpisodes records that two episodes are one incident, writing the audit event in the
	// transaction that makes the change. Nothing is rewritten: the absorbed episode keeps its
	// identity, its Signals and its record.
	MergeEpisodes(ctx context.Context, who authz.Principal, org tenancy.Organization,
		merge Merge) (Episode, error)
}

// Query is a narrowed, ordered, paged request for a tenant's episodes.
type Query struct {
	// Search narrows by title and by the source's grouping key. The key is searchable because it
	// is what an operator has in front of them when they arrive from their own alerting.
	Search string
	// Integration narrows to what arrived through one installation, and is nil when the
	// caller named none.
	Integration *uuid.UUID
	// Status narrows to open or resolved episodes, and is zero when the caller named neither.
	Status Status
	// Sort is the field to order by, already checked against what this listing serves.
	Sort       string
	Descending bool
	Cursor     string
	Limit      int
}

// Page is what one query answered.
type Page struct {
	Episodes []Episode
	// Next resumes, and is empty on the last page. Its presence is also how a caller knows rows
	// were left out.
	Next string
}

// SignalPage is a position within one episode's Signals.
type SignalPage struct {
	Limit int
	After string
}

// SignalList is a page of an episode's Signals.
type SignalList struct {
	Signals []Signal
	Next    string
}
