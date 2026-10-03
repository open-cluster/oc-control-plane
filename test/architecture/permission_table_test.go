package gates_test

import (
	"go/ast"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/open-cluster/oc-control-plane/internal/api"
	"github.com/open-cluster/oc-control-plane/internal/auth/authz"
)

func apiRoutes(t *testing.T) []authz.Route {
	t.Helper()

	table := api.Handlers{Logger: slog.Default()}.Routes()
	if len(table) == 0 {
		t.Fatal("the application API declares no routes; every gate here would pass vacuously")
	}
	return table
}

func routeKey(route authz.Route) string { return route.Method + " " + route.Pattern }

func TestTheApplicationAPIRouteTableIsAuthorizable(t *testing.T) {
	t.Parallel()

	_, err := authz.Router(apiRoutes(t), authz.Guard{
		Resolve: func(*http.Request) (authz.Principal, error) { return authz.Principal{}, authz.ErrNoCredential },
	})
	if err != nil {
		t.Fatalf("the application API route table cannot be authorized correctly: %v", err)
	}
}

func TestEveryPrivilegedRouteRequiresADeclaredPermission(t *testing.T) {
	t.Parallel()
	for _, route := range apiRoutes(t) {
		if route.Permission == "" {
			continue
		}
		if !authz.Declared(route.Permission) {
			t.Errorf("%s requires %q, which this build does not declare",
				routeKey(route), route.Permission)
			continue
		}
	}
}

func TestTheAuthenticatedOnlyRoutesAreTheNamedSelfServiceOperations(t *testing.T) {
	t.Parallel()

	permitted := map[string]string{
		"PUT /api/v1/auth/local/password": "reauthenticates the User to change only their own local credential",
		"GET /api/v1/session":             "its subject is the caller themselves",
		"GET /api/v1/integrations/connect/callback": "a provider registration holds one " +
			"redirect URI, so the path can name no organization and there is no tenant in " +
			"it to check a membership against. The tenant comes from the single-use flow " +
			"the state redeems, and the handler itself then refuses unless the returning " +
			"caller is the principal that started it AND still holds integration.create in " +
			"the organization that flow named",
	}

	found := make(map[string]bool)
	for _, route := range apiRoutes(t) {
		if route.Permission != "" {
			continue
		}
		found[routeKey(route)] = true
		if _, allowed := permitted[routeKey(route)]; !allowed {
			t.Errorf("%s needs a credential and no permission and is not recorded with its reason",
				routeKey(route))
		}
	}
	for pattern := range permitted {
		if !found[pattern] {
			t.Errorf("%s is recorded as authenticated-only and no longer exists; remove it "+
				"from the list so the list keeps meaning something", pattern)
		}
	}
}

func TestNoRouteIsRegisteredTwice(t *testing.T) {
	t.Parallel()

	seen := make(map[string]bool)
	for _, route := range apiRoutes(t) {
		if seen[routeKey(route)] {
			t.Errorf("%s is registered twice", routeKey(route))
		}
		seen[routeKey(route)] = true
	}
}

func TestThePR2RouteCutoverHasOneCanonicalShape(t *testing.T) {
	t.Parallel()

	routes := apiRoutes(t)
	found := make(map[string]bool, len(routes))
	for _, route := range routes {
		found[routeKey(route)] = true
		if strings.Contains(route.Pattern, "/organizations/{organization}/") {
			t.Errorf("%s still carries the Organization in its path", routeKey(route))
		}
	}

	expected := []string{
		"DELETE /api/v1/integrations/{integration}",
		"DELETE /api/v1/members/{user}",
		"GET /api/v1/audit-events",
		"GET /api/v1/conversations",
		"GET /api/v1/conversations/{conversation}",
		"GET /api/v1/conversations/{conversation}/turns",
		"GET /api/v1/incidents",
		"GET /api/v1/incidents/{incident}",
		"GET /api/v1/incidents/{incident}/alert-events",
		"GET /api/v1/incidents/{incident}/postmortem",
		"GET /api/v1/integration-types",
		"GET /api/v1/integrations",
		"GET /api/v1/integrations/connect/callback",
		"GET /api/v1/integrations/{integration}",
		"GET /api/v1/investigations",
		"GET /api/v1/investigations/{investigation}",
		"GET /api/v1/investigations/{investigation}/events",
		"GET /api/v1/members",
		"GET /api/v1/policy",
		"GET /api/v1/relays",
		"GET /api/v1/relays/summary",
		"GET /api/v1/relays/{registration}/failures",
		"GET /api/v1/relays/{registration}/integrations",
		"GET /api/v1/session",
		"PATCH /api/v1/incidents/{incident}/postmortem",
		"PATCH /api/v1/integrations/{integration}",
		"PATCH /api/v1/members/{user}",
		"POST /api/v1/conversations",
		"POST /api/v1/conversations/{conversation}/messages",
		"POST /api/v1/incidents/{incident}/merge",
		"POST /api/v1/incidents/{incident}/postmortem",
		"POST /api/v1/incidents/{incident}/postmortem/regenerate",
		"POST /api/v1/incidents/{incident}/postmortem/review",
		"POST /api/v1/integration-types/{type}/connect",
		"POST /api/v1/integrations",
		"POST /api/v1/integrations/{integration}/disable",
		"POST /api/v1/integrations/{integration}/enable",
		"POST /api/v1/integrations/{integration}/rotate-webhook-secret",
		"POST /api/v1/integrations/{integration}/verify",
		"POST /api/v1/investigations",
		"POST /api/v1/investigations/{investigation}/cancel",
		"POST /api/v1/local-users",
		"POST /api/v1/relays/bootstrap-tokens",
		"POST /api/v1/relays/{registration}/clear-conflict",
		"POST /api/v1/slack/conversations/{conversation}/messages/{sequence}/recover",
		"PUT /api/v1/policy",
		"PUT /api/v1/auth/local/password",
	}
	wanted := make(map[string]bool, len(expected))
	for _, key := range expected {
		wanted[key] = true
		if !found[key] {
			t.Errorf("canonical route %s is absent", key)
		}
	}
	for key := range found {
		if !wanted[key] {
			t.Errorf("undeclared route %s is present in the canonical inventory", key)
		}
	}
}

