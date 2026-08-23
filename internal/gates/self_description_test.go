package gates_test

import (
	"go/ast"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/open-cluster/oc-control-plane/internal/authz"
	"github.com/open-cluster/oc-control-plane/internal/describe"
	"github.com/open-cluster/oc-control-plane/internal/operator"
	"github.com/open-cluster/oc-control-plane/internal/table"
)

// THE DEPLOYMENT'S SELF-DESCRIPTION IS DERIVED, AND THESE ARE WHAT KEEP IT THAT WAY.
//
// The document exists because a console had to guess: which optional surfaces this
// deployment serves, and how a listing may be queried. Both answers already existed in the
// route table and in each listing's Spec, and neither was served — so a client implemented
// its own dialect and its suite went green over requests this service answers 400.
//
// A document is only worth having while it cannot lag. Assembly from the same handler values
// the router is built from covers most of that, and it cannot cover the one mistake that
// matters most: a listing or a body that was simply never CONTRIBUTED. That is an
// ordinary-looking omission in review and invisible to the compiler, which is what these
// gates are for.

// The surface under test, assembled with nil dependencies. What is being asserted is the
// SHAPE of the description, and no handler runs.
func describedSurface(t *testing.T) describe.Document {
	t.Helper()

	document := operator.Handlers{Logger: slog.Default()}.Description()
	if len(document.Routes) == 0 {
		t.Fatal("the description names no routes; every gate here would pass vacuously")
	}
	return document
}

// The document and the route table are the same set, in both directions.
//
// Both halves matter and they fail differently. A route missing from the document is a route
// a console cannot know about; a route in the document that the table does not serve is the
// same lie told the other way round, and it is what a hand-maintained document decays into.
func TestTheDescriptionAndTheRouteTableAreTheSameSet(t *testing.T) {
	t.Parallel()

	described := map[string]bool{}
	for _, route := range describedSurface(t).Routes {
		described[route.Method+" "+route.Path] = true
	}

	served := map[string]bool{}
	for _, route := range operatorRoutes(t) {
		served[route.Key()] = true
		if !described[route.Key()] {
			t.Errorf("%s is served and the deployment's description does not name it",
				route.Key())
		}
	}
	for key := range described {
		if !served[key] {
			t.Errorf("the description names %s and this listener does not serve it", key)
		}
	}
}

// Every route the description says something ABOUT is a route that exists.
//
// A listing or a body bound to a route key with a typo in it is the failure this catches:
// the contribution compiles, the document is served, and the entry describes nothing.
func TestEverythingDescribedNamesARouteThatExists(t *testing.T) {
	t.Parallel()

	served := map[string]bool{}
	for _, route := range operatorRoutes(t) {
		served[route.Key()] = true
	}

	document := describedSurface(t)
	for _, listing := range document.Listings {
		if !served[listing.Method+" "+listing.Path] {
			t.Errorf("a listing is described for %s %s, which this listener does not serve",
				listing.Method, listing.Path)
		}
	}
	for _, body := range document.Bodies {
		if !served[body.Method+" "+body.Path] {
			t.Errorf("a request body is described for %s %s, which this listener does not "+
				"serve", body.Method, body.Path)
		}
	}
}

// Every listing declared anywhere in this repository is described.
//
// This is the half the compiler cannot see. A capability that declares a Spec, parses with
// it, and never contributes it serves a listing whose contract is unpublished — and the
// document then looks complete while a console goes on guessing about that one. Counting the
// declarations in the SOURCE is the only way to notice, in the manner of the permission
// table's own gate.
func TestEveryDeclaredListingIsDescribed(t *testing.T) {
	t.Parallel()

	declared := listingSpecsDeclared(t)
	if declared == 0 {
		t.Fatal("no table.Spec declarations found; this gate would pass vacuously")
	}
	if described := len(describedSurface(t).Listings); described != declared {
		t.Errorf("this build declares %d listing specs and describes %d listings; a listing "+
			"that is parsed with a Spec and never contributed to the description is one a "+
			"console cannot learn to query", declared, described)
	}
}

