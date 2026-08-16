package investigation

import (
	"strings"
	"testing"

	"github.com/open-cluster/oc-control-plane/internal/integrations"
)

// The router in isolation: deterministic, explainable, and honest about what it can
// read. The catalog is the stub type from runner_test.

func TestRouteSelectsByTermOverlapAndExplainsIt(t *testing.T) {
	t.Parallel()

	catalog := stubType(t, nil)
	payments := stubIntegration("Payments Slack")
	unrelated := stubIntegration("Random Room")
	labelled := stubIntegration("Ops")
	labelled.Labels = map[string]string{"service": "payments"}

	selected, remainder := route(catalog,
		[]integrations.Integration{unrelated, payments, labelled},
		subjectTerms("payments checkout latency"))

	if len(selected) != selectedSources {
		t.Fatalf("selected %d, want %d", len(selected), selectedSources)
	}
	names := selected[0].integration.Name + " " + selected[1].integration.Name
	if !strings.Contains(names, "Payments Slack") || !strings.Contains(names, "Ops") {
		t.Errorf("selected %q; both term matches outrank the unrelated room", names)
	}
	for _, choice := range selected {
		if !strings.Contains(choice.reason, "payments") {
			t.Errorf("reason %q does not name the term that chose it", choice.reason)
		}
	}
	if len(remainder) != 1 || remainder[0].integration.Name != "Random Room" {
		t.Errorf("remainder = %+v", remainder)
	}
}

func TestRouteIsStableAcrossRuns(t *testing.T) {
	t.Parallel()

	catalog := stubType(t, nil)
	candidates := []integrations.Integration{
		stubIntegration("Alpha"), stubIntegration("Beta"), stubIntegration("Gamma"),
	}
	terms := subjectTerms("nothing matches any name")

	first, _ := route(catalog, candidates, terms)
	second, _ := route(catalog, candidates, terms)
	if first[0].integration.Name != second[0].integration.Name ||
		first[1].integration.Name != second[1].integration.Name {
		t.Errorf("two identical routings chose differently: %q,%q then %q,%q",
			first[0].integration.Name, first[1].integration.Name,
			second[0].integration.Name, second[1].integration.Name)
	}
}

func TestRouteSkipsDisabledAndToollessCandidates(t *testing.T) {
	t.Parallel()

	catalog := stubType(t, nil)
	disabled := stubIntegration("Disabled Slack")
	disabled.DisabledAt = disabled.CreatedAt.Add(1)
	toolless := stubIntegration("Alertmanager")
	toolless.Type = 1

	selected, remainder := route(catalog,
		[]integrations.Integration{disabled, toolless}, nil)
	if len(selected) != 0 || len(remainder) != 0 {
		t.Errorf("selected %+v remainder %+v; neither can be read", selected, remainder)
	}
}

func TestSubjectTermsDropNoiseAndDuplicates(t *testing.T) {
	t.Parallel()

	terms := subjectTerms("What is wrong with the payments PAYMENTS pod?", "payments")
	joined := strings.Join(terms, " ")
	if strings.Count(joined, "payments") != 1 {
		t.Errorf("terms = %v; duplicates must collapse", terms)
	}
	for _, noise := range []string{"what", "the", "is"} {
		if strings.Contains(" "+joined+" ", " "+noise+" ") {
			t.Errorf("terms = %v carry the noise word %q", terms, noise)
		}
	}
	if !strings.Contains(joined, "pod") {
		t.Errorf("terms = %v; a three-letter identifier is signal", terms)
	}
}
