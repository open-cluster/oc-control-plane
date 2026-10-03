package github

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/open-cluster/oc-control-plane/internal/integrations"
)

func runInWindow(
	t *testing.T, app *App, client *Client, name string, args map[string]any,
	from, until time.Time,
) (integrations.ToolResult, error) {
	t.Helper()
	return toolNamed(t, app, client, name).Run(testContext(t), integrations.ToolRequest{
		Integration: integrations.Integration{
			Installation: &integrations.Installation{Key: integrations.InstallationKey{"77"}},
		},
		Arguments:   args,
		WindowFrom:  from,
		WindowUntil: until,
	})
}

func TestReadCommitsReportsTheWindowItWasNarrowedTo(t *testing.T) {
	t.Parallel()

	fake := newFakeGitHub(t)
	healthyInstallation(fake)
	grantsPayments(fake)
	fake.answers["/repos/acme-corp/payments/commits"] = func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`[]`))
	}

	from := mustTime(t, "2026-08-22T08:00:00Z")
	until := mustTime(t, "2026-08-22T10:00:00Z")

	result, err := runInWindow(t, appAgainst(t, fake), NewClient(fake.URL),
		"github.read_commits", map[string]any{
			"repositoryId": float64(1296269),
		}, from, until)
	if err != nil {
		t.Fatalf("reading commits: %v", err)
	}
	if !result.WindowFrom.Equal(from) || !result.WindowUntil.Equal(until) {
		t.Errorf("result window = %v to %v; want the window the read actually covered, "+
			"%v to %v", result.WindowFrom, result.WindowUntil, from, until)
	}
}

func TestReadWorkflowRunsReportsItsWindow(t *testing.T) {
	t.Parallel()

	fake := newFakeGitHub(t)
	healthyInstallation(fake)
	grantsPayments(fake)
	fake.answers["/repos/acme-corp/payments/actions/runs"] = func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"workflow_runs":[]}`))
	}

	from := mustTime(t, "2026-08-22T08:00:00Z")
	until := mustTime(t, "2026-08-22T10:00:00Z")

	result, err := runInWindow(t, appAgainst(t, fake), NewClient(fake.URL),
		"github.read_workflow_runs", map[string]any{
			"repositoryId": float64(1296269),
		}, from, until)
	if err != nil {
		t.Fatalf("reading workflow runs: %v", err)
	}
	if !result.WindowFrom.Equal(from) || !result.WindowUntil.Equal(until) {
		t.Errorf("result window = %v to %v; want %v to %v",
			result.WindowFrom, result.WindowUntil, from, until)
	}
}

func TestAnUnwindowedReadReportsNoWindow(t *testing.T) {
	t.Parallel()

	fake := newFakeGitHub(t)
	healthyInstallation(fake)
	fake.answers["/installation/repositories"] = func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"total_count":0,"repositories":[]}`))
	}

	result, err := runInWindow(t, appAgainst(t, fake), NewClient(fake.URL),
		"github.list_repositories", map[string]any{},
		mustTime(t, "2026-08-22T08:00:00Z"), mustTime(t, "2026-08-22T10:00:00Z"))
	if err != nil {
		t.Fatalf("listing repositories: %v", err)
	}
	if !result.WindowFrom.IsZero() || !result.WindowUntil.IsZero() {
		t.Errorf("a listing that reads no window claims %v to %v",
			result.WindowFrom, result.WindowUntil)
	}
}

func TestNoToolPromisesAnUnboundedRecentTail(t *testing.T) {
	t.Parallel()

	for _, tool := range tools(nil, nil) {
		text := tool.Description + " " + tool.WhenToUse + " " + tool.WhenNotToUse
		for _, argument := range tool.Arguments {
			text += " " + argument.Description
		}
		if strings.Contains(strings.ToLower(text), "recent tail") {
			t.Errorf("%s promises a recent tail; every windowed read is clamped into "+
				"the investigation's window and there is no unbounded path", tool.Name)
		}
	}
}
