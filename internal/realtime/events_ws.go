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
type LiveSnapshot struct {
	Type       string                     `json:"type"`
	Dashboard  *storage.DashboardStats    `json:"dashboard"`
	Traffic    []storage.TrafficPoint     `json:"traffic"`
	Traces     *storage.TracesResponse    `json:"traces"`
	ServiceMap *storage.ServiceMapMetrics `json:"service_map"`
}

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

// Start begins the periodic flush loops. Call in a goroutine.
func (h *EventHub) Start(ctx context.Context, snapshotInterval, batchInterval time.Duration) {
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

// BroadcastLog adds a log entry to the real-time buffer.
func (h *EventHub) BroadcastLog(l LogEntry) {
	select {
	case h.logsCh <- l:
	default:
	}
}

// BroadcastMetric adds a metric entry to the real-time buffer.
func (h *EventHub) BroadcastMetric(m MetricEntry) {
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

	// Send immediate snapshot so the client has data right away
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
	conn.Close(websocket.StatusNormalClosure, "bye")
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
		service := service // Capture
		g.Go(func() error {
			snap := h.computeSnapshot(service)
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
				conn.Close(websocket.StatusGoingAway, "write error")
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

func (h *EventHub) sendBatch(conn *websocket.Conn, batchType string, data interface{}) {
	msg, _ := json.Marshal(HubBatch{Type: batchType, Data: data})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := conn.Write(ctx, websocket.MessageText, msg); err != nil {
		h.removeClient(conn)
		conn.Close(websocket.StatusGoingAway, "write error")
	}
}

// sendSnapshotTo sends a snapshot to a single client.
func (h *EventHub) sendSnapshotTo(conn *websocket.Conn, service string) {
	snapshot := h.computeSnapshot(service)
	if snapshot == nil {
		return
	}
	msg, err := json.Marshal(snapshot)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	conn.Write(ctx, websocket.MessageText, msg)
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

	if stats, err := h.repo.GetDashboardStats(start, now, serviceNames); err == nil {
		snapshot.Dashboard = stats
	}

	if traffic, err := h.repo.GetTrafficMetrics(start, now, serviceNames); err == nil {
		snapshot.Traffic = traffic
	}

	if traces, err := h.repo.GetTracesFiltered(start, now, serviceNames, "", "", 25, 0, "timestamp", "desc"); err == nil {
		snapshot.Traces = traces
	}

	if smap, err := h.repo.GetServiceMapMetrics(start, now); err == nil {
		snapshot.ServiceMap = smap
	}

	return snapshot
}

func (h *EventHub) Stop() {
	h.stopOnce.Do(func() {
		close(h.stopCh)
	})
}

