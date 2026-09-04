package investigation

import (
	"net/http"
	"testing"

	"github.com/open-cluster/oc-control-plane/internal/auth/authz"
)

func TestInvestigationCancellationIsAnExplicitAuthorizedOperatorRoute(t *testing.T) {
	t.Parallel()

	const pattern = "/api/v1/investigations/{investigation}/cancel"
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

func TestInvestigationRoutesAreTheCanonicalFive(t *testing.T) {
	t.Parallel()

	want := map[string]authz.Permission{
		"GET /api/v1/investigations":                         authz.InvestigationRead,
		"POST /api/v1/investigations":                        authz.InvestigationOpen,
		"GET /api/v1/investigations/{investigation}":         authz.InvestigationRead,
		"POST /api/v1/investigations/{investigation}/cancel": authz.InvestigationCancel,
		"GET /api/v1/investigations/{investigation}/events":  authz.InvestigationRead,
	}
	routes := (Handlers{}).Routes()
	if len(routes) != len(want) {
		t.Fatalf("investigation routes = %d, want %d", len(routes), len(want))
	}
	for _, route := range routes {
		key := route.Method() + " " + route.Pattern()
		permission, ok := want[key]
		if !ok {
			t.Errorf("unexpected investigation route %s", key)
			continue
		}
		if route.Permission() != permission {
			t.Errorf("%s permission = %q, want %q", key, route.Permission(), permission)
		}
	}
}
