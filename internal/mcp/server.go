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

	"github.com/RandomCodeSpace/otelcontext/internal/authn"
	"github.com/RandomCodeSpace/otelcontext/internal/graph"
	"github.com/RandomCodeSpace/otelcontext/internal/graphrag"
	"github.com/RandomCodeSpace/otelcontext/internal/httpconst"
	"github.com/RandomCodeSpace/otelcontext/internal/storage"
	"github.com/RandomCodeSpace/otelcontext/internal/telemetry"
	"github.com/RandomCodeSpace/otelcontext/internal/topology"
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
	topology      topology.Provider
	graphRAG      *graphrag.GraphRAG
	defaultTenant string

	// aggregateMode makes every successful tool result carry coverage body
	// metadata. Set only in AGGREGATE_MODE=aggregate; legacy and shadow
	// responses stay byte-for-byte what they were.
	aggregateMode bool

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
//
// The vectordb-backed semantic similarity argument was removed on 2026-05-24
// when find_similar_logs was cut from the MCP surface and the vectordb package
// was deleted.
func New(
	defaultTenant string,
	repo *storage.Repository,
	metrics *telemetry.Metrics,
	topologyProvider topology.Provider,
) *Server {
	if defaultTenant == "" {
		defaultTenant = storage.DefaultTenantID
	}
	s := &Server{
		repo:          repo,
		metrics:       metrics,
		topology:      topologyProvider,
		defaultTenant: defaultTenant,
		callSlots:     make(chan struct{}, defaultMaxConcurrentCalls),
		callTimeout:   defaultCallTimeout,
		cache:         newResultCache(defaultCacheTTL, 4096),
	}
	metrics.RegisterReadCache("mcp_result", s.cache.Stats)
	return s
}

// SetCallLimit configures the maximum number of concurrent tools/call
// invocations. <= 0 disables the cap (legacy behavior).
//
// Startup-only: this swaps the underlying channel reference without
// quiescing in-flight callers. An already-running call will release into
// the OLD channel when it completes, leaving the NEW semaphore one slot
// short until process restart. Call exactly once during construction
// (main.go does); never from a request-handling goroutine.
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

// SetAggregateMode makes tool results carry coverage body metadata. Call it
// with true only in AGGREGATE_MODE=aggregate.
func (s *Server) SetAggregateMode(on bool) {
	s.aggregateMode = on
}

// Handler returns an http.Handler for the MCP server with CORS applied.
// Works correctly when mounted with http.StripPrefix.
func (s *Server) Handler() http.Handler {
	return corsMiddleware("*", http.HandlerFunc(s.ServeHTTP))
}

