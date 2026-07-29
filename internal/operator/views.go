package operator

import (
	"context"
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
	// More says the organization has relays this page does not show. A truncated list that
	// looks complete is how an operator concludes a relay is gone.
	More bool `json:"more"`
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
func writeJSON(writer http.ResponseWriter, status int, body any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(body)
}

// contextWithTimeout bounds one request's work. The request's own context still cancels it, so
// a caller that hangs up stops the query it started.
func contextWithTimeout(
	request *http.Request, within time.Duration,
) (context.Context, context.CancelFunc) {
	return context.WithTimeout(request.Context(), within)
}
