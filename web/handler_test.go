package web_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/open-cluster/oc-control-plane/web"
)

func TestHandlerServesSameOriginApplicationAndBrowserRoutes(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name        string
		method      string
		path        string
		status      int
		contentType string
		marker      string
	}{
		{name: "home", method: http.MethodGet, path: "/", status: http.StatusOK, contentType: "text/html", marker: "OpenCluster Control Plane"},
		{name: "deep link", method: http.MethodGet, path: "/organizations/local/investigations/example/sources", status: http.StatusOK, contentType: "text/html", marker: "OpenCluster Control Plane"},
		{name: "javascript", method: http.MethodGet, path: "/app.js", status: http.StatusOK, contentType: "javascript", marker: "same-origin"},
		{name: "stylesheet", method: http.MethodGet, path: "/style.css", status: http.StatusOK, contentType: "text/css", marker: "color-scheme"},
		{name: "unsafe method", method: http.MethodPost, path: "/", status: http.StatusMethodNotAllowed},
		{name: "missing asset", method: http.MethodGet, path: "/unknown.js", status: http.StatusNotFound},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			response := httptest.NewRecorder()
			web.Handler().ServeHTTP(response, httptest.NewRequest(test.method, test.path, nil))
			if response.Code != test.status {
				t.Fatalf("status = %d, want %d: %s", response.Code, test.status, response.Body.String())
			}
			if test.contentType != "" && !strings.Contains(response.Header().Get("Content-Type"), test.contentType) {
				t.Errorf("content type = %q, want %q", response.Header().Get("Content-Type"), test.contentType)
			}
			if test.marker != "" && !strings.Contains(response.Body.String(), test.marker) {
				t.Errorf("response omits required application marker %q", test.marker)
			}
		})
	}
}

func TestApplicationRestoresInvestigationDeepLinksAndEveryTransparencyView(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		path    string
		markers []string
	}{
		{path: "/", markers: []string{"id=\"hypotheses\"", "id=\"sources\""}},
		{path: "/app.js", markers: []string{
			"window.location.pathname", "decodeURIComponent", "/hypotheses",
			"scrollIntoView", "'concluded'", "if (await refreshInvestigation())", "returnTo=",
			"X-OpenCluster-Organization", "/api/v1/auth/local/sign-in",
		}},
	} {
		response := httptest.NewRecorder()
		web.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, test.path, nil))
		if response.Code != http.StatusOK {
			t.Fatalf("GET %s = %d", test.path, response.Code)
		}
		for _, marker := range test.markers {
			if !strings.Contains(response.Body.String(), marker) {
				t.Errorf("GET %s omits required investigation navigation behavior %q", test.path, marker)
			}
		}
	}
}
