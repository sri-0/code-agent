package logging

import (
	"net/http"
	"time"

	"github.com/rs/zerolog"
)

// CreateLoggingMiddleware returns an HTTP middleware that logs each request
// with method, path, status, duration, and attaches a request-scoped logger
// to the context via zerolog.Ctx.
func CreateLoggingMiddleware(logger zerolog.Logger) func(next http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()

			crw := &customResponseWriter{ResponseWriter: w, status: http.StatusOK}

			reqLogger := logger.With().
				Str("ip", r.RemoteAddr).
				Str("method", r.Method).
				Str("path", r.URL.Path).
				Str("user_agent", r.UserAgent()).
				Logger()
			ctx := reqLogger.WithContext(r.Context())
			r = r.WithContext(ctx)

			next.ServeHTTP(crw, r)

			reqLogger.Info().
				Int("status", crw.status).
				Dur("duration", time.Since(start)).
				Msg("HTTP request")
		})
	}
}

type customResponseWriter struct {
	http.ResponseWriter
	status int
}

func (crw *customResponseWriter) WriteHeader(status int) {
	crw.status = status
	crw.ResponseWriter.WriteHeader(status)
}
