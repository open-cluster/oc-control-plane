package incident

import (
	"testing"

	"github.com/google/uuid"
)

// WHICH INTEGRATION DELIVERED THIS, BY NAME.
//
// The view carried the integration's identity alone, so a console rendering "Delivered
// through" had a field whose value restated its own label — assembling a name in the
// browser would be a value this service never sent, and a console is right to refuse to.
// A responder arriving from their own alerting wants to know whether to go and look at
// Alertmanager or at something else.

func TestAnEpisodeSaysWhichIntegrationDeliveredItByName(t *testing.T) {
	t.Parallel()

	view := viewOf(Episode{
		ID:              uuid.New(),
		Integration:     uuid.New(),
		IntegrationName: "Alertmanager — production",
	})
	if view.IntegrationName != "Alertmanager — production" {
		t.Errorf("integrationName = %q, want the delivering integration's name",
			view.IntegrationName)
	}
	if view.IntegrationID == "" {
		t.Error("the identity was dropped; it is what a link is built from and the name " +
			"is what a person reads, so the view carries both")
	}
}

func TestAnEpisodeWithNoResolvableIntegrationKeepsTheIdentity(t *testing.T) {
	t.Parallel()

	// The honest rendering of a name this service could not resolve: the identity is
	// present and the name is absent, rather than an empty string a console would render
	// as a blank attribute or a placeholder it invented.
	//
	// Unreachable through the API today — DeleteIntegration refuses while an episode
	// references the integration — and the read is written not to depend on that staying
	// true, because the alternative failure is a listing that drops rows.
	integration := uuid.New()
	view := viewOf(Episode{ID: uuid.New(), Integration: integration})
	if view.IntegrationName != "" {
		t.Errorf("integrationName = %q, want it absent", view.IntegrationName)
	}
	if view.IntegrationID != integration.String() {
		t.Errorf("integrationId = %q, want the identity kept", view.IntegrationID)
	}
}
