package github

import (
	"strings"
	"testing"
)

// A partly-registered installation flow is refused where it is built, not discovered by a
// customer at the last step of a flow it cannot finish.
func TestAPartlyRegisteredInstallationFlowIsRefused(t *testing.T) {
	t.Parallel()

	for name, partial := range map[string]struct{ slug, clientID, secret string }{
		"no slug":          {"", "Iv1.deployment", "shhh"},
		"no client id":     {"opencluster", "", "shhh"},
		"no client secret": {"opencluster", "Iv1.deployment", ""},
		"nothing at all":   {"", "", ""},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			installer, err := NewInstaller(partial.slug, partial.clientID, partial.secret, "")
			if err == nil {
				t.Fatal("a partial installation flow was accepted")
			}
			if installer != nil {
				t.Error("a refused installation flow still produced one")
			}
			if !strings.Contains(err.Error(), "slug") {
				t.Errorf("the refusal %q does not name what is missing", err)
			}
		})
	}
}

// Nothing about the client secret appears in the refusal, for the reason no credential
// appears in any error here: the error is the thing an operator pastes into a ticket.
func TestARefusedInstallationFlowDoesNotQuoteTheSecret(t *testing.T) {
	t.Parallel()

	_, err := NewInstaller("opencluster", "", "the-client-secret", "")
	if err == nil {
		t.Fatal("a flow with no client id was accepted")
	}
	if strings.Contains(err.Error(), "the-client-secret") {
		t.Errorf("the refusal quotes the client secret: %v", err)
	}
}

// A deployment that registered a flow but never said where it is publicly reachable cannot
// build a redirect URI, and says so rather than sending a customer to a callback that
// resolves nowhere.
func TestAnInstallationFlowNeedsAStateAndACallback(t *testing.T) {
	t.Parallel()

	installer, err := NewInstaller("opencluster", "Iv1.deployment", "shhh", "")
	if err != nil {
		t.Fatalf("building the installer: %v", err)
	}
	if _, err := installer.authorize("", "https://oc.example/callback"); err == nil {
		t.Error("an installation was started with no state")
	}
	if _, err := installer.authorize("a-state", ""); err == nil {
		t.Error("an installation was started with nowhere to come back to")
	}
}