// Every write either publishes the shape it accepts or is recorded here as taking no body.
//
// Every write on this surface decodes with DisallowUnknownFields, so a body carrying an
// undeclared field is refused rather than partly applied. That is right, and it is only fair
// if the shape is knowable. The routes that genuinely take no body are a NAMED list with the
// reason each one needs none, so adding a write without publishing its shape fails the build
// until somebody says which of the two it is.
func TestEveryWriteEitherPublishesItsBodyOrIsRecordedAsTakingNone(t *testing.T) {
	t.Parallel()

	// Decided entirely by the path and the caller. Each of these would be a body carrying
	// nothing, and an empty schema published for it would say less than its absence.
	bodyless := map[string]string{
		"POST /operator/v1/session/sign-out": "the session being ended is the caller's own",
		"POST /operator/v1/organizations/{organization}/sign-in/saml/{provider}/callback": "" +
			"it is a form post from the identity provider carrying SAMLResponse, not a " +
			"JSON body of ours; the shape is SAML's and publishing a reflected schema for " +
			"it would describe a request no browser sends",
		"POST /operator/v1/organizations/{organization}/integration-types/{type}/connect": "" +
			"the flow is started from the type in the path and the caller's own identity",
		"POST /operator/v1/organizations/{organization}/integrations/{integration}/verify": "" +
			"verification re-probes what is already recorded",
		"POST /operator/v1/organizations/{organization}/integrations/{integration}/webhook/rotate-secret": "" +
			"rotation mints a new secret and takes nothing",
		"POST /operator/v1/organizations/{organization}/members/{user}/revoke-sessions": "" +
			"the member whose sessions end is named in the path",
		"POST /operator/v1/organizations/{organization}/api-tokens/{token}/revoke": "" +
			"the token being revoked is named in the path",
		"POST /operator/v1/organizations/{organization}/relays/{registration}/clear-conflict": "" +
			"withdrawing the mark takes no argument",
		"POST /operator/v1/organizations/{organization}/relays/bootstrap-tokens": "" +
			"the token's lifetime and scope are this build's, not the caller's",
	}

	described := map[string]bool{}
	for _, body := range describedSurface(t).Bodies {
		described[body.Method+" "+body.Path] = true
	}

	found := map[string]bool{}
	for _, route := range operatorRoutes(t) {
		switch route.Method() {
		case http.MethodGet, http.MethodDelete:
			// A DELETE is decided by its path, and a GET has no body at all.
			continue
		}
		if strings.HasPrefix(route.Pattern(), "/scim/v2/") {
			// RFC 7643's shapes, served from the standard's own /Schemas endpoint. A
			// second reflected copy beside the authoritative one is how the two disagree.
			continue
		}
		if described[route.Key()] {
			continue
		}
		found[route.Key()] = true
		if _, allowed := bodyless[route.Key()]; !allowed {
			t.Errorf("%s accepts a request body nobody can learn the shape of; describe it, "+
				"or record it above with the reason it needs none", route.Key())
		}
	}
	for key := range bodyless {
		if !found[key] {
			t.Errorf("%s is recorded as taking no body and either no longer exists or now "+
				"publishes one; remove it so the list keeps meaning something", key)
		}
	}
}

// The listing conventions the document states are the ones internal/table actually enforces.
//
// Asserted by EXERCISING Parse rather than by comparing against a restatement of it. A gate
// that held one written-down list against another would pass while both were wrong together,
// which is the failure the document exists to prevent rather than to reproduce.
func TestTheStatedListingRulesAreTheOnesEnforced(t *testing.T) {
	t.Parallel()

	rules := describedSurface(t).Rules
	spec := table.Spec{
		Searchable:  true,
		Sortable:    []string{"at"},
		DefaultSort: table.Sort{Field: "at", Descending: true},
	}

	// Every reserved name is accepted by a listing that declares it as no filter at all.
	// The VALUE is the listing's own, because reserved means the name is understood rather
	// than that anything may be said with it: a sort naming a field this listing is not
	// ordered by is still refused, and rightly.
	values := map[string]string{
		"search": "anything",
		"sort":   spec.DefaultSort.Field,
		"cursor": "",
		"limit":  "10",
	}
	for _, name := range rules.Reserved {
		value, known := values[name]
		if !known {
			t.Fatalf("the document reserves %q and this gate has no value to try it with; "+
				"a reserved name added without one would be asserted by nothing", name)
		}
		if _, err := table.Parse(map[string][]string{name: {value}}, spec); err != nil {
			t.Errorf("the document reserves %q and Parse refuses it: %v", name, err)
		}
	}
	// And a name that is neither reserved nor a filter is refused, so the reserved list is
	// a real distinction rather than a description of "anything goes".
	if _, err := table.Parse(map[string][]string{"nonsense": {"1"}}, spec); err == nil {
		t.Error("Parse accepted a parameter that is neither reserved nor a filter, so the " +
			"document's reserved list distinguishes nothing")
	}

	// Every published sort prefix parses as the sign it claims to be.
	for _, prefix := range rules.SortPrefixes {
		parsed, err := table.Parse(map[string][]string{"sort": {prefix + "at"}}, spec)
		if err != nil {
			t.Errorf("the document offers the %q sort prefix and Parse refuses it: %v",
				prefix, err)
			continue
		}
		if want := prefix == "-"; parsed.Sort.Descending != want {
			t.Errorf("%q sorted descending=%v, want %v",
				prefix+"at", parsed.Sort.Descending, want)
		}
	}

	// The default and the ceiling are the numbers a caller is actually given.
	if parsed, _ := table.Parse(nil, spec); parsed.Limit != rules.DefaultLimit {
		t.Errorf("the document states a default limit of %d and Parse gives %d",
			rules.DefaultLimit, parsed.Limit)
	}
	over := map[string][]string{"limit": {"100000"}}
	parsed, err := table.Parse(over, spec)
	switch {
	case rules.LimitIsClamped && err != nil:
		t.Errorf("the document says a limit above the ceiling is clamped and Parse refused "+
			"it: %v", err)
	case rules.LimitIsClamped && parsed.Limit != rules.MaxLimit:
		t.Errorf("the document states a ceiling of %d and Parse gave %d",
			rules.MaxLimit, parsed.Limit)
	}
}

