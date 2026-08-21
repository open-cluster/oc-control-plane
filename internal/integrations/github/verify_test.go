package github

import (
	"net/http"
	"strings"
	"testing"

	"github.com/open-cluster/oc-control-plane/internal/integrations"
)

// The probe against the fake vendor: a reachable installation with repositories is
// active, a suspended or unknown one is failed with the reason, and a deployment with no
// App at all says so instead of pretending to check.

// appAgainst builds a configured App whose client reaches the fake.
func appAgainst(t *testing.T, fake *fakeGitHub) *App {
	t.Helper()
	app, err := NewApp("12345", pemPKCS1(testKey(t)), NewClient(fake.URL))
	if err != nil {
		t.Fatalf("building the app: %v", err)
	}
	return app
}

// healthyInstallation teaches the fake a working installation 77 with two repositories.
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

func TestProbeAgainstAHealthyInstallationIsActive(t *testing.T) {
	t.Parallel()

	fake := newFakeGitHub(t)
	healthyInstallation(fake)

	verified := probe(testContext(t), appAgainst(t, fake), NewClient(fake.URL), 77, nil)
	if verified.Status != integrations.StatusActive {
		t.Fatalf("status = %s, want active; note: %s", verified.Status, verified.Note)
	}
	if !strings.Contains(verified.Note, "acme-corp") || !strings.Contains(verified.Note, "2") {
		t.Errorf("the note %q does not name the account and what it grants", verified.Note)
	}
}

func TestProbeAgainstAnUnknownInstallationIsFailed(t *testing.T) {
	t.Parallel()

	fake := newFakeGitHub(t)
	fake.answers["/app/installations/77"] = func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusNotFound)
		_, _ = writer.Write([]byte(`{"message":"Not Found"}`))
	}

	verified := probe(testContext(t), appAgainst(t, fake), NewClient(fake.URL), 77, nil)
	if verified.Status != integrations.StatusFailed {
		t.Fatalf("status = %s, want failed; note: %s", verified.Status, verified.Note)
	}
	if !strings.Contains(verified.Note, "does not know installation 77") {
		t.Errorf("the note %q does not say what github said", verified.Note)
	}
}

func TestProbeAgainstASuspendedInstallationIsFailed(t *testing.T) {
	t.Parallel()

	fake := newFakeGitHub(t)
	fake.answer("/app/installations/77", `{"id":77,
		"account":{"login":"acme-corp","type":"Organization"},
		"repository_selection":"selected","suspended_at":"2026-08-01T00:00:00Z"}`)

	verified := probe(testContext(t), appAgainst(t, fake), NewClient(fake.URL), 77, nil)
	if verified.Status != integrations.StatusFailed {
		t.Fatalf("status = %s, want failed; note: %s", verified.Status, verified.Note)
	}
	if !strings.Contains(verified.Note, "suspended") {
		t.Errorf("the note %q does not say the installation is suspended", verified.Note)
	}
}

func TestProbeWithNoRepositoriesGrantedIsDegraded(t *testing.T) {
	t.Parallel()

	fake := newFakeGitHub(t)
	healthyInstallation(fake)
	fake.answer("/installation/repositories", `{"total_count":0,"repositories":[]}`)

	verified := probe(testContext(t), appAgainst(t, fake), NewClient(fake.URL), 77, nil)
	if verified.Status != integrations.StatusDegraded {
		t.Fatalf("status = %s, want degraded; note: %s", verified.Status, verified.Note)
	}
	if !strings.Contains(verified.Note, "no repositories") {
		t.Errorf("the note %q does not say what is missing", verified.Note)
	}
}

func TestProbeWithoutAConfiguredAppSaysSo(t *testing.T) {
	t.Parallel()

	verified := probe(testContext(t), nil, NewClient("http://127.0.0.1:1"), 77, nil)
	if verified.Status != integrations.StatusFailed {
		t.Fatalf("status = %s, want failed; note: %s", verified.Status, verified.Note)
	}
	if !strings.Contains(verified.Note, "GitHub App") {
		t.Errorf("the note %q does not name what the deployment lacks", verified.Note)
	}
}

func TestProbeAgainstAnUnreachableVendorIsFailedWithoutGuessing(t *testing.T) {
	t.Parallel()

	unreachable := NewClient("http://127.0.0.1:1")
	app, err := NewApp("12345", pemPKCS1(testKey(t)), NewClient("http://127.0.0.1:1"))
	if err != nil {
		t.Fatalf("building the app: %v", err)
	}

	verified := probe(testContext(t), app, unreachable, 77, nil)
	if verified.Status != integrations.StatusFailed {
		t.Fatalf("status = %s, want failed; note: %s", verified.Status, verified.Note)
	}
	if !strings.Contains(verified.Note, "could not be reached") {
		t.Errorf("the note %q does not say the vendor was unreachable", verified.Note)
	}
}

func TestProbeRecordsWhichAccountAndHowFarItReaches(t *testing.T) {
	t.Parallel()

	fake := newFakeGitHub(t)
	healthyInstallation(fake)

	verified := probe(testContext(t), appAgainst(t, fake), NewClient(fake.URL), 77, nil)
	facts := verified.Facts
	if facts[FactAccount] != "acme-corp" || facts[FactAccountType] != "Organization" {
		t.Errorf("the facts %v do not name the account that answered", facts)
	}
	if facts[FactRepositorySelection] != "selected" || facts[FactRepositoryCount] != 2 {
		t.Errorf("the facts %v do not say how far the installation reaches", facts)
	}
	if facts[FactRepositoryCountAtLeast] != false {
		t.Errorf("a whole page of repositories was recorded as truncated: %v", facts)
	}
}

// A 404 is GitHub's answer both for an id that never existed and for an installation that
// was removed. Only the record can tell them apart, and an operator who is told "check the
// id" for an app somebody uninstalled goes looking for a number they never typed.
func TestProbeTellsARemovedInstallationApartFromAnUnknownOne(t *testing.T) {
	t.Parallel()

	fake := newFakeGitHub(t)
	fake.answers["/app/installations/77"] = func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusNotFound)
		_, _ = writer.Write([]byte(`{"message":"Not Found"}`))
	}
	previously := map[string]any{FactAccount: "acme-corp", FactAccountType: "Organization"}

	verified := probe(testContext(t), appAgainst(t, fake), NewClient(fake.URL), 77, previously)
	if verified.Status != integrations.StatusFailed {
		t.Fatalf("status = %s, want failed; note: %s", verified.Status, verified.Note)
	}
	if !strings.Contains(verified.Note, "no longer installed on acme-corp") {
		t.Errorf("the note %q reads as an unknown id rather than a removal", verified.Note)
	}
}
