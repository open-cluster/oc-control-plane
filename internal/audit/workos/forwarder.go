// Package workos forwards committed audit events to an optional hosted WorkOS sink.
package workos

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/open-cluster/oc-control-plane/internal/audit"
)

const (
	defaultAPIURL  = "https://api.workos.com"
	requestTimeout = 10 * time.Second
	maxErrorBody   = 1024
)

// Forwarder translates authoritative PostgreSQL audit events into WorkOS events.
type Forwarder struct {
	endpoint      string
	apiKey        string
	organizations map[string]string
	client        *http.Client
}

// New configures a hosted-only remote sink with explicit local-to-WorkOS tenant mapping.
func New(endpoint, apiKey string, organizations map[string]string) (*Forwarder, error) {
	if strings.TrimSpace(endpoint) == "" {
		endpoint = defaultAPIURL
	}
	parsed, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil || parsed.Host == "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("the WorkOS API endpoint must be an absolute HTTPS URL")
	}
	if parsed.Scheme != "https" &&
		(parsed.Scheme != "http" || parsed.Hostname() != "localhost" &&
			!net.ParseIP(parsed.Hostname()).IsLoopback()) {
		return nil, errors.New("the WorkOS API endpoint must use HTTPS")
	}
	if strings.TrimSpace(apiKey) == "" {
		return nil, errors.New("the hosted WorkOS API credential is missing")
	}
	if len(organizations) == 0 {
		return nil, errors.New("hosted WorkOS audit forwarding requires organization mappings")
	}
	mapped := make(map[string]string, len(organizations))
	for local, remote := range organizations {
		if strings.TrimSpace(local) == "" || strings.TrimSpace(remote) == "" {
			return nil, errors.New("WorkOS organization mappings cannot contain empty identities")
		}
		mapped[strings.TrimSpace(local)] = strings.TrimSpace(remote)
	}
	return &Forwarder{
		endpoint: strings.TrimRight(parsed.String(), "/"), apiKey: strings.TrimSpace(apiKey),
		organizations: mapped, client: &http.Client{Timeout: requestTimeout},
	}, nil
}

// Forward sends one committed event with its durable identity as the idempotency key.
func (f *Forwarder) Forward(ctx context.Context, recorded audit.Recorded) error {
	organization, found := f.organizations[recorded.Organization]
	if !found {
		return errors.New("the audit event has no WorkOS organization mapping")
	}
	if strings.TrimSpace(recorded.ID) == "" {
		return errors.New("the audit event has no durable idempotency identity")
	}
	actorID := recorded.Actor.ID
	if actorID == "" {
		actorID = "system"
	}
	body := struct {
		Organization string `json:"organization_id"`
		Event        any    `json:"event"`
	}{
		Organization: organization,
		Event: map[string]any{
			"action":      string(recorded.Action),
			"occurred_at": recorded.OccurredAt.UTC().Format(time.RFC3339Nano),
			"version":     1,
			"actor": map[string]any{
				"type": recorded.Actor.Kind.String(), "id": actorID,
				"name": recorded.Actor.DisplayName,
			},
			"targets": []map[string]string{{
				"type": string(recorded.Target.Kind), "id": recorded.Target.ID,
			}},
			"context": map[string]string{"location": recorded.SourceAddress},
			"metadata": map[string]any{
				"outcome": recorded.Outcome.String(), "request_id": recorded.RequestID,
				"detail": recorded.Detail.Safe(),
			},
		},
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return errors.New("the audit event could not be encoded for WorkOS")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost,
		f.endpoint+"/audit_logs/events", bytes.NewReader(encoded))
	if err != nil {
		return errors.New("the WorkOS audit request could not be created")
	}
	request.Header.Set("Authorization", "Bearer "+f.apiKey)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", recorded.ID)
	response, err := f.client.Do(request)
	if err != nil {
		return errors.New("the WorkOS audit request failed")
	}
	defer func() { _ = response.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxErrorBody))
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("the WorkOS audit service returned HTTP %d", response.StatusCode)
	}
	return nil
}
