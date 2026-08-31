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
	"golang.org/x/sync/errgroup"
)

// LiveSnapshot is the data payload pushed to all event WS clients.
//
// The identity and coverage fields are ADDITIVE and only populated in
// aggregate mode; `omitempty` keeps the legacy payload unchanged.
//
// A client replaces state by (series, window_start, revision) and RESETS
// wholesale when Epoch changes: the revision counter restarts at zero on every
// process generation, so revision alone cannot be trusted across a restart.
type LiveSnapshot struct {
	Type       string                     `json:"type"`
	Dashboard  *storage.DashboardStats    `json:"dashboard"`
	Traffic    []storage.TrafficPoint     `json:"traffic"`
	Traces     *storage.TracesResponse    `json:"traces"`
	ServiceMap *storage.ServiceMapMetrics `json:"service_map"`

	Epoch        string `json:"epoch,omitempty"`
	Revision     uint64 `json:"revision,omitempty"`
	Reset        bool   `json:"reset,omitempty"`
	Coverage     string `json:"coverage,omitempty"`
	CoverageNote string `json:"coverage_note,omitempty"`
	Source       string `json:"source,omitempty"`
	Truncated    bool   `json:"truncated,omitempty"`

	DroppedServices   uint64 `json:"dropped_services,omitempty"`
	DroppedOperations uint64 `json:"dropped_operations,omitempty"`
	DroppedEdges      uint64 `json:"dropped_edges,omitempty"`
	DroppedMetrics    uint64 `json:"dropped_metrics,omitempty"`
}

// AggregatePublisher supplies revision-driven snapshots in aggregate mode. It
// is an interface so the hub needs neither the aggregate engine nor a live
// store to be tested.
type AggregatePublisher interface {
	// Epoch identifies the process generation of Revision.
	Epoch() string
	// Revision is the aggregate engine's monotonic revision.
	Revision() uint64
	// Snapshot builds the coalesced payload for one service filter (empty
	// means all services). It is summary, recent traffic, service health and
	// topology — never the seven-day history.
	Snapshot(ctx context.Context, service string) *LiveSnapshot
}

// DefaultPublishFloor is the minimum spacing between aggregate publications.
// Frozen at 2 s in #164: fast enough to feel live, slow enough that a busy
// engine cannot turn every commit into a broadcast.
const DefaultPublishFloor = 2 * time.Second

// eventClientQueue bounds how many undelivered messages a single event-WS
// client may hold. Overflow disconnects the client rather than dropping
// messages silently: the log/metric batches are incremental, so a gap is
// indistinguishable from "nothing happened" and would quietly lie to the
// dashboard. A reconnect re-seeds with a full snapshot.
const eventClientQueue = 256

// clientFilter tracks a client's active service filter and its immutable
// tenant scope.
//
// service is client-selectable (empty = all services). tenant is NOT: it is
// fixed at handshake time from the authenticated principal and there is no
// protocol message that can change it. Empty tenant means authentication is
// not configured (development), in which case the socket sees everything —
// exactly as it did before per-tenant scoping existed.
type clientFilter struct {
	conn    *websocket.Conn
	send    chan []byte
	service string
	tenant  string
	closed  atomic.Bool
}

// matches reports whether an entry belongs to this client's scope.
func (cf *clientFilter) matches(tenant, service string) bool {
	if cf.tenant != "" && cf.tenant != tenant {
		return false
	}
	return cf.service == "" || cf.service == service
}

// enqueue hands a message to the client's writer goroutine. It reports false
// when the queue is full — the caller then disconnects the client.
func (cf *clientFilter) enqueue(msg []byte) bool {
	if cf.closed.Load() {
		return false
	}
	select {
	case cf.send <- msg:
		return true
	default:
		return false
	}
}

// scopeKey identifies one snapshot audience: a tenant plus a service filter.
// Snapshots are computed once per key, not once per client.
type scopeKey struct {
	tenant  string
	service string
}

