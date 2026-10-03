package incident

import (
	"context"

	"github.com/google/uuid"

	"github.com/open-cluster/oc-control-plane/internal/auth/authz"
)

type Store interface {
	QueryIncidents(ctx context.Context, org uuid.UUID, query Query) (Page, error)
	Incident(ctx context.Context, org uuid.UUID, id uuid.UUID) (Incident, error)
	IncidentAlertEvents(ctx context.Context, org uuid.UUID,
		id uuid.UUID, page AlertEventPage) (AlertEventList, error)
	MergeIncidents(ctx context.Context, who authz.Principal, org uuid.UUID,
		merge Merge) (Incident, error)
}

type Query struct {
	Search      string
	Integration *uuid.UUID
	Status      Status
	Sort        string
	Descending  bool
	Cursor      string
	Limit       int
}

type Page struct {
	Incidents []Incident
	Next      string
}

type AlertEventPage struct {
	Limit int
	After string
}

type AlertEventList struct {
	AlertEvents []AlertEvent
	Next        string
}
