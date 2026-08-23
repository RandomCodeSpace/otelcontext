package realtime

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/authn"
	"github.com/RandomCodeSpace/otelcontext/internal/storage"
	"github.com/coder/websocket"
)

// LogEntry is a lightweight struct for WebSocket broadcast payloads.
//
// Tenant is transport-only: it decides which sockets may see the entry and is
// never serialized, so the wire payload is unchanged from before per-tenant
// scoping existed.
type LogEntry struct {
	Tenant         string    `json:"-"`
	ID             uint      `json:"id"`
	TraceID        string    `json:"trace_id"`
	SpanID         string    `json:"span_id"`
	Severity       string    `json:"severity"`
	Body           string    `json:"body"`
	ServiceName    string    `json:"service_name"`
	AttributesJSON string    `json:"attributes_json"`
	AIInsight      string    `json:"ai_insight,omitempty"`
	Timestamp      time.Time `json:"timestamp"`
}

// MetricEntry represents a raw metric point for real-time visualization.
// Tenant is transport-only — see LogEntry.
type MetricEntry struct {
	Tenant      string         `json:"-"`
	Name        string         `json:"name"`
	ServiceName string         `json:"service_name"`
	Value       float64        `json:"value"`
	Timestamp   time.Time      `json:"timestamp"`
	Attributes  map[string]any `json:"attributes"`
}

// tenantTagged is implemented by every entry type that can be delivered to a
// tenant-scoped socket. It exists so log and metric fan-out share one
// filtering implementation instead of two that can drift apart.
type tenantTagged interface {
	tenantID() string
}

func (l LogEntry) tenantID() string    { return l.Tenant }
func (m MetricEntry) tenantID() string { return m.Tenant }

// HubBatch is a unified payload for WebSocket broadcasts.
type HubBatch struct {
	Type string `json:"type"` // "logs" or "metrics"
	Data any    `json:"data"` // Slice of entries
}

// Hub is a buffered WebSocket broadcast hub.
//
// Instead of broadcasting each log individually (which would freeze the UI at high throughput),
// it buffers logs and flushes them as a JSON array when either:
//   - Buffer size >= maxBufferSize (default: 100)
//   - Flush ticker fires (default: every 500ms)
type Hub struct {
	clients    map[*client]struct{}
	register   chan *client
	unregister chan *client
	broadcast  chan LogEntry
	metricsCh  chan MetricEntry

	logBuffer     []LogEntry
	metricBuffer  []MetricEntry
	bufferMu      sync.Mutex
	maxBufferSize int
	flushInterval time.Duration

	// maxClients caps simultaneous WebSocket connections. 0 = unlimited
	// (legacy). When set, HandleWebSocket rejects new connects past the cap
	// with HTTP 503 instead of admitting unbounded clients that would
	// exhaust file descriptors and per-client send-channel memory.
	maxClients  int
	clientCount atomic.Int64

	// aggregateMode disables per-event broadcasts. In aggregate mode the raw
	// log/metric stream is no longer the source of truth for the UI, and
	// re-broadcasting every event would contradict the coalesced,
	// revision-driven publication the event hub performs (#164).
	aggregateMode atomic.Bool

	stopCh chan struct{}
	// runOwner is claimed exactly once, by whichever of Run or Stop gets there
	// first. It balances the wg count taken in NewHub: Run consumes it when it
	// starts the loop, Stop consumes it when the hub is stopped without ever
	// having been run. Taking the count at construction is what makes
	// wg.Add happen-before wg.Wait, which `go hub.Run()` cannot guarantee.
	runOwner atomic.Bool
	// lifecycleMu serialises writerWg.Add against Stop's writerWg.Wait.
	// Without it the handler can Add from zero while Stop is already
	// waiting, which is WaitGroup misuse and a real data race.
	lifecycleMu sync.Mutex
	closing     bool
	wg          sync.WaitGroup
	writerWg    sync.WaitGroup // tracks writer goroutines
	devMode     bool

	// enforceOrigin and originHosts implement WS_ALLOWED_ORIGINS. Enforcement
	// is on whenever authentication is enabled or APP_ENV=production; the
	// empty host list then means same-host only.
	enforceOrigin bool
	originHosts   []string

	// onConnectionChange is called when the number of active connections changes.
	onConnectionChange func(count int)

	// Metric callbacks (optional)
	onMessageSent    func(msgType string) // WSMessagesSent.WithLabelValues(type).Inc()
	onSlowClientDrop func()               // WSSlowClientsRemoved.Inc()

	logPool    sync.Pool
	metricPool sync.Pool
}

