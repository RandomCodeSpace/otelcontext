package ui

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

// builtDistFS returns a fake Vite build output: index.html with both
// precompressed siblings, a hashed JS asset with both siblings, a hashed
// CSS asset with no siblings, and a root-level static file.
func builtDistFS() fstest.MapFS {
	return fstest.MapFS{
		"index.html":              {Data: []byte("<html>index</html>")},
		"index.html.br":           {Data: []byte("br-index")},
		"index.html.gz":           {Data: []byte("gz-index")},
		"assets/app-abc123.js":    {Data: []byte("console.log('app')")},
		"assets/app-abc123.js.br": {Data: []byte("br-js")},
		"assets/app-abc123.js.gz": {Data: []byte("gz-js")},
		"assets/app-abc123.css":   {Data: []byte("body{}")},
		"vite.svg":                {Data: []byte("<svg/>")},
	}
}

func get(t *testing.T, h http.Handler, path string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestServeAsset_BrotliNegotiation(t *testing.T) {
	h := newSPAHandler(builtDistFS())
	rec := get(t, h, "/assets/app-abc123.js", map[string]string{"Accept-Encoding": "gzip, br"})

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	if got := rec.Header().Get("Content-Encoding"); got != "br" {
		t.Errorf("Content-Encoding = %q, want br", got)
	}
	if got := rec.Header().Get("Cache-Control"); got != "public, max-age=31536000, immutable" {
		t.Errorf("Cache-Control = %q", got)
	}
	if got := rec.Header().Get("Vary"); got != "Accept-Encoding" {
		t.Errorf("Vary = %q", got)
	}
	// Content-Type must come from the ORIGINAL .js extension, not .br.
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "javascript") {
		t.Errorf("Content-Type = %q, want javascript", ct)
	}
	if rec.Body.String() != "br-js" {
		t.Errorf("body = %q, want br sibling bytes", rec.Body.String())
	}
}

func TestServeAsset_GzipFallback(t *testing.T) {
	h := newSPAHandler(builtDistFS())
	rec := get(t, h, "/assets/app-abc123.js", map[string]string{"Accept-Encoding": "gzip"})

	if got := rec.Header().Get("Content-Encoding"); got != "gzip" {
		t.Errorf("Content-Encoding = %q, want gzip", got)
	}
	if rec.Body.String() != "gz-js" {
		t.Errorf("body = %q, want gz sibling bytes", rec.Body.String())
	}
}

func TestServeAsset_BrotliQZeroFallsBackToGzip(t *testing.T) {
	h := newSPAHandler(builtDistFS())
	rec := get(t, h, "/assets/app-abc123.js", map[string]string{"Accept-Encoding": "br;q=0, gzip"})

	if got := rec.Header().Get("Content-Encoding"); got != "gzip" {
		t.Errorf("Content-Encoding = %q, want gzip", got)
	}
}

func TestServeAsset_IdentityWhenNoAcceptEncoding(t *testing.T) {
	h := newSPAHandler(builtDistFS())
	rec := get(t, h, "/assets/app-abc123.js", nil)

	if got := rec.Header().Get("Content-Encoding"); got != "" {
		t.Errorf("Content-Encoding = %q, want empty", got)
	}
	if rec.Body.String() != "console.log('app')" {
		t.Errorf("body = %q, want original bytes", rec.Body.String())
	}
	if got := rec.Header().Get("Cache-Control"); !strings.Contains(got, "immutable") {
		t.Errorf("Cache-Control = %q, want immutable", got)
	}
}

