package authz

import "net/http"

// Route declares one protected application endpoint.
type Route struct {
	Method     string
	Pattern    string
	Permission Permission
	Handler    http.Handler
}

func routeID(route Route) string { return route.Method + " " + route.Pattern }
