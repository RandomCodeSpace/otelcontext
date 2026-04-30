package ui

import (
	"embed"
	"fmt"
	"io/fs"
	"net/http"
	"path"
	"strings"

	"github.com/RandomCodeSpace/otelcontext/internal/graph"
	"github.com/RandomCodeSpace/otelcontext/internal/storage"
	"github.com/RandomCodeSpace/otelcontext/internal/telemetry"
	"github.com/RandomCodeSpace/otelcontext/internal/vectordb"
)

//go:embed static/* dist
var content embed.FS

type Server struct {
	repo       *storage.Repository
	metrics    *telemetry.Metrics
	topo       *graph.Graph
	vidx       *vectordb.Index
	mcpEnabled bool
	mcpPath    string
}

func NewServer(repo *storage.Repository, metrics *telemetry.Metrics, topo *graph.Graph, vidx *vectordb.Index) *Server {
	return &Server{
		repo:    repo,
		metrics: metrics,
		topo:    topo,
		vidx:    vidx,
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

	// Serve React SPA from dist/ for all non-API paths.
	// API routes are registered before this is called, so they take priority.
	distFS, err := fs.Sub(content, "dist")
	if err != nil {
		return fmt.Errorf("ui: failed to create dist sub-fs: %w", err)
	}
	fileServer := http.FileServer(http.FS(distFS))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// Sanitize: clean the URL path and reject anything that would escape
		// the dist root. embed.FS already rejects ".." segments, but we
		// gate at the boundary so static analyzers don't have to taint-track
		// the user-supplied URL through the FS call.
		clean := path.Clean("/" + r.URL.Path)
		if strings.Contains(clean, "..") {
			http.NotFound(w, r)
			return
		}
		rel := strings.TrimPrefix(clean, "/")
		if rel == "" {
			rel = "index.html"
		}
		// Try the file as-is; if not found, fall back to index.html (SPA routing).
		f, openErr := distFS.Open(rel)
		if openErr == nil {
			_ = f.Close()
			fileServer.ServeHTTP(w, r)
			return
		}
		// SPA fallback — let the React router handle the path.
		r2 := r.Clone(r.Context())
		r2.URL.Path = "/"
		fileServer.ServeHTTP(w, r2)
	})

	return nil
}
