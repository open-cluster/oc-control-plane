package observability

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/open-cluster/oc-control-plane/internal/correlation"
)

func HTTPRequestLogger(logger *slog.Logger, next http.Handler) http.Handler {
	if logger == nil {
		logger = slog.Default()
	}
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requestLogger := LoggerFor(request.Context(), logger, correlation.From(request.Context()))
		recorder := &statusRecorder{ResponseWriter: writer, status: http.StatusOK}
		started := time.Now()
		ctx := context.WithValue(request.Context(), loggerKey{}, requestLogger)

		next.ServeHTTP(recorder, request.WithContext(ctx))

		requestLogger.Info("request served",
			slog.String("method", request.Method),
			slog.String("path", request.URL.Path),
			slog.Int("status", recorder.status),
			slog.Duration("duration", time.Since(started)))
	})
}

func LoggerFrom(ctx context.Context, fallback *slog.Logger) *slog.Logger {
	if logger, ok := ctx.Value(loggerKey{}).(*slog.Logger); ok {
		return logger
	}
	return fallback
}

type loggerKey struct{}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}