// client represents a single WebSocket connection.
type client struct {
	conn *websocket.Conn
	send chan []byte
	// tenant is the single tenant this socket is scoped to, resolved from the
	// authenticated principal at handshake time. Empty means unscoped, which
	// only happens when authentication is not configured (development) — a
	// scoped socket never receives another tenant's entries.
	tenant string
	closed atomic.Bool // guards against double-close of send channel
}

// NewHub creates a new buffered WebSocket hub.
func NewHub(onConnectionChange func(count int)) *Hub {
	h := &Hub{
		clients:            make(map[*client]struct{}),
		register:           make(chan *client),
		unregister:         make(chan *client),
		broadcast:          make(chan LogEntry, 5000),
		metricsCh:          make(chan MetricEntry, 5000),
		maxBufferSize:      100,
		flushInterval:      500 * time.Millisecond,
		stopCh:             make(chan struct{}),
		onConnectionChange: onConnectionChange,
	}

	h.logPool.New = func() any {
		return make([]LogEntry, 0, h.maxBufferSize)
	}
	h.metricPool.New = func() any {
		return make([]MetricEntry, 0, h.maxBufferSize)
	}

	h.logBuffer = h.logPool.Get().([]LogEntry)
	h.metricBuffer = h.metricPool.Get().([]MetricEntry)

	// Balanced by Run (loop exit) or by Stop (hub never run). See runOwner.
	h.wg.Add(1)

	return h
}

// Run starts the hub's main event loop. Should be called in a goroutine.
func (h *Hub) Run() {
	if !h.runOwner.CompareAndSwap(false, true) {
		// Stop already released the construction-time count, or Run was
		// called twice. Either way there is no loop to start.
		return
	}
	defer h.wg.Done()

	flushTicker := time.NewTicker(h.flushInterval)
	defer flushTicker.Stop()

	for {
		select {
		case <-h.stopCh:
			h.flush()
			// Close every client's send channel so the writer goroutines
			// (blocked on `for msg := range c.send`) wake up and exit.
			// Without this, writerWg.Wait() in Stop() hangs whenever any
			// connected client is idle. CAS guard mirrors the unregister
			// handler so concurrent close paths can't double-close.
			for c := range h.clients {
				if c.closed.CompareAndSwap(false, true) {
					close(c.send)
				}
			}
			return

		case c := <-h.register:
			h.clients[c] = struct{}{}
			slog.Info("🔌 WebSocket client connected", "total", len(h.clients))
			if h.onConnectionChange != nil {
				h.onConnectionChange(len(h.clients))
			}

		case c := <-h.unregister:
			if _, ok := h.clients[c]; ok {
				delete(h.clients, c)
				if c.closed.CompareAndSwap(false, true) {
					close(c.send)
				}
				slog.Info("🔌 WebSocket client disconnected", "total", len(h.clients))
				if h.onConnectionChange != nil {
					h.onConnectionChange(len(h.clients))
				}
			}

		case entry := <-h.broadcast:
			h.bufferMu.Lock()
			h.logBuffer = append(h.logBuffer, entry)
			shouldFlush := len(h.logBuffer) >= h.maxBufferSize
			h.bufferMu.Unlock()

			if shouldFlush {
				h.flush()
			}

		case metric := <-h.metricsCh:
			h.bufferMu.Lock()
			h.metricBuffer = append(h.metricBuffer, metric)
			shouldFlush := len(h.metricBuffer) >= h.maxBufferSize
			h.bufferMu.Unlock()

			if shouldFlush {
				h.flush()
			}

		case <-flushTicker.C:
			h.flush()
		}
	}
}

