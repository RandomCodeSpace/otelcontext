package mcp

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/RandomCodeSpace/argus/internal/graph"
	"github.com/RandomCodeSpace/argus/internal/storage"
	"github.com/RandomCodeSpace/argus/internal/telemetry"
	"github.com/RandomCodeSpace/argus/internal/vectordb"
)

const (
	mcpProtocolVersion = "2024-11-05"
	serverName         = "argus-mcp"
	serverVersion      = "1.0.0"
)

// Server is the HTTP Streamable MCP server.
// POST /mcp  — JSON-RPC 2.0 request/response
// GET  /mcp  — SSE stream for real-time notifications (optional subscription)
type Server struct {
	repo      *storage.Repository
	metrics   *telemetry.Metrics
	svcGraph  *graph.Graph
	vectorIdx *vectordb.Index
}

// New creates a new MCP server.
func New(
	repo *storage.Repository,
	metrics *telemetry.Metrics,
	svcGraph *graph.Graph,
	vectorIdx *vectordb.Index,
) *Server {
	return &Server{
		repo:      repo,
		metrics:   metrics,
		svcGraph:  svcGraph,
		vectorIdx: vectorIdx,
	}
}

// Handler returns an http.Handler that serves both POST (RPC) and GET (SSE).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /mcp", s.handleRPC)
	mux.HandleFunc("GET /mcp", s.handleSSE)
	// Also accept at root of sub-handler so callers can mount at any path.
	mux.HandleFunc("POST /", s.handleRPC)
	mux.HandleFunc("GET /", s.handleSSE)
	return mux
}

// handleRPC processes JSON-RPC 2.0 requests.
func (s *Server) handleRPC(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")

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

	var result interface{}
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

	case "tools/list":
		result = ToolsListResult{Tools: toolDefs}

	case "tools/call":
		params, ok := parseToolCallParams(req.Params)
		if !ok {
			rpcErr = &RPCError{Code: ErrInvalidParams, Message: "invalid tools/call params"}
			break
		}
		callResult := s.toolHandler(params.Name, params.Arguments)
		result = callResult

	case "ping":
		result = map[string]string{"status": "ok", "ts": time.Now().UTC().Format(time.RFC3339)}

	case "resources/list":
		result = map[string]any{
			"resources": []map[string]any{
				{"uri": "argus://system/graph", "name": "System Graph", "mimeType": "application/json"},
				{"uri": "argus://metrics/prometheus", "name": "Prometheus Metrics", "mimeType": "text/plain"},
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
	json.NewEncoder(w).Encode(resp)
}

// handleSSE streams server-sent events for real-time MCP subscriptions.
// Clients can GET /mcp to receive periodic system graph snapshots.
func (s *Server) handleSSE(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "SSE not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	// Send initial endpoint event (MCP Streamable HTTP spec).
	writeSSE(w, flusher, "endpoint", `{"type":"endpoint","capabilities":{"tools":{}}}`)

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
					"uri":  "argus://system/graph",
					"data": string(data),
				},
			}
			notifData, _ := json.Marshal(notification)
			writeSSE(w, flusher, "message", string(notifData))
		}
	}
}

// writeSSE writes a single SSE event to the response.
func writeSSE(w http.ResponseWriter, f http.Flusher, event, data string) {
	// Escape newlines in data per SSE spec.
	data = strings.ReplaceAll(data, "\n", "\ndata: ")
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
	f.Flush()
}

// writeError writes a JSON-RPC error response.
func writeError(w http.ResponseWriter, id interface{}, code int, msg string) {
	resp := JSONRPCResponse{
		JSONRPC: "2.0",
		ID:      id,
		Error:   &RPCError{Code: code, Message: msg},
	}
	json.NewEncoder(w).Encode(resp)
}

// parseToolCallParams flexibly parses the params field of a tools/call request.
func parseToolCallParams(raw interface{}) (ToolCallParams, bool) {
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
