package alertevent

import "time"

type AlertEventStatus int16

const (
	AlertEventFiring AlertEventStatus = iota + 1
	AlertEventResolved
)

type AlertEvent struct {
	// SourceKey identifies the alert as its source names it and is stable across Incidents.
	SourceKey string
	// GroupingKey is the source's own notion of what belongs together. Empty produces one
	// Incident per Alert Event rather than inferring a group.
	GroupingKey string
	Status      AlertEventStatus
	Title       string
	Summary     string
	Labels      map[string]string
	// Annotations remain untrusted source text while preserving operational pointers.
	Annotations map[string]string
	// GeneratorURL is the source's pointer to the alert occurrence.
	GeneratorURL string
	// StartedAt and ResolvedAt use the source's clock, not receipt time.
	StartedAt  time.Time
	ResolvedAt time.Time
}

// AlertDelivery is the provider-owned meaning of one authenticated webhook body.
type AlertDelivery struct {
	ProviderIdentity string
	LifecyclePhase   string
	// ContentDigest is computed after validation so semantically identical encodings share
	// an identity where the provider contract requires canonicalization.
	ContentDigest []byte
	// Truncated is how many alerts the source says it omitted.
	Truncated   int
	AlertEvents []AlertEvent
}
