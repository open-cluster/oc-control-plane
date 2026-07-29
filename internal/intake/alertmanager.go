package intake

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/open-cluster/oc-control-plane/internal/storage"
)

// alertmanagerKind is the value the alert_source.kind column holds for this adapter.
const alertmanagerKind = "prometheus.alertmanager"

// Bounds on what one delivery may contain. The body size limit already bounds this
// indirectly; these exist so an over-long field is a normalisation decision made here rather
// than a constraint violation surfacing from the database as a server error.
const (
	maxTitleRunes    = 512
	maxSummaryRunes  = 4096
	maxSourceKeyLen  = 512
	maxAlertsPerBody = 2048
)

// alertmanagerPayload is the v4 webhook contract. Only the fields this platform uses are
// declared: an unknown field is ignored rather than refused, because Alertmanager adds to this
// payload between releases and a customer upgrading theirs must not stop being able to deliver.
type alertmanagerPayload struct {
	Alerts []alertmanagerAlert `json:"alerts"`
	// TruncatedAlerts is how many Alertmanager left out because the payload hit its configured
	// maximum. It sends this instead of the alerts, so ignoring it would record a partial
	// picture of a storm as a complete one — which is the moment a complete picture matters.
	TruncatedAlerts int `json:"truncatedAlerts"`
}

type alertmanagerAlert struct {
	Status string `json:"status"`
	// Fingerprint is Alertmanager's own identity for this alert. It is the deduplication key
	// because the source already has one, and inventing a different one guarantees
	// disagreement about what "the same alert" means.
	Fingerprint string            `json:"fingerprint"`
	Labels      map[string]string `json:"labels"`
	Annotations map[string]string `json:"annotations"`
	StartsAt    time.Time         `json:"startsAt"`
	EndsAt      time.Time         `json:"endsAt"`
}

// alertmanagerAdapter normalises Prometheus Alertmanager's v4 webhook payload.
type alertmanagerAdapter struct{}

// Normalise turns one delivery into Signals.
//
// A delivery carrying no alerts is an error rather than an empty success. Alertmanager does
// not send one, so it means the body was not the payload it claims to be — and accepting it
// would record a delivery that proves the integration works while carrying nothing.
func (alertmanagerAdapter) Normalise(body []byte) (Normalised, error) {
	var payload alertmanagerPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return Normalised{}, fmt.Errorf("payload is not alertmanager json: %w", err)
	}
	if len(payload.Alerts) == 0 {
		return Normalised{}, errors.New("payload carries no alerts")
	}
	if len(payload.Alerts) > maxAlertsPerBody {
		return Normalised{}, fmt.Errorf("payload carries %d alerts, more than the %d accepted",
			len(payload.Alerts), maxAlertsPerBody)
	}
	if payload.TruncatedAlerts < 0 {
		return Normalised{}, errors.New("payload reports a negative number of omitted alerts")
	}

	signals := make([]storage.Signal, 0, len(payload.Alerts))
	for _, alert := range payload.Alerts {
		signal, err := signalFrom(alert)
		if err != nil {
			// One unusable alert fails the whole delivery. Accepting the rest would leave the
			// source told it succeeded while part of what it sent was silently dropped, and it
			// will never send that part again.
			return Normalised{}, err
		}
		signals = append(signals, signal)
	}
	return Normalised{Signals: signals, Truncated: payload.TruncatedAlerts}, nil
}

func signalFrom(alert alertmanagerAlert) (storage.Signal, error) {
	if alert.Fingerprint == "" {
		return storage.Signal{}, errors.New("alert carries no fingerprint to identify it by")
	}
	if len(alert.Fingerprint) > maxSourceKeyLen {
		return storage.Signal{}, errors.New("alert fingerprint is longer than any identity")
	}

	signal, err := stateOf(alert)
	if err != nil {
		return storage.Signal{}, err
	}

	signal.SourceKey = alert.Fingerprint
	signal.Title = truncate(titleOf(alert), maxTitleRunes)
	signal.Summary = truncate(summaryOf(alert), maxSummaryRunes)
	signal.Labels = alert.Labels
	return signal, nil
}

// stateOf reads the alert's status and the times that bound this episode of it.
//
// StartsAt is read for both statuses, and that is the load-bearing part. It is what identifies
// the episode, so a resolution has to carry the same one the firing did or it would resolve
// nothing and open a second episode instead. Alertmanager sends it on both, which is what makes
// this model possible at all.
func stateOf(alert alertmanagerAlert) (storage.Signal, error) {
	if alert.StartsAt.IsZero() {
		return storage.Signal{}, errors.New("alert carries no start time to identify its episode by")
	}

	switch alert.Status {
	case "firing":
		return storage.Signal{Status: storage.SignalFiring, StartedAt: alert.StartsAt}, nil
	case "resolved":
		if alert.EndsAt.IsZero() {
			return storage.Signal{}, errors.New("a resolved alert carries no end time")
		}
		if alert.EndsAt.Before(alert.StartsAt) {
			return storage.Signal{}, errors.New("an alert ended before it started")
		}
		return storage.Signal{
			Status:     storage.SignalResolved,
			StartedAt:  alert.StartsAt,
			ResolvedAt: alert.EndsAt,
		}, nil
	default:
		// No fall-through default that guesses. A status this adapter does not know is a
		// payload it does not understand, and inventing "firing" would create an incident from
		// something that might be the opposite.
		return storage.Signal{}, fmt.Errorf("alert status %q is not one this adapter knows", alert.Status)
	}
}

// titleOf names the alert. Alertmanager always sets alertname, but a payload that omits it is
// still usable — the identity is the fingerprint — so this degrades rather than refusing.
func titleOf(alert alertmanagerAlert) string {
	if name := alert.Labels["alertname"]; name != "" {
		return name
	}
	return "unnamed alert"
}

// summaryOf prefers the annotation meant for humans, then the longer one. Both are free text
// from a customer's systems and stay untrusted for their whole life: they may be
// attacker-influenced and must never become an instruction, a destination, or an
// authorisation claim downstream.
func summaryOf(alert alertmanagerAlert) string {
	if summary := alert.Annotations["summary"]; summary != "" {
		return summary
	}
	return alert.Annotations["description"]
}

// truncate bounds a field by runes rather than bytes, so cutting a multi-byte character in
// half cannot produce text that is no longer valid UTF-8.
func truncate(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit])
}
