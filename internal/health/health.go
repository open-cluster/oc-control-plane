package health

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/open-cluster/oc-control-plane/internal/correlation"
	"github.com/open-cluster/oc-control-plane/internal/telemetry"
)

const readinessTimeout = 3 * time.Second

type Handlers struct {
	Ready   func(context.Context) error
	Metrics http.Handler
	Logger  *slog.Logger
}

func (h Handlers) Router() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /healthz", h.instrumented("healthz", http.HandlerFunc(h.live)))
	mux.Handle("GET /readyz", h.instrumented("readyz", http.HandlerFunc(h.ready)))
	mux.Handle("GET /metrics", h.Metrics)
	return mux
}

func (h Handlers) instrumented(route string, next http.Handler) http.Handler {
	return otelhttp.NewHandler(
		correlation.Middleware(observability.HTTPRequestLogger(h.Logger, next)), route)
}

func (h Handlers) live(writer http.ResponseWriter, _ *http.Request) {
	writeStatus(writer, http.StatusOK, "ok")
}

func (h Handlers) ready(writer http.ResponseWriter, request *http.Request) {
	ctx, cancel := context.WithTimeout(request.Context(), readinessTimeout)
	defer cancel()

	if err := h.Ready(ctx); err != nil {
		observability.LoggerFrom(request.Context(), h.Logger).Warn("readiness check failed",
			slog.String("error", err.Error()))
		writeStatus(writer, http.StatusServiceUnavailable, "unready")
		return
	}
	writeStatus(writer, http.StatusOK, "ready")
}

func writeStatus(writer http.ResponseWriter, code int, status string) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(code)
	_, _ = writer.Write([]byte(`{"status":"` + status + `"}`))
}
