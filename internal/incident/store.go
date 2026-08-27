package incident

import (
	"context"

	"github.com/google/uuid"

	"github.com/open-cluster/oc-control-plane/internal/auth/authz"
	"github.com/open-cluster/oc-control-plane/internal/auth/tenancy"
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
// GROUPING IS NOT HERE. An incident is created by the delivery that produced its first AlertEvent, in
// the transaction that writes the AlertEvents, because an incident assigned afterwards is a history that
// changed. What this interface holds is what an operator does with incidents once they exist.
type Store interface {
	// QueryIncidents reports a page of a tenant's incidents.
	QueryIncidents(ctx context.Context, org tenancy.Organization, query Query) (Page, error)
	// Incident reads one, scoped to the tenant.
	Incident(ctx context.Context, org tenancy.Organization, id uuid.UUID) (Incident, error)
	// IncidentAlertEvents reports the AlertEvents grouped into one incident, oldest first, so a reader
	// follows the failure in the order it was reported.
	IncidentAlertEvents(ctx context.Context, org tenancy.Organization,
		id uuid.UUID, page AlertEventPage) (AlertEventList, error)
	// MergeIncidents records that two incidents are one incident, writing the audit event in the
	// transaction that makes the change. Nothing is rewritten: the absorbed incident keeps its
	// identity, its AlertEvents and its record.
	MergeIncidents(ctx context.Context, who authz.Principal, org tenancy.Organization,
		merge Merge) (Incident, error)
}

// Query is a narrowed, ordered, paged request for a tenant's incidents.
type Query struct {
	// Search narrows by title and by the source's grouping key. The key is searchable because it
	// is what an operator has in front of them when they arrive from their own alerting.
	Search string
	// Integration narrows to what arrived through one installation, and is nil when the
	// caller named none.
	Integration *uuid.UUID
	// Status narrows to open or resolved incidents, and is zero when the caller named neither.
	Status Status
	// Sort is the field to order by, already checked against what this listing serves.
	Sort       string
	Descending bool
	Cursor     string
	Limit      int
}

// Page is what one query answered.
type Page struct {
	Incidents []Incident
	// Next resumes, and is empty on the last page. Its presence is also how a caller knows rows
	// were left out.
	Next string
}

// AlertEventPage is a position within one incident's AlertEvents.
type AlertEventPage struct {
	Limit int
	After string
}

// AlertEventList is a page of an incident's AlertEvents.
type AlertEventList struct {
	AlertEvents []AlertEvent
	Next        string
}
