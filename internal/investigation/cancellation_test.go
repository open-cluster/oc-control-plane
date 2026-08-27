package investigation

import (
	"net/http"
	"testing"

	"github.com/open-cluster/oc-control-plane/internal/auth/authz"
)

func TestInvestigationCancellationIsAnExplicitAuthorizedOperatorRoute(t *testing.T) {
	t.Parallel()

	const pattern = "/api/v1/organizations/{organization}/investigations/{investigation}/cancel"
	for _, route := range (Handlers{}).Routes() {
		if route.Method() != http.MethodPost || route.Pattern() != pattern {
			continue
		}
		if route.Permission() != authz.Permission("investigation.cancel") {
			t.Fatalf("cancellation permission = %q, want investigation.cancel", route.Permission())
		}
		if !authz.Grants(authz.Admin, route.Permission()) ||
			!authz.Grants(authz.Editor, route.Permission()) ||
			authz.Grants(authz.Viewer, route.Permission()) {
			t.Fatal("cancellation must be granted to administrators and editors, never viewers")
		}
		return
	}
	t.Fatal("running investigations expose no authenticated cancellation route")
}

func TestInvestigationReportActivityAndSourcesAreAuthorizedReadViews(t *testing.T) {
	t.Parallel()

	base := "/api/v1/organizations/{organization}/investigations/{investigation}"
	for _, suffix := range []string{"/report", "/activity", "/sources", "/hypotheses"} {
		found := false
		for _, route := range (Handlers{}).Routes() {
			if route.Method() == http.MethodGet && route.Pattern() == base+suffix &&
				route.Permission() == authz.InvestigationRead {
				found = true
			}
		}
		if !found {
			t.Errorf("investigation transparency view %s is absent or incorrectly authorized", suffix)
		}
	}
}
