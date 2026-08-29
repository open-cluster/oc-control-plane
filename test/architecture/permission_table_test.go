package gates_test

import (
	"go/ast"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/open-cluster/oc-control-plane/internal/api"
	"github.com/open-cluster/oc-control-plane/internal/auth/authz"
)

// The permission table IS the operator API's index, and these gates are what make story 33
// true: a new route without a declared permission cannot ship.
//
// Three mechanisms hold it, and each covers what the others cannot.
//
//  1. The COMPILER. authz.Privileged takes the permission as a positional argument, so a route
//     that needs one and does not name one does not compile. There is no constructor that
//     reaches a privileged handler without it.
//  2. STARTUP. authz.Router validates the table before it becomes a mux, so an undeclared
//     permission, a privileged route naming no organization, or a duplicate is a process that
//     refuses to start rather than a route that is served open.
//  3. THESE GATES. The compiler cannot see a route registered on a mux directly, bypassing the
//     table entirely — that is an ordinary-looking line in a capability package and it would be
//     invisible in review. This is the half that catches it.

// The surface the gates below read. It is assembled with nil dependencies deliberately: what is
// under test is the SHAPE of the table, and no handler runs.
func operatorRoutes(t *testing.T) authz.Table {
	t.Helper()

	table := api.Handlers{Logger: slog.Default()}.Routes()
	if len(table) == 0 {
		t.Fatal("the operator surface declares no routes; every gate here would pass vacuously")
	}
	return table
}

// Every route must be authorizable, which is what authz.Table.Validate decides. Running it here
// as well as at startup means a mistake fails the build rather than the first deployment.
func TestTheOperatorRouteTableIsAuthorizable(t *testing.T) {
	t.Parallel()

	if err := operatorRoutes(t).Validate(); err != nil {
		t.Fatalf("the operator route table cannot be authorized correctly: %v", err)
	}
}

// Every privileged route requires a permission this build declares, and every permission this
// build declares is required by at least one route.
//
// The second half is the one that decays silently. A permission no route requires is a
// capability nobody can exercise and a line in the role table that means nothing — and it reads
// as though the product does something it does not.
func TestEveryPermissionIsReachableAndEveryRouteDeclaresOne(t *testing.T) {
	t.Parallel()

	// Permissions decided somewhere other than the route table, recorded here with their
	// reason rather than being an unexplained gap. Currently none.
	decidedInAHandler := map[authz.Permission]string{}

	required := make(map[authz.Permission]bool)
	for _, route := range operatorRoutes(t) {
		if route.Access() != authz.AccessPrivileged {
			continue
		}
		if !authz.Declared(route.Permission()) {
			t.Errorf("%s requires %q, which this build does not declare",
				route.Key(), route.Permission())
			continue
		}
		required[route.Permission()] = true
	}

	for _, permission := range authz.Permissions() {
		if required[permission] {
			continue
		}
		if reason, recorded := decidedInAHandler[permission]; recorded {
			t.Logf("%s is decided in a handler: %s", permission, reason)
			continue
		}
		t.Errorf("no route requires %s; a permission nothing needs is a line in the role table "+
			"that means nothing, and it reads as though the product does something it does not",
			permission)
	}
}

// The unauthenticated surface is a NAMED list. A new public route is a security decision,
// and this gate is what makes it one somebody has to write down rather than one that lands in a
// diff nobody reads twice.
func TestThePublicSurfaceIsExactlyTheRoutesSignInNeeds(t *testing.T) {
	t.Parallel()

	// Each is public because a caller who is not signed in is precisely who needs it, and each
	// answers a tenant that does not exist exactly as it answers one that has configured no way
	// in — so none of them is a way to enumerate customers.
	permitted := map[string]string{
		"GET /api/v1/auth/oidc/start": "starting a sign-in " +
			"is what a caller with no credential is trying to do",
		"GET /api/v1/auth/oidc/callback": "the identity provider sends the browser here, and " +
			"it carries a state rather than a credential",
		"POST /api/v1/auth/local/bootstrap": "the configured bootstrap " +
			"credential authorizes the one-time first Admin creation inside the handler",
		"POST /api/v1/auth/local/sign-in": "a person presents " +
			"their local password here to obtain a session",
	}

	found := make(map[string]bool)
	for _, route := range operatorRoutes(t) {
		if route.Access() != authz.AccessPublic {
			continue
		}
		found[route.Key()] = true
		if _, allowed := permitted[route.Key()]; !allowed {
			t.Errorf("%s is reachable with no credential and is not one of the routes recorded "+
				"as needing to be; add it above with the reason, or give it a permission",
				route.Key())
		}
	}
	for pattern := range permitted {
		if !found[pattern] {
			t.Errorf("%s is recorded as public and no longer exists; remove it from the list "+
				"so the list keeps meaning something", pattern)
		}
	}
}