// EventHub manages WebSocket clients and pushes live data snapshots
// filtered per-client's selected service. Debounces rapid ingestion
// bursts and only computes snapshots every flush interval.
type EventHub struct {
	repo   *storage.Repository
	onConn func()
	onDisc func()

	mu      sync.Mutex
	clients map[*websocket.Conn]*clientFilter
	pending bool

	// maxClients caps simultaneous event-WS connections (WS_MAX_CLIENTS).
	// 0 = unlimited (legacy default). Past the cap the handshake is refused
	// with 503 before the upgrade, so a connection flood cannot exhaust file
	// descriptors or per-client queue memory.
	maxClients  int
	clientCount atomic.Int64

	// Origin policy, mirroring Hub. Enforcement is on when authentication is
	// enabled or APP_ENV=production.
	enforceOrigin bool
	originHosts   []string

	writerWg sync.WaitGroup

	// Real-time batching
	logsCh       chan LogEntry
	metricsCh    chan MetricEntry
	logBuffer    []LogEntry
	metricBuffer []MetricEntry

	stopOnce sync.Once
	stopCh   chan struct{}

	// aggPub, when set, switches the hub to revision-driven publication.
	// Per-event log/metric broadcasts are disabled in that mode: the
	// coalesced snapshot is the only data message.
	aggPub       AggregatePublisher
	publishFloor time.Duration
	// lastEpoch and lastRev are what was last published to every client.
	lastEpoch  string
	lastRev    uint64
	published  bool
	retryReset bool
}

// NewEventHub creates a new event notification hub.
func NewEventHub(repo *storage.Repository, onConnect, onDisconnect func()) *EventHub {
	return &EventHub{
		repo:         repo,
		onConn:       onConnect,
		onDisc:       onDisconnect,
		clients:      make(map[*websocket.Conn]*clientFilter),
		logsCh:       make(chan LogEntry, 1000),
		metricsCh:    make(chan MetricEntry, 1000),
		logBuffer:    make([]LogEntry, 0, 100),
		metricBuffer: make([]MetricEntry, 0, 100),
		stopCh:       make(chan struct{}),
	}
}

// SetAggregatePublisher switches the hub to revision-driven publication. Pass
// a zero floor to take DefaultPublishFloor. Call once at startup, before the
// hub takes connections.
func (h *EventHub) SetAggregatePublisher(p AggregatePublisher, floor time.Duration) {
	if p == nil {
		return
	}
	if floor <= 0 {
		floor = DefaultPublishFloor
	}
	h.mu.Lock()
	h.aggPub = p
	h.publishFloor = floor
	h.mu.Unlock()
}

// aggregatePublisher returns the configured publisher, or nil in legacy mode.
func (h *EventHub) aggregatePublisher() AggregatePublisher {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.aggPub
}

// Start begins the periodic flush loops. Call in a goroutine.
func (h *EventHub) Start(ctx context.Context, snapshotInterval, batchInterval time.Duration) {
	if h.aggregatePublisher() != nil {
		h.runAggregate(ctx)
		return
	}

	snapshotTicker := time.NewTicker(snapshotInterval)
	batchTicker := time.NewTicker(batchInterval)
	defer snapshotTicker.Stop()
	defer batchTicker.Stop()

	slog.Info("🌐 EventHub started",
		"snapshot_interval", snapshotInterval,
		"batch_interval", batchInterval)

	for {
		select {
		case <-ctx.Done():
			slog.Info("🌐 EventHub stopping via context...")
			return
		case <-h.stopCh:
			slog.Info("🌐 EventHub stopping via signal...")
			return
		case <-snapshotTicker.C:
			h.flushSnapshots()
		case <-batchTicker.C:
			h.flushBatches()
		case entry := <-h.logsCh:
			h.mu.Lock()
			h.logBuffer = append(h.logBuffer, entry)
			h.mu.Unlock()
		case entry := <-h.metricsCh:
			h.mu.Lock()
			h.metricBuffer = append(h.metricBuffer, entry)
			h.mu.Unlock()
		}
	}
}

