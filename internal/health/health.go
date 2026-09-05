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

func (h Handlers) live(writer http.ResponseWriter, _ *http.Request) {
	writeStatus(writer, http.StatusOK, "ok")
}

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

func loggerFrom(ctx context.Context, fallback *slog.Logger) *slog.Logger {
	if logger, ok := ctx.Value(loggerKey{}).(*slog.Logger); ok {
		return logger
	}
	return fallback
}
