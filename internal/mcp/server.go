package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
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

	// defaultMaxConcurrentCalls bounds the number of in-flight tools/call
	// invocations across the whole MCP endpoint. Beyond this, tools/call
	// returns the "server overloaded" RPC error so the client backs off
	// rather than piling pressure on the DB / GraphRAG.
	defaultMaxConcurrentCalls = 32

	// defaultCallTimeout is the per-invocation deadline applied to every
	// tools/call. Beyond this the handler returns an RPC error and frees
	// its concurrency slot — the goroutine still runs to completion in
	// the background but its result is not returned to the client.
	defaultCallTimeout = 30 * time.Second

	// defaultCacheTTL is the lifetime of a memoized tool result. Short
	// enough that observability lag is imperceptible; long enough to
	// absorb tight polling loops from agent clients.
	defaultCacheTTL = 5 * time.Second

	// sseHeartbeatInterval is the cadence of the SSE keep-alive comment
	// we send so reverse proxies (nginx, Envoy, Istio) don't time out
	// idle connections. 25s sits comfortably under the typical 30-60s
	// idle timeout these proxies default to.
	sseHeartbeatInterval = 25 * time.Second

	// ErrServerOverloaded is the JSON-RPC error code we surface when the
	// server-wide concurrency cap is exceeded. JSON-RPC reserves -32000
	// to -32099 for server errors; we pick a stable code in that band so
	// agent clients can detect-and-back-off deterministically.
	ErrServerOverloaded = -32000
	// ErrCallTimeout is the JSON-RPC error code returned when a tool
	// invocation runs past defaultCallTimeout.
	ErrCallTimeout = -32001
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

	// callSlots is a counting-semaphore implemented as a buffered channel:
	// buffer size is the max concurrent tools/call invocations. A non-
	// blocking send acquires a slot, a receive on defer releases it.
	// nil-valued (no cap) when SetCallLimit is given a value <= 0.
	callSlots chan struct{}
	// callTimeout is applied as a context deadline to every tools/call.
	callTimeout time.Duration
	// cache memoizes results for a whitelist of cheap GraphRAG tools.
	cache *resultCache

	// inFlight is a live counter exposed via Stats() for tests / metrics.
	inFlight atomic.Int64
	// counters bump on each outcome — also exposed for tests/metrics.
	cacheHits     atomic.Int64
	overloaded    atomic.Int64
	timedOut      atomic.Int64
	callsServiced atomic.Int64
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
		callSlots:     make(chan struct{}, defaultMaxConcurrentCalls),
		callTimeout:   defaultCallTimeout,
		cache:         newResultCache(defaultCacheTTL, 4096),
	}
}

// SetCallLimit configures the maximum number of concurrent tools/call
// invocations. <= 0 disables the cap (legacy behavior). Subsequent calls
// resize the underlying semaphore — be aware that an in-flight call holds
// a slot of the previous size; the new size only governs new acquisitions.
func (s *Server) SetCallLimit(maxConcurrent int) {
	if maxConcurrent <= 0 {
		s.callSlots = nil
		return
	}
	s.callSlots = make(chan struct{}, maxConcurrent)
}

// SetCallTimeout overrides the per-invocation deadline. A zero or negative
// value disables the timeout (handlers run until they return on their own).
func (s *Server) SetCallTimeout(d time.Duration) {
	s.callTimeout = d
}

// SetCacheTTL overrides the result-cache lifetime. <= 0 disables caching
// for the whitelisted GraphRAG tools.
func (s *Server) SetCacheTTL(d time.Duration) {
	if d <= 0 {
		s.cache = newResultCache(0, 0)
		return
	}
	s.cache = newResultCache(d, 4096)
}

// Stats returns counters used by tests and observability.
type Stats struct {
	InFlight      int64
	CallsServiced int64
	CacheHits     int64
	Overloaded    int64
	TimedOut      int64
	CacheSize     int
}

