package api

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/open-cluster/oc-control-plane/internal/store/postgres"
)

type relayView struct {
	RegistrationID     string        `json:"registrationId"`
	ClusterFingerprint string        `json:"clusterFingerprint"`
	RelayVersion       string        `json:"relayVersion"`
	Compatibility      string        `json:"compatibility"`
	RegisteredAt       time.Time     `json:"registeredAt"`
	RevokedAt          *time.Time    `json:"revokedAt,omitempty"`
	Connected          bool          `json:"connected"`
	LastSeenAt         *time.Time    `json:"lastSeenAt,omitempty"`
	Capabilities       []string      `json:"capabilities"`
	SessionConflict    *conflictView `json:"sessionConflict,omitempty"`
}

type relaySummaryView struct {
	Total           int `json:"total"`
	Connected       int `json:"connected"`
	Disconnected    int `json:"disconnected"`
	Revoked         int `json:"revoked"`
	Degraded        int `json:"degraded"`
	ActiveRequests  int `json:"activeRequests"`
	LivenessSeconds int `json:"livenessSeconds"`
}

type servedIntegrationView struct {
	ID       string  `json:"id"`
	Type     string  `json:"type"`
	Name     string  `json:"name"`
	Status   *string `json:"status"`
	Disabled bool    `json:"disabled"`
}

type relayFailureView struct {
	JobID             string    `json:"jobId"`
	CapabilityID      string    `json:"capabilityId"`
	CapabilityVersion int       `json:"capabilityVersion"`
	IntegrationID     string    `json:"integrationId"`
	Outcome           string    `json:"outcome"`
	At                time.Time `json:"at"`
}

type bootstrapTokenView struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expiresAt"`
	Notice    string    `json:"notice"`
}

type conflictView struct {
	DetectedAt    time.Time `json:"detectedAt"`
	DistinctHosts int       `json:"distinctHosts"`
	MultipleHosts bool      `json:"multipleHosts"`
}

type errorView struct {
	Error string `json:"error"`
}

func viewOf(relay storage.RelaySummary) relayView {
	view := relayView{
		RegistrationID:     relay.RegistrationID.String(),
		ClusterFingerprint: relay.ClusterFingerprint,
		RelayVersion:       relay.RelayVersion,
		Compatibility:      "unknown",
		RegisteredAt:       relay.RegisteredAt,
		Connected:          relay.Connected,
		Capabilities:       relay.Capabilities,
	}
	if relay.ProtocolVersion != 0 {
		view.Compatibility = "compatible"
	}
	if view.Capabilities == nil {
		view.Capabilities = []string{}
	}
	if !relay.LastSeenAt.IsZero() {
		seen := relay.LastSeenAt
		view.LastSeenAt = &seen
	}
	if !relay.RevokedAt.IsZero() {
		revoked := relay.RevokedAt
		view.RevokedAt = &revoked
	}
	if !relay.Conflict.DetectedAt.IsZero() {
		view.SessionConflict = &conflictView{
			DetectedAt:    relay.Conflict.DetectedAt,
			DistinctHosts: relay.Conflict.DistinctHosts,
			MultipleHosts: relay.Conflict.DistinctHosts > 1,
		}
	}
	return view
}

func writeJSON(writer http.ResponseWriter, status int, body any) {
	writer.Header().Set("Content-Type", "application/json")
	// Prevent a shared cache from serving one Organization's response to another.
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("X-Content-Type-Options", "nosniff")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(body)
}