// notifyRefresh marks that new data has arrived. The actual snapshot
// happens on the next snapshotTicker flush.
func (h *EventHub) NotifyRefresh() {
	h.mu.Lock()
	h.pending = true
	h.mu.Unlock()
}

// BroadcastLog adds a log entry to the real-time buffer. In aggregate mode
// per-event broadcasts are disabled and this is a no-op: the coalesced
// revision-driven snapshot is the only data message clients receive.
func (h *EventHub) BroadcastLog(l LogEntry) {
	if h.aggregatePublisher() != nil {
		return
	}
	select {
	case h.logsCh <- l:
	default:
	}
}

// BroadcastMetric adds a metric entry to the real-time buffer. Disabled in
// aggregate mode, for the same reason as BroadcastLog.
func (h *EventHub) BroadcastMetric(m MetricEntry) {
	if h.aggregatePublisher() != nil {
		return
	}
	select {
	case h.metricsCh <- m:
	default:
	}
}

// SetMaxClients caps simultaneous event-WS connections. 0 disables the cap.
// Call once at startup, before the hub takes traffic.
func (h *EventHub) SetMaxClients(n int) {
	if n < 0 {
		n = 0
	}
	h.maxClients = n
}

// SetOriginPolicy configures WebSocket origin enforcement — see
// Hub.SetOriginPolicy. Call once at startup.
func (h *EventHub) SetOriginPolicy(enforce bool, allowedHosts []string) {
	h.enforceOrigin = enforce
	h.originHosts = append([]string(nil), allowedHosts...)
}

// ActiveClients reports currently-connected event-WS clients.
func (h *EventHub) ActiveClients() int64 { return h.clientCount.Load() }

// HandleWebSocket upgrades an HTTP request to a WebSocket connection,
// registers it as an event client, and listens for filter messages.
//
// The connection is scoped to exactly one tenant, taken from the principal the
// handshake gate authenticated. Writes are serialized through one bounded
// queue and one writer goroutine per client, so a stalled reader can neither
// block the snapshot loop nor grow without bound.
func (h *EventHub) HandleWebSocket(w http.ResponseWriter, r *http.Request) {
	if h.maxClients > 0 {
		if n := h.clientCount.Add(1); n > int64(h.maxClients) {
			h.clientCount.Add(-1)
			slog.Warn("Event WS connection rejected: max-clients cap reached",
				"max_clients", h.maxClients, "current", n-1)
			http.Error(w, "WebSocket connections at capacity, retry later", http.StatusServiceUnavailable)
			return
		}
	} else {
		h.clientCount.Add(1)
	}
	counted := true
	releaseSlot := func() {
		if counted {
			counted = false
			h.clientCount.Add(-1)
		}
	}

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// Origin verification is skipped only while the policy is off, which
		// is the unauthenticated development default. Production and every
		// authenticated deployment enforce WS_ALLOWED_ORIGINS (empty list =
		// same host).
		InsecureSkipVerify: !h.enforceOrigin,
		OriginPatterns:     h.originHosts,
		Subprotocols:       []string{authn.WSSubprotocol},
	})
	if err != nil {
		releaseSlot()
		slog.Error("Event WS accept failed", "error", err)
		return
	}

	// Check for initial service filter from query params
	initialService := r.URL.Query().Get("service")
	cf := h.addClient(conn, initialService, connTenantScope(r))

	h.writerWg.Add(1)
	go func() { // #nosec G118 -- long-lived WS writer goroutine outlives the HTTP request intentionally
		defer h.writerWg.Done()
		defer releaseSlot()
		for msg := range cf.send {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			err := conn.Write(ctx, websocket.MessageText, msg)
			cancel()
			if err != nil {
				slog.Debug("Event WS send failed", "error", err)
				h.removeClient(conn)
				_ = conn.Close(websocket.StatusGoingAway, "write error")
				return
			}
		}
	}()

	// Send immediate FULL snapshot so the client has data right away. In
	// aggregate mode it carries {epoch, revision} and reset=true: a fresh
	// client has nothing to merge into and must adopt the snapshot whole.
	if !h.sendSnapshotTo(cf) && h.aggregatePublisher() != nil {
		// A failed reconnect seed must remain retryable even when the revision
		// was observed earlier while no clients were connected.
		h.mu.Lock()
		h.retryReset = true
		h.mu.Unlock()
	}

	// Read loop: client can send {"service":"xxx"} to change filter. The
	// tenant scope is not negotiable and is deliberately absent here.
	for {
		_, msg, readErr := conn.Read(r.Context())
		if readErr != nil {
			break
		}
		var filterMsg struct {
			Service string `json:"service"`
		}
		if json.Unmarshal(msg, &filterMsg) == nil {
			h.updateClientFilter(conn, filterMsg.Service)
		}
	}

	h.removeClient(conn)
	_ = conn.Close(websocket.StatusNormalClosure, "bye")
}

