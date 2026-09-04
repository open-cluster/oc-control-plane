package health

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/open-cluster/oc-control-plane/internal/telemetry"
)

// RequestIDHeader carries the correlation identifier for one request. An inbound value is
// deliberately NOT trusted: a client-supplied identifier would let a caller collide two
// unrelated requests in the logs, so one is always minted here and echoed back.
const RequestIDHeader = "X-Request-Id"

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

// instrumented wraps one route with tracing, request correlation, and access logging.
func (h Handlers) instrumented(route string, next http.Handler) http.Handler {
	return otelhttp.NewHandler(h.correlated(next), route)
}

// correlated mints a request identifier, binds it to the response and to a logger carrying
// the trace identifiers, and records the outcome.
func (h Handlers) correlated(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requestID := newRequestID()
		writer.Header().Set(RequestIDHeader, requestID)

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

func newRequestID() string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "unavailable"
	}
	return hex.EncodeToString(raw[:])
}

// statusRecorder captures the status code for the access log.
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
