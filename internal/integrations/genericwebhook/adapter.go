// Package genericwebhook accepts the canonical Alert Event contract for alert sources
// without a first-class provider adapter.
package genericwebhook

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/open-cluster/oc-control-plane/internal/integrations"
	"github.com/open-cluster/oc-control-plane/internal/store/postgres"
)

type payload struct {
	EventID          string     `json:"eventId"`
	Status           string     `json:"status"`
	Title            string     `json:"title"`
	Severity         string     `json:"severity"`
	StartedAt        time.Time  `json:"startedAt"`
	ResolvedAt       *time.Time `json:"resolvedAt,omitempty"`
	DeduplicationKey string     `json:"deduplicationKey"`
	Labels           stringMap  `json:"labels,omitempty"`
	Annotations      stringMap  `json:"annotations,omitempty"`
	SourceURL        string     `json:"sourceUrl,omitempty"`
}

// Adapter normalizes the canonical Generic Webhook payload.
type Adapter struct{}

func (Adapter) Authenticate(headers http.Header, integration integrations.Integration) bool {
	return integrations.AuthenticateWebhookToken(headers, integration)
}

func (Adapter) Normalise(body []byte) (storage.NormalizedDelivery, error) {
	if !utf8.Valid(body) {
		return storage.NormalizedDelivery{}, fmt.Errorf("payload is not valid UTF-8")
	}
	if err := rejectDuplicateKeys(body); err != nil {
		return storage.NormalizedDelivery{}, err
	}
	if err := rejectNullFields(body, "resolvedAt", "sourceUrl"); err != nil {
		return storage.NormalizedDelivery{}, err
	}
	var decoded payload
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&decoded); err != nil {
		return storage.NormalizedDelivery{}, fmt.Errorf("payload is not generic webhook json: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return storage.NormalizedDelivery{}, fmt.Errorf("payload contains a trailing JSON value")
	}
	decoded.Title = strings.TrimSpace(decoded.Title)
	if err := validate(decoded); err != nil {
		return storage.NormalizedDelivery{}, err
	}
	decoded.StartedAt = decoded.StartedAt.UTC()
	if decoded.ResolvedAt != nil {
		resolved := decoded.ResolvedAt.UTC()
		decoded.ResolvedAt = &resolved
	}
	labels := make(map[string]string, len(decoded.Labels)+1)
	for key, value := range decoded.Labels {
		labels[key] = value
	}
	labels["severity"] = decoded.Severity

	event := storage.AlertEvent{
		SourceKey:    decoded.EventID,
		GroupingKey:  decoded.DeduplicationKey,
		Status:       storage.AlertEventFiring,
		Title:        decoded.Title,
		Labels:       labels,
		Annotations:  decoded.Annotations,
		GeneratorURL: decoded.SourceURL,
		StartedAt:    decoded.StartedAt,
	}
	if decoded.Status == "resolved" {
		event.Status = storage.AlertEventResolved
		if decoded.ResolvedAt != nil {
			event.ResolvedAt = *decoded.ResolvedAt
		}
	}
	canonical, err := json.Marshal(decoded)
	if err != nil {
		return storage.NormalizedDelivery{}, fmt.Errorf("encoding canonical generic webhook: %w", err)
	}
	digest := sha256.Sum256(canonical)
	return storage.NormalizedDelivery{
		ProviderIdentity: decoded.EventID,
		LifecyclePhase:   decoded.Status,
		ContentDigest:    digest[:],
		AlertEvents:      []storage.AlertEvent{event},
	}, nil
}