// flush sends the buffered logs and metrics as JSON batches to all connected clients.
func (h *Hub) flush() {
	h.bufferMu.Lock()
	if len(h.logBuffer) == 0 && len(h.metricBuffer) == 0 {
		h.bufferMu.Unlock()
		return
	}

	// Swap buffers
	logBatch := h.logBuffer
	h.logBuffer = h.logPool.Get().([]LogEntry)

	metricBatch := h.metricBuffer
	h.metricBuffer = h.metricPool.Get().([]MetricEntry)
	h.bufferMu.Unlock()

	// Broadcast Logs if any
	if len(logBatch) > 0 {
		broadcastTagged(h, "logs", logBatch)
		// Recycle logBatch
		logBatch = logBatch[:0]
		h.logPool.Put(logBatch) //nolint:staticcheck // SA6002: []T pool; pointer wrap would require broader refactor
	}

	// Broadcast Metrics if any
	if len(metricBatch) > 0 {
		broadcastTagged(h, "metrics", metricBatch)
		// Recycle metricBatch
		metricBatch = metricBatch[:0]
		h.metricPool.Put(metricBatch) //nolint:staticcheck // SA6002: []T pool; pointer wrap would require broader refactor
	}
}

// broadcastTagged fans a batch out to every connected client, filtered by the
// client's tenant scope. One marshal per distinct scope, not per client: the
// unscoped payload is built once and each tenant's filtered payload at most
// once, so a hundred dashboards on one tenant still cost one encode.
//
// A client with an empty scope (authentication not configured) keeps the
// pre-existing behaviour of receiving the whole batch.
func broadcastTagged[T tenantTagged](h *Hub, batchType string, entries []T) {
	cache := make(map[string][]byte, 4)
	sent := 0
	var slow []*client
	for c := range h.clients {
		msg, cached := cache[c.tenant]
		if !cached {
			msg = encodeForScope(batchType, c.tenant, entries)
			cache[c.tenant] = msg
		}
		if msg == nil {
			continue // nothing in this batch belongs to the client's tenant
		}
		select {
		case c.send <- msg:
			sent++
		default:
			slow = append(slow, c)
		}
	}
	for _, c := range slow {
		delete(h.clients, c)
		if c.closed.CompareAndSwap(false, true) {
			close(c.send)
		}
		slog.Warn("Hub: slow client removed", "total", len(h.clients))
		if h.onConnectionChange != nil {
			h.onConnectionChange(len(h.clients))
		}
		if h.onSlowClientDrop != nil {
			h.onSlowClientDrop()
		}
	}
	if sent > 0 && h.onMessageSent != nil {
		h.onMessageSent(batchType)
	}
}

// encodeForScope marshals the batch a single tenant scope may see. It returns
// nil when the scope has nothing to receive — entries carrying no tenant are
// invisible to a scoped socket (fail closed).
func encodeForScope[T tenantTagged](batchType, tenant string, entries []T) []byte {
	payload := entries
	if tenant != "" {
		filtered := make([]T, 0, len(entries))
		for _, e := range entries {
			if e.tenantID() == tenant {
				filtered = append(filtered, e)
			}
		}
		if len(filtered) == 0 {
			return nil
		}
		payload = filtered
	}
	data, err := json.Marshal(HubBatch{Type: batchType, Data: payload})
	if err != nil {
		slog.Error("Hub: failed to marshal batch", "error", err, "type", batchType)
		return nil
	}
	return data
}

