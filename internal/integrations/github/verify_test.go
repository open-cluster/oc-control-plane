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

// deployedAgainst is a deployment reaching the fake for both origins, which is what a
// deployment offering the installation flow looks like.
func deployedAgainst(t *testing.T, fake *fakeGitHub) deployment {
	t.Helper()
	return deployment{
		app: appAgainst(t, fake), client: NewClient(fake.URL), webURL: fake.URL,
	}
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

	verified := probe(testContext(t), deployedAgainst(t, fake), 77, nil)
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

	verified := probe(testContext(t), deployedAgainst(t, fake), 77, nil)
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

	verified := probe(testContext(t), deployedAgainst(t, fake), 77, nil)
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

	verified := probe(testContext(t), deployedAgainst(t, fake), 77, nil)
	if verified.Status != integrations.StatusDegraded {
		t.Fatalf("status = %s, want degraded; note: %s", verified.Status, verified.Note)
	}
	if !strings.Contains(verified.Note, "no repositories") {
		t.Errorf("the note %q does not say what is missing", verified.Note)
	}
}

func TestProbeWithoutAConfiguredAppSaysSo(t *testing.T) {
	t.Parallel()

	verified := probe(testContext(t), deployment{client: NewClient("http://127.0.0.1:1")}, 77, nil)
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

	verified := probe(testContext(t), deployment{app: app, client: unreachable}, 77, nil)
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

	verified := probe(testContext(t), deployedAgainst(t, fake), 77, nil)
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

	verified := probe(testContext(t), deployedAgainst(t, fake), 77, previously)
	if verified.Status != integrations.StatusFailed {
		t.Fatalf("status = %s, want failed; note: %s", verified.Status, verified.Note)
	}
	if !strings.Contains(verified.Note, "no longer installed on acme-corp") {
		t.Errorf("the note %q reads as an unknown id rather than a removal", verified.Note)
	}
}

// Changing which repositories are selected is GitHub's decision to host, so a verified
// integration records where that page is. GitHub files an organization's installations
// under the organization and a personal one under the account, and the origin is this
// deployment's — a GitHub Enterprise host is not github.com.
//
// Asserted through the probe rather than against the URL builder: the probe is this
// provider's seam, and the account type arrives the way it really does, in what GitHub
// answered.
func TestTheRecordedManageLinkPointsAtGitHubsOwnSettings(t *testing.T) {
	t.Parallel()

	for name, want := range map[string]struct {
		accountType string
		path        string
	}{
		"an organization": {
			"Organization", "/organizations/acme-corp/settings/installations/77",
		},
		"a personal account":                      {"User", "/settings/installations/77"},
		"an account type github has not shown us": {"Enterprise", ""},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			fake := newFakeGitHub(t)
			healthyInstallation(fake)
			fake.answer("/app/installations/77", `{"id":77,
				"account":{"login":"acme-corp","type":"`+want.accountType+`"},
				"repository_selection":"selected","suspended_at":null}`)

			verified := probe(testContext(t), deployedAgainst(t, fake), 77, nil)
			link, _ := verified.Facts[FactManageURL].(string)
			if want.path == "" {
				if link != "" {
					t.Fatalf("a link was guessed for an unrecognised account type: %q", link)
				}
				return
			}
			if link != fake.URL+want.path {
				t.Errorf("manage link = %q, want %q", link, fake.URL+want.path)
			}
		})
	}
}

// A login is a customer's own text, so it cannot leave the path segment it is put in.
func TestAManageLinkEscapesTheAccountItNames(t *testing.T) {
	t.Parallel()

	fake := newFakeGitHub(t)
	healthyInstallation(fake)
	fake.answer("/app/installations/77", `{"id":77,
		"account":{"login":"acme corp/../evil","type":"Organization"},
		"repository_selection":"selected","suspended_at":null}`)

	verified := probe(testContext(t), deployedAgainst(t, fake), 77, nil)
	link, _ := verified.Facts[FactManageURL].(string)
	if strings.Contains(link, "/../") || strings.Contains(link, " ") {
		t.Errorf("the link %q pastes the account in unescaped", link)
	}
	if !strings.HasPrefix(link, fake.URL) ||
		!strings.HasSuffix(link, "/settings/installations/77") {
		t.Errorf("the link %q no longer addresses this installation here", link)
	}
}

// WHERE A BROWSER REACHES THIS DEPLOYMENT'S GITHUB.
//
// The last case is the one that matters: an overridden API origin with no browser origin
// beside it is an Enterprise host whose web interface nothing told this build about, and a
// link to github.com would send somebody to a different company's settings page.
func TestTheBrowserOriginIsResolvedFromWhatTheDeploymentSaid(t *testing.T) {
	t.Parallel()

	enterprise := NewClient("https://github.acme.internal/api/v3")
	installer, err := NewInstaller("oc", "Iv1.x", "secret", "https://github.acme.internal")
	if err != nil {
		t.Fatalf("building the installer: %v", err)
	}

	for name, resolved := range map[string]struct {
		configured string
		installer  *Installer
		client     *Client
		want       string
	}{
		"configured outright": {
			"https://github.acme.internal/", nil, enterprise, "https://github.acme.internal",
		},
		"from the installation flow": {
			"", installer, enterprise, "https://github.acme.internal",
		},
		"nothing overridden at all": {"", nil, NewClient(""), "https://github.com"},
		"an api origin overridden and no browser origin beside it": {
			"", nil, enterprise, "",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got := browserOrigin(resolved.configured, resolved.installer, resolved.client)
			if got != resolved.want {
				t.Errorf("browser origin = %q, want %q", got, resolved.want)
			}
		})
	}
}

// A deployment connected through the configuration form gets the link too, as long as it
// said which GitHub it talks to. The documentation promises it on that page without
// qualification, and it has to be true there.
func TestTheConfigurationFormPathAlsoRecordsAManageLink(t *testing.T) {
	t.Parallel()

	fake := newFakeGitHub(t)
	healthyInstallation(fake)

	// No installer: this deployment registered no installation flow, and named its
	// origins instead.
	definition := Definition(nil, appAgainst(t, fake), NewClient(fake.URL), fake.URL)
	verified := definition.Probe(testContext(t), integrations.ProbeInput{
		Integration: integrations.Integration{
			Configuration: map[string]any{"installationId": float64(77)},
		},
	})
	if verified.Status != integrations.StatusActive {
		t.Fatalf("status = %s: %s", verified.Status, verified.Note)
	}
	link, _ := verified.Facts[FactManageURL].(string)
	if !strings.HasPrefix(link, fake.URL) {
		t.Errorf("the form path recorded %q, and the documentation promises a link", link)
	}
}

// And a deployment that cannot know says nothing rather than guessing.
func TestADeploymentThatCannotKnowTheOriginRecordsNoManageLink(t *testing.T) {
	t.Parallel()

	fake := newFakeGitHub(t)
	healthyInstallation(fake)

	definition := Definition(nil, appAgainst(t, fake), NewClient(fake.URL), "")
	verified := definition.Probe(testContext(t), integrations.ProbeInput{
		Integration: integrations.Integration{
			Configuration: map[string]any{"installationId": float64(77)},
		},
	})
	if _, recorded := verified.Facts[FactManageURL]; recorded {
		t.Errorf("a link was guessed for a deployment with no browser origin: %v",
			verified.Facts)
	}
}