func (h *EventHub) addClient(c *websocket.Conn, service, tenant string) *clientFilter {
	cf := &clientFilter{
		conn:    c,
		send:    make(chan []byte, eventClientQueue),
		service: service,
		tenant:  tenant,
	}
	h.mu.Lock()
	h.clients[c] = cf
	h.mu.Unlock()
	if h.onConn != nil {
		h.onConn()
	}
	return cf
}

func (h *EventHub) removeClient(c *websocket.Conn) {
	h.mu.Lock()
	cf, ok := h.clients[c]
	delete(h.clients, c)
	h.mu.Unlock()
	if !ok {
		return
	}
	// Closing the queue is what stops the writer goroutine; the CAS guard
	// makes every removal path idempotent.
	if cf.closed.CompareAndSwap(false, true) {
		close(cf.send)
	}
	if h.onDisc != nil {
		h.onDisc()
	}
}

// deliver enqueues one message, disconnecting a client whose queue is full.
func (h *EventHub) deliver(cf *clientFilter, msg []byte) {
	if cf.enqueue(msg) {
		return
	}
	slog.Warn("Event WS client removed: write queue full", "queue", eventClientQueue)
	h.removeClient(cf.conn)
	_ = cf.conn.Close(websocket.StatusPolicyViolation, "client too slow")
}

func (h *EventHub) updateClientFilter(c *websocket.Conn, service string) {
	h.mu.Lock()
	if cf, ok := h.clients[c]; ok {
		cf.service = service
	}
	h.mu.Unlock()
}

// flushSnapshots computes per-scope snapshots in parallel and pushes them to
// matching clients. A scope is (tenant, service filter): two clients on
// different tenants never share a computed snapshot, so a snapshot cannot
// carry another tenant's rows.
func (h *EventHub) flushSnapshots() {
	h.mu.Lock()
	if !h.pending {
		h.mu.Unlock()
		return
	}
	h.pending = false

	if len(h.clients) == 0 {
		h.mu.Unlock()
		return
	}
	groups := h.groupByScopeLocked()
	h.mu.Unlock()

	// Compute snapshots in parallel using errgroup
	g, _ := errgroup.WithContext(context.Background())
	snapshotMap := make(map[scopeKey]*LiveSnapshot)
	var snapMu sync.Mutex

	for scope := range groups {
		g.Go(func() error {
			snap := h.snapshotFor(scope)
			if snap != nil {
				snapMu.Lock()
				snapshotMap[scope] = snap
				snapMu.Unlock()
			}
			return nil
		})
	}

	if err := g.Wait(); err != nil {
		slog.Error("❌ Parallel snapshot computation failed", "error", err)
	}

	// Broadcast memoized snapshots to matching clients
	for scope, clients := range groups {
		snap, ok := snapshotMap[scope]
		if !ok {
			continue
		}
		msg, err := json.Marshal(snap)
		if err != nil {
			slog.Error("Event WS marshal failed", "error", err)
			continue
		}
		for _, cf := range clients {
			h.deliver(cf, msg)
		}
	}
}

