package operator

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/open-cluster/oc-control-plane/internal/storage"
)

// What the operator surface says on the wire. It is kept apart from the handlers because it is
// a contract: a field renamed here is a dashboard broken somewhere else, which is not true of
// anything in the handlers themselves.

type relayView struct {
	RegistrationID string `json:"registrationId"`
	// ClusterFingerprint is a DIGEST and is labelled one. It is what the relay claimed about the
	// cluster it sits in, pinned at enrolment so a later change refuses rather than silently
	// re-attributing evidence.
	ClusterFingerprint string     `json:"clusterFingerprint"`
	RelayVersion       string     `json:"relayVersion"`
	RegisteredAt       time.Time  `json:"registeredAt"`
	RevokedAt          *time.Time `json:"revokedAt,omitempty"`
	// Connected is derived from the durable presence rather than from any one process's session
	// registry, so the answer is the same from every instance.
	Connected bool `json:"connected"`
	// LastSeenAt is absent for a relay that has never held a session since presence began being
	// recorded, which is a different fact from being disconnected now.
	LastSeenAt *time.Time `json:"lastSeenAt,omitempty"`
	// Capabilities is what it advertised at enrolment. It is an attestation rather than an
	// authorization: what a relay may be ASKED to run is decided centrally.
	Capabilities []string `json:"capabilities"`
	// ServesEnvironments is derived from the Connections bound to this Relay, and it is here
	// INSTEAD of an Environment column. A Relay is Organization-scoped and carries no
	// Environment; what it can be said to serve is whatever its Connections reach, which changes
	// when one is bound rather than needing to be maintained.
	ServesEnvironments []string `json:"servesEnvironments"`
	// SessionConflict is absent when nothing has been seen, so its presence is the finding
	// rather than a field an operator has to compare against a zero value.
	SessionConflict *conflictView `json:"sessionConflict,omitempty"`
}

// fleetView is a hundred relays assessed rather than listed.
type fleetView struct {
	Total          int `json:"total"`
	Connected      int `json:"connected"`
	Disconnected   int `json:"disconnected"`
	Revoked        int `json:"revoked"`
	Outdated       int `json:"outdated"`
	Degraded       int `json:"degraded"`
	ActiveRequests int `json:"activeRequests"`
	// LivenessSeconds is how recently a relay must have been heard from to be counted connected.
	// Reported so a number nobody could interpret does not have to be.
	LivenessSeconds int `json:"livenessSeconds"`
	// MinimumVersion is the floor `outdated` was counted against, and OutdatedCounted says
	// whether it was counted at all. Zero outdated because nothing was compared and zero
	// outdated because everything is current are different facts, and a console that could not
	// tell them apart would report a fleet as current on the strength of a missing setting.
	MinimumVersion  string `json:"minimumVersion,omitempty"`
	OutdatedCounted bool   `json:"outdatedCounted"`
}

// servedConnectionView is one Connection a Relay serves, which is what disabling that Relay
// would cost.
type servedConnectionView struct {
	ID            string `json:"id"`
	EnvironmentID string `json:"environmentId"`
	Integration   string `json:"integration"`
	Name          string `json:"name"`
	Role          string `json:"role"`
	State         string `json:"state"`
	Disabled      bool   `json:"disabled"`
}

// relayFailureView is one execution a Relay did not complete. It carries no reason, and the
// envelope's `partial` says why rather than leaving an empty column to be read as "no reason".
type relayFailureView struct {
	JobID             string `json:"jobId"`
	CapabilityID      string `json:"capabilityId"`
	CapabilityVersion int    `json:"capabilityVersion"`
	ConnectionID      string `json:"connectionId"`
	// Outcome is `failed` or `cancelled`. Both produced nothing; only one is the Relay's fault.
	Outcome string    `json:"outcome"`
	At      time.Time `json:"at"`
}

// bootstrapTokenView is the one response on this surface that carries a credential.
type bootstrapTokenView struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expiresAt"`
	Notice    string    `json:"notice"`
}

// conflictView is the finding stated rather than left to be worked out.
type conflictView struct {
	DetectedAt    time.Time `json:"detectedAt"`
	DistinctHosts int       `json:"distinctHosts"`
	// MultipleHosts is the credential-theft signature said plainly. An operator scanning a list
	// should not have to already know that the difference between one host and two is the
	// difference between a relay that cannot hold a connection and a credential in two places.
	MultipleHosts bool `json:"multipleHosts"`
}

// trailView is what has happened to one relay identity. It exists because the current state
// cannot say: withdrawing a finding destroys it, so without this the second occurrence would
// look exactly like the first.
type trailView struct {
	Events []conflictEventView `json:"events"`
	Next   string              `json:"next,omitempty"`
}

type conflictEventView struct {
	// Kind is "detected" or "withdrawn": something the control plane saw, or something a person
	// said about what it saw.
	Kind string    `json:"kind"`
	At   time.Time `json:"at"`
	// DistinctHosts and MultipleHosts describe a detection, and are absent from a withdrawal,
	// which observed nothing of its own.
	DistinctHosts int  `json:"distinctHosts,omitempty"`
	MultipleHosts bool `json:"multipleHosts,omitempty"`
	// WithdrawnFrom is where a withdrawal came from. It is an address and not a person: the
	// surface is behind one shared token, so this is the whole of the attribution there is.
	WithdrawnFrom string `json:"withdrawnFrom,omitempty"`
}

func eventViewOf(event storage.ConflictEvent) conflictEventView {
	return conflictEventView{
		Kind:          event.Kind.String(),
		At:            event.At,
		DistinctHosts: event.DistinctHosts,
		MultipleHosts: event.DistinctHosts > 1,
		WithdrawnFrom: event.WithdrawnFrom,
	}
}

type errorView struct {
	Error string `json:"error"`
}

func viewOf(relay storage.RelaySummary) relayView {
	view := relayView{
		RegistrationID:     relay.RegistrationID.String(),
		ClusterFingerprint: relay.ClusterFingerprint,
		RelayVersion:       relay.RelayVersion,
		RegisteredAt:       relay.RegisteredAt,
		Connected:          relay.Connected,
		Capabilities:       relay.Capabilities,
		ServesEnvironments: relay.ServesEnvironments,
	}
	if view.Capabilities == nil {
		view.Capabilities = []string{}
	}
	if view.ServesEnvironments == nil {
		// An empty list rather than null. "This relay serves no Connections yet" is a fact worth
		// rendering, and a client should not have to handle two spellings of it.
		view.ServesEnvironments = []string{}
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

// writeJSON sends a response body. An encoding failure cannot be reported to the caller — the
// status is already written — so it is dropped here and would surface as a truncated body,
// which is visibly wrong rather than quietly wrong.
//
// Nothing this surface returns may be stored or re-typed by anything in front of it: every
// response carries data from a named tenant, and a cache holding one operator's answer is a
// cross-tenant disclosure waiting for the next request.
func writeJSON(writer http.ResponseWriter, status int, body any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("X-Content-Type-Options", "nosniff")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(body)
}
