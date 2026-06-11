package api

import (
	"compress/gzip"
	"net/http"
	"strings"
	"sync"

	"github.com/RandomCodeSpace/otelcontext/internal/httpconst"
)

// gzipWriterPool recycles gzip writers across requests — a gzip.Writer holds
// ~hundreds of KB of compression state, far too much to allocate per call.
var gzipWriterPool = sync.Pool{
	New: func() any { return gzip.NewWriter(nil) },
}

// GzipMiddleware compresses GET /api/* responses for clients that accept
// gzip. The 120-service system-graph JSON shrinks 5-8× on the wire.
//
// Everything outside /api/* passes through untouched, which keeps the
// streaming surfaces safe by construction: WebSocket upgrades (/ws*) still
// see an http.Hijacker, OTLP ingest (/v1/*) and Prometheus scrapes
// (/metrics*) are write paths/exposition formats with their own encodings,
// and MCP SSE must flush uncompressed frames. Those prefixes — plus the
// configured MCP path — are also excluded explicitly in case an operator
// ever nests one under /api/.
//
// Wire this innermost (directly around the mux): only handler output is
// compressed; error responses written by outer middleware (auth, rate
// limit) stay identity-encoded.
func GzipMiddleware(mcpPath string) func(http.Handler) http.Handler {
	if mcpPath == "" {
		mcpPath = "/mcp"
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !gzipEligible(r, mcpPath) {
				next.ServeHTTP(w, r)
				return
			}
			// Vary on every eligible path — even identity responses — so a
			// shared cache never hands a gzipped body to an identity-only
			// client or vice versa.
			w.Header().Add("Vary", "Accept-Encoding")
			if !httpconst.AcceptsEncoding(r, "gzip") {
				next.ServeHTTP(w, r)
				return
			}
			gw := &gzipResponseWriter{ResponseWriter: w}
			defer gw.close()
			next.ServeHTTP(gw, r)
		})
	}
}

// gzipEligible reports whether the request may have its response gzipped.
// The /api/ prefix gate already excludes /ws*, /v1/*, and /metrics* — those
// prefixes cannot co-exist with /api/ — so only the MCP path (which an
// operator could configure under /api/) needs an explicit check.
func gzipEligible(r *http.Request, mcpPath string) bool {
	if r.Method != http.MethodGet {
		return false
	}
	p := r.URL.Path
	if !strings.HasPrefix(p, "/api/") {
		return false
	}
	if p == mcpPath || strings.HasPrefix(p, mcpPath+"/") {
		return false
	}
	return true
}

// gzipResponseWriter lazily engages compression on the first write:
// WriteHeader decides — 204/304 and responses that already carry a
// Content-Encoding pass through untouched, everything else gets
// Content-Encoding: gzip with Content-Length dropped (the compressed size
// isn't known up front; net/http falls back to chunked transfer).
type gzipResponseWriter struct {
	http.ResponseWriter
	gz          *gzip.Writer
	wroteHeader bool
}

func (w *gzipResponseWriter) WriteHeader(code int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
	h := w.Header()
	if code != http.StatusNoContent && code != http.StatusNotModified && h.Get("Content-Encoding") == "" {
		h.Set("Content-Encoding", "gzip")
		h.Del("Content-Length")
		gz := gzipWriterPool.Get().(*gzip.Writer)
		gz.Reset(w.ResponseWriter)
		w.gz = gz
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *gzipResponseWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	if w.gz != nil {
		return w.gz.Write(b)
	}
	return w.ResponseWriter.Write(b)
}

// Flush completes the current gzip block before flushing downstream so
// incremental delivery keeps working for handlers that stream.
func (w *gzipResponseWriter) Flush() {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	if w.gz != nil {
		_ = w.gz.Flush()
	}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap exposes the underlying writer to http.ResponseController for
// per-request deadline control. ResponseController still finds our Flush
// first, so the gzip buffer is never bypassed.
func (w *gzipResponseWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

// close finalises the gzip stream (writing the trailer) and returns the
// writer to the pool. No-op when compression never engaged.
func (w *gzipResponseWriter) close() {
	if w.gz == nil {
		return
	}
	_ = w.gz.Close()
	gzipWriterPool.Put(w.gz)
	w.gz = nil
}
