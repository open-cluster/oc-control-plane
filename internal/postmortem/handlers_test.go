package postmortem

import (
	"net/http"
	"testing"

	"github.com/open-cluster/oc-control-plane/internal/auth/authz"
)

func TestPostmortemRoutesMatchThePublicContract(t *testing.T) {
	t.Parallel()

	routes := (Handlers{}).Routes()
	base := "/api/v1/incidents/{incident}/postmortem"
	want := map[string]authz.Permission{
		http.MethodGet + " " + base:                  authz.PostmortemRead,
		http.MethodPost + " " + base:                 authz.PostmortemWrite,
		http.MethodPost + " " + base + "/regenerate": authz.PostmortemWrite,
		http.MethodPatch + " " + base:                authz.PostmortemWrite,
		http.MethodPost + " " + base + "/review":     authz.PostmortemWrite,
	}
	if len(routes) != len(want) {
		t.Fatalf("routes = %d, want %d", len(routes), len(want))
	}
	for _, route := range routes {
		key := route.Method + " " + route.Pattern
		if permission, ok := want[key]; !ok || route.Permission != permission {
			t.Errorf("route %q permission %q", key, route.Permission)
		}
	}
}
