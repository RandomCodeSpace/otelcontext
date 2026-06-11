package api

import (
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// jsonEcho is a handler that writes the given payload as JSON in several
// chunks (exercising the multi-Write path through the gzip writer).
func jsonEcho(payload string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		half := len(payload) / 2
		_, _ = w.Write([]byte(payload[:half]))
		_, _ = w.Write([]byte(payload[half:]))
	})
}

func gzipGet(t *testing.T, h http.Handler, path string, acceptGzip bool) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if acceptGzip {
		req.Header.Set("Accept-Encoding", "gzip")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func gunzip(t *testing.T, body io.Reader) string {
	t.Helper()
	zr, err := gzip.NewReader(body)
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	defer zr.Close()
	b, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("gunzip read: %v", err)
	}
	return string(b)
}

func TestGzipMiddleware_CompressesAPIGet(t *testing.T) {
	payload := strings.Repeat(`{"service":"checkout","status":"healthy"},`, 100)
	h := GzipMiddleware("/mcp")(jsonEcho(payload))

	rec := gzipGet(t, h, "/api/system/graph", true)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	if got := rec.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", got)
	}
	if got := rec.Header().Get("Vary"); got != "Accept-Encoding" {
		t.Errorf("Vary = %q", got)
	}
	if cl := rec.Header().Get("Content-Length"); cl != "" {
		t.Errorf("Content-Length should be dropped, got %q", cl)
	}
	if rec.Body.Len() >= len(payload) {
		t.Errorf("body not actually compressed: %d >= %d", rec.Body.Len(), len(payload))
	}
	if got := gunzip(t, rec.Body); got != payload {
		t.Errorf("decompressed body mismatch (len %d vs %d)", len(got), len(payload))
	}
}

func TestGzipMiddleware_SkipsWithoutAcceptEncoding(t *testing.T) {
	payload := strings.Repeat("x", 4096)
	h := GzipMiddleware("/mcp")(jsonEcho(payload))

	rec := gzipGet(t, h, "/api/logs", false)

	if got := rec.Header().Get("Content-Encoding"); got != "" {
		t.Fatalf("Content-Encoding = %q, want empty", got)
	}
	if rec.Body.String() != payload {
		t.Errorf("identity body mangled")
	}
	// Vary still set: caches must key on Accept-Encoding either way.
	if got := rec.Header().Get("Vary"); got != "Accept-Encoding" {
		t.Errorf("Vary = %q", got)
	}
}

func TestGzipMiddleware_SkipsNonAPIPaths(t *testing.T) {
	payload := strings.Repeat("y", 2048)
	for _, path := range []string{"/ws", "/ws/events", "/v1/traces", "/metrics/prometheus", "/mcp", "/mcp/session", "/"} {
		h := GzipMiddleware("/mcp")(jsonEcho(payload))
		rec := gzipGet(t, h, path, true)
		if got := rec.Header().Get("Content-Encoding"); got != "" {
			t.Errorf("%s: Content-Encoding = %q, want empty", path, got)
		}
		if rec.Body.String() != payload {
			t.Errorf("%s: body mangled", path)
		}
	}
}

func TestGzipMiddleware_SkipsNonGET(t *testing.T) {
	h := GzipMiddleware("/mcp")(jsonEcho(strings.Repeat("z", 2048)))
	req := httptest.NewRequest(http.MethodPost, "/api/admin/vacuum", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if got := rec.Header().Get("Content-Encoding"); got != "" {
		t.Errorf("POST: Content-Encoding = %q, want empty", got)
	}
}

func TestGzipMiddleware_RespectsExistingContentEncoding(t *testing.T) {
	h := GzipMiddleware("/mcp")(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Encoding", "br")
		_, _ = w.Write([]byte("pre-compressed-bytes"))
	}))
	rec := gzipGet(t, h, "/api/whatever", true)
	if got := rec.Header().Get("Content-Encoding"); got != "br" {
		t.Fatalf("Content-Encoding = %q, want br untouched", got)
	}
	if rec.Body.String() != "pre-compressed-bytes" {
		t.Errorf("double-compressed an already-encoded response")
	}
}

func TestGzipMiddleware_No304Body(t *testing.T) {
	h := GzipMiddleware("/mcp")(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotModified)
	}))
	rec := gzipGet(t, h, "/api/stats", true)
	if rec.Code != http.StatusNotModified {
		t.Fatalf("want 304, got %d", rec.Code)
	}
	if got := rec.Header().Get("Content-Encoding"); got != "" {
		t.Errorf("304 must not carry Content-Encoding, got %q", got)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("304 body should be empty, got %d bytes", rec.Body.Len())
	}
}

func TestGzipMiddleware_EmptyHandlerBody(t *testing.T) {
	// Handler that never writes: net/http sends an implicit 200 with no
	// body; the wrapper must not inject a stray empty gzip stream header.
	h := GzipMiddleware("/mcp")(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {}))
	rec := gzipGet(t, h, "/api/empty", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("empty handler should produce empty body, got %d bytes", rec.Body.Len())
	}
}

func TestGzipMiddleware_FlushSupported(t *testing.T) {
	h := GzipMiddleware("/mcp")(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("first"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		_, _ = w.Write([]byte("second"))
	}))
	rec := gzipGet(t, h, "/api/stream", true)
	if !rec.Flushed {
		t.Errorf("Flush did not propagate to the underlying writer")
	}
	if got := gunzip(t, rec.Body); got != "firstsecond" {
		t.Errorf("flushed body = %q", got)
	}
}

// TestGzipMiddleware_PoolReuseConcurrent hammers the middleware from many
// goroutines to prove the sync.Pool recycling is race-free and never
// cross-wires response bodies.
func TestGzipMiddleware_PoolReuseConcurrent(t *testing.T) {
	payload := strings.Repeat(`{"n":1},`, 512)
	h := GzipMiddleware("/mcp")(jsonEcho(payload))
	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 20 {
				rec := httptest.NewRecorder()
				req := httptest.NewRequest(http.MethodGet, "/api/x", nil)
				req.Header.Set("Accept-Encoding", "gzip")
				h.ServeHTTP(rec, req)
				zr, err := gzip.NewReader(rec.Body)
				if err != nil {
					t.Errorf("gzip.NewReader: %v", err)
					return
				}
				b, err := io.ReadAll(zr)
				_ = zr.Close()
				if err != nil || string(b) != payload {
					t.Errorf("corrupted body under concurrency (err=%v, len=%d)", err, len(b))
					return
				}
			}
		}()
	}
	wg.Wait()
}