// groupByScopeLocked buckets connected clients by (tenant, service). Callers
// must hold h.mu.
func (h *EventHub) groupByScopeLocked() map[scopeKey][]*clientFilter {
	groups := make(map[scopeKey][]*clientFilter, len(h.clients))
	for _, cf := range h.clients {
		k := scopeKey{tenant: cf.tenant, service: cf.service}
		groups[k] = append(groups[k], cf)
	}
	return groups
}

// flushBatches flushes buffered logs and metrics to clients, respecting each
// client's tenant scope first and its service filter second.
func (h *EventHub) flushBatches() {
	h.mu.Lock()
	logs := h.logBuffer
	h.logBuffer = make([]LogEntry, 0, 100)
	metrics := h.metricBuffer
	h.metricBuffer = make([]MetricEntry, 0, 100)
	clients := make([]*clientFilter, 0, len(h.clients))
	for _, cf := range h.clients {
		clients = append(clients, cf)
	}
	h.mu.Unlock()

	if len(logs) == 0 && len(metrics) == 0 {
		return
	}

	for _, cf := range clients {
		// 1. Filter Logs
		clientLogs := make([]LogEntry, 0)
		for _, l := range logs {
			if cf.matches(l.Tenant, l.ServiceName) {
				clientLogs = append(clientLogs, l)
			}
		}

		// 2. Filter Metrics
		clientMetrics := make([]MetricEntry, 0)
		for _, m := range metrics {
			if cf.matches(m.Tenant, m.ServiceName) {
				clientMetrics = append(clientMetrics, m)
			}
		}

		// 3. Send Batches
		if len(clientLogs) > 0 {
			h.sendBatch(cf, "logs", clientLogs)
		}
		if len(clientMetrics) > 0 {
			h.sendBatch(cf, "metrics", clientMetrics)
		}
	}
}

func (h *EventHub) sendBatch(cf *clientFilter, batchType string, data any) {
	msg, err := json.Marshal(HubBatch{Type: batchType, Data: data})
	if err != nil {
		slog.Error("Event WS marshal failed", "error", err, "type", batchType)
		return
	}
	h.deliver(cf, msg)
}

// sendSnapshotTo sends a snapshot to a single client and reports whether a
// full replacement was queued.
func (h *EventHub) sendSnapshotTo(cf *clientFilter) bool {
	snapshot := h.snapshotFor(scopeKey{tenant: cf.tenant, service: cf.service})
	if snapshot == nil {
		return false
	}
	if h.aggregatePublisher() != nil {
		snapshot.Reset = true
	}
	msg, err := json.Marshal(snapshot)
	if err != nil {
		return false
	}
	h.deliver(cf, msg)
	return true
}

// snapshotFor produces the payload for one scope from whichever source owns
// the numbers in this mode. The tenant travels on the context, which is what
// scopes both the repository queries and the aggregate publisher.
func (h *EventHub) snapshotFor(scope scopeKey) *LiveSnapshot {
	ctx := context.Background()
	if scope.tenant != "" {
		ctx = storage.WithTenantContext(ctx, scope.tenant)
	}
	if pub := h.aggregatePublisher(); pub != nil {
		return pub.Snapshot(ctx, scope.service)
	}
	return h.computeSnapshot(ctx, scope.service)
}

// runAggregate is the revision-driven publication loop.
//
// Publication happens only on a revision (or epoch) change, at most once per
// publishFloor, with TRAILING-EDGE delivery: the loop re-reads the revision at
// every tick, so a change that lands inside a spacing interval is published at
// the end of that interval instead of disappearing for want of a later event.
func (h *EventHub) runAggregate(ctx context.Context) {
	h.mu.Lock()
	floor := h.publishFloor
	h.mu.Unlock()

	slog.Info("🌐 EventHub started in aggregate mode", "publish_floor", floor)

	tick := time.NewTicker(floor)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			slog.Info("🌐 EventHub stopping via context...")
			return
		case <-h.stopCh:
			slog.Info("🌐 EventHub stopping via signal...")
			return
		case <-tick.C:
			h.publishIfChanged()
		}
	}
}