func TestServeAsset_NoSiblingServesIdentity(t *testing.T) {
	h := newSPAHandler(builtDistFS())
	rec := get(t, h, "/assets/app-abc123.css", map[string]string{"Accept-Encoding": "gzip, br"})

	if got := rec.Header().Get("Content-Encoding"); got != "" {
		t.Errorf("Content-Encoding = %q, want empty (no siblings exist)", got)
	}
	if rec.Body.String() != "body{}" {
		t.Errorf("body = %q", rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/css") {
		t.Errorf("Content-Type = %q, want text/css", ct)
	}
}

func TestServeAsset_Missing404(t *testing.T) {
	h := newSPAHandler(builtDistFS())
	rec := get(t, h, "/assets/nope-zzz.js", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d", rec.Code)
	}
}

func TestIndex_ETagAnd304(t *testing.T) {
	h := newSPAHandler(builtDistFS())

	rec := get(t, h, "/", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-cache" {
		t.Errorf("Cache-Control = %q, want no-cache", got)
	}
	etag := rec.Header().Get("ETag")
	if etag == "" || !strings.HasPrefix(etag, `"`) {
		t.Fatalf("ETag = %q, want quoted non-empty", etag)
	}
	if rec.Body.String() != "<html>index</html>" {
		t.Errorf("body = %q", rec.Body.String())
	}

	rec2 := get(t, h, "/", map[string]string{"If-None-Match": etag})
	if rec2.Code != http.StatusNotModified {
		t.Fatalf("want 304, got %d", rec2.Code)
	}
	if rec2.Body.Len() != 0 {
		t.Errorf("304 body should be empty, got %q", rec2.Body.String())
	}
	if got := rec2.Header().Get("ETag"); got != etag {
		t.Errorf("304 ETag = %q, want %q", got, etag)
	}
}

func TestIndex_PrecompressedVariant(t *testing.T) {
	h := newSPAHandler(builtDistFS())
	rec := get(t, h, "/", map[string]string{"Accept-Encoding": "br"})

	if got := rec.Header().Get("Content-Encoding"); got != "br" {
		t.Errorf("Content-Encoding = %q, want br", got)
	}
	if rec.Body.String() != "br-index" {
		t.Errorf("body = %q, want br variant", rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}
	// ETag must be identical across encodings — it identifies the document,
	// and Vary: Accept-Encoding disambiguates the representation.
	identity := get(t, h, "/", nil)
	if rec.Header().Get("ETag") != identity.Header().Get("ETag") {
		t.Errorf("ETag differs between encodings")
	}
	if got := rec.Header().Get("Vary"); got != "Accept-Encoding" {
		t.Errorf("Vary = %q", got)
	}
}

func TestSPAFallback_ServesIndexForClientRoutes(t *testing.T) {
	h := newSPAHandler(builtDistFS())
	for _, path := range []string{"/traces", "/logs", "/traces/deep/route"} {
		rec := get(t, h, path, nil)
		if rec.Code != http.StatusOK {
			t.Errorf("%s: want 200, got %d", path, rec.Code)
			continue
		}
		if rec.Body.String() != "<html>index</html>" {
			t.Errorf("%s: body = %q, want index", path, rec.Body.String())
		}
		// Fallback responses are the mutable SPA shell — same caching rules
		// as "/" so a deploy is picked up on the next poll.
		if got := rec.Header().Get("Cache-Control"); got != "no-cache" {
			t.Errorf("%s: Cache-Control = %q, want no-cache", path, got)
		}
	}
}

func TestSPAFallback_DoesNotClaimReservedOrAssetPaths(t *testing.T) {
	h := newSPAHandler(builtDistFS())
	for _, path := range []string{
		"/api/x",            // unknown API route must 404, not get HTML
		"/v1/unknown",       // OTLP namespace
		"/ws/nope",          // WebSocket namespace
		"/mcp/extra",        // MCP namespace
		"/metrics/unknown",  // Prometheus namespace
		"/missing.png",      // asset-shaped: extension means real 404
		"/release/v1.2/doc", // dot anywhere in the path: preserved legacy spaFS behavior
	} {
		rec := get(t, h, path, nil)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: want 404, got %d (body %q)", path, rec.Code, rec.Body.String())
		}
	}
}

func TestRealFile_DelegatesToFileServer(t *testing.T) {
	h := newSPAHandler(builtDistFS())
	rec := get(t, h, "/vite.svg", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	if rec.Body.String() != "<svg/>" {
		t.Errorf("body = %q", rec.Body.String())
	}
}

func TestHeadRequest_NoBody(t *testing.T) {
	h := newSPAHandler(builtDistFS())
	req := httptest.NewRequest(http.MethodHead, "/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("HEAD body should be empty, got %q", rec.Body.String())
	}
	if rec.Header().Get("ETag") == "" {
		t.Errorf("HEAD response missing ETag")
	}
}

// TestStubDist covers the source-only checkout where dist holds just the
// .gitkeep placeholder: the handler must degrade to a plain 404 instead of
// panicking or serving a directory listing.
func TestStubDist_Graceful404(t *testing.T) {
	h := newSPAHandler(fstest.MapFS{".gitkeep": {Data: nil}})
	for _, path := range []string{"/", "/traces"} {
		rec := get(t, h, path, nil)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: want 404 in stub mode, got %d", path, rec.Code)
		}
		body, _ := io.ReadAll(rec.Body)
		if !strings.Contains(string(body), "UI not built") {
			t.Errorf("%s: stub body should explain the missing build, got %q", path, body)
		}
	}
}

// TestRegisterRoutes_RealEmbed exercises the production wiring against the
// actual embedded FS (a stub dist in a source-only checkout).
func TestRegisterRoutes_RealEmbed(t *testing.T) {
	s := NewServer(nil, nil, nil)
	s.SetMCPConfig(true, "/mcp")
	mux := http.NewServeMux()
	if err := s.RegisterRoutes(mux); err != nil {
		t.Fatalf("RegisterRoutes: %v", err)
	}
	// /static/style.css is committed and embedded — must always serve.
	rec := get(t, mux, "/static/style.css", nil)
	if rec.Code != http.StatusOK {
		t.Errorf("/static/style.css: want 200, got %d", rec.Code)
	}
}

// Regression for semgrep filepath-clean-misuse: request paths are
// rooted-cleaned before any lookup, so traversal shapes can never escape
// the dist tree (fs.ValidPath is the second layer of the same defense).
func TestServeHTTP_PathTraversalRootedClean(t *testing.T) {
	h := newSPAHandler(builtDistFS())

	// Climbing above the root collapses to a rooted path: asset-shaped
	// targets 404, extensionless ones get the SPA shell — never a file read.
	rec := get(t, h, "/../../etc/secret.txt", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("traversal path: got %d, want 404", rec.Code)
	}
	rec = get(t, h, "/../../etc/passwd", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "index") {
		t.Fatalf("traversal fallback: got %d %q, want SPA shell", rec.Code, rec.Body.String())
	}

	// ".." inside /assets collapses to the real target, which is served by
	// its own handler (index: no-cache), never via the immutable asset path.
	rec = get(t, h, "/assets/../index.html", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "index") {
		t.Fatalf("collapsed path: got %d %q, want index", rec.Code, rec.Body.String())
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-cache" {
		t.Fatalf("collapsed path served with asset caching: Cache-Control=%q", cc)
	}
}
