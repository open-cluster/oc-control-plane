package reasoning

import (
	"strings"
	"testing"

	"github.com/open-cluster/oc-control-plane/internal/investigation"
)

func TestTheBriefKeepsOlderOperatorFactsUntrustedAndPriorLimitationsVisible(t *testing.T) {
	t.Parallel()
	rendered := renderBrief(&investigation.Brief{
		OperatorStatements: []investigation.BriefMessage{{
			FromPerson: true, Actor: "on-call", Text: "traffic stayed flat",
		}},
		Limitations: []string{"database wait telemetry is unavailable"},
	})
	for _, expected := range []string{
		"KNOWN LIMITATIONS", "database wait telemetry is unavailable",
		"OLDER OPERATOR TESTIMONY", "unverified person-authored context",
		"operator on-call: traffic stayed flat",
	} {
		if !strings.Contains(rendered, expected) {
			t.Errorf("brief is missing %q:\n%s", expected, rendered)
		}
	}
}
