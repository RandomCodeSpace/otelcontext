package mcp

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/RandomCodeSpace/central-ops/pkg/httputil"
	"github.com/RandomCodeSpace/otelcontext/internal/graph"
	"github.com/RandomCodeSpace/otelcontext/internal/graphrag"
	"github.com/RandomCodeSpace/otelcontext/internal/storage"
	"github.com/RandomCodeSpace/otelcontext/internal/telemetry"
	"github.com/RandomCodeSpace/otelcontext/internal/vectordb"
)

const (
	mcpProtocolVersion = "2024-11-05"
	serverName         = "OtelContext-mcp"
	serverVersion      = "1.0.0"

	// mcpTenantHeader is the canonical header MCP clients use to scope tool
	// invocations to a particular tenant. When absent, queries run under
	// defaultTenant (injected at construction time).
	mcpTenantHeader = "X-Tenant-ID"
)

// Server is the HTTP Streamable MCP server.
// POST /mcp  — JSON-RPC 2.0 request/response
// GET  /mcp  — SSE stream for real-time notifications
// OPTIONS /mcp — CORS preflight
type Server struct {
	repo          *storage.Repository
	metrics       *telemetry.Metrics
	svcGraph      *graph.Graph
	vectorIdx     *vectordb.Index
	graphRAG      *graphrag.GraphRAG
	defaultTenant string
}

// New creates a new MCP server. defaultTenant is the fallback tenant applied
// to header-less MCP requests; an empty string falls back to
// storage.DefaultTenantID. Required at construction time so production startup
// cannot accidentally drop cfg.DefaultTenant — a missing argument is a compile
// error rather than a silent regression.
func New(
	defaultTenant string,
	repo *storage.Repository,
	metrics *telemetry.Metrics,
	svcGraph *graph.Graph,
	vectorIdx *vectordb.Index,
) *Server {
	if defaultTenant == "" {
		defaultTenant = storage.DefaultTenantID
	}
	return &Server{
		repo:          repo,
		metrics:       metrics,
		svcGraph:      svcGraph,
		vectorIdx:     vectorIdx,
		defaultTenant: defaultTenant,
	}
}

// SetDefaultTenant overrides the fallback tenant at runtime. Empty strings are
// ignored so callers can pass through optional config without clobbering the
// constructor-provided value.
func (s *Server) SetDefaultTenant(t string) {
	if t != "" {
		s.defaultTenant = t
	}
}

// SetGraphRAG wires the GraphRAG instance for advanced query tools.
func (s *Server) SetGraphRAG(g *graphrag.GraphRAG) {
	s.graphRAG = g
}

// Handler returns an http.Handler for the MCP server with CORS applied.
// Works correctly when mounted with http.StripPrefix.
func (s *Server) Handler() http.Handler {
	return httputil.CORSMiddleware("*", http.HandlerFunc(s.ServeHTTP))
}

// ServeHTTP dispatches by HTTP method — no path routing needed.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		s.handleRPC(w, r)
	case http.MethodGet:
		s.handleSSE(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleRPC processes JSON-RPC 2.0 requests.
func (s *Server) handleRPC(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20)) // 1 MB cap
	if err != nil {
		writeError(w, nil, ErrInvalidRequest, "failed to read request body")
		return
	}

	var req JSONRPCRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, nil, ErrParseError, "invalid JSON")
		return
	}

	if req.JSONRPC != "2.0" {
		writeError(w, req.ID, ErrInvalidRequest, "jsonrpc must be '2.0'")
		return
	}

	slog.Debug("MCP RPC", "method", req.Method)

	var result any
	var rpcErr *RPCError

	switch req.Method {
	case "initialize":
		result = InitializeResult{
			ProtocolVersion: mcpProtocolVersion,
			ServerInfo:      ServerInfo{Name: serverName, Version: serverVersion},
			Capabilities: map[string]any{
				"tools":     map[string]any{},
				"resources": map[string]any{},
			},
		}

	case "initialized", "notifications/initialized":
		// Client acknowledges initialization — no response needed (notification).
		w.WriteHeader(http.StatusAccepted)
		return

	case "tools/list":
		result = ToolsListResult{Tools: toolDefs}

	case "tools/call":
		params, ok := parseToolCallParams(req.Params)
		if !ok {
			rpcErr = &RPCError{Code: ErrInvalidParams, Message: "invalid tools/call params"}
			break
		}
		// Resolve tenant from the MCP HTTP transport: header wins, else default.
		// Downstream tool handlers pull the tenant off ctx via mcpCtx(r.Context()).
		tenant := strings.TrimSpace(r.Header.Get(mcpTenantHeader))
		if tenant == "" {
			tenant = s.defaultTenant
		}
		callCtx := storage.WithTenantContext(r.Context(), tenant)
		result = s.toolHandler(callCtx, params.Name, params.Arguments)

	case "ping":
		result = map[string]string{"status": "ok", "ts": time.Now().UTC().Format(time.RFC3339)}

	case "resources/list":
		result = map[string]any{
			"resources": []map[string]any{
				{"uri": "OtelContext://system/graph", "name": "System Graph", "mimeType": "application/json"},
				{"uri": "OtelContext://metrics/prometheus", "name": "Prometheus Metrics", "mimeType": "text/plain"},
			},
		}

	default:
		rpcErr = &RPCError{Code: ErrMethodNotFound, Message: fmt.Sprintf("method not found: %s", req.Method)}
	}

	resp := JSONRPCResponse{JSONRPC: "2.0", ID: req.ID}
	if rpcErr != nil {
		resp.Error = rpcErr
	} else {
		resp.Result = result
	}
	_ = json.NewEncoder(w).Encode(resp)
}

// handleSSE streams server-sent events for real-time MCP subscriptions.
func (s *Server) handleSSE(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "SSE not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	// Send initial endpoint event per MCP Streamable HTTP spec.
	writeSSE(w, flusher, "endpoint", `{"jsonrpc":"2.0","method":"notifications/initialized","params":{}}`)

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			if s.svcGraph == nil {
				continue
			}
			snap := s.svcGraph.Snapshot()
			data, err := json.Marshal(snap)
			if err != nil {
				continue
			}
			notification := map[string]any{
				"jsonrpc": "2.0",
				"method":  "notifications/resources/updated",
				"params": map[string]any{
					"uri":  "OtelContext://system/graph",
					"data": string(data),
				},
			}
			notifData, _ := json.Marshal(notification)
			writeSSE(w, flusher, "message", string(notifData))
		}
	}
}

// writeSSE writes a single SSE event.
func writeSSE(w http.ResponseWriter, f http.Flusher, event, data string) {
	data = strings.ReplaceAll(data, "\n", "\ndata: ")
	_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
	f.Flush()
}

// writeError writes a JSON-RPC error response.
func writeError(w http.ResponseWriter, id any, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	resp := JSONRPCResponse{
		JSONRPC: "2.0",
		ID:      id,
		Error:   &RPCError{Code: code, Message: msg},
	}
	_ = json.NewEncoder(w).Encode(resp)
}

// parseToolCallParams flexibly parses the params field of a tools/call request.
func parseToolCallParams(raw any) (ToolCallParams, bool) {
	if raw == nil {
		return ToolCallParams{}, false
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return ToolCallParams{}, false
	}
	var p ToolCallParams
	if err := json.Unmarshal(b, &p); err != nil {
		return ToolCallParams{}, false
	}
	return p, true
}