func TestIntegrationStateHasExplicitCanonicalOperations(t *testing.T) {
	t.Parallel()

	found := make(map[string]bool)
	for _, route := range apiRoutes(t) {
		found[routeKey(route)] = true
	}
	for _, key := range []string{
		"POST /api/v1/integrations/{integration}/enable",
		"POST /api/v1/integrations/{integration}/disable",
	} {
		if !found[key] {
			t.Errorf("%s is absent", key)
		}
	}
	if found["POST /api/v1/integrations/{integration}/enabled"] {
		t.Error("the body-toggle Integration state route is still present")
	}
}

func TestNoCapabilityRegistersARouteOutsideTheTable(t *testing.T) {
	t.Parallel()

	permitted := map[string]string{
		"internal/auth/authz": "Router builds the mux from the table; it is the registration every " +
			"other package is required to go through",
		"internal/health": "owns the liveness, readiness, and metrics route tree that the " +
			"application mounts on the shared HTTP listener; the routes carry no tenant data",
		"internal/webhooks": "owns the inbound route tree that the application mounts on the " +
			"shared HTTP listener; each Integration authenticates with its own secret rather " +
			"than with a principal",
		"internal/app": "mounts the already-assembled health, intake, and permission-table " +
			"routers on the deployment's one HTTP server; it declares no application route",
	}

	inspected := 0
	for _, directory := range internalPackages(t) {
		relative, err := filepath.Rel(moduleRoot, directory)
		if err != nil {
			t.Fatal(err)
		}
		name := filepath.ToSlash(relative)
		if _, allowed := permitted[name]; allowed {
			continue
		}
		for _, file := range parseProductionFiles(t, directory) {
			inspected++
			ast.Inspect(file, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				selector, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				if selector.Sel.Name != "Handle" && selector.Sel.Name != "HandleFunc" {
					return true
				}
				t.Errorf("%s calls %s directly; every route on the application API must be "+
					"declared in the package's Routes() table, or it is served with no "+
					"authorization decision", name, selector.Sel.Name)
				return true
			})
		}
	}
	if inspected == 0 {
		t.Fatal("no production files were read; the gate would pass vacuously")
	}
	for name := range permitted {
		if _, err := os.Stat(filepath.Join(moduleRoot, name)); err != nil {
			t.Errorf("%s is recorded as having a listener of its own and no longer exists; "+
				"remove it so the list keeps meaning something", name)
		}
	}
}

func TestOrganizationScopedHandlersDoNotReparseTheOrganizationPath(t *testing.T) {
	t.Parallel()

	for _, directory := range internalPackages(t) {
		relative, err := filepath.Rel(moduleRoot, directory)
		if err != nil {
			t.Fatal(err)
		}
		name := filepath.ToSlash(relative)
		if name == "internal/auth/authz" {
			continue
		}
		for _, file := range parseProductionFiles(t, directory) {
			for _, declaration := range file.Decls {
				function, ok := declaration.(*ast.FuncDecl)
				if !ok || function.Body == nil {
					continue
				}
				if name == "internal/auth/identity" &&
					function.Name.Name == "preAuthenticationOrganization" {
					continue
				}
				ast.Inspect(function.Body, func(node ast.Node) bool {
					call, ok := node.(*ast.CallExpr)
					if !ok || len(call.Args) != 1 {
						return true
					}
					selector, ok := call.Fun.(*ast.SelectorExpr)
					if !ok || selector.Sel.Name != "PathValue" {
						return true
					}
					literal, ok := call.Args[0].(*ast.BasicLit)
					if ok && literal.Value == `"organization"` {
						t.Errorf("%s.%s reparses the Organization path; handlers must consume "+
							"the verified active Organization from request context",
							name, function.Name.Name)
					}
					return true
				})
			}
		}
	}
}