// SetDevMode controls whether cross-origin WebSocket connections are accepted.
// Should be true only in development environments.
func (h *Hub) SetDevMode(devMode bool) {
	h.devMode = devMode
}

// SetOriginPolicy configures WebSocket origin enforcement. When enforce is
// true the browser Origin header must match one of allowedHosts, or the
// request host when the list is empty. Call once at startup.
func (h *Hub) SetOriginPolicy(enforce bool, allowedHosts []string) {
	h.enforceOrigin = enforce
	h.originHosts = append([]string(nil), allowedHosts...)
}

// SetMaxClients caps simultaneous WebSocket connections. 0 disables the cap
// (default). Configure once at startup before HandleWebSocket starts taking
// traffic — the cap is read concurrently from each upgrade attempt.
func (h *Hub) SetMaxClients(n int) {
	if n < 0 {
		n = 0
	}
	h.maxClients = n
}

// ActiveClients reports the count of currently-connected WebSocket clients.
// Updated atomically as connections are accepted and torn down.
func (h *Hub) ActiveClients() int64 { return h.clientCount.Load() }

// SetWSMetrics wires WebSocket metric callbacks.
func (h *Hub) SetWSMetrics(onMessageSent func(string), onSlowClientDrop func()) {
	h.onMessageSent = onMessageSent
	h.onSlowClientDrop = onSlowClientDrop
}

// SetAggregateMode disables per-event log and metric broadcasts. Keepalive and
// connection handling are unaffected.
func (h *Hub) SetAggregateMode(on bool) { h.aggregateMode.Store(on) }

// Broadcast adds a log entry to the broadcast buffer. No-op in aggregate mode.
func (h *Hub) Broadcast(entry LogEntry) {
	if h.aggregateMode.Load() {
		return
	}
	select {
	case h.broadcast <- entry:
	default:
		// Drop if internal channel is full
	}
}

// BroadcastMetric adds a metric entry to the broadcast buffer. No-op in
// aggregate mode.
func (h *Hub) BroadcastMetric(entry MetricEntry) {
	if h.aggregateMode.Load() {
		return
	}
	select {
	case h.metricsCh <- entry:
	default:
		// Drop if internal channel is full
	}
}

// admitWriter reserves a writer slot on writerWg, refusing once Stop has
// begun. Every writerWg.Add goes through here so each one is ordered before
// Stop's Wait by lifecycleMu.
func (h *Hub) admitWriter() bool {
	h.lifecycleMu.Lock()
	defer h.lifecycleMu.Unlock()
	if h.closing {
		return false
	}
	h.writerWg.Add(1)
	return true
}

// Stop gracefully shuts down the hub.
func (h *Hub) Stop() {
	h.lifecycleMu.Lock()
	h.closing = true
	h.lifecycleMu.Unlock()

	close(h.stopCh)
	if h.runOwner.CompareAndSwap(false, true) {
		// Run was never called, so nothing will ever call Done for the
		// construction-time count. Release it here or Wait blocks forever.
		h.wg.Done()
	}
	h.wg.Wait()
	h.writerWg.Wait()
	slog.Info("🛑 WebSocket hub stopped")
}

