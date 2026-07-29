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

type rosterView struct {
	Relays []relayView `json:"relays"`
	// Next resumes the next page, and is absent when this is the last one. Its presence is also
	// how a caller knows relays were left out: a truncated list that looks complete is how an
	// operator concludes a relay is gone.
	Next string `json:"next,omitempty"`
}

type relayView struct {
	RegistrationID     string     `json:"registrationId"`
	ClusterFingerprint string     `json:"clusterFingerprint"`
	RelayVersion       string     `json:"relayVersion"`
	RegisteredAt       time.Time  `json:"registeredAt"`
	RevokedAt          *time.Time `json:"revokedAt,omitempty"`
	// SessionConflict is absent when nothing has been seen, so its presence is the finding
	// rather than a field an operator has to compare against a zero value.
	SessionConflict *conflictView `json:"sessionConflict,omitempty"`
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