// corsMiddleware wraps next with permissive CORS headers so MCP clients
// running in a browser (or any cross-origin caller) can hit /mcp. Allows
// only the verbs and request headers the MCP transport actually uses;
// preflight short-circuits with 204. Inlined here to avoid pulling a
// private helper module just for one ~10-line middleware.
func corsMiddleware(origin string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Access-Control-Allow-Origin", origin)
		h.Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		h.Set("Access-Control-Allow-Headers", "Content-Type, Authorization, Accept, "+mcpTenantHeader+", Mcp-Session-Id")
		h.Set("Access-Control-Expose-Headers", "Mcp-Session-Id")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
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
	w.Header().Set(httpconst.HeaderContentType, httpconst.ContentTypeJSON)

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
		// Downstream tool handlers pull the tenant off ctx via mcpCtx(r.Context()).
		tenant := s.requestTenant(r)
		cacheTenant := s.topologyCacheScope(r.Context(), tenant, params.Name)

		// Cache fast-path: cheap, idempotent GraphRAG tools are memoized
		// for a few seconds so polling agent clients don't cripple the
		// in-memory store under load.
		if cached, hit := s.cache.Get(cacheTenant, params.Name, params.Arguments); hit {
			s.cacheHits.Add(1)
			result = cached
			break
		}

		// Concurrency gate: non-blocking acquire. Use an `acquired` flag
		// rather than a `break` inside `select{default}` (which only breaks
		// the select, not the surrounding switch — refactor footgun).
		acquired := s.callSlots == nil
		if !acquired {
			select {
			case s.callSlots <- struct{}{}:
				acquired = true
			default:
				s.overloaded.Add(1)
				rpcErr = &RPCError{Code: ErrServerOverloaded, Message: "MCP server at capacity, retry shortly"}
			}
		}
		if !acquired {
			break
		}

		s.inFlight.Add(1)
		callCtx, cancel := s.deriveCallCtx(r.Context())
		callCtx = storage.WithTenantContext(callCtx, tenant)
		// release fires when the inner goroutine finishes — not when the
		// HTTP request returns. On timeout the request returns immediately
		// with ErrCallTimeout but the slot stays held until the runaway
		// tool actually completes, which is what defends the concurrency
		// cap from being defeated by slow handlers.
		release := func() {
			if s.callSlots != nil {
				<-s.callSlots
			}
			s.inFlight.Add(-1)
		}
		toolResult, timedOut := s.runWithTimeout(callCtx, cancel, params.Name, params.Arguments, release)
		if timedOut {
			s.timedOut.Add(1)
			rpcErr = &RPCError{Code: ErrCallTimeout, Message: fmt.Sprintf("tool %q exceeded %s deadline", params.Name, s.callTimeout)}
			break
		}
		s.callsServiced.Add(1)
		if !toolResult.IsError {
			s.cache.Set(cacheTenant, params.Name, params.Arguments, toolResult)
		}
		result = toolResult

	case "ping":
		result = map[string]string{"status": "ok", "ts": time.Now().UTC().Format(time.RFC3339)}

	case "resources/list":
		result = map[string]any{
			"resources": []map[string]any{
				{"uri": "OtelContext://system/graph", "name": "System Graph", "mimeType": httpconst.ContentTypeJSON},
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
	ctx := storage.WithTenantContext(r.Context(), s.requestTenant(r))
	lastIdentity := topology.Identity{}
	haveIdentity := false
	if data, identity, ok := s.topologyNotification(ctx, lastIdentity, haveIdentity); ok {
		writeSSE(w, flusher, "message", data)
		lastIdentity, haveIdentity = identity, true
	}

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
			if s.topology == nil {
				continue
			}
			if s.topology.Source() == topology.SourceAggregate {
				identity := s.topology.Identity(ctx)
				if haveIdentity && identity.Epoch == lastIdentity.Epoch && identity.Revision == lastIdentity.Revision {
					continue
				}
				if haveIdentity && identity.Epoch == lastIdentity.Epoch && identity.Revision < lastIdentity.Revision {
					continue
				}
			}
			data, identity, ok := s.topologyNotification(ctx, lastIdentity, haveIdentity)
			if !ok {
				continue
			}
			writeSSE(w, flusher, "message", data)
			lastIdentity, haveIdentity = identity, true
		}
	}
}

type mcpTopologyPayload struct {
	Nodes     map[string]*graph.ServiceNode
	Edges     []graph.ServiceEdge
	UpdatedAt time.Time

	Source       string `json:"source,omitempty"`
	Coverage     string `json:"coverage,omitempty"`
	CoverageNote string `json:"coverage_note,omitempty"`
	Epoch        string `json:"epoch,omitempty"`
	Revision     uint64 `json:"revision,omitempty"`
	Reset        bool   `json:"reset,omitempty"`
	Truncated    bool   `json:"truncated,omitempty"`

	DroppedServices   uint64 `json:"dropped_services,omitempty"`
	DroppedOperations uint64 `json:"dropped_operations,omitempty"`
	DroppedEdges      uint64 `json:"dropped_edges,omitempty"`
	DroppedMetrics    uint64 `json:"dropped_metrics,omitempty"`
}

