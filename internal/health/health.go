// Package health serves the operational surface every deployment needs whether or not any
// domain is exposed: liveness, readiness, and the metrics scrape, with the request
// correlation those routes run under.
//
// It is named after what it does rather than after being an HTTP layer. Each capability
// serves its own surface — intake, the operator surface, the relay endpoint — and a package
// named for the layer would collect them, which is the grouping ADR-016 exists to prevent.
//
// The package deliberately does not import internal/store/postgres. It depends on the BEHAVIOUR
// it needs (can the databases be reached?) rather than on the type that provides it, which
// keeps database access inside the package that owns database resolution. The import gate
// in test/architecture enforces this.
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

// readinessTimeout bounds the dependency check so a hung database makes readiness fail
// rather than making the readiness endpoint itself hang.
const readinessTimeout = 3 * time.Second

// Handlers is the HTTP surface's dependencies.
type Handlers struct {
	// Ready reports whether dependencies are reachable. A non-nil error means unready.
	Ready func(context.Context) error
	// Metrics serves the scrape endpoint.
	Metrics http.Handler
	// Logger is the base logger; per-request correlation is layered on top of it.
	Logger *slog.Logger
}

// Router returns the HTTP surface with correlation, tracing, and logging applied.
//
// Metrics are served OUTSIDE the tracing middleware: a scrape every fifteen seconds would
// otherwise produce a trace every fifteen seconds forever, which is noise that costs money.
func (h Handlers) Router() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /healthz", h.instrumented("healthz", http.HandlerFunc(h.live)))
	mux.Handle("GET /readyz", h.instrumented("readyz", http.HandlerFunc(h.ready)))
	mux.Handle("GET /metrics", h.Metrics)
	return mux
}

func (h Handlers) instrumented(route string, next http.Handler) http.Handler {
	return otelhttp.NewHandler(correlation.Middleware(h.logged(next)), route)
}

func (h Handlers) logged(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requestID := correlation.From(request.Context())
		logger := observability.LoggerFor(request.Context(), h.Logger, requestID)
		recorder := &statusRecorder{ResponseWriter: writer, status: http.StatusOK}
		started := time.Now()

		next.ServeHTTP(recorder, request.WithContext(withLogger(request.Context(), logger)))

		logger.Info("request served",
			slog.String("method", request.Method),
			slog.String("path", request.URL.Path),
			slog.Int("status", recorder.status),
			slog.Duration("duration", time.Since(started)))
	})
}

// live reports process health only. It must not consult a dependency: a liveness probe
// that fails on a database outage tells the orchestrator to restart a healthy process,
// turning a dependency incident into a crash loop.
func (h Handlers) live(writer http.ResponseWriter, _ *http.Request) {
	writeStatus(writer, http.StatusOK, "ok")
}

// ready reports whether this instance can serve traffic. Unlike liveness it consults
// dependencies, and it recovers on its own once they return — no restart required.
func (h Handlers) ready(writer http.ResponseWriter, request *http.Request) {
	ctx, cancel := context.WithTimeout(request.Context(), readinessTimeout)
	defer cancel()

	if err := h.Ready(ctx); err != nil {
		loggerFrom(request.Context(), h.Logger).Warn("readiness check failed",
			slog.String("error", err.Error()))
		writeStatus(writer, http.StatusServiceUnavailable, "unready")
		return
	}
	writeStatus(writer, http.StatusOK, "ready")
}

func writeStatus(writer http.ResponseWriter, code int, status string) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(code)
	// The body is a fixed literal, so a write failure has nothing to report and nothing to
	// retry; the status code has already been sent.
	_, _ = writer.Write([]byte(`{"status":"` + status + `"}`))
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

type loggerKey struct{}

func withLogger(ctx context.Context, logger *slog.Logger) context.Context {
	return context.WithValue(ctx, loggerKey{}, logger)
}

// loggerFrom returns the request's correlated logger, falling back to the base logger when
// called outside a request.
func loggerFrom(ctx context.Context, fallback *slog.Logger) *slog.Logger {
	if logger, ok := ctx.Value(loggerKey{}).(*slog.Logger); ok {
		return logger
	}
	return fallback
}
