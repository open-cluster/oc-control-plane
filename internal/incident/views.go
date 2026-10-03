package incident

import (
	"encoding/json"
	"io"
	"net/http"
	"time"
)

const maxRequestBytes = 8 << 10

type incidentView struct {
	ID              string `json:"id"`
	IntegrationID   string `json:"integrationId"`
	IntegrationName string `json:"integrationName,omitempty"`
	Title           string `json:"title"`
	Status          string `json:"status"`

	Grouping groupingView `json:"grouping"`

	FirstSeenAt        time.Time  `json:"firstSeenAt"`
	LastSeenAt         time.Time  `json:"lastSeenAt"`
	ResolvedAt         *time.Time `json:"resolvedAt"`
	AlertEventCount    int        `json:"alertEventCount"`
	PostmortemEligible bool       `json:"postmortemEligible"`

	Supersession *supersessionView `json:"supersededBy,omitempty"`

	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

type groupingView struct {
	Basis       string `json:"basis"`
	Explanation string `json:"explanation"`
	Key         string `json:"key"`
}

type supersessionView struct {
	IncidentID string    `json:"incidentId"`
	Reason     string    `json:"reason"`
	At         time.Time `json:"at"`
}

type alertEventView struct {
	ID         string            `json:"id"`
	Title      string            `json:"title"`
	Summary    string            `json:"summary"`
	Labels     map[string]string `json:"labels"`
	Status     string            `json:"status"`
	StartedAt  time.Time         `json:"startedAt"`
	ResolvedAt *time.Time        `json:"resolvedAt"`
	ReceivedAt time.Time         `json:"receivedAt"`
}

type mergeRequest struct {
	Into   string `json:"into"`
	Reason string `json:"reason"`
}

type errorView struct {
	Error string `json:"error"`
}

func viewOf(incident Incident) incidentView {
	view := incidentView{
		ID:              incident.ID.String(),
		IntegrationID:   incident.Integration.String(),
		IntegrationName: incident.IntegrationName,
		Title:           incident.Title,
		Status:          incident.Status.String(),
		Grouping: groupingView{
			Basis:       incident.Basis.String(),
			Explanation: incident.Basis.Explain(),
			Key:         incident.GroupingKey,
		},
		FirstSeenAt:        incident.FirstSeenAt,
		LastSeenAt:         incident.LastSeenAt,
		AlertEventCount:    incident.AlertEventCount,
		PostmortemEligible: incident.Status == StatusResolved,
		CreatedAt:          incident.CreatedAt,
		UpdatedAt:          incident.UpdatedAt,
	}
	if !incident.ResolvedAt.IsZero() {
		resolved := incident.ResolvedAt
		view.ResolvedAt = &resolved
	}
	if incident.SupersededBy != nil {
		view.Supersession = &supersessionView{
			IncidentID: incident.SupersededBy.String(),
			Reason:     incident.SupersedeReason,
			At:         incident.SupersededAt,
		}
	}
	return view
}

func alertEventViewOf(alertEvent AlertEvent) alertEventView {
	view := alertEventView{
		ID:         alertEvent.ID.String(),
		Title:      alertEvent.Title,
		Summary:    alertEvent.Summary,
		Labels:     alertEvent.Labels,
		Status:     "resolved",
		StartedAt:  alertEvent.StartedAt,
		ReceivedAt: alertEvent.ReceivedAt,
	}
	if alertEvent.Firing {
		view.Status = "firing"
	}
	if alertEvent.Labels == nil {
		view.Labels = map[string]string{}
	}
	if !alertEvent.ResolvedAt.IsZero() {
		resolved := alertEvent.ResolvedAt
		view.ResolvedAt = &resolved
	}
	return view
}

func decode(writer http.ResponseWriter, request *http.Request, into any) bool {
	body := http.MaxBytesReader(writer, request.Body, maxRequestBytes)
	decoder := json.NewDecoder(body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(into); err != nil {
		writeJSON(writer, http.StatusBadRequest, errorView{Error: "request body is not understood"})
		return false
	}
	if _, err := decoder.Token(); err != io.EOF {
		writeJSON(writer, http.StatusBadRequest, errorView{Error: "request body is not understood"})
		return false
	}
	return true
}

func writeJSON(writer http.ResponseWriter, status int, body any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("X-Content-Type-Options", "nosniff")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(body)
}
