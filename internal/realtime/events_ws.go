package realtime

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
	"time"

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

// clientFilter tracks a client's active service filter.
// Empty string = all services (no filter).
type clientFilter struct {
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
	lastEpoch string
	lastRev   uint64
	published bool
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

// HandleWebSocket upgrades an HTTP request to a WebSocket connection,
// registers it as an event client, and listens for filter messages.
func (h *EventHub) HandleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true,
	})
	if err != nil {
		slog.Error("Event WS accept failed", "error", err)
		return
	}

	// Check for initial service filter from query params
	initialService := r.URL.Query().Get("service")
	h.addClient(conn, initialService)

	// Send immediate FULL snapshot so the client has data right away. In
	// aggregate mode it carries {epoch, revision} and reset=true: a fresh
	// client has nothing to merge into and must adopt the snapshot whole.
	h.sendSnapshotTo(conn, initialService)

	// Read loop: client can send {"service":"xxx"} to change filter
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

func (h *EventHub) addClient(c *websocket.Conn, service string) {
	h.mu.Lock()
	h.clients[c] = &clientFilter{service: service}
	h.mu.Unlock()
	if h.onConn != nil {
		h.onConn()
	}
}

func (h *EventHub) removeClient(c *websocket.Conn) {
	h.mu.Lock()
	delete(h.clients, c)
	h.mu.Unlock()
	if h.onDisc != nil {
		h.onDisc()
	}
}

func (h *EventHub) updateClientFilter(c *websocket.Conn, service string) {
	h.mu.Lock()
	if cf, ok := h.clients[c]; ok {
		cf.service = service
	}
	h.mu.Unlock()
}

// flushSnapshots computes per-service snapshots in parallel and pushes to matching clients.
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

	// Group clients by service filter
	groups := make(map[string][]*websocket.Conn)
	for c, cf := range h.clients {
		groups[cf.service] = append(groups[cf.service], c)
	}
	h.mu.Unlock()

	// Compute snapshots in parallel using errgroup
	g, ctx := errgroup.WithContext(context.Background())
	snapshotMap := make(map[string]*LiveSnapshot)
	var snapMu sync.Mutex

	for service := range groups {
		// Capture
		g.Go(func() error {
			snap := h.snapshotFor(service)
			if snap != nil {
				snapMu.Lock()
				snapshotMap[service] = snap
				snapMu.Unlock()
			}
			return nil
		})
	}

	if err := g.Wait(); err != nil {
		slog.Error("❌ Parallel snapshot computation failed", "error", err)
	}

	// Broadcast memoized snapshots to matching clients
	for service, clients := range groups {
		snap, ok := snapshotMap[service]
		if !ok {
			continue
		}

		msg, err := json.Marshal(snap)
		if err != nil {
			slog.Error("Event WS marshal failed", "error", err)
			continue
		}

		for _, conn := range clients {
			writeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
			if err := conn.Write(writeCtx, websocket.MessageText, msg); err != nil {
				slog.Debug("Event WS send failed, removing client", "error", err)
				h.removeClient(conn)
				_ = conn.Close(websocket.StatusGoingAway, "write error")
			}
			cancel()
		}
	}
}

// flushBatches flushes buffered logs and metrics to clients, respecting filters.
func (h *EventHub) flushBatches() {
	h.mu.Lock()
	logs := h.logBuffer
	h.logBuffer = make([]LogEntry, 0, 100)
	metrics := h.metricBuffer
	h.metricBuffer = make([]MetricEntry, 0, 100)
	clients := make(map[*websocket.Conn]*clientFilter)
	for c, cf := range h.clients {
		clients[c] = cf
	}
	h.mu.Unlock()

	if len(logs) == 0 && len(metrics) == 0 {
		return
	}

	for conn, filter := range clients {
		// 1. Filter Logs
		clientLogs := make([]LogEntry, 0)
		for _, l := range logs {
			if filter.service == "" || filter.service == l.ServiceName {
				clientLogs = append(clientLogs, l)
			}
		}

		// 2. Filter Metrics
		clientMetrics := make([]MetricEntry, 0)
		for _, m := range metrics {
			if filter.service == "" || filter.service == m.ServiceName {
				clientMetrics = append(clientMetrics, m)
			}
		}

		// 3. Send Batches
		if len(clientLogs) > 0 {
			h.sendBatch(conn, "logs", clientLogs)
		}
		if len(clientMetrics) > 0 {
			h.sendBatch(conn, "metrics", clientMetrics)
		}
	}
}

func (h *EventHub) sendBatch(conn *websocket.Conn, batchType string, data any) {
	msg, _ := json.Marshal(HubBatch{Type: batchType, Data: data})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := conn.Write(ctx, websocket.MessageText, msg); err != nil {
		h.removeClient(conn)
		_ = conn.Close(websocket.StatusGoingAway, "write error")
	}
}

// sendSnapshotTo sends a snapshot to a single client.
func (h *EventHub) sendSnapshotTo(conn *websocket.Conn, service string) {
	snapshot := h.snapshotFor(service)
	if snapshot == nil {
		return
	}
	if h.aggregatePublisher() != nil {
		snapshot.Reset = true
	}
	msg, err := json.Marshal(snapshot)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = conn.Write(ctx, websocket.MessageText, msg)
}

// snapshotFor produces the payload for one service filter from whichever
// source owns the numbers in this mode.
func (h *EventHub) snapshotFor(service string) *LiveSnapshot {
	if pub := h.aggregatePublisher(); pub != nil {
		return pub.Snapshot(context.Background(), service)
	}
	return h.computeSnapshot(service)
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
	if h.published && h.lastEpoch == epoch && h.lastRev == rev {
		h.mu.Unlock()
		return
	}
	// An epoch change means the revision counter restarted: clients cannot
	// merge across it and are told to reset.
	reset := h.published && h.lastEpoch != epoch
	h.lastEpoch, h.lastRev, h.published = epoch, rev, true
	if len(h.clients) == 0 {
		h.mu.Unlock()
		return
	}
	groups := make(map[string][]*websocket.Conn)
	for c, cf := range h.clients {
		groups[cf.service] = append(groups[cf.service], c)
	}
	h.mu.Unlock()

	for service, clients := range groups {
		snap := pub.Snapshot(context.Background(), service)
		if snap == nil {
			continue
		}
		snap.Reset = reset
		msg, err := json.Marshal(snap)
		if err != nil {
			slog.Error("Event WS marshal failed", "error", err)
			continue
		}
		for _, conn := range clients {
			writeCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			if err := conn.Write(writeCtx, websocket.MessageText, msg); err != nil {
				slog.Debug("Event WS send failed, removing client", "error", err)
				h.removeClient(conn)
				_ = conn.Close(websocket.StatusGoingAway, "write error")
			}
			cancel()
		}
	}
}

// computeSnapshot queries the DB for the last 15 minutes of data,
// optionally filtered by a single service name.
func (h *EventHub) computeSnapshot(service string) *LiveSnapshot {
	now := time.Now()
	start := now.Add(-15 * time.Minute)

	var serviceNames []string
	if service != "" {
		serviceNames = []string{service}
	}

	snapshot := &LiveSnapshot{Type: "live_snapshot"}

	// WebSocket snapshots are not tenant-scoped in the current protocol;
	// use the default-tenant context so repo query helpers behave the same as
	// a single-tenant install.
	ctx := context.Background()

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
	})
}
