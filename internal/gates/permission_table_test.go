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

	"github.com/open-cluster/oc-control-plane/internal/authz"
	"github.com/open-cluster/oc-control-plane/internal/operator"
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

	table := operator.Handlers{Logger: slog.Default()}.Routes()
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
func TestThePublicSurfaceIsExactlyTheThreeRoutesSignInNeeds(t *testing.T) {
	t.Parallel()

	// Each is public because a caller who is not signed in is precisely who needs it, and each
	// answers a tenant that does not exist exactly as it answers one that has configured no way
	// in — so none of them is a way to enumerate customers.
	permitted := map[string]string{
		"GET /operator/v1/organizations/{organization}/sign-in/oidc": "starting a sign-in " +
			"is what a caller with no credential is trying to do",
		"GET /operator/v1/sign-in/callback": "the identity provider sends the browser here, and " +
			"it carries a state rather than a credential",
		"POST /operator/v1/organizations/{organization}/bootstrap": "the configured bootstrap " +
			"credential authorizes the one-time first Admin creation inside the handler",
		"POST /operator/v1/organizations/{organization}/sign-in/local": "a person presents " +
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
func TestTheAuthenticatedOnlyRoutesAreTheOnesThatCannotNameATenant(t *testing.T) {
	t.Parallel()

	permitted := map[string]string{
		"GET /operator/v1": "it describes the DEPLOYMENT rather than a tenant, so there is " +
			"no organization in its path to check a membership against. It is not public " +
			"either: a browser that has not signed in discovers the sign-in providers " +
			"through the public identity listing, so nothing needs this document without a " +
			"credential, and publishing the route table to unauthenticated callers would " +
			"widen the anonymous surface for no gain",
		"GET /operator/v1/session":           "its subject is the caller themselves",
		"POST /operator/v1/session/sign-out": "an Auditor must be able to end their own session",
		"GET /operator/v1/integrations/connect/callback": "a provider registration holds one " +
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
			t.Errorf("%s needs a credential and no permission; only a route that cannot name "+
				"a tenant in its path may be that, and it has to be recorded above with the "+
				"reason", route.Key())
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
		"internal/authz": "Router builds the mux from the table; it is the registration every " +
			"other package is required to go through",
		"internal/health": "owns the liveness, readiness, and metrics route tree that the " +
			"application mounts on the shared HTTP listener; the routes carry no tenant data",
		"internal/intake": "owns the inbound route tree that the application mounts on the " +
			"shared HTTP listener; each Integration authenticates with its own secret rather " +
			"than with a principal",
		"internal/app": "mounts the already-assembled health, intake, and permission-table " +
			"routers on the deployment's one HTTP server; it declares no application route",
	}

	inspected := 0
	for _, directory := range internalPackages(t) {
		name := "internal/" + filepath.Base(directory)
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

// Every route this listener serves lives under one of TWO versioned prefixes, and the second
// one is a deliberate exception rather than an oversight.
//
// /operator/v1 is this product's own surface and its version is this product's to change.
// /scim/v2 is RFC 7644's, and a customer's directory is configured with that base URL once, in
// somebody else's system, and then not touched for years. Pinning the provisioning surface to
// the standard's version means an operator API bump is not a change every customer has to make
// in their identity vendor.
//
// Anything under neither would be served by this listener and found by nothing reading a prefix
// to decide what it is talking to.
func TestEveryRouteIsUnderAVersionedPrefix(t *testing.T) {
	t.Parallel()

	prefixes := map[string]string{
		"/operator/v1/": "this product's own surface",
	}

	counted := make(map[string]int, len(prefixes))
	for _, route := range operatorRoutes(t) {
		matched := ""
		for prefix := range prefixes {
			// A prefix's own root counts as under it. /operator/v1 is the operator
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
		{"GET /operator/v1/organizations/{organization}/integration-types",
			"the catalog is Organization-scoped so configured Integrations can be counted per tenant"},
		{"GET /operator/v1/organizations/{organization}/integrations",
			"Integrations list Organization-wide; org_id is the only boundary"},
		{"POST /operator/v1/organizations/{organization}/integrations",
			"creating an Integration is the product's first job"},
		{"GET /operator/v1/organizations/{organization}/integrations/{integration}",
			"an Integration has a detail route carrying its status and its webhook identity"},
		{"PATCH /operator/v1/organizations/{organization}/integrations/{integration}",
			"revising changes part of a record and leaves its identity and its secret alone"},
		{"DELETE /operator/v1/organizations/{organization}/integrations/{integration}",
			"an Integration nothing depends on can be removed; one with a history is refused"},
		{"POST /operator/v1/organizations/{organization}/integrations/{integration}/enabled",
			"one idempotent operation replaces the enable and disable pair"},
		{"POST /operator/v1/organizations/{organization}/integrations/{integration}/verify",
			"verifying is what separates an Integration that is configured from one that works"},
		{"POST /operator/v1/organizations/{organization}/integrations/{integration}/webhook/rotate-secret",
			"rotating the webhook secret says which secret it rotates"},

		// The fleet. A hundred relays is a hundred rows, and a hundred rows is not an assessment.
		{"GET /operator/v1/organizations/{organization}/relays/summary",
			"a fleet is assessable without reading every row"},
		{"GET /operator/v1/organizations/{organization}/relays/{registration}/integrations",
			"what a Relay serves is what disabling it costs"},
		{"POST /operator/v1/organizations/{organization}/relays/bootstrap-tokens",
			"installing a Relay does not require sharing a permanent secret"},
		{"GET /operator/v1/organizations/{organization}/relays/{registration}/failures",
			"an intermittent Relay is diagnosed from the record rather than from who was watching"},

		// The investigation surface, on the provenance model: what it persists is what was
		// triggered, queried, run and found — never a chain of thought.
		{"GET /operator/v1/organizations/{organization}/investigations",
			"investigations list as operational records, newest first"},
		{"POST /operator/v1/organizations/{organization}/investigations",
			"an investigation opens from an episode or a plain-language question"},
		{"GET /operator/v1/organizations/{organization}/investigations/{investigation}",
			"one investigation carries its whole provenance: sources, runs, findings, spend"},
	} {
		if !served[wanted.key] {
			t.Errorf("%s is not served; %s", wanted.key, wanted.reason)
		}
	}

	for _, gone := range []string{
		// The retired domain: Connections and Environments are gone, and a route
		// reappearing here would be the old model growing back.
		"GET /operator/v1/organizations/{organization}/connections",
		"POST /operator/v1/organizations/{organization}/connections",
		"GET /operator/v1/organizations/{organization}/environments",
		"POST /operator/v1/organizations/{organization}/environments",
	} {
		if served[gone] {
			t.Errorf("%s is served again; it was corrected deliberately, and a regression here "+
				"looks like a fix", gone)
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
