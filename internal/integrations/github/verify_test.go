package github

import (
	"net/http"
	"strings"
	"testing"

	"github.com/open-cluster/oc-control-plane/internal/integrations"
)

func appAgainst(t *testing.T, fake *fakeGitHub) *App {
	t.Helper()
	app, err := NewApp("12345", pemPKCS1(testKey(t)), NewClient(fake.URL))
	if err != nil {
		t.Fatalf("building the app: %v", err)
	}
	return app
}

func deployedAgainst(t *testing.T, fake *fakeGitHub) deployment {
	t.Helper()
	return deployment{app: appAgainst(t, fake), client: NewClient(fake.URL)}
}

func healthyInstallation(fake *fakeGitHub) {
	fake.answer("/app/installations/77", `{"id":77,
		"account":{"login":"acme-corp","type":"Organization"},
		"repository_selection":"selected","suspended_at":null}`)
	fake.answers["/app/installations/77/access_tokens"] = func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusCreated)
		_, _ = writer.Write([]byte(`{"token":"ghs_minted","expires_at":"2036-01-01T00:00:00Z"}`))
	}
	fake.answer("/installation/repositories", `{"total_count":2,"repositories":[
		{"id":1,"name":"payments","full_name":"acme-corp/payments"},
		{"id":2,"name":"deploy","full_name":"acme-corp/deploy"}]}`)
}

func TestProbeAgainstAHealthyInstallationIsVerified(t *testing.T) {
	t.Parallel()
	fake := newFakeGitHub(t)
	healthyInstallation(fake)

	verified := probe(testContext(t), deployedAgainst(t, fake), 77)
	if verified.Status != integrations.StatusVerified {
		t.Fatalf("status = %s, want verified; note: %s", verified.Status, verified.Note)
	}
	if !strings.Contains(verified.Note, "acme-corp") || !strings.Contains(verified.Note, "2") {
		t.Errorf("the note %q does not name the account and what it grants", verified.Note)
	}
}

func TestProbeRefusalsAreFailed(t *testing.T) {
	t.Parallel()

	tests := map[string]func(*fakeGitHub){
		"unknown installation": func(fake *fakeGitHub) {
			fake.answers["/app/installations/77"] = func(writer http.ResponseWriter, _ *http.Request) {
				writer.WriteHeader(http.StatusNotFound)
			}
		},
		"suspended installation": func(fake *fakeGitHub) {
			fake.answer("/app/installations/77", `{"id":77,"account":{"login":"acme-corp","type":"Organization"},"suspended_at":"2026-08-01T00:00:00Z"}`)
		},
	}
	for name, configure := range tests {
		t.Run(name, func(t *testing.T) {
			fake := newFakeGitHub(t)
			configure(fake)
			verified := probe(testContext(t), deployedAgainst(t, fake), 77)
			if verified.Status != integrations.StatusFailed {
				t.Fatalf("status = %s, want failed; note: %s", verified.Status, verified.Note)
			}
		})
	}
}

func TestProbeWithNoRepositoriesIsStillVerified(t *testing.T) {
	t.Parallel()
	fake := newFakeGitHub(t)
	healthyInstallation(fake)
	fake.answer("/installation/repositories", `{"total_count":0,"repositories":[]}`)

	verified := probe(testContext(t), deployedAgainst(t, fake), 77)
	if verified.Status != integrations.StatusVerified || !strings.Contains(verified.Note, "no repositories") {
		t.Fatalf("verification = %+v", verified)
	}
}

func TestProbeWithoutAConfiguredAppSaysSo(t *testing.T) {
	t.Parallel()
	verified := probe(testContext(t), deployment{client: NewClient("http://127.0.0.1:1")}, 77)
	if verified.Status != integrations.StatusFailed || !strings.Contains(verified.Note, "GitHub App") {
		t.Fatalf("verification = %+v", verified)
	}
}
