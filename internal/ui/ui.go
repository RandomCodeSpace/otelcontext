package ui

import (
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"strings"

	"github.com/RandomCodeSpace/otelcontext/internal/graph"
	"github.com/RandomCodeSpace/otelcontext/internal/storage"
	"github.com/RandomCodeSpace/otelcontext/internal/telemetry"
)

// spaFS wraps an fs.FS so http.FileServer transparently serves index.html
// for any extensionless path that doesn't resolve to a real file — the
// usual single-page-app routing where the React router owns client-side
// URLs. Asset-shaped paths (anything with a ".") still 404 normally so a
// missing /favicon.ico doesn't surprise the browser with an HTML body.
//
// Wrapping the FS — rather than calling Open() against r.URL.Path in our
// own handler — keeps the user-controlled name behind the stdlib
// http.FileServer boundary, where path.Clean has already happened.
type spaFS struct{ fs.FS }

func (s spaFS) Open(name string) (fs.File, error) {
	f, err := s.FS.Open(name)
	if err == nil {
		return f, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	// SPA fallback only for extensionless paths (treated as client-side
	// routes); legitimate asset 404s still propagate.
	if strings.Contains(name, ".") {
		return nil, err
	}
	return s.FS.Open("index.html")
}

//go:embed static/* dist
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
	// registered before this is called, so they take priority. spaFS
	// converts extensionless 404s into index.html so the React router
	// can claim them.
	distFS, err := fs.Sub(content, "dist")
	if err != nil {
		return fmt.Errorf("ui: failed to create dist sub-fs: %w", err)
	}
	mux.Handle("/", http.FileServer(http.FS(spaFS{distFS})))

	return nil
}
