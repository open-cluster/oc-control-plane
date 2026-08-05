package connection

import (
	"time"

	"github.com/open-cluster/oc-control-plane/internal/storage"
)

// What the lifecycle operations say on the wire. Kept apart from the handlers for the same
// reason every view file here is: a field renamed is a client broken somewhere else.

// detailView is one Connection in full, as its own page rather than as a row in a list.
//
// It carries three things the list does not, because each costs a read the list should not pay
// per row: the credential's metadata, where a trigger's source should deliver, and how delivery
// has been going.
type detailView struct {
	connectionView
	State string `json:"state"`
	// ConfigurationRevision is what makes "it changed" answerable, and it is what a validation
	// result names so a result cannot keep looking current after the configuration it described
	// was edited.
	ConfigurationRevision int            `json:"configurationRevision"`
	Configuration         map[string]any `json:"configuration"`
	// Credential is metadata and never the credential. It is absent when the Connection holds
	// none, which an evidence Connection served by a Relay's own service account legitimately
	// does not.
	Credential *credentialView `json:"credential,omitempty"`
	// Trigger is present only for a Connection that answers triggers.
	Trigger *triggerView `json:"trigger,omitempty"`
}

// credentialView is the answer to "which credential is in use, and is it the one that expired"
// — asked and answered without a secret reaching a browser.
type credentialView struct {
	Method    string `json:"method"`
	Reference string `json:"reference"`
	// Fingerprint is a MINTED identity for this credential rather than a hash of it. A truncated
	// hash would let anyone holding a database dump confirm a guess offline, which is exactly the
	// property digest-only storage exists to deny.
	Fingerprint string     `json:"fingerprint"`
	CreatedAt   time.Time  `json:"createdAt"`
	RotatedAt   *time.Time `json:"rotatedAt,omitempty"`
	// ExpiresAt is absent when the credential has no stated expiry, which is a fact about the
	// credential rather than a field somebody forgot.
	ExpiresAt *time.Time `json:"expiresAt,omitempty"`
}

// triggerView is where a source should deliver and how that has been going.
type triggerView struct {
	// DeliveryEndpoint is where the customer's own system posts. It is absent when this
	// deployment has not been told what URL it is reachable at — a guess assembled from the
	// request's Host header would be a URL that works from wherever the console is and not from
	// the customer's alerting, which is the one place it has to work.
	DeliveryEndpoint string `json:"deliveryEndpoint,omitempty"`
	// Observed reports that anything has ever been attempted through this Connection. False is a
	// fact — nothing has ever arrived — and is different from a source that has stopped.
	Observed bool `json:"observed"`
	// LastReceivedAt and LastAcceptedAt are SEPARATE, and that is the point. A source delivering
	// every thirty seconds and being rejected every time has a recent one and an ancient other;
	// a single "last delivery" field would report whichever the implementer happened to pick and
	// would make a broken intake look healthy.
	LastReceivedAt *time.Time `json:"lastReceivedAt,omitempty"`
	LastAcceptedAt *time.Time `json:"lastAcceptedAt,omitempty"`
	Accepted       int        `json:"accepted"`
	Duplicates     int        `json:"duplicates"`
	Rejected       int        `json:"rejected"`
	// RejectionsByReason is what to go and fix — a signature, a clock skew — without reading the
	// history to work it out.
	RejectionsByReason map[string]int `json:"rejectionsByReason"`
	// WindowDays is how far back these counts reach, stated rather than assumed: a count with no
	// window is a number nobody can compare to anything.
	WindowDays int `json:"windowDays"`
}

// validationView is one recorded check.
type validationView struct {
	ID                    string `json:"id"`
	ConfigurationRevision int    `json:"configurationRevision"`
	// Outcome is `passed`, `partial` or `failed`. Partial is a first-class result and not a soft
	// failure: "authentication worked and two of five capabilities are unavailable" is something
	// to act on, and a boolean would turn it into something to retry.
	Outcome              string                    `json:"outcome"`
	Authentication       string                    `json:"authentication"`
	AuthenticationReason string                    `json:"authenticationReason,omitempty"`
	Capabilities         []capabilityReadinessView `json:"capabilities"`
	// Note is what this run does NOT establish, carried with the result so a green tick is not
	// worth more than it should be.
	Note        string    `json:"note,omitempty"`
	StartedAt   time.Time `json:"startedAt"`
	CompletedAt time.Time `json:"completedAt"`
}

type capabilityReadinessView struct {
	Capability string `json:"capability"`
	// Readiness is the COVERAGE vocabulary — available, unauthorized, unavailable,
	// not_attempted — rather than a second one invented for validation. A validation result and
	// a coverage gap are the same fact asked at different times.
	Readiness string `json:"readiness"`
	Reason    string `json:"reason,omitempty"`
}

