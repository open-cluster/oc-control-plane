// Package alertmanager is the Prometheus Alertmanager provider: the Integration Type
// definition, its verification, and the adapter that normalises the v4 webhook payload
// into AlertEvents.
//
// A vendor's payload shape exists inside its own provider package and nowhere else, so
// nothing downstream of Normalise can tell which system delivered a AlertEvent. The payload
// types are unexported, so that boundary is enforced by the compiler rather than by a
// reviewer noticing.
//
// Everything this package reads is untrusted text from a customer's systems. It may be
// attacker-influenced and must never become an instruction, a destination, or an
// authorisation claim downstream.
package alertmanager

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/open-cluster/oc-control-plane/internal/integrations"
	"github.com/open-cluster/oc-control-plane/internal/store/postgres"
)

// Bounds on what one delivery may contain. The body size limit already bounds this
// indirectly; these exist so an over-long field is a normalisation decision made here rather
// than a constraint violation surfacing from the database as a server error.
const (
	maxTitleRunes    = 512
	maxSummaryRunes  = 4096
	maxSourceKeyLen  = 512
	maxAlertsPerBody = 2048
	// maxGeneratorURLRunes bounds the alert's own origin pointer. A longer one is
	// pathological and cut rather than refused: the alert itself is still usable.
	maxGeneratorURLRunes = 2048
)

// payload is the v4 webhook contract. Only the fields this platform uses are declared: an
// unknown field is ignored rather than refused, because Alertmanager adds to this payload
// between releases and a customer upgrading theirs must not stop being able to deliver.
type payload struct {
	Alerts []alert `json:"alerts"`
	// GroupKey is Alertmanager's own identity for the set of alerts it is delivering together. It
	// is computed from the group_by the customer's own operator wrote, which is what makes it
	// usable as an Incident's key: when two alerts land in one incident here it is because
	// their own system already decided they belong together. Nothing on this platform infers it.
	//
	// An empty one is not an error. Alertmanager always sends it, and a payload that omits it
	// simply produces one incident per alert — a split rather than a merge, which is the failure
	// worth having.
	GroupKey string `json:"groupKey"`
	// TruncatedAlerts is how many Alertmanager left out because the payload hit its configured
	// maximum. It sends this instead of the alerts, so ignoring it would record a partial
	// picture of a storm as a complete one — which is the moment a complete picture matters.
	TruncatedAlerts int `json:"truncatedAlerts"`
}

type alert struct {
	Status string `json:"status"`
	// GeneratorURL is where the source says this alert came from — the alert's own
	// pointer back to its origin, preserved rather than thrown away at intake.
	GeneratorURL string `json:"generatorURL"`
	// Fingerprint is Alertmanager's own identity for this alert. It is the deduplication key
	// because the source already has one, and inventing a different one guarantees
	// disagreement about what "the same alert" means.
	Fingerprint string            `json:"fingerprint"`
	Labels      map[string]string `json:"labels"`
	Annotations map[string]string `json:"annotations"`
	StartsAt    time.Time         `json:"startsAt"`
	EndsAt      time.Time         `json:"endsAt"`
}

// Adapter normalises Prometheus Alertmanager's v4 webhook payload.
type Adapter struct{}

func (Adapter) Authenticate(headers http.Header, integration integrations.Integration) bool {
	return integrations.AuthenticateWebhookToken(headers, integration)
}

// Normalise turns one delivery into AlertEvents and reports how many the source says it left out.
//
// A delivery carrying no alerts is an error rather than an empty success. Alertmanager does
// not send one, so it means the body was not the payload it claims to be — and accepting it
// would record a delivery that proves the integration works while carrying nothing.
func (Adapter) Normalise(body []byte) (storage.NormalizedDelivery, error) {
	var decoded payload
	if err := json.Unmarshal(body, &decoded); err != nil {
		return storage.NormalizedDelivery{}, fmt.Errorf("payload is not alertmanager json: %w", err)
	}
	if len(decoded.Alerts) == 0 {
		return storage.NormalizedDelivery{}, errors.New("payload carries no alerts")
	}
	if len(decoded.Alerts) > maxAlertsPerBody {
		return storage.NormalizedDelivery{}, fmt.Errorf("payload carries %d alerts, more than the %d accepted",
			len(decoded.Alerts), maxAlertsPerBody)
	}
	if decoded.TruncatedAlerts < 0 {
		return storage.NormalizedDelivery{}, errors.New("payload reports a negative number of omitted alerts")
	}

	// The group key is bounded here rather than being allowed to fail the write, for the same
	// reason a title is: an over-long field is a normalisation decision this adapter makes, not a
	// constraint violation surfacing from the database as a server error.
	groupKey := truncate(decoded.GroupKey, maxSourceKeyLen)

	alertEvents := make([]storage.AlertEvent, 0, len(decoded.Alerts))
	for _, one := range decoded.Alerts {
		alertEvent, err := signalFrom(one, groupKey)
		if err != nil {
			// One unusable alert fails the whole delivery. Accepting the rest would leave the
			// source told it succeeded while part of what it sent was silently dropped, and it
			// will never send that part again.
			return storage.NormalizedDelivery{}, err
		}
		alertEvents = append(alertEvents, alertEvent)
	}
	digest := sha256.Sum256(body)
	return storage.NormalizedDelivery{
		ProviderIdentity: fmt.Sprintf("%x", digest),
		ContentDigest:    digest[:],
		Truncated:        decoded.TruncatedAlerts,
		AlertEvents:      alertEvents,
	}, nil
}

