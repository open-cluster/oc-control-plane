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

	// The one permission decided from a request BODY rather than from the route. Appointing an
	// owner is the same route as any other membership change, and which permission it needs
	// depends on what is being granted, so the table cannot carry it. It is recorded here with
	// its reason rather than being an unexplained gap.
	decidedInAHandler := map[authz.Permission]string{
		authz.MemberOwnerManage: "PUT .../members/{user} declares member.manage; appointing an " +
			"OWNER needs this on top, and which of the two applies is decided from the body",
	}

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

// The unauthenticated surface is a NAMED list. A fourth public route is a security decision,
// and this gate is what makes it one somebody has to write down rather than one that lands in a
// diff nobody reads twice.
func TestThePublicSurfaceIsExactlyTheThreeRoutesSignInNeeds(t *testing.T) {
	t.Parallel()

	// Each is public because a caller who is not signed in is precisely who needs it, and each
	// answers a tenant that does not exist exactly as it answers one that has configured no way
	// in — so none of them is a way to enumerate customers.
	permitted := map[string]string{
		"GET /operator/v1/organizations/{organization}/sign-in/providers": "a console must be " +
			"able to render a chooser before anybody has signed in",
		"GET /operator/v1/organizations/{organization}/sign-in/{provider}": "starting a sign-in " +
			"is what a caller with no credential is trying to do",
		"GET /operator/v1/sign-in/callback": "the identity provider sends the browser here, and " +
			"it carries a state rather than a credential",
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

// The routes that need a credential and no permission are likewise a named list. Each is a
// route whose subject is the caller themselves — requiring a permission would mean an Auditor
// could not sign out.
func TestTheAuthenticatedOnlyRoutesAreTheOnesAboutTheCallerThemselves(t *testing.T) {
	t.Parallel()

	permitted := map[string]bool{
		"GET /operator/v1/session":           true,
		"POST /operator/v1/session/sign-out": true,
	}

	for _, route := range operatorRoutes(t) {
		if route.Access() != authz.AccessAuthenticated {
			continue
		}
		if !permitted[route.Key()] {
			t.Errorf("%s needs a credential and no permission; only the routes that describe "+
				"the caller to themselves may be that, because every other one touches a "+
				"tenant's data", route.Key())
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
		"internal/health": "its own listener, deliberately: liveness, readiness and metrics " +
			"are exposed to whatever scrapes them and carry no tenant data",
		"internal/intake": "its own listener, deliberately: the one surface a customer's own " +
			"infrastructure reaches inbound, where each Connection authenticates with its own " +
			"secret rather than with a principal",
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

// Every route on this surface lives under /operator/v1. A route somewhere else would be served
// by this listener and would not be found by anything reading the version prefix to decide what
// it is talking to.
func TestEveryRouteIsUnderTheVersionedPrefix(t *testing.T) {
	t.Parallel()

	const prefix = "/operator/v1/"
	for _, route := range operatorRoutes(t) {
		if !strings.HasPrefix(route.Pattern(), prefix) {
			t.Errorf("%s is not under %s", route.Key(), prefix)
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
		{"GET /operator/v1/organizations/{organization}/integrations",
			"the catalog is Organization-scoped so configured Connections can be counted per tenant"},
		{"GET /operator/v1/organizations/{organization}/connections",
			"Connections list Organization-wide, with the Environment as a filter"},
		{"POST /operator/v1/organizations/{organization}/connections",
			"creation names its Environment in the body"},
		{"POST /operator/v1/organizations/{organization}/connections/{connection}/enabled",
			"one idempotent operation replaces the enable and disable pair"},
		{"POST /operator/v1/organizations/{organization}/connections/{connection}/trigger/rotate-secret",
			"rotating a trigger verification secret says which secret it rotates"},
	} {
		if !served[wanted.key] {
			t.Errorf("%s is not served; %s", wanted.key, wanted.reason)
		}
	}

	for _, gone := range []string{
		"GET /operator/v1/integrations",
		"GET /operator/v1/organizations/{organization}/environments/{environment}/connections",
		"POST /operator/v1/organizations/{organization}/environments/{environment}/connections",
		"POST /operator/v1/organizations/{organization}/connections/{connection}/enable",
		"POST /operator/v1/organizations/{organization}/connections/{connection}/disable",
		"POST /operator/v1/organizations/{organization}/connections/{connection}/rotate-secret",
	} {
		if served[gone] {
			t.Errorf("%s is served again; it was corrected deliberately, and a regression here "+
				"looks like a fix", gone)
		}
	}
}

// A role's permissions are a product statement, and the whole point of the table is that
// reading it answers "what can an Investigator do". This renders that answer into the test
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
	// A sanity bound on the whole thing: the widest role is the owner, and it holds every
	// permission. Anything wider would mean a permission outside the declared set.
	if len(authz.PermissionsOf(authz.OrganizationOwner)) != len(authz.Permissions()) {
		t.Errorf("the owner holds %d of %d permissions",
			len(authz.PermissionsOf(authz.OrganizationOwner)), len(authz.Permissions()))
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