// Stats returns a snapshot of the server-wide counters. Safe to call
// from any goroutine; values are best-effort point-in-time.
func (s *Server) Stats() Stats {
	return Stats{
		InFlight:      s.inFlight.Load(),
		CallsServiced: s.callsServiced.Load(),
		CacheHits:     s.cacheHits.Load(),
		Overloaded:    s.overloaded.Load(),
		TimedOut:      s.timedOut.Load(),
		CacheSize:     s.cache.Stats(),
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

		// Cache fast-path: cheap, idempotent GraphRAG tools are memoized
		// for a few seconds so polling agent clients don't cripple the
		// in-memory store under load.
		if cached, hit := s.cache.Get(tenant, params.Name, params.Arguments); hit {
			s.cacheHits.Add(1)
			result = cached
			break
		}

		// Concurrency gate: non-blocking acquire. Beyond the cap we surface
		// a JSON-RPC server-overloaded error; clients are expected to retry
		// with backoff.
		if s.callSlots != nil {
			select {
			case s.callSlots <- struct{}{}:
				// acquired
			default:
				s.overloaded.Add(1)
				rpcErr = &RPCError{Code: ErrServerOverloaded, Message: "MCP server at capacity, retry shortly"}
				break
			}
		}
		// rpcErr was set inside the select-default; if so, skip the call.
		if rpcErr != nil {
			break
		}

		s.inFlight.Add(1)
		callCtx, cancel := s.deriveCallCtx(r.Context())
		callCtx = storage.WithTenantContext(callCtx, tenant)
		toolResult, timedOut := s.runWithTimeout(callCtx, cancel, params.Name, params.Arguments)
		if s.callSlots != nil {
			<-s.callSlots
		}
		s.inFlight.Add(-1)
		if timedOut {
			s.timedOut.Add(1)
			rpcErr = &RPCError{Code: ErrCallTimeout, Message: fmt.Sprintf("tool %q exceeded %s deadline", params.Name, s.callTimeout)}
			break
		}
		s.callsServiced.Add(1)
		s.cache.Set(tenant, params.Name, params.Arguments, toolResult)
		result = toolResult

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
	// Heartbeat keeps the SSE connection alive across reverse-proxy idle
	// timeouts (typical 30-60s on nginx / Envoy / Istio). Without a
	// periodic byte on the wire, the proxy closes the stream and clients
	// see "connection reset" mid-session — the textbook MCP HTTP
	// streamable failure mode under low-update-rate workloads.
	heartbeat := time.NewTicker(sseHeartbeatInterval)
	defer heartbeat.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-heartbeat.C:
			// SSE comments (lines starting with `:`) are valid heartbeats —
			// the spec defines them as ignored content, but they reset
			// proxy idle timers.
			_, _ = fmt.Fprintf(w, ": keep-alive\n\n")
			flusher.Flush()
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

// deriveCallCtx builds a per-call context, attaching a deadline when
// callTimeout > 0. The returned cancel must always be invoked once the
// call returns to release timer resources, even on the no-timeout path.
func (s *Server) deriveCallCtx(parent context.Context) (context.Context, context.CancelFunc) {
	if s.callTimeout <= 0 {
		return context.WithCancel(parent)
	}
	return context.WithTimeout(parent, s.callTimeout)
}

// runWithTimeout invokes toolHandler with the derived context and returns
// the result along with a timed-out flag. We always run the tool on a
// goroutine so that a slow handler can be aborted (its goroutine still
// runs to completion in the background — toolHandler itself respects the
// ctx through GORM and time.AfterFunc, so the work eventually winds
// down). cancel is the CancelFunc returned by deriveCallCtx.
func (s *Server) runWithTimeout(ctx context.Context, cancel context.CancelFunc, name string, args map[string]any) (ToolCallResult, bool) {
	defer cancel()
	type out struct{ res ToolCallResult }
	done := make(chan out, 1)
	go func() {
		done <- out{res: s.toolHandler(ctx, name, args)}
	}()
	select {
	case o := <-done:
		return o.res, false
	case <-ctx.Done():
		return ToolCallResult{}, true
	}
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
