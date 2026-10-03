package alertevent

import "time"

type AlertEventStatus int16

const (
	AlertEventFiring AlertEventStatus = iota + 1
	AlertEventResolved
)

type AlertEvent struct {
	SourceKey    string
	GroupingKey  string
	Status       AlertEventStatus
	Title        string
	Summary      string
	Labels       map[string]string
	Annotations  map[string]string
	GeneratorURL string
	StartedAt    time.Time
	ResolvedAt   time.Time
}

type AlertDelivery struct {
	ProviderIdentity string
	LifecyclePhase   string
	ContentDigest    []byte
	Truncated        int
	AlertEvents      []AlertEvent
}
