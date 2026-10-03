package alertmanager

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/open-cluster/oc-control-plane/internal/alertevent"
	"github.com/open-cluster/oc-control-plane/internal/integrations"
)

const (
	maxTitleRunes        = 512
	maxSummaryRunes      = 4096
	maxSourceKeyLen      = 512
	maxAlertsPerBody     = 2048
	maxGeneratorURLRunes = 2048
)

type payload struct {
	Alerts          []alert `json:"alerts"`
	GroupKey        string  `json:"groupKey"`
	TruncatedAlerts int     `json:"truncatedAlerts"`
}

type alert struct {
	Status       string            `json:"status"`
	GeneratorURL string            `json:"generatorURL"`
	Fingerprint  string            `json:"fingerprint"`
	Labels       map[string]string `json:"labels"`
	Annotations  map[string]string `json:"annotations"`
	StartsAt     time.Time         `json:"startsAt"`
	EndsAt       time.Time         `json:"endsAt"`
}

type Adapter struct{}

func (Adapter) Authenticate(headers http.Header, integration integrations.Integration) bool {
	return integrations.AuthenticateWebhookToken(headers, integration)
}

func (Adapter) Normalize(body []byte) (alertevent.AlertDelivery, error) {
	var decoded payload
	// Ignore unknown fields because Alertmanager extends this payload between releases.
	if err := json.Unmarshal(body, &decoded); err != nil {
		return alertevent.AlertDelivery{}, fmt.Errorf("payload is not alertmanager json: %w", err)
	}
	if len(decoded.Alerts) == 0 {
		return alertevent.AlertDelivery{}, errors.New("payload carries no alerts")
	}
	if len(decoded.Alerts) > maxAlertsPerBody {
		return alertevent.AlertDelivery{}, fmt.Errorf("payload carries %d alerts, more than the %d accepted",
			len(decoded.Alerts), maxAlertsPerBody)
	}
	if decoded.TruncatedAlerts < 0 {
		return alertevent.AlertDelivery{}, errors.New("payload reports a negative number of omitted alerts")
	}

	groupKey := truncate(decoded.GroupKey, maxSourceKeyLen)

	alertEvents := make([]alertevent.AlertEvent, 0, len(decoded.Alerts))
	for _, one := range decoded.Alerts {
		alertEvent, err := signalFrom(one, groupKey)
		if err != nil {
			return alertevent.AlertDelivery{}, err
		}
		alertEvents = append(alertEvents, alertEvent)
	}
	digest := sha256.Sum256(body)
	return alertevent.AlertDelivery{
		ProviderIdentity: fmt.Sprintf("%x", digest),
		ContentDigest:    digest[:],
		Truncated:        decoded.TruncatedAlerts,
		AlertEvents:      alertEvents,
	}, nil
}

func signalFrom(one alert, groupKey string) (alertevent.AlertEvent, error) {
	if one.Fingerprint == "" {
		return alertevent.AlertEvent{}, errors.New("alert carries no fingerprint to identify it by")
	}
	if len(one.Fingerprint) > maxSourceKeyLen {
		return alertevent.AlertEvent{}, errors.New("alert fingerprint is longer than any identity")
	}

	alertEvent, err := stateOf(one)
	if err != nil {
		return alertevent.AlertEvent{}, err
	}

	alertEvent.SourceKey = one.Fingerprint
	alertEvent.GroupingKey = groupKey
	alertEvent.Title = truncate(titleOf(one), maxTitleRunes)
	alertEvent.Summary = truncate(summaryOf(one), maxSummaryRunes)
	alertEvent.Labels = one.Labels
	alertEvent.Annotations = one.Annotations
	alertEvent.GeneratorURL = truncate(one.GeneratorURL, maxGeneratorURLRunes)
	return alertEvent, nil
}

func stateOf(one alert) (alertevent.AlertEvent, error) {
	if one.StartsAt.IsZero() {
		return alertevent.AlertEvent{}, errors.New("alert carries no start time to identify its incident by")
	}

	switch one.Status {
	case "firing":
		return alertevent.AlertEvent{Status: alertevent.AlertEventFiring, StartedAt: one.StartsAt}, nil
	case "resolved":
		if one.EndsAt.IsZero() {
			return alertevent.AlertEvent{}, errors.New("a resolved alert carries no end time")
		}
		if one.EndsAt.Before(one.StartsAt) {
			return alertevent.AlertEvent{}, errors.New("an alert ended before it started")
		}
		return alertevent.AlertEvent{
			Status:     alertevent.AlertEventResolved,
			StartedAt:  one.StartsAt,
			ResolvedAt: one.EndsAt,
		}, nil
	default:
		return alertevent.AlertEvent{}, fmt.Errorf("alert status %q is not one this adapter knows", one.Status)
	}
}

func titleOf(one alert) string {
	if name := one.Labels["alertname"]; name != "" {
		return name
	}
	return "unnamed alert"
}

func summaryOf(one alert) string {
	if summary := one.Annotations["summary"]; summary != "" {
		return summary
	}
	return one.Annotations["description"]
}

func truncate(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit])
}