// The routes that need a credential and no permission are likewise a named list, recorded
// here with the reason each one cannot declare a permission. Two describe the caller to
// themselves — requiring a permission would mean an Auditor could not sign out. The third
// is a vendor's return trip, which arrives before this surface knows which tenant it
// concerns.
func TestTheAuthenticatedOnlyRoutesAreTheNamedSelfServiceOperations(t *testing.T) {
	t.Parallel()

	permitted := map[string]string{
		"GET /api/v1/session":       "its subject is the caller themselves",
		"DELETE /api/v1/session":    "an Auditor must be able to end their own session",
		"GET /api/v1/organizations": "a User may list memberships before selecting one",
		"POST /api/v1/organizations": "edition policy, not an existing tenant Permission, " +
			"decides whether a User may create another Organization",
		"GET /api/v1/permissions": "membership is verified for the selected Organization, " +
			"but reading one's own effective Permissions requires no Permission",
		"GET /api/v1/meta": "capabilities describe the authenticated deployment rather than " +
			"tenant-owned state, so no Organization or tenant Permission applies",
		"GET /api/v1/integrations/connect/callback": "a provider registration holds one " +
			"redirect URI, so the path can name no organization and there is no tenant in " +
			"it to check a membership against. The tenant comes from the single-use flow " +
			"the state redeems, and the handler itself then refuses unless the returning " +
			"caller is the principal that started it AND still holds integration.create in " +
			"the organization that flow named",
	}

	found := make(map[string]bool)
	for _, route := range operatorRoutes(t) {
		if route.Access() != authz.AccessAuthenticated {
			continue
		}
		found[route.Key()] = true
		if _, allowed := permitted[route.Key()]; !allowed {
			t.Errorf("%s needs a credential and no permission and is not recorded with its reason",
				route.Key())
		}
	}
	for pattern := range permitted {
		if !found[pattern] {
			t.Errorf("%s is recorded as authenticated-only and no longer exists; remove it "+
				"from the list so the list keeps meaning something", pattern)
		}
	}
}

// Every route reachable with a credential answers exactly one method, and net/http would panic
// on a duplicate at registration. Catching it here names the route instead of a stack trace.
func TestNoRouteIsRegisteredTwice(t *testing.T) {
	t.Parallel()

	seen := make(map[string]bool)
	for _, route := range operatorRoutes(t) {
		if seen[route.Key()] {
			t.Errorf("%s is registered twice", route.Key())
		}
		seen[route.Key()] = true
	}
}

