package ui

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

func testAssets() fstest.MapFS {
	return fstest.MapFS{
		"static/index.html":  {Data: []byte("<html>shell</html>")},
		"static/app.js":      {Data: []byte("console.log('app')")},
		"static/app.css":     {Data: []byte("body{}")},
		"static/favicon.svg": {Data: []byte("<svg/>")},
	}
}

func request(t *testing.T, handler http.Handler, method, target string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, nil)
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func newTestHandler(t *testing.T) http.Handler {
	t.Helper()
	handler, err := newEmbeddedHandler(testAssets())
	if err != nil {
		t.Fatalf("newEmbeddedHandler: %v", err)
	}
	return handler
}

func TestIndexServesEmbeddedShellWithETag(t *testing.T) {
	handler := newTestHandler(t)
	rec := request(t, handler, http.MethodGet, "/", nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if rec.Body.String() != "<html>shell</html>" {
		t.Fatalf("body = %q", rec.Body.String())
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-cache" {
		t.Errorf("Cache-Control = %q, want no-cache", got)
	}
	etag := rec.Header().Get("ETag")
	if etag == "" {
		t.Fatal("ETag is empty")
	}

	notModified := request(t, handler, http.MethodGet, "/", map[string]string{"If-None-Match": etag})
	if notModified.Code != http.StatusNotModified {
		t.Fatalf("conditional status = %d, want 304", notModified.Code)
	}
	if notModified.Body.Len() != 0 {
		t.Errorf("304 body = %q, want empty", notModified.Body.String())
	}
}

func TestStaticAssetCachingAndContentType(t *testing.T) {
	handler := newTestHandler(t)
	rec := request(t, handler, http.MethodGet, "/static/app.js", nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if rec.Body.String() != "console.log('app')" {
		t.Errorf("body = %q", rec.Body.String())
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-cache" {
		t.Errorf("Cache-Control = %q, want no-cache", got)
	}
	if got := rec.Header().Get("Content-Type"); !strings.Contains(got, "javascript") {
		t.Errorf("Content-Type = %q, want javascript", got)
	}
	if rec.Header().Get("ETag") == "" {
		t.Error("ETag is empty")
	}
}

func TestClientRouteFallbackAndMachineNamespace404(t *testing.T) {
	handler := newTestHandler(t)

	for _, target := range []string{"/map", "/services/checkout"} {
		rec := request(t, handler, http.MethodGet, target, nil)
		if rec.Code != http.StatusOK || rec.Body.String() != "<html>shell</html>" {
			t.Errorf("%s: got %d %q, want shell", target, rec.Code, rec.Body.String())
		}
	}

	for _, target := range []string{
		"/api/missing",
		"/api",
		"/v1/missing",
		"/ws/missing",
		"/mcp/missing",
		"/metrics/missing",
		"/missing.png",
		"/static/missing.js",
	} {
		rec := request(t, handler, http.MethodGet, target, nil)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 404", target, rec.Code)
		}
	}
}

func TestHeadHasHeadersAndNoBody(t *testing.T) {
	handler := newTestHandler(t)
	rec := request(t, handler, http.MethodHead, "/static/app.css", nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("body = %q, want empty", rec.Body.String())
	}
	if rec.Header().Get("Content-Length") == "" {
		t.Error("Content-Length is empty")
	}
}

func TestRootedPathCleaningDoesNotEscapeAssets(t *testing.T) {
	handler := newTestHandler(t)

	rec := request(t, handler, http.MethodGet, "/../../etc/secret.txt", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("traversal status = %d, want 404", rec.Code)
	}

	rec = request(t, handler, http.MethodGet, "/static/../index.html", nil)
	if rec.Code != http.StatusOK || rec.Body.String() != "<html>shell</html>" {
		t.Fatalf("cleaned path = %d %q, want index", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-cache" {
		t.Errorf("cleaned index Cache-Control = %q, want no-cache", got)
	}
}

func TestMissingIndexFailsRegistration(t *testing.T) {
	_, err := newEmbeddedHandler(fstest.MapFS{
		"static/app.js": {Data: []byte("app")},
	})
	if err == nil || !strings.Contains(err.Error(), "index.html") {
		t.Fatalf("error = %v, want missing index error", err)
	}
}

func TestRegisterRoutesUsesCommittedAssets(t *testing.T) {
	mux := http.NewServeMux()
	if err := RegisterRoutes(mux); err != nil {
		t.Fatalf("RegisterRoutes: %v", err)
	}

	index := request(t, mux, http.MethodGet, "/", nil)
	if index.Code != http.StatusOK || !strings.Contains(index.Body.String(), "Service constellation") {
		t.Fatalf("index = %d, contains screen heading = %v", index.Code, strings.Contains(index.Body.String(), "Service constellation"))
	}
	for _, marker := range []string{
		"type=\"module\" src=\"/static/app.js\"",
		"id=\"service-map\"",
		"id=\"inspector\" aria-labelledby=\"inspector-title\" aria-hidden=\"true\" inert",
		"id=\"command-dialog\"",
		"id=\"shortcut-dialog\"",
	} {
		if !strings.Contains(index.Body.String(), marker) {
			t.Errorf("index is missing %q", marker)
		}
	}
	for _, target := range []string{"/static/app.css", "/static/app.js", "/static/favicon.svg"} {
		rec := request(t, mux, http.MethodGet, target, nil)
		if rec.Code != http.StatusOK || rec.Body.Len() == 0 {
			t.Errorf("%s: status = %d, bytes = %d", target, rec.Code, rec.Body.Len())
		}
	}
}

func TestCommittedUIHasNoFrontendBuildContract(t *testing.T) {
	entries, err := fs.ReadDir(content, "static")
	if err != nil {
		t.Fatalf("read embedded UI: %v", err)
	}
	wantAssets := map[string]bool{
		"app.css":     true,
		"app.js":      true,
		"favicon.svg": true,
		"index.html":  true,
	}
	for _, entry := range entries {
		if entry.IsDir() || !wantAssets[entry.Name()] {
			t.Errorf("embedded UI contains unexpected generated or nested asset %q", entry.Name())
		}
		delete(wantAssets, entry.Name())
	}
	for name := range wantAssets {
		t.Errorf("embedded UI is missing source asset %q", name)
	}

	for _, name := range []string{"index.html", "app.css", "app.js"} {
		body, readErr := fs.ReadFile(content, "static/"+name)
		if readErr != nil {
			t.Fatalf("read %s: %v", name, readErr)
		}
		text := strings.ReplaceAll(strings.ToLower(string(body)), "http://www.w3.org/2000/svg", "")
		for _, forbidden := range []string{"http://", "https://", "@import", "node_modules", "sourceMappingURL"} {
			if strings.Contains(text, strings.ToLower(forbidden)) {
				t.Errorf("%s contains external or generated asset marker %q", name, forbidden)
			}
		}
	}

	forbiddenFiles := map[string]bool{
		"package.json":      true,
		"package-lock.json": true,
		"pnpm-lock.yaml":    true,
		"yarn.lock":         true,
		"bun.lock":          true,
		"bun.lockb":         true,
		"vite.config.js":    true,
		"vite.config.ts":    true,
		"webpack.config.js": true,
	}
	err = filepath.WalkDir(filepath.Join("..", ".."), func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if entry.Name() == "node_modules" {
				return filepath.SkipDir
			}
			if strings.HasPrefix(entry.Name(), ".") && path != filepath.Join("..", "..") {
				return filepath.SkipDir
			}
		}
		if forbiddenFiles[entry.Name()] {
			t.Errorf("frontend build dependency is not allowed: %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("scan repository frontend contract: %v", err)
	}
}

func TestReadmeCoversTheUserJourney(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "..", "README.md"))
	if err != nil {
		t.Fatalf("read README: %v", err)
	}
	readme := string(body)
	for _, marker := range []string{
		"style=for-the-badge",
		"docs/assets/otelcontext-overview.webp",
		"## What you get",
		"## Quick start",
		"## Investigate with MCP",
		"## Common configuration",
		"docs/OPERATIONS.md",
		"There is no Node.js install or frontend build step.",
	} {
		if !strings.Contains(readme, marker) {
			t.Errorf("README is missing user-journey marker %q", marker)
		}
	}
}
