package ui

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"

	"github.com/RandomCodeSpace/otelcontext/internal/graph"
	"github.com/RandomCodeSpace/otelcontext/internal/httpconst"
	"github.com/RandomCodeSpace/otelcontext/internal/storage"
	"github.com/RandomCodeSpace/otelcontext/internal/telemetry"
)

// The built UI is generated at release time (scripts/release.sh) and is not
// committed; only internal/ui/dist/.gitkeep is tracked. The `all:` prefix
// embeds that dotfile so a source-only checkout still compiles. A plain
// `go build` therefore serves no SPA at "/" until the UI is built — release
// tags carry the real dist so `go install <module>@<tag>` is UI-complete.
//
//go:embed static/* all:dist
var content embed.FS

type Server struct {
	repo       *storage.Repository
	metrics    *telemetry.Metrics
	topo       *graph.Graph
	mcpEnabled bool
	mcpPath    string
}

// NewServer constructs the embedded-UI server.
//
// The vectordb argument was removed on 2026-05-24 when the vectordb package
// was deleted alongside the find_similar_logs MCP tool cut.
func NewServer(repo *storage.Repository, metrics *telemetry.Metrics, topo *graph.Graph) *Server {
	return &Server{
		repo:    repo,
		metrics: metrics,
		topo:    topo,
		mcpPath: "/mcp",
	}
}

// SetMCPConfig configures MCP metadata shown in the UI.
func (s *Server) SetMCPConfig(enabled bool, path string) {
	s.mcpEnabled = enabled
	if path != "" {
		s.mcpPath = path
	}
}

func (s *Server) RegisterRoutes(mux *http.ServeMux) error {
	mux.Handle("/static/", http.FileServer(http.FS(content)))

	// Serve React SPA from dist/ for all non-API paths. API routes are
	// registered before this is called, so they take priority; anything
	// that falls through lands on the spaHandler below.
	distFS, err := fs.Sub(content, "dist")
	if err != nil {
		return fmt.Errorf("ui: failed to create dist sub-fs: %w", err)
	}
	mux.Handle("/", newSPAHandler(distFS))

	return nil
}

// reservedPrefixes are machine namespaces that must 404 — never receive the
// SPA shell — when no explicit mux route matched. Serving HTML to an OTLP
// exporter, a WebSocket client, or an MCP agent that hit an unknown path
// would only confuse it.
var reservedPrefixes = []string{"/api/", "/v1/", "/ws", "/mcp", "/metrics"}

// spaHandler serves the embedded Vite build:
//
//   - "/" and SPA fallback routes → index.html with Cache-Control: no-cache
//     plus a startup-computed ETag so steady-state reloads are 304s.
//   - "/assets/*" (Vite hashed filenames) → immutable one-year caching with
//     Accept-Encoding negotiation against build-time .br/.gz siblings
//     (emitted by ui/scripts/precompress.mjs and picked up by `all:dist`).
//   - any other real file → plain http.FileServer.
//   - unknown extensionless non-reserved paths → index.html (client-side
//     routing). Asset-shaped paths (anything with a ".") still 404 normally
//     so a missing /favicon.ico doesn't surprise the browser with HTML —
//     same contract as the previous spaFS wrapper.
type spaHandler struct {
	dist       fs.FS
	fileServer http.Handler

	// index.html variants, loaded once at startup. indexPlain == nil means
	// dist holds only the source-only stub (.gitkeep) — the dev case where
	// the UI was never built.
	indexPlain []byte
	indexGz    []byte
	indexBr    []byte
	indexETag  string
}

func newSPAHandler(dist fs.FS) *spaHandler {
	h := &spaHandler{
		dist:       dist,
		fileServer: http.FileServer(http.FS(dist)),
	}
	if b, err := fs.ReadFile(dist, "index.html"); err == nil {
		h.indexPlain = b
		sum := sha256.Sum256(b)
		h.indexETag = `"` + hex.EncodeToString(sum[:8]) + `"`
		if gz, err := fs.ReadFile(dist, "index.html.gz"); err == nil {
			h.indexGz = gz
		}
		if br, err := fs.ReadFile(dist, "index.html.br"); err == nil {
			h.indexBr = br
		}
	}
	return h
}

func (h *spaHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := path.Clean(r.URL.Path)
	switch {
	case p == "/" || p == "/index.html":
		h.serveIndex(w, r)
	case strings.HasPrefix(p, "/assets/"):
		h.serveAsset(w, r, strings.TrimPrefix(p, "/"))
	default:
		h.serveOther(w, r, p)
	}
}