// The description names every optional surface this build can serve.
//
// The point of the surfaces list is that a 404 stops being the discovery mechanism, and it
// only achieves that while it is complete. A surface added without an entry leaves a console
// exactly where it started.
func TestEveryOptionalSurfaceIsNamed(t *testing.T) {
	t.Parallel()

	// Every optional surface this build has, with what turning it off does. A new one fails
	// this gate until it is recorded, which is the moment to decide whether a console can
	// find out about it any other way.
	optional := map[string]string{
		"conversations": "the conversation routes answer 404 while it is off",
	}

	named := map[string]bool{}
	for _, surface := range describedSurface(t).Surfaces {
		named[surface.Name] = true
		if surface.Note == "" {
			t.Errorf("the %q surface is named with no note; a console rendering a name it "+
				"does not recognise has nothing to show a person", surface.Name)
		}
		if _, known := optional[surface.Name]; !known {
			t.Errorf("the description names an optional surface %q that is not recorded "+
				"above", surface.Name)
		}
	}
	for name, effect := range optional {
		if !named[name] {
			t.Errorf("%q is an optional surface (%s) and the description does not name it, "+
				"so a 404 is still the only way to discover it", name, effect)
		}
	}
}

// The switch is visible in the document, in both positions.
//
// A surfaces list that read the same either way would be a list nobody could act on.
func TestTheDescriptionSaysWhetherAnOptionalSurfaceIsServed(t *testing.T) {
	t.Parallel()

	for _, enabled := range []bool{false, true} {
		document := operator.Handlers{
			Logger: slog.Default(), ConversationsEnabled: enabled,
		}.Description()

		found := false
		for _, surface := range document.Surfaces {
			if surface.Name != "conversations" {
				continue
			}
			found = true
			if surface.Enabled != enabled {
				t.Errorf("conversations enabled=%v is described as enabled=%v",
					enabled, surface.Enabled)
			}
		}
		if !found {
			t.Errorf("conversations enabled=%v is not named at all", enabled)
		}

		// The ROUTES do not move with the switch. The table is the API's index and is the
		// same table in every build; what changes is the one line saying whether it is
		// reachable, and a document whose shape moved with configuration would be one
		// nobody could review.
		routes := 0
		for _, route := range document.Routes {
			if strings.Contains(route.Path, "/conversations") {
				routes++
			}
		}
		if routes == 0 {
			t.Errorf("conversations enabled=%v describes no conversation routes; the table "+
				"is the same in every build", enabled)
		}
	}
}

// Every privileged route in the description carries the permission it requires, and nothing
// else does. It is the same assertion the permission table gate makes, made against the
// document, because the document is what a client will believe.
func TestTheDescribedPermissionsAreTheDeclaredOnes(t *testing.T) {
	t.Parallel()

	for _, route := range describedSurface(t).Routes {
		switch route.Access {
		case authz.AccessPrivileged.String():
			if route.Permission == "" {
				t.Errorf("%s %s is described as privileged and names no permission",
					route.Method, route.Path)
			}
		default:
			if route.Permission != "" {
				t.Errorf("%s %s is described as %s and names permission %q",
					route.Method, route.Path, route.Access, route.Permission)
			}
		}
	}
}

// listingSpecsDeclared counts the package-level table.Spec declarations in this repository.
//
// Source rather than reflection, for the reason the permission table gate reads source: a
// Spec that is declared and never contributed is invisible to everything else, and it is an
// entirely ordinary-looking line in review.
func listingSpecsDeclared(t *testing.T) int {
	t.Helper()

	count, inspected := 0, 0
	for _, directory := range internalPackages(t) {
		for _, file := range parseProductionFiles(t, directory) {
			inspected++
			ast.Inspect(file, func(node ast.Node) bool {
				composite, ok := node.(*ast.CompositeLit)
				if !ok {
					return true
				}
				selector, ok := composite.Type.(*ast.SelectorExpr)
				if !ok || selector.Sel.Name != "Spec" {
					return true
				}
				// table.Spec is the only Spec on this surface, and a listing is exactly a
				// place one is built. The contract's own package holds its Specs in tests,
				// which are not read here.
				if pkg, ok := selector.X.(*ast.Ident); ok && pkg.Name == "table" {
					count++
				}
				return true
			})
		}
	}
	if inspected == 0 {
		t.Fatal("no production files were read; the gate would pass vacuously")
	}
	return count
}
