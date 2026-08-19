package operator

import (
	_ "embed"
	"net/http"

	"github.com/open-cluster/oc-control-plane/internal/authz"
)

// A development-only API reference for the Integrations capability: the routes this
// session's testing needs, not the whole operator surface. It exists so a person can
// exercise create/verify against a real vendor from a browser instead of a shell.
//
// Both routes are unauthenticated on purpose and covers everything they may ever cover:
// static schema text, no tenant data, no secret. Swagger UI's own "Try it out" calls carry
// whatever Bearer token is entered into its Authorize dialog, which is the credential that
// does the real work — this page itself proves nothing and reads nothing.
//
// The Swagger UI assets are loaded from a CDN rather than vendored, because this is a
// development aid rather than a shipped surface, and the alternative is committing a
// third-party JS bundle this repository does not otherwise carry.

//go:embed openapi.json
var openAPISpec []byte

const docsPage = `<!doctype html>
<html>
<head>
<meta charset="utf-8" />
<title>OpenCluster Control Plane — Integrations (dev)</title>
<link rel="stylesheet" href="https://unpkg.com/swagger-ui-dist@5/swagger-ui.css" />
</head>
<body>
<div id="swagger-ui"></div>
<script src="https://unpkg.com/swagger-ui-dist@5/swagger-ui-bundle.js"></script>
<script>
  window.onload = () => SwaggerUIBundle({
    url: "/operator/v1/openapi.json",
    dom_id: "#swagger-ui",
    presets: [SwaggerUIBundle.presets.apis],
    persistAuthorization: true,
  });
</script>
</body>
</html>`

// docsRoutes is this package's own contribution alongside the relay routes it already
// registers directly — see Routes() in operator.go.
func docsRoutes() authz.Table {
	return authz.Table{
		authz.Public(http.MethodGet, "/operator/v1/openapi.json",
			http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				writer.Header().Set("Content-Type", "application/json")
				writer.Header().Set("Cache-Control", "no-store")
				_, _ = writer.Write(openAPISpec)
			})),
		authz.Public(http.MethodGet, "/operator/v1/docs",
			http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				writer.Header().Set("Content-Type", "text/html; charset=utf-8")
				writer.Header().Set("Cache-Control", "no-store")
				_, _ = writer.Write([]byte(docsPage))
			})),
	}
}