// HandleWebSocket is the HTTP handler that upgrades connections to WebSocket.
func (h *Hub) HandleWebSocket(w http.ResponseWriter, r *http.Request) {
	// Cap admission BEFORE the WebSocket upgrade so a flood of new clients
	// can't exhaust file descriptors / per-client send-channel memory.
	// CompareAndSwap-ish reservation: increment optimistically, roll back
	// if we exceeded the cap. Race-free because clientCount is atomic and
	// every cleanup path decrements it.
	if h.maxClients > 0 {
		if n := h.clientCount.Add(1); n > int64(h.maxClients) {
			h.clientCount.Add(-1)
			slog.Warn("WebSocket connection rejected: max-clients cap reached",
				"max_clients", h.maxClients,
				"current", n-1,
				"remote", r.RemoteAddr,
			)
			http.Error(w, "WebSocket connections at capacity, retry later", http.StatusServiceUnavailable)
			return
		}
	} else {
		h.clientCount.Add(1)
	}
	clientCounted := true
	releaseSlot := func() {
		if clientCounted {
			clientCounted = false
			h.clientCount.Add(-1)
		}
	}

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// Cross-origin is allowed in dev mode only, and never once the origin
		// policy is enforced (authenticated or production deployments).
		InsecureSkipVerify: h.devMode && !h.enforceOrigin,
		OriginPatterns:     h.originHosts,
		// Echo ONLY otelcontext.v1. A browser carrying a credential offers
		// `otelcontext.v1, auth.<base64url>`; selecting the first entry keeps
		// the token out of the negotiated protocol and out of every log line.
		Subprotocols: []string{authn.WSSubprotocol},
	})
	if err != nil {
		releaseSlot()
		slog.Error("WebSocket upgrade failed", "error", err)
		return
	}

	c := &client{
		conn:   conn,
		send:   make(chan []byte, 256),
		tenant: connTenantScope(r),
	}

	// Reserve the writer slot before the registration handshake so the
	// count is taken while the hub is provably still accepting.
	if !h.admitWriter() {
		releaseSlot()
		_ = conn.Close(websocket.StatusGoingAway, "server shutting down")
		return
	}

	// Registration races Stop(): once the run loop returns on stopCh nothing
	// drains h.register, so an unconditional send blocks this goroutine
	// forever and Stop()'s wg.Wait() never returns. Refuse the connection
	// instead of joining a hub that is going away.
	select {
	case h.register <- c:
	case <-h.stopCh:
		h.writerWg.Done()
		releaseSlot()
		_ = conn.Close(websocket.StatusGoingAway, "server shutting down")
		return
	}

	// Writer goroutine
	go func() { // #nosec G118 -- long-lived WS writer goroutine outlives HTTP request intentionally
		defer h.writerWg.Done()
		// Release the admission slot when the writer exits — the writer
		// outlives the HandleWebSocket reader loop, so this is the last
		// goroutine alive for this client.
		defer releaseSlot()
		defer func() {
			// An unconditional send here is what wedged Stop(): the run loop
			// returns on stopCh and then nothing drains h.unregister, so this
			// goroutine blocks forever and writerWg.Wait() never returns.
			// Select on stopCh so the send is abandoned the moment the drain
			// side is gone; the CAS guard makes the direct close safe against
			// the run loop's own stop-path close.
			select {
			case h.unregister <- c:
			case <-h.stopCh:
				if c.closed.CompareAndSwap(false, true) {
					close(c.send)
				}
			}
			_ = conn.Close(websocket.StatusNormalClosure, "closing")
		}()

		for msg := range c.send {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			err := conn.Write(ctx, websocket.MessageText, msg)
			cancel()
			if err != nil {
				slog.Debug("WebSocket write failed", "error", err)
				return
			}
		}
	}()

	// Reader goroutine — keeps connection alive, handles close.
	// Use request context so the read unblocks when the connection drops.
	for {
		_, _, err := conn.Read(r.Context())
		if err != nil {
			break
		}
	}
	// Force the writer goroutine to exit once the conn is dead, otherwise
	// it stays blocked on `for msg := range c.send` until the next broadcast
	// happens to be selected for this client — which leaks the admission
	// slot and the goroutine indefinitely under low traffic. CAS guard
	// mirrors every other close site.
	if c.closed.CompareAndSwap(false, true) {
		close(c.send)
	}
}

// connTenantScope reads the tenant the handshake gate pinned onto the request
// context. Empty means the socket is unscoped, which happens only when
// authentication is not configured — the gate always pins exactly one tenant.
func connTenantScope(r *http.Request) string {
	if r == nil || !storage.HasTenantContext(r.Context()) {
		return ""
	}
	return storage.TenantFromContext(r.Context())
}
