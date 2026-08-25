package workos_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/open-cluster/oc-control-plane/internal/audit"
	"github.com/open-cluster/oc-control-plane/internal/audit/workos"
)

func TestForwardedEventUsesTheDurableIdentityAndMappedOrganization(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/audit_logs/events" {
			t.Errorf("unexpected WorkOS request: %s %s", request.Method, request.URL.Path)
		}
		if request.Header.Get("Authorization") != "Bearer sk_test_private" {
			t.Errorf("WorkOS credential was not presented as a bearer token")
		}
		if request.Header.Get("Idempotency-Key") != "audit-event-1" {
			t.Errorf("idempotency key = %q, want the durable audit event identity",
				request.Header.Get("Idempotency-Key"))
		}
		var body struct {
			Organization string `json:"organization_id"`
			Event        struct {
				Action  string `json:"action"`
				Version int    `json:"version"`
				Actor   struct {
					ID   string `json:"id"`
					Type string `json:"type"`
				} `json:"actor"`
				Targets []struct {
					ID   string `json:"id"`
					Type string `json:"type"`
				} `json:"targets"`
				Metadata map[string]any `json:"metadata"`
			} `json:"event"`
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatalf("decoding the WorkOS request: %v", err)
		}
		if body.Organization != "org_workos_acme" || body.Event.Action != "investigation.cancelled" ||
			body.Event.Version != 1 || body.Event.Actor.ID != "user-1" ||
			body.Event.Actor.Type != "user" || len(body.Event.Targets) != 1 ||
			body.Event.Targets[0].ID != "investigation-1" {
			t.Errorf("forwarded WorkOS event has the wrong organization or durable facts: %+v", body)
		}
		encoded, _ := json.Marshal(body.Event.Metadata)
		if strings.Contains(string(encoded), "credential-value") {
			t.Error("credential-shaped audit detail escaped into the hosted event")
		}
		writer.WriteHeader(http.StatusCreated)
	}))
	t.Cleanup(server.Close)

	forwarder, err := workos.New(server.URL, "sk_test_private",
		map[string]string{"org-acme": "org_workos_acme"})
	if err != nil {
		t.Fatalf("creating the hosted forwarder: %v", err)
	}
	err = forwarder.Forward(context.Background(), audit.Recorded{
		ID: "audit-event-1",
		Event: audit.Event{
			Organization: "org-acme",
			Actor:        audit.Actor{Kind: audit.ActorUser, ID: "user-1", DisplayName: "On-call"},
			Action:       audit.ActionInvestigationCancelled,
			Target:       audit.Target{Kind: audit.TargetInvestigation, ID: "investigation-1"},
			Outcome:      audit.OutcomeAllowed, OccurredAt: time.Date(2026, 8, 25, 10, 0, 0, 0, time.UTC),
			Detail: audit.Detail{"password": "credential-value", "subject": "checkout"},
		},
	})
	if err != nil {
		t.Fatalf("forwarding the committed audit event: %v", err)
	}
}

func TestAnUnmappedOrganizationNeverReachesTheHostedProvider(t *testing.T) {
	t.Parallel()

	forwarder, err := workos.New("https://api.workos.com", "sk_test_private",
		map[string]string{"org-acme": "org_workos_acme"})
	if err != nil {
		t.Fatal(err)
	}
	err = forwarder.Forward(context.Background(), audit.Recorded{
		ID: "audit-event-2", Event: audit.Event{Organization: "org-other"},
	})
	if err == nil || !strings.Contains(err.Error(), "has no WorkOS organization mapping") {
		t.Fatalf("an unmapped Organization was not refused safely: %v", err)
	}
}