func internalPackages(t *testing.T) []string {
	t.Helper()

	root := filepath.Join(moduleRoot, "internal")
	var directories []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !entry.IsDir() {
			return nil
		}
		found, readErr := os.ReadDir(path)
		if readErr != nil {
			return readErr
		}
		for _, file := range found {
			name := file.Name()
			if !file.IsDir() && strings.HasSuffix(name, ".go") &&
				!strings.HasSuffix(name, "_test.go") {
				directories = append(directories, path)
				return nil
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking internal/: %v", err)
	}
	if len(directories) == 0 {
		t.Fatal("internal/ holds no production packages; the gate would pass vacuously")
	}
	return directories
}

func TestEveryPatternRegistersOnAServeMux(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	for _, route := range apiRoutes(t) {
		func() {
			defer func() {
				if recovered := recover(); recovered != nil {
					t.Errorf("%s cannot be registered: %v", routeKey(route), recovered)
				}
			}()
			mux.Handle(routeKey(route), route.Handler)
		}()
	}
}

func TestEveryRouteIsUnderAVersionedPrefix(t *testing.T) {
	t.Parallel()

	prefixes := map[string]string{
		"/api/v1/": "this product's own surface",
	}

	counted := make(map[string]int, len(prefixes))
	for _, route := range apiRoutes(t) {
		matched := ""
		for prefix := range prefixes {
			if route.Pattern == strings.TrimSuffix(prefix, "/") ||
				strings.HasPrefix(route.Pattern, prefix) {
				matched = prefix
			}
		}
		if matched == "" {
			t.Errorf("%s is under no versioned prefix; the ones this listener serves are %v",
				routeKey(route), prefixes)
			continue
		}
		counted[matched]++
	}
	for prefix, reason := range prefixes {
		if counted[prefix] == 0 {
			t.Errorf("%s is recorded as a prefix this listener serves (%s) and nothing is "+
				"under it", prefix, reason)
		}
	}
}

func TestTheCorrectedPathsAreTheOnesServed(t *testing.T) {
	t.Parallel()

	served := make(map[string]bool)
	for _, route := range apiRoutes(t) {
		served[routeKey(route)] = true
	}

	for _, wanted := range []struct {
		key    string
		reason string
	}{
		{"GET /api/v1/integration-types",
			"the catalog is Organization-scoped so configured Integrations can be counted per tenant"},
		{"GET /api/v1/integrations",
			"Integrations list Organization-wide; org_id is the only boundary"},
		{"POST /api/v1/integrations",
			"creating an Integration is the product's first job"},
		{"GET /api/v1/integrations/{integration}",
			"an Integration has a detail route carrying its status and its webhook identity"},
		{"PATCH /api/v1/integrations/{integration}",
			"revising changes part of a record and leaves its identity and its secret alone"},
		{"DELETE /api/v1/integrations/{integration}",
			"an Integration nothing depends on can be removed; one with a history is refused"},
		{"POST /api/v1/integrations/{integration}/enable",
			"enabling is explicit and idempotent"},
		{"POST /api/v1/integrations/{integration}/disable",
			"disabling is explicit and idempotent"},
		{"POST /api/v1/integrations/{integration}/verify",
			"verifying is what separates an Integration that is configured from one that works"},
		{"POST /api/v1/integrations/{integration}/rotate-webhook-secret",
			"rotating the webhook secret says which secret it rotates"},

		{"GET /api/v1/relays/summary",
			"a fleet is assessable without reading every row"},
		{"GET /api/v1/relays/{registration}/integrations",
			"what a Relay serves is what disabling it costs"},
		{"POST /api/v1/relays/bootstrap-tokens",
			"installing a Relay does not require sharing a permanent secret"},
		{"GET /api/v1/relays/{registration}/failures",
			"an intermittent Relay is diagnosed from the record rather than from who was watching"},

		{"GET /api/v1/investigations",
			"investigations list as operational records, newest first"},
		{"POST /api/v1/investigations",
			"a direct investigation opens from an Incident"},
		{"GET /api/v1/investigations/{investigation}",
			"one Investigation carries its conclusion and durable Tool Runs"},
	} {
		if !served[wanted.key] {
			t.Errorf("%s is not served; %s", wanted.key, wanted.reason)
		}
	}
}