// publishIfChanged publishes one coalesced snapshot per service filter when
// the engine's {epoch, revision} identity has moved since the last
// publication. It is exported behaviour only through the loop; tests drive it
// directly so the 2 s floor does not become a 2 s test.
func (h *EventHub) publishIfChanged() {
	pub := h.aggregatePublisher()
	if pub == nil {
		return
	}
	epoch, rev := pub.Epoch(), pub.Revision()

	h.mu.Lock()
	if h.published && !h.retryReset && h.lastEpoch == epoch && h.lastRev == rev {
		h.mu.Unlock()
		return
	}
	if h.published && h.lastEpoch == epoch && rev < h.lastRev {
		h.mu.Unlock()
		return
	}
	reset := h.retryReset || (h.published && h.lastEpoch != epoch)
	if len(h.clients) == 0 {
		h.lastEpoch, h.lastRev, h.published = epoch, rev, true
		h.retryReset = false
		h.mu.Unlock()
		return
	}
	groups := h.groupByScopeLocked()
	h.mu.Unlock()

	messages := make(map[scopeKey][]byte, len(groups))
	for scope := range groups {
		snap := h.snapshotFor(scope)
		if snap == nil {
			// A provider error is not an empty topology. Keep the last good
			// identity so the same revision is retried on the next tick.
			return
		}
		snap.Reset = reset
		msg, err := json.Marshal(snap)
		if err != nil {
			slog.Error("Event WS marshal failed", "error", err)
			return
		}
		messages[scope] = msg
	}

	h.mu.Lock()
	h.lastEpoch, h.lastRev, h.published = epoch, rev, true
	h.retryReset = false
	h.mu.Unlock()
	for scope, clients := range groups {
		for _, cf := range clients {
			h.deliver(cf, messages[scope])
		}
	}
}

// computeSnapshot queries the DB for the last 15 minutes of data,
// optionally filtered by a single service name.
func (h *EventHub) computeSnapshot(ctx context.Context, service string) *LiveSnapshot {
	if h.repo == nil {
		// No relational source wired (aggregate-only wiring, or a socket-layer
		// test): there is nothing to snapshot, and every query below would
		// dereference a nil repository.
		return nil
	}
	now := time.Now()
	start := now.Add(-15 * time.Minute)

	var serviceNames []string
	if service != "" {
		serviceNames = []string{service}
	}

	snapshot := &LiveSnapshot{Type: "live_snapshot"}

	if stats, err := h.repo.GetDashboardStats(ctx, start, now, serviceNames); err == nil {
		snapshot.Dashboard = stats
	}

	if traffic, err := h.repo.GetTrafficMetrics(ctx, start, now, serviceNames); err == nil {
		snapshot.Traffic = traffic
	}

	if traces, err := h.repo.GetTracesFiltered(ctx, start, now, serviceNames, "", "", 25, 0, "timestamp", "desc"); err == nil {
		snapshot.Traces = traces
	}

	if smap, err := h.repo.GetServiceMapMetrics(ctx, start, now); err == nil {
		snapshot.ServiceMap = smap
	}

	return snapshot
}

func (h *EventHub) Stop() {
	h.stopOnce.Do(func() {
		close(h.stopCh)
		// Close every client queue so the writer goroutines exit; without
		// this an idle client keeps one goroutine parked on its channel.
		h.mu.Lock()
		for c, cf := range h.clients {
			delete(h.clients, c)
			if cf.closed.CompareAndSwap(false, true) {
				close(cf.send)
			}
		}
		h.mu.Unlock()
		h.writerWg.Wait()
	})
}