func TestThePR2RouteCutoverHasOneCanonicalShape(t *testing.T) {
	t.Parallel()

	routes := operatorRoutes(t)
	found := make(map[string]bool, len(routes))
	for _, route := range routes {
		found[route.Key()] = true
		if strings.Contains(route.Pattern(), "/organizations/{organization}/") {
			t.Errorf("%s still carries the Organization in its path; the verified header is the sole selector",
				route.Key())
		}
	}

	expected := []string{
		"DELETE /api/v1/integrations/{integration}",
		"DELETE /api/v1/members/{membership}",
		"DELETE /api/v1/session",
		"DELETE /api/v1/sessions/{session}",
		"GET /api/v1/audit-events",
		"GET /api/v1/auth/oidc/callback",
		"GET /api/v1/auth/oidc/start",
		"GET /api/v1/conversations",
		"GET /api/v1/conversations/{conversation}",
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
		"GET /api/v1/investigations/{investigation}/activity",
		"GET /api/v1/investigations/{investigation}/events",
		"GET /api/v1/investigations/{investigation}/hypotheses",
		"GET /api/v1/investigations/{investigation}/report",
		"GET /api/v1/investigations/{investigation}/sources",
		"GET /api/v1/members",
		"GET /api/v1/meta",
		"GET /api/v1/organizations",
		"GET /api/v1/permissions",
		"GET /api/v1/policy",
		"GET /api/v1/relays",
		"GET /api/v1/relays/summary",
		"GET /api/v1/relays/{registration}/failures",
		"GET /api/v1/relays/{registration}/integrations",
		"GET /api/v1/relays/{registration}/session-conflicts",
		"GET /api/v1/session",
		"GET /api/v1/sessions",
		"GET /api/v1/webhook-deliveries",
		"GET /api/v1/webhook-deliveries/{delivery}",
		"PATCH /api/v1/incidents/{incident}/postmortem",
		"PATCH /api/v1/integrations/{integration}",
		"PATCH /api/v1/members/{membership}",
		"POST /api/v1/auth/local/bootstrap",
		"POST /api/v1/auth/local/sign-in",
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
		"POST /api/v1/organizations",
		"POST /api/v1/relays/bootstrap-tokens",
		"POST /api/v1/relays/{registration}/clear-conflict",
		"POST /api/v1/webhook-deliveries/{delivery}/replay",
		"PUT /api/v1/local-users/{user}/password",
		"PUT /api/v1/policy",
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
	for _, route := range operatorRoutes(t) {
		found[route.Key()] = true
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

// The half the compiler cannot see: a capability that registers a route on a mux DIRECTLY,
// bypassing the table and therefore the authorization decision entirely.
//
// It would be one ordinary-looking line in an ordinary-looking file, and it would serve a
// tenant's data to anybody. The gate reads the source of every package that contributes to the
// operator surface and refuses a mux registration anywhere in it.
func TestNoCapabilityRegistersARouteOutsideTheTable(t *testing.T) {
	t.Parallel()

	// EVERY package under internal/ is read, not a list of the ones that contribute routes
	// today. A list would mean a package added to the surface and not added to the list was a
	// package this gate was not reading — and the failure would be an absence nobody sees,
	// which is the shape of mistake the gate exists to catch in the first place.
	//
	// Two packages legitimately build a mux of their own, and each is here with the reason it
	// is not the operator surface. Adding a third is a decision somebody has to write down.
	permitted := map[string]string{
		// The one legitimate registration in the product: authz.Router is the function that
		// turns the validated table INTO the mux. Every other package must reach the mux
		// through it, which is exactly what this gate enforces.
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
				// http.ServeMux is the only thing in these packages with those methods. A call
				// to either means a route that never passed through the table, and therefore a
				// route served with no authorization decision at all.
				t.Errorf("%s calls %s directly; every route on the operator surface must be "+
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

// internalPackages reports every directory under internal/ that holds production Go files.
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

// A route's pattern must be one net/http can serve, and a privileged one must name an
// organization the guard can resolve a membership against. Validate enforces both; this asserts
// the patterns are also well-formed enough to register, which Validate deliberately does not
// try to decide for itself.
func TestEveryPatternRegistersOnAServeMux(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	for _, route := range operatorRoutes(t) {
		func() {
			defer func() {
				if recovered := recover(); recovered != nil {
					t.Errorf("%s cannot be registered: %v", route.Key(), recovered)
				}
			}()
			mux.Handle(route.Key(), route.Handler())
		}()
	}
}

// Every operator route lives under the product's versioned API prefix.
func TestEveryRouteIsUnderAVersionedPrefix(t *testing.T) {
	t.Parallel()

	prefixes := map[string]string{
		"/api/v1/": "this product's own surface",
	}

	counted := make(map[string]int, len(prefixes))
	for _, route := range operatorRoutes(t) {
		matched := ""
		for prefix := range prefixes {
			// A prefix's own root counts as under it. /api/v1 is the operator
			// surface's index — the document saying what this deployment serves — and a
			// gate that refused an API's base path would be refusing the one route whose
			// whole job is to describe the prefix it sits at.
			if route.Pattern() == strings.TrimSuffix(prefix, "/") ||
				strings.HasPrefix(route.Pattern(), prefix) {
				matched = prefix
			}
		}
		if matched == "" {
			t.Errorf("%s is under no versioned prefix; the ones this listener serves are %v",
				route.Key(), prefixes)
			continue
		}
		counted[matched]++
	}
	// Each prefix is asserted to be in use. One recorded here and served by nothing would be a
	// list that had stopped describing the surface.
	for prefix, reason := range prefixes {
		if counted[prefix] == 0 {
			t.Errorf("%s is recorded as a prefix this listener serves (%s) and nothing is "+
				"under it", prefix, reason)
		}
	}
}

// The paths the specification corrects, asserted as paths rather than as prose.
//
// They are breaking changes made deliberately and versioned together, and the reason each is
// here is that the old shape is the one a reviewer's fingers will type. A regression would look
// like a fix.
func TestTheCorrectedPathsAreTheOnesServed(t *testing.T) {
	t.Parallel()

	served := make(map[string]bool)
	for _, route := range operatorRoutes(t) {
		served[route.Key()] = true
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

		// The fleet. A hundred relays is a hundred rows, and a hundred rows is not an assessment.
		{"GET /api/v1/relays/summary",
			"a fleet is assessable without reading every row"},
		{"GET /api/v1/relays/{registration}/integrations",
			"what a Relay serves is what disabling it costs"},
		{"POST /api/v1/relays/bootstrap-tokens",
			"installing a Relay does not require sharing a permanent secret"},
		{"GET /api/v1/relays/{registration}/failures",
			"an intermittent Relay is diagnosed from the record rather than from who was watching"},

		// The investigation surface, on the provenance model: what it persists is what was
		// triggered, queried, run and found — never a chain of thought.
		{"GET /api/v1/investigations",
			"investigations list as operational records, newest first"},
		{"POST /api/v1/investigations",
			"an investigation opens from an incident or a plain-language question"},
		{"GET /api/v1/investigations/{investigation}",
			"one investigation carries its whole provenance: sources, runs, findings, spend"},
	} {
		if !served[wanted.key] {
			t.Errorf("%s is not served; %s", wanted.key, wanted.reason)
		}
	}
}

// A role's permissions are a product statement, and the whole point of the table is that
// reading it answers "what can an Editor do". This renders that answer into the test
// output, so a reviewer gets it from a test run rather than from the source.
func TestTheRoleTableIsLegible(t *testing.T) {
	t.Parallel()

	for _, role := range authz.Roles() {
		held := authz.PermissionsOf(role)
		names := make([]string, 0, len(held))
		for _, permission := range held {
			names = append(names, string(permission))
		}
		t.Logf("%-24s %2d: %s", role, len(names), strings.Join(names, ", "))
		if len(names) == 0 {
			t.Errorf("%s holds nothing; a role that grants no permission is a role nobody can "+
				"be usefully given", role)
		}
	}
	// A sanity bound on the whole thing: the widest role is the Admin, and it holds every
	// permission. Anything wider would mean a permission outside the declared set.
	if len(authz.PermissionsOf(authz.Admin)) != len(authz.Permissions()) {
		t.Errorf("the admin holds %d of %d permissions",
			len(authz.PermissionsOf(authz.Admin)), len(authz.Permissions()))
	}
}

// A permission string is a verb-noun pair an operator reads in a 403. One that is not shaped
// like the others is one nobody can guess from a neighbouring route.
func TestEveryPermissionIsAVerbNounString(t *testing.T) {
	t.Parallel()

	for _, permission := range authz.Permissions() {
		text := string(permission)
		if !strings.Contains(text, ".") || text != strings.ToLower(text) {
			t.Errorf("%q is not a lower-case dotted verb-noun string", text)
		}
		if quoted := strconv.Quote(text); strings.Contains(quoted, `\`) {
			t.Errorf("%s contains an escape", quoted)
		}
	}
}
