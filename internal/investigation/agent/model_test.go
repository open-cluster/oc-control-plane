package agent

import (
	"testing"
)

func TestModelEndpointAllowsPlaintextOnlyOnLoopback(t *testing.T) {
	for _, scenario := range []struct {
		url     string
		allowed bool
	}{{"http://127.0.0.1:8080", true}, {"http://vendor.example", false}} {
		deployment := Deployment{Provider: "anthropic", Model: "claude-sonnet-5",
			Credential: Secret("test-credential"), BaseURL: scenario.url}.WithDefaults()
		if valid := deployment.Validate() == nil; valid != scenario.allowed {
			t.Errorf("model endpoint %q allowed = %t, want %t", scenario.url, valid, scenario.allowed)
		}
	}
}