// deliveryView is one delivery attempt in the history.
type deliveryView struct {
	ID string `json:"id"`
	// Outcome is `accepted`, `duplicate` or `rejected`. Duplicate is its own answer: a source
	// retrying because it never saw a response has done nothing wrong, and counting those as
	// rejections would make a healthy retry look like a broken signature.
	Outcome string `json:"outcome"`
	Reason  string `json:"reason,omitempty"`
	// SignalCount is how many SignalUpdates the delivery carried.
	SignalCount int `json:"signalCount"`
	// Test marks an operator's own probe rather than the customer's system.
	Test       bool      `json:"test"`
	ReceivedAt time.Time `json:"receivedAt"`
}

// dependentsView is what points at a Connection, and it is the body of BOTH the report and the
// refusal to delete — so an operator sees the same shape whether they asked in advance or found
// out by trying.
type dependentsView struct {
	Error      string          `json:"error,omitempty"`
	Dependents dependentCounts `json:"dependents"`
}

type dependentCounts struct {
	Signals    int `json:"signals"`
	Deliveries int `json:"deliveries"`
	Jobs       int `json:"jobs"`
	// LastEvidenceInEnvironment is the one dependent that is about the FUTURE rather than about
	// history: removing this would leave its Environment with nothing to investigate through.
	LastEvidenceInEnvironment bool `json:"lastEvidenceInEnvironment"`
	Any                       bool `json:"any"`
}

// testEventView says what a test event proved and, as plainly, what it did not.
type testEventView struct {
	Recorded bool   `json:"recorded"`
	Proves   string `json:"proves"`
	// DoesNotProve is here rather than in documentation because the whole value of a test event
	// is somebody believing the result, and a result believed to mean more than it does is worse
	// than no result.
	DoesNotProve string `json:"doesNotProve"`
}

// reviseRequest is a change to a Connection's configuration.
//
// It carries no credential field, deliberately. Rotating one is its own operation with its own
// permission and its own audit event; folding it in would make "I changed the endpoint" and "I
// replaced the credential" the same line in the record.
type reviseRequest struct {
	Configuration map[string]any    `json:"configuration"`
	Labels        map[string]string `json:"labels"`
}

func detailViewOf(found storage.Connection, endpoint string) detailView {
	view := detailView{
		connectionView:        viewOf(found),
		State:                 found.State.String(),
		ConfigurationRevision: found.ConfigurationRevision,
		Configuration:         found.Configuration,
	}
	if view.Configuration == nil {
		view.Configuration = map[string]any{}
	}
	if found.Credential.Held() {
		credential := credentialView{
			Method:      found.Credential.Method,
			Reference:   found.Credential.Reference,
			Fingerprint: found.Credential.Fingerprint,
			CreatedAt:   found.Credential.CreatedAt,
		}
		if !found.Credential.RotatedAt.IsZero() {
			rotated := found.Credential.RotatedAt
			credential.RotatedAt = &rotated
		}
		if !found.Credential.ExpiresAt.IsZero() {
			expires := found.Credential.ExpiresAt
			credential.ExpiresAt = &expires
		}
		view.Credential = &credential
	}
	if endpoint != "" {
		// The endpoint is part of the trigger section even when no delivery has been observed,
		// because it is what an operator needs BEFORE the first one.
		view.Trigger = &triggerView{
			DeliveryEndpoint:   endpoint,
			RejectionsByReason: map[string]int{},
		}
	}
	return view
}

func triggerViewOf(health storage.TriggerHealth) *triggerView {
	view := &triggerView{
		Observed:           health.Observed,
		Accepted:           health.Accepted,
		Duplicates:         health.Duplicates,
		Rejected:           health.Rejected,
		RejectionsByReason: health.RejectionsByReason,
		WindowDays:         int(health.Window.Hours() / 24),
	}
	if view.RejectionsByReason == nil {
		view.RejectionsByReason = map[string]int{}
	}
	if !health.LastReceivedAt.IsZero() {
		received := health.LastReceivedAt
		view.LastReceivedAt = &received
	}
	if !health.LastAcceptedAt.IsZero() {
		accepted := health.LastAcceptedAt
		view.LastAcceptedAt = &accepted
	}
	return view
}

func validationViewOf(recorded storage.Validation) validationView {
	capabilities := make([]capabilityReadinessView, 0, len(recorded.Capabilities))
	for _, readiness := range recorded.Capabilities {
		capabilities = append(capabilities, capabilityReadinessView{
			Capability: readiness.Capability,
			Readiness:  readiness.Readiness.String(),
			Reason:     readiness.Reason,
		})
	}
	return validationView{
		ID:                    recorded.ID.String(),
		ConfigurationRevision: recorded.ConfigurationRevision,
		Outcome:               recorded.Outcome.String(),
		Authentication:        recorded.Authentication.String(),
		AuthenticationReason:  recorded.AuthenticationReason,
		Capabilities:          capabilities,
		Note:                  recorded.Note,
		StartedAt:             recorded.StartedAt,
		CompletedAt:           recorded.CompletedAt,
	}
}

func deliveryViewOf(delivery storage.RecordedDelivery) deliveryView {
	return deliveryView{
		ID:          delivery.ID.String(),
		Outcome:     delivery.Disposition.String(),
		Reason:      delivery.Reason,
		SignalCount: delivery.SignalCount,
		Test:        delivery.Test,
		ReceivedAt:  delivery.ReceivedAt,
	}
}
