// Package httpapi assembles the HTTP layer: routing and middleware.
//
// Handlers live with their modules; this package only wires them together and
// adds the cross-cutting concerns — correlation ids, structured access logs,
// panic recovery and metrics — that should apply uniformly to every endpoint.
package httpapi

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"coverletter/internal/apperror"
	"coverletter/internal/httpx"
	"coverletter/internal/id"
	"coverletter/internal/obs"
)

// requestID assigns a correlation id to every request, reusing an inbound
// X-Request-ID so a trace can span services.
func requestID() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rid := strings.TrimSpace(r.Header.Get("X-Request-ID"))
			if rid == "" || len(rid) > 128 {
				rid = id.New()
			}
			w.Header().Set("X-Request-ID", rid)
			next.ServeHTTP(w, r.WithContext(httpx.WithRequestID(r.Context(), rid)))
		})
	}
}

// recoverPanic converts a panic into a 500 rather than a dropped connection, and
// logs the stack so the failure is diagnosable.
func recoverPanic() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rec := recover(); rec != nil {
					slog.Error("panic recovered",
						slog.Any("panic", rec),
						slog.String("path", r.URL.Path),
						slog.String("request_id", httpx.RequestID(r.Context())),
						slog.String("stack", string(debug.Stack())),
					)
					httpx.WriteError(w, r, apperror.Internal(
						context.Cause(r.Context()),
						"An unexpected error occurred.",
					))
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// accessLog records one structured line per request.
func accessLog() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			recorder := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

			next.ServeHTTP(recorder, r)

			duration := time.Since(start)
			// The request path is logged rather than the raw URL: query strings
			// can carry user data and there are none this API needs.
			slog.Info("request",
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Int("status", recorder.status),
				slog.Int64("bytes", recorder.written),
				slog.Duration("duration", duration),
				slog.String("request_id", httpx.RequestID(r.Context())),
				slog.String("remote_ip", clientIP(r)),
			)
		})
	}
}

// metrics records request counts, errors and durations.
func metrics(registry *obs.Registry) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			recorder := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

			next.ServeHTTP(recorder, r)

			registry.Observe("http.request.duration", time.Since(start))
			registry.Inc(statusMetricName(r.Method, recorder.status))
			if recorder.status >= http.StatusInternalServerError {
				registry.Inc("http.errors.total")
			}
		})
	}
}

// statusMetricName encodes the method and status into the metric name so the
// registry stays free of label-cardinality problems.
func statusMetricName(method string, status int) string {
	return "http.requests.total." + strings.ToLower(method) + ".status_" + strconv.Itoa(status)
}

// statusRecorder captures the status code and byte count for logging.
type statusRecorder struct {
	http.ResponseWriter
	status  int
	written int64
	wrote   bool
}

func (s *statusRecorder) WriteHeader(code int) {
	if !s.wrote {
		s.status = code
		s.wrote = true
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	s.wrote = true
	n, err := s.ResponseWriter.Write(b)
	s.written += int64(n)
	return n, err
}

// Flush forwards to the wrapped writer so streaming responses and
// http.Flusher-aware middleware keep working through the recorder.
func (s *statusRecorder) Flush() {
	if flusher, ok := s.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

// clientIP returns the peer address without trusting X-Forwarded-For, which a
// client can forge and which would poison the logs.
func clientIP(r *http.Request) string {
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}