func validate(decoded payload) error {
	if err := boundedRequired("eventId", decoded.EventID, 256); err != nil {
		return err
	}
	if err := boundedRequired("title", strings.TrimSpace(decoded.Title), 512); err != nil {
		return err
	}
	if err := boundedRequired("deduplicationKey", decoded.DeduplicationKey, 256); err != nil {
		return err
	}
	if decoded.StartedAt.IsZero() {
		return fmt.Errorf("startedAt is required and must be RFC 3339")
	}
	switch decoded.Status {
	case "firing":
		if decoded.ResolvedAt != nil {
			return fmt.Errorf("resolvedAt is forbidden while firing")
		}
	case "resolved":
		if decoded.ResolvedAt == nil {
			return fmt.Errorf("resolvedAt is required when resolved")
		}
		if decoded.ResolvedAt.Before(decoded.StartedAt) {
			return fmt.Errorf("resolvedAt is earlier than startedAt")
		}
	default:
		return fmt.Errorf("status must be firing or resolved")
	}
	if decoded.Severity != "info" && decoded.Severity != "warning" && decoded.Severity != "critical" {
		return fmt.Errorf("severity must be info, warning, or critical")
	}
	if err := validateMap("labels", decoded.Labels, 256); err != nil {
		return err
	}
	if err := validateMap("annotations", decoded.Annotations, 2048); err != nil {
		return err
	}
	if decoded.SourceURL != "" {
		if strings.ContainsRune(decoded.SourceURL, 0) {
			return fmt.Errorf("sourceUrl contains a NUL character")
		}
		if utf8.RuneCountInString(decoded.SourceURL) > 2048 {
			return fmt.Errorf("sourceUrl is longer than 2048 characters")
		}
		parsed, err := url.Parse(decoded.SourceURL)
		if err != nil || !parsed.IsAbs() || (parsed.Scheme != "http" && parsed.Scheme != "https") ||
			parsed.Host == "" || parsed.User != nil {
			return fmt.Errorf("sourceUrl must be an absolute http or https URL without credentials")
		}
	}
	return nil
}

func boundedRequired(name, value string, maximum int) error {
	if strings.ContainsRune(value, 0) {
		return fmt.Errorf("%s contains a NUL character", name)
	}
	length := utf8.RuneCountInString(value)
	if length < 1 || length > maximum {
		return fmt.Errorf("%s must contain between 1 and %d characters", name, maximum)
	}
	return nil
}

func validateMap(name string, values stringMap, maximumValueLength int) error {
	if len(values) > 32 {
		return fmt.Errorf("%s contains more than 32 entries", name)
	}
	for key, value := range values {
		if strings.ContainsRune(key, 0) || strings.ContainsRune(value, 0) {
			return fmt.Errorf("%s contains a NUL character", name)
		}
		if length := utf8.RuneCountInString(key); length < 1 || length > 64 {
			return fmt.Errorf("%s key must contain between 1 and 64 characters", name)
		}
		if utf8.RuneCountInString(value) > maximumValueLength {
			return fmt.Errorf("%s value is longer than %d characters", name, maximumValueLength)
		}
	}
	return nil
}

func rejectNullFields(body []byte, names ...string) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return err
	}
	for _, name := range names {
		if value, present := fields[name]; present && bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return fmt.Errorf("%s must not be null", name)
		}
	}
	return nil
}

type stringMap map[string]string

func (m *stringMap) UnmarshalJSON(data []byte) error {
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return fmt.Errorf("map must be an object")
	}
	type plainMap map[string]string
	var decoded plainMap
	if err := json.Unmarshal(data, &decoded); err != nil {
		return fmt.Errorf("map must contain only string values: %w", err)
	}
	*m = stringMap(decoded)
	return nil
}

func rejectDuplicateKeys(body []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := scanJSONValue(decoder); err != nil {
		return fmt.Errorf("payload is not canonical JSON: %w", err)
	}
	if _, err := decoder.Token(); err != io.EOF {
		return fmt.Errorf("payload contains a trailing JSON value")
	}
	return nil
}

func scanJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, composite := token.(json.Delim)
	if !composite {
		return nil
	}
	switch delimiter {
	case '{':
		seen := map[string]struct{}{}
		for decoder.More() {
			keyToken, keyErr := decoder.Token()
			if keyErr != nil {
				return keyErr
			}
			key, ok := keyToken.(string)
			if !ok {
				return fmt.Errorf("object key is not a string")
			}
			if _, duplicate := seen[key]; duplicate {
				return fmt.Errorf("duplicate object key %q", key)
			}
			seen[key] = struct{}{}
			if err := scanJSONValue(decoder); err != nil {
				return err
			}
		}
	case '[':
		for decoder.More() {
			if err := scanJSONValue(decoder); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("unexpected JSON delimiter %q", delimiter)
	}
	closing, err := decoder.Token()
	if err != nil {
		return err
	}
	want := json.Delim('}')
	if delimiter == '[' {
		want = ']'
	}
	if closing != want {
		return fmt.Errorf("unexpected JSON delimiter %q", closing)
	}
	return nil
}