func (s *Server) topologyNotification(ctx context.Context, last topology.Identity, haveLast bool) (string, topology.Identity, bool) {
	if s.topology == nil {
		return "", topology.Identity{}, false
	}
	snapshot, err := s.topology.Snapshot(ctx, topology.Query{})
	if err != nil {
		slog.Debug("MCP topology update retained last good state", "error", err)
		return "", topology.Identity{}, false
	}
	identity := snapshot.Meta.Identity()
	if identity.Epoch == "" && s.topology.Source() == topology.SourceAggregate {
		identity = s.topology.Identity(ctx)
	}
	if haveLast && identity.Epoch == last.Epoch && identity.Revision < last.Revision {
		return "", topology.Identity{}, false
	}
	payload := mcpTopologyPayload{
		Nodes:             make(map[string]*graph.ServiceNode, len(snapshot.Nodes)),
		Edges:             make([]graph.ServiceEdge, 0, len(snapshot.Edges)),
		UpdatedAt:         snapshot.Meta.End,
		Source:            string(snapshot.Meta.Source),
		Coverage:          snapshot.Meta.Coverage,
		CoverageNote:      snapshot.Meta.CoverageNote,
		Epoch:             identity.Epoch,
		Revision:          identity.Revision,
		Reset:             !haveLast || identity.Epoch != last.Epoch,
		Truncated:         snapshot.Meta.Truncated,
		DroppedServices:   snapshot.Meta.DroppedServices,
		DroppedOperations: snapshot.Meta.DroppedOperations,
		DroppedEdges:      snapshot.Meta.DroppedEdges,
		DroppedMetrics:    snapshot.Meta.DroppedMetrics,
	}
	if payload.UpdatedAt.IsZero() {
		payload.UpdatedAt = time.Now().UTC()
	}
	for _, node := range snapshot.Nodes {
		alerts := append([]string(nil), node.Alerts...)
		if alerts == nil {
			alerts = []string{}
		}
		payload.Nodes[node.Name] = &graph.ServiceNode{
			Name:              node.Name,
			HealthScore:       node.HealthScore,
			Status:            node.Status,
			RequestRateRPS:    node.RequestRateRPS,
			ErrorRate:         node.ErrorRate,
			AvgLatencyMs:      node.AvgLatencyMs,
			P99LatencyMs:      node.P99LatencyMs,
			LatencyProvenance: node.LatencyProvenance,
			SpanCount:         node.SpanCount,
			Alerts:            alerts,
		}
	}
	for _, edge := range snapshot.Edges {
		payload.Edges = append(payload.Edges, graph.ServiceEdge{
			Source:       edge.Source,
			Target:       edge.Target,
			CallCount:    edge.CallCount,
			AvgLatencyMs: edge.AvgLatencyMs,
			ErrorRate:    edge.ErrorRate,
			Status:       edge.Status,
		})
	}
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return "", topology.Identity{}, false
	}
	notification := map[string]any{
		"jsonrpc": "2.0",
		"method":  "notifications/resources/updated",
		"params": map[string]any{
			"uri":  "OtelContext://system/graph",
			"data": string(payloadJSON),
		},
	}
	notificationJSON, err := json.Marshal(notification)
	if err != nil {
		return "", topology.Identity{}, false
	}
	return string(notificationJSON), identity, true
}

// requestTenant resolves the tenant an MCP request runs under. A bound
// principal (tenant key or trusted external identity, stashed on the context
// by the HTTP auth gate) pins the tenant: a contradicting X-Tenant-ID is
// ignored and counted on the auth conflict counter. Operator and
// unauthenticated requests keep the header-then-default precedence.
func (s *Server) requestTenant(r *http.Request) string {
	asserted := strings.TrimSpace(r.Header.Get(mcpTenantHeader))
	if bound, ok := authn.BoundTenantFromContext(r.Context()); ok {
		if asserted != "" && storage.SanitizeTenantID(asserted) != bound {
			authn.RecordConflict("mcp", "header")
		}
		return bound
	}
	if asserted == "" {
		return s.defaultTenant
	}
	return asserted
}

func (s *Server) topologyCacheScope(ctx context.Context, tenant, tool string) string {
	if s.topology == nil || s.topology.Source() != topology.SourceAggregate ||
		(tool != "get_service_map" && tool != "get_service_health") {
		return tenant
	}
	ctx = storage.WithTenantContext(ctx, tenant)
	return tenant + "\x00topology=" + s.topology.Identity(ctx).String()
}

// writeSSE writes a single SSE event.
func writeSSE(w http.ResponseWriter, f http.Flusher, event, data string) {
	data = strings.ReplaceAll(data, "\n", "\ndata: ")
	_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
	f.Flush()
}

// writeError writes a JSON-RPC error response.
func writeError(w http.ResponseWriter, id any, code int, msg string) {
	w.Header().Set(httpconst.HeaderContentType, httpconst.ContentTypeJSON)
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

// runWithTimeout invokes toolHandler with the derived context. The release
// callback fires AFTER the inner goroutine returns — including on timeout
// where the request thread has already given up. This keeps the
// concurrency-cap slot held until the runaway handler actually completes,
// which is the whole point of the cap.
//
// cancel is the CancelFunc from deriveCallCtx; we own its lifecycle so
// we always invoke it when the goroutine exits (idempotent if already
// fired by the deadline).
func (s *Server) runWithTimeout(ctx context.Context, cancel context.CancelFunc, name string, args map[string]any, release func()) (ToolCallResult, bool) {
	type out struct{ res ToolCallResult }
	done := make(chan out, 1)
	go func() {
		defer cancel()
		if release != nil {
			defer release()
		}
		done <- out{res: s.toolHandler(ctx, name, args)}
	}()
	select {
	case o := <-done:
		return o.res, false
	case <-ctx.Done():
		// The handler goroutine cancels ctx (deferred) AFTER sending its
		// result, so a finished handler can leave BOTH channels ready and
		// select picks randomly — a queued result must win over a spurious
		// timeout. Only an empty done channel is a real deadline overrun
		// (goroutine still running; the deferred release() frees the slot
		// whenever it finishes).
		select {
		case o := <-done:
			return o.res, false
		default:
			return ToolCallResult{}, true
		}
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
