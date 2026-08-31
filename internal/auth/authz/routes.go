package authz

import (
	"fmt"
	"net/http"
	"strings"
)

// Access says how a route is reached. There are exactly three values and each route names one
// by which constructor built it, so "I forgot the check" is not a state a route can be in.
type Access int

const (
	// accessUnset is the zero value and is never a legal route. It exists so that a Route
	// built by literal — which the unexported fields already prevent outside this package —
	// would still fail validation rather than default to something.
	AccessUnset Access = iota
	// AccessPublic is reachable with no credential at all. The sign-in redirect and its
	// callback are the whole set, and the gate in test/architecture holds them to a named list.
	AccessPublic
	// AccessAuthenticated needs a credential and no permission. Only the routes that describe
	// the caller to themselves are this: who am I, and end my own session.
	AccessAuthenticated
	// AccessPrivileged needs a credential, a membership in the selected Organization,
	// and the permission the route declares.
	AccessPrivileged
)

func (a Access) String() string {
	switch a {
	case AccessUnset:
		return "unset"
	case AccessPublic:
		return "public"
	case AccessAuthenticated:
		return "authenticated"
	case AccessPrivileged:
		return "privileged"
	default:
		return "unrecognised"
	}
}

// Route is one entry in the table that IS the operator API's index.
type Route struct {
	method               string
	pattern              string
	permission           Permission
	access               Access
	organizationScoped   bool
	organizationOptional bool
	handler              http.Handler
}

// Privileged declares a route needing a membership in the selected Organization and the
// permission it states. Every route on this surface that touches a tenant's data is one.
func Privileged(
	method string,
	pattern string,
	permission Permission,
	handler http.Handler,
) Route {
	return Route{
		method:             method,
		pattern:            pattern,
		permission:         permission,
		access:             AccessPrivileged,
		organizationScoped: true,
		handler:            handler,
	}
}

// Authenticated declares a route that needs a credential and nothing else. It is for the
// routes whose subject is the caller themselves — reading who is signed in, and ending that
// session — where requiring a permission would mean an auditor could not sign out.
func Authenticated(method, pattern string, handler http.Handler) Route {
	return Route{method: method, pattern: pattern, access: AccessAuthenticated, handler: handler}
}

// OrganizationAuthenticated declares a route that verifies the selected Organization and
// membership but requires no Permission. It serves self-describing tenant facts such as the
// caller's effective permissions.
func OrganizationAuthenticated(method, pattern string, handler http.Handler) Route {
	return Route{
		method: method, pattern: pattern, access: AccessAuthenticated,
		organizationScoped: true, handler: handler,
	}
}

// OptionalOrganizationAuthenticated declares a self-service route that verifies an active
// Organization only when the caller supplies the selector.
func OptionalOrganizationAuthenticated(method, pattern string, handler http.Handler) Route {
	return Route{
		method: method, pattern: pattern, access: AccessAuthenticated,
		organizationOptional: true, handler: handler,
	}
}

// Public declares a route reachable with no credential. Adding one is a security decision and
// the gate holds the set to a named list, so a new one fails the build until it is recorded
// there with its reason.
func Public(method, pattern string, handler http.Handler) Route {
	return Route{method: method, pattern: pattern, access: AccessPublic, handler: handler}
}

// Method is the HTTP method this route answers.
func (r Route) Method() string { return r.method }

// Pattern is the path pattern, in net/http.ServeMux's syntax.
func (r Route) Pattern() string { return r.pattern }

// Permission is what a caller must hold. It is empty for anything but a privileged route.
func (r Route) Permission() Permission { return r.permission }

// Access says how this route is reached.
func (r Route) Access() Access { return r.access }

// OrganizationScoped reports whether the route requires a verified active Organization.
func (r Route) OrganizationScoped() bool { return r.organizationScoped }

// OrganizationOptional reports whether the caller may select an Organization for this route
// but the route remains valid without one.
func (r Route) OrganizationOptional() bool { return r.organizationOptional }

// Handler is what serves the request once the decision has been made.
func (r Route) Handler() http.Handler { return r.handler }

// Key is the mux pattern this route registers under, and the identity a duplicate is detected
// by.
func (r Route) Key() string { return r.method + " " + r.pattern }

// Table is the whole surface's routes. It travels as a type rather than a slice so that the
// validation below is something a caller cannot skip by assembling their own.
type Table []Route

// Validate rejects missing handlers, malformed keys, duplicates, unset access, and
// permissions that do not match the route's access class. Each is a failure to start rather
// than a route that is open or a net/http registration panic.
func (t Table) Validate() error {
	seen := make(map[string]bool, len(t))
	for _, route := range t {
		if route.handler == nil {
			return fmt.Errorf("authz: route %q has no handler", route.Key())
		}
		if route.method == "" || !strings.HasPrefix(route.pattern, "/") {
			return fmt.Errorf("authz: route %q is not a method and an absolute pattern",
				route.Key())
		}
		if seen[route.Key()] {
			return fmt.Errorf("authz: route %q is registered twice", route.Key())
		}
		seen[route.Key()] = true

		switch route.access {
		case AccessPrivileged:
			if !Declared(route.permission) {
				return fmt.Errorf("authz: route %q requires %q, which this build does not "+
					"declare as a permission", route.Key(), route.permission)
			}
		case AccessAuthenticated, AccessPublic:
			if route.permission != "" {
				return fmt.Errorf("authz: route %q is %s and also declares the permission %q; "+
					"one of the two is a mistake", route.Key(), route.access, route.permission)
			}
		default:
			return fmt.Errorf("authz: route %q has no access declared; build it with "+
				"Privileged, Authenticated or Public", route.Key())
		}
	}
	if len(t) == 0 {
		return fmt.Errorf("authz: the route table is empty")
	}
	return nil
}
