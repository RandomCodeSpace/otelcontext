package ui

import (
	"embed"
	"fmt"
	"html/template"
	"net/http"
	"strings"

	"github.com/RandomCodeSpace/otelcontext/internal/graph"
	"github.com/RandomCodeSpace/otelcontext/internal/storage"
	"github.com/RandomCodeSpace/otelcontext/internal/telemetry"
	"github.com/RandomCodeSpace/otelcontext/internal/vectordb"
)

//go:embed templates/*.html static/*
var content embed.FS

type Server struct {
	repo       *storage.Repository
	metrics    *telemetry.Metrics
	topo       *graph.Graph
	vidx       *vectordb.Index
	tmpl       *template.Template
	mcpEnabled bool
	mcpPath    string
}

// fmtNum formats an integer-like value with K / M / B suffix.
func fmtNum(v any) string {
	var n float64
	switch val := v.(type) {
	case int:
		n = float64(val)
	case int32:
		n = float64(val)
	case int64:
		n = float64(val)
	case float64:
		n = val
	case float32:
		n = float64(val)
	default:
		return fmt.Sprintf("%v", v)
	}
	switch {
	case n >= 1_000_000_000:
		return fmt.Sprintf("%.2fB", n/1_000_000_000)
	case n >= 1_000_000:
		return fmt.Sprintf("%.2fM", n/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%.1fK", n/1_000)
	default:
		return fmt.Sprintf("%.0f", n)
	}
}

func NewServer(repo *storage.Repository, metrics *telemetry.Metrics, topo *graph.Graph, vidx *vectordb.Index) *Server {
	tmpl := template.New("OtelContext").Funcs(template.FuncMap{
		"text_uppercase": strings.ToUpper,
		"text_lowercase": strings.ToLower,
		"fmt_num":        fmtNum,
	})
	tmpl = template.Must(tmpl.ParseFS(content, "templates/*.html"))

	return &Server{
		repo:    repo,
		metrics: metrics,
		topo:    topo,
		vidx:    vidx,
		tmpl:    tmpl,
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

	mux.HandleFunc("/", s.handleDashboard)
	mux.HandleFunc("/logs", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/", http.StatusMovedPermanently)
	})
	mux.HandleFunc("/traces", s.handleTraces)
	mux.HandleFunc("/traces/", s.handleTraceDetail)
	mux.HandleFunc("/services", s.handleServices)
	mux.HandleFunc("/mcp-console", s.handleMCPConsole)
	// Redirect old standalone pages to dashboard
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/", http.StatusMovedPermanently)
	})
	mux.HandleFunc("/storage", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/", http.StatusMovedPermanently)
	})

	return nil
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}

	traces, _ := s.repo.RecentTraces(10)
	stats, _ := s.repo.GetStats()
	health := s.metrics.GetHealthStats()

	err := s.tmpl.ExecuteTemplate(w, "dashboard.html", map[string]any{
		"Title":       "Dashboard - OtelContext",
		"Traces":      traces,
		"Stats":       stats,
		"HealthStats": health,
		"MCPPath":     s.mcpPath,
		"MCPEnabled":  s.mcpEnabled,
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query().Get("q")
	limit := 100

	var logs []storage.Log
	var err error

	if query != "" {
		logs, err = s.repo.SearchLogs(query, limit)
	} else {
		logs, err = s.repo.RecentLogs(limit)
	}

	if err != nil {
		http.Error(w, "Failed to load logs: "+err.Error(), http.StatusInternalServerError)
		return
	}

	err = s.tmpl.ExecuteTemplate(w, "logs.html", map[string]any{
		"Title": "Logs - OtelContext",
		"Logs":  logs,
		"Query": query,
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (s *Server) handleTraces(w http.ResponseWriter, r *http.Request) {
	traces, err := s.repo.RecentTraces(50)
	if err != nil {
		http.Error(w, "Failed to load traces: "+err.Error(), http.StatusInternalServerError)
		return
	}

	err = s.tmpl.ExecuteTemplate(w, "traces.html", map[string]any{
		"Title":  "Traces - OtelContext",
		"Traces": traces,
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (s *Server) handleTraceDetail(w http.ResponseWriter, r *http.Request) {
	traceID := strings.TrimPrefix(r.URL.Path, "/traces/")
	if traceID == "" {
		http.Redirect(w, r, "/traces", http.StatusFound)
		return
	}

	trace, err := s.repo.GetTrace(traceID)
	if err != nil {
		http.Error(w, "Trace not found", http.StatusNotFound)
		return
	}

	err = s.tmpl.ExecuteTemplate(w, "trace_detail.html", map[string]any{
		"Title": "Trace: " + traceID + " - OtelContext",
		"Trace": trace,
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}


func (s *Server) handleServices(w http.ResponseWriter, r *http.Request) {
	nodes := s.topo.GetNodes()

	err := s.tmpl.ExecuteTemplate(w, "services.html", map[string]any{
		"Title": "Services - OtelContext",
		"Nodes": nodes,
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (s *Server) handleMCPConsole(w http.ResponseWriter, r *http.Request) {
	err := s.tmpl.ExecuteTemplate(w, "mcp_console.html", map[string]any{
		"Title":      "MCP Console - OtelContext",
		"MCPPath":    s.mcpPath,
		"MCPEnabled": s.mcpEnabled,
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}


