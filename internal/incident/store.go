package incident

import (
	"context"

	"github.com/google/uuid"

	"github.com/open-cluster/oc-control-plane/internal/auth/authz"
	"github.com/open-cluster/oc-control-plane/internal/auth/tenancy"
)

type Store interface {
	QueryIncidents(ctx context.Context, org tenancy.Organization, query Query) (Page, error)
	Incident(ctx context.Context, org tenancy.Organization, id uuid.UUID) (Incident, error)
	IncidentAlertEvents(ctx context.Context, org tenancy.Organization,
		id uuid.UUID, page AlertEventPage) (AlertEventList, error)
	MergeIncidents(ctx context.Context, who authz.Principal, org tenancy.Organization,
		merge Merge) (Incident, error)
}

// Query is a narrowed, ordered, paged request for a tenant's incidents.
type Query struct {
	Search      string
	Integration *uuid.UUID
	Status      Status
	Sort        string
	Descending  bool
	Cursor      string
	Limit       int
}

// Page is what one query answered.
type Page struct {
	Incidents []Incident
	Next      string
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
