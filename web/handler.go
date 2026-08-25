// Package web serves the same-origin operator application embedded in the control plane.
package web

import (
	"embed"
	"net/http"
	"path"
	"strings"
)

//go:embed index.html app.js style.css
var application embed.FS

// Handler serves browser routes and their embedded assets without exposing other files.
func Handler() http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet && request.Method != http.MethodHead {
			writer.Header().Set("Allow", "GET, HEAD")
			http.Error(writer, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		name := "index.html"
		contentType := "text/html; charset=utf-8"
		switch request.URL.Path {
		case "/app.js":
			name, contentType = "app.js", "text/javascript; charset=utf-8"
		case "/style.css":
			name, contentType = "style.css", "text/css; charset=utf-8"
		default:
			if strings.Contains(path.Base(request.URL.Path), ".") {
				http.NotFound(writer, request)
				return
			}
		}

		content, err := application.ReadFile(name)
		if err != nil {
			http.Error(writer, "application unavailable", http.StatusInternalServerError)
			return
		}
		writer.Header().Set("Content-Type", contentType)
		writer.Header().Set("X-Content-Type-Options", "nosniff")
		writer.Header().Set("Content-Security-Policy", "default-src 'self'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
		if request.Method != http.MethodHead {
			_, _ = writer.Write(content)
		}
	})
}