// signalFrom normalises one alert. The group key is the DELIVERY's, applied to every alert in it,
// because that is the level Alertmanager decides grouping at. The internal model carries it per
// AlertEvent so that a source which groups per alert needs no change to anything but its own adapter.
func signalFrom(one alert, groupKey string) (storage.AlertEvent, error) {
	if one.Fingerprint == "" {
		return storage.AlertEvent{}, errors.New("alert carries no fingerprint to identify it by")
	}
	if len(one.Fingerprint) > maxSourceKeyLen {
		return storage.AlertEvent{}, errors.New("alert fingerprint is longer than any identity")
	}

	alertEvent, err := stateOf(one)
	if err != nil {
		return storage.AlertEvent{}, err
	}

	alertEvent.SourceKey = one.Fingerprint
	alertEvent.GroupingKey = groupKey
	alertEvent.Title = truncate(titleOf(one), maxTitleRunes)
	alertEvent.Summary = truncate(summaryOf(one), maxSummaryRunes)
	alertEvent.Labels = one.Labels
	// Annotations travel whole, like labels: they carry the operator's own runbook and
	// dashboard pointers, which are exactly what an investigation wants at hand.
	alertEvent.Annotations = one.Annotations
	alertEvent.GeneratorURL = truncate(one.GeneratorURL, maxGeneratorURLRunes)
	return alertEvent, nil
}

// stateOf reads the alert's status and the times that bound this incident of it.
//
// StartsAt is read for both statuses, and that is the load-bearing part. It is what identifies
// the incident, so a resolution has to carry the same one the firing did or it would resolve
// nothing and open a second incident instead. Alertmanager sends it on both, which is what makes
// this model possible at all.
func stateOf(one alert) (storage.AlertEvent, error) {
	if one.StartsAt.IsZero() {
		return storage.AlertEvent{}, errors.New("alert carries no start time to identify its incident by")
	}

	switch one.Status {
	case "firing":
		return storage.AlertEvent{Status: storage.AlertEventFiring, StartedAt: one.StartsAt}, nil
	case "resolved":
		if one.EndsAt.IsZero() {
			return storage.AlertEvent{}, errors.New("a resolved alert carries no end time")
		}
		if one.EndsAt.Before(one.StartsAt) {
			return storage.AlertEvent{}, errors.New("an alert ended before it started")
		}
		return storage.AlertEvent{
			Status:     storage.AlertEventResolved,
			StartedAt:  one.StartsAt,
			ResolvedAt: one.EndsAt,
		}, nil
	default:
		// No fall-through default that guesses. A status this adapter does not know is a
		// payload it does not understand, and inventing "firing" would create an incident from
		// something that might be the opposite.
		return storage.AlertEvent{}, fmt.Errorf("alert status %q is not one this adapter knows", one.Status)
	}
}

// titleOf names the alert. Alertmanager always sets alertname, but a payload that omits it is
// still usable — the identity is the fingerprint — so this degrades rather than refusing.
func titleOf(one alert) string {
	if name := one.Labels["alertname"]; name != "" {
		return name
	}
	return "unnamed alert"
}

// summaryOf prefers the annotation meant for humans, then the longer one. Both are free text
// from a customer's systems and stay untrusted for their whole life.
func summaryOf(one alert) string {
	if summary := one.Annotations["summary"]; summary != "" {
		return summary
	}
	return one.Annotations["description"]
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