// serveOther handles everything that isn't index.html or a hashed asset:
// real embedded files delegate to the stdlib file server (which handles
// Content-Type, ranges, and path traversal), everything else either 404s or
// falls back to the SPA shell.
func (h *spaHandler) serveOther(w http.ResponseWriter, r *http.Request, p string) {
	name := strings.TrimPrefix(p, "/")
	if name != "" {
		if f, err := h.dist.Open(name); err == nil {
			info, statErr := f.Stat()
			_ = f.Close()
			if statErr == nil && !info.IsDir() {
				h.fileServer.ServeHTTP(w, r)
				return
			}
		}
	}
	if strings.Contains(name, ".") || isReserved(p) {
		http.NotFound(w, r)
		return
	}
	h.serveIndex(w, r)
}

func isReserved(p string) bool {
	for _, prefix := range reservedPrefixes {
		if strings.HasPrefix(p, prefix) {
			return true
		}
	}
	return false
}

// serveIndex serves the SPA shell. no-cache (not no-store) keeps the bytes
// in the browser cache but forces revalidation, so a redeploy is picked up
// on the next navigation while steady-state loads are If-None-Match → 304.
func (h *spaHandler) serveIndex(w http.ResponseWriter, r *http.Request) {
	if h.indexPlain == nil {
		// Source-only checkout: dist holds just the .gitkeep stub. A plain
		// 404 with a pointer beats an empty page or a directory listing.
		http.Error(w, "UI not built — release binaries embed the SPA (see scripts/release.sh)", http.StatusNotFound)
		return
	}
	hdr := w.Header()
	hdr.Set("Cache-Control", "no-cache")
	hdr.Set("ETag", h.indexETag)
	hdr.Set("Vary", "Accept-Encoding")
	if httpconst.ETagMatch(r.Header.Get("If-None-Match"), h.indexETag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	body := h.indexPlain
	switch {
	case h.indexBr != nil && httpconst.AcceptsEncoding(r, "br"):
		hdr.Set("Content-Encoding", "br")
		body = h.indexBr
	case h.indexGz != nil && httpconst.AcceptsEncoding(r, "gzip"):
		hdr.Set("Content-Encoding", "gzip")
		body = h.indexGz
	}
	writeBody(w, r, body, "text/html; charset=utf-8")
}

// serveAsset serves a Vite-emitted hashed asset (/assets/<name>-<hash>.<ext>).
// The hash makes the content immutable, so the response is cacheable for a
// year; build-time precompressed .br/.gz siblings are negotiated via
// Accept-Encoding with Content-Type taken from the ORIGINAL extension.
func (h *spaHandler) serveAsset(w http.ResponseWriter, r *http.Request, name string) {
	body, encoding, ok := h.assetVariant(r, name)
	if !ok {
		http.NotFound(w, r)
		return
	}
	hdr := w.Header()
	hdr.Set("Cache-Control", "public, max-age=31536000, immutable")
	hdr.Set("Vary", "Accept-Encoding")
	if encoding != "" {
		hdr.Set("Content-Encoding", encoding)
	}
	ctype := mime.TypeByExtension(path.Ext(name))
	if ctype == "" {
		ctype = "application/octet-stream"
	}
	writeBody(w, r, body, ctype)
}

// assetVariant picks the best representation the client accepts: brotli
// sibling first (smallest), then gzip, then the original bytes.
func (h *spaHandler) assetVariant(r *http.Request, name string) (body []byte, encoding string, ok bool) {
	if httpconst.AcceptsEncoding(r, "br") {
		if b, err := fs.ReadFile(h.dist, name+".br"); err == nil {
			return b, "br", true
		}
	}
	if httpconst.AcceptsEncoding(r, "gzip") {
		if b, err := fs.ReadFile(h.dist, name+".gz"); err == nil {
			return b, "gzip", true
		}
	}
	b, err := fs.ReadFile(h.dist, name)
	if err != nil {
		return nil, "", false
	}
	return b, "", true
}

func writeBody(w http.ResponseWriter, r *http.Request, body []byte, ctype string) {
	hdr := w.Header()
	hdr.Set(httpconst.HeaderContentType, ctype)
	hdr.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(body) //nolint:gosec // G705 false positive: body is read-only embedded build output (fs.ValidPath rejects traversal), never request data
	}
}
