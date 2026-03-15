package api

import (
	"bufio"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/RandomCodeSpace/argus/internal/telemetry"
)

// responseWriter wraps http.ResponseWriter to capture the status code.
type responseWriter struct {
	http.ResponseWriter
	statusCode int
}

func wrapResponseWriter(w http.ResponseWriter) *responseWriter {
	return &responseWriter{ResponseWriter: w, statusCode: http.StatusOK}
}

func (rw *responseWriter) WriteHeader(code int) {
	rw.statusCode = code
	rw.ResponseWriter.WriteHeader(code)
}

// Hijack implements http.Hijacker so WebSocket upgrades work through the middleware.
func (rw *responseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return rw.ResponseWriter.(http.Hijacker).Hijack()
}

// MetricsMiddleware records argus_http_requests_total and argus_http_request_duration_seconds
// for every HTTP request.
func MetricsMiddleware(metrics *telemetry.Metrics, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rw := wrapResponseWriter(w)
		next.ServeHTTP(rw, r)
		duration := time.Since(start).Seconds()

		path := sanitizePath(r.URL.Path)
		status := strconv.Itoa(rw.statusCode)

		metrics.HTTPRequestsTotal.WithLabelValues(r.Method, path, status).Inc()
		metrics.HTTPRequestDuration.WithLabelValues(r.Method, path).Observe(duration)
	})
}

// sanitizePath normalizes URL paths to avoid high-cardinality label explosions.
// Dynamic segments (UUIDs, numeric IDs) are collapsed to {id}.
func sanitizePath(path string) string {
	// Keep well-known API prefixes; collapse long dynamic segments.
	// Fast path: if the path is short and contains no digits it's already clean.
	if len(path) <= 20 {
		return path
	}

	// Walk segments and replace pure-numeric or UUID-like segments with {id}.
	out := make([]byte, 0, len(path))
	start := 0
	for i := 0; i <= len(path); i++ {
		if i == len(path) || path[i] == '/' {
			seg := path[start:i]
			if isIDSegment(seg) {
				out = append(out, []byte("{id}")...)
			} else {
				out = append(out, []byte(seg)...)
			}
			if i < len(path) {
				out = append(out, '/')
			}
			start = i + 1
		}
	}
	return string(out)
}

func isIDSegment(s string) bool {
	if len(s) == 0 {
		return false
	}
	// UUID: 32-36 chars with hyphens
	if len(s) >= 32 {
		return true
	}
	// Pure numeric
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return len(s) > 0
}
