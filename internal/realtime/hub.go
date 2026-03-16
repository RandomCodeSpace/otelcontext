package realtime

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// LogEntry is a lightweight struct for WebSocket broadcast payloads.
type LogEntry struct {
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
type MetricEntry struct {
	Name        string                 `json:"name"`
	ServiceName string                 `json:"service_name"`
	Value       float64                `json:"value"`
	Timestamp   time.Time              `json:"timestamp"`
	Attributes  map[string]interface{} `json:"attributes"`
}

// HubBatch is a unified payload for WebSocket broadcasts.
type HubBatch struct {
	Type string      `json:"type"` // "logs" or "metrics"
	Data interface{} `json:"data"` // Slice of entries
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

	stopCh  chan struct{}
	wg      sync.WaitGroup
	devMode bool

	// onConnectionChange is called when the number of active connections changes.
	onConnectionChange func(count int)

	// Metric callbacks (optional)
	onMessageSent    func(msgType string) // WSMessagesSent.WithLabelValues(type).Inc()
	onSlowClientDrop func()              // WSSlowClientsRemoved.Inc()

	logPool    sync.Pool
	metricPool sync.Pool
}

// client represents a single WebSocket connection.
type client struct {
	conn *websocket.Conn
	send chan []byte
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

	h.logPool.New = func() interface{} {
		return make([]LogEntry, 0, h.maxBufferSize)
	}
	h.metricPool.New = func() interface{} {
		return make([]MetricEntry, 0, h.maxBufferSize)
	}

	h.logBuffer = h.logPool.Get().([]LogEntry)
	h.metricBuffer = h.metricPool.Get().([]MetricEntry)

	return h
}

// Run starts the hub's main event loop. Should be called in a goroutine.
func (h *Hub) Run() {
	h.wg.Add(1)
	defer h.wg.Done()

	flushTicker := time.NewTicker(h.flushInterval)
	defer flushTicker.Stop()

	for {
		select {
		case <-h.stopCh:
			h.flush()
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
				close(c.send)
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
		h.broadcastBatch(HubBatch{Type: "logs", Data: logBatch})
		// Recycle logBatch
		logBatch = logBatch[:0]
		h.logPool.Put(logBatch)
	}

	// Broadcast Metrics if any
	if len(metricBatch) > 0 {
		h.broadcastBatch(HubBatch{Type: "metrics", Data: metricBatch})
		// Recycle metricBatch
		metricBatch = metricBatch[:0]
		h.metricPool.Put(metricBatch)
	}
}

func (h *Hub) broadcastBatch(batch HubBatch) {
	data, err := json.Marshal(batch)
	if err != nil {
		slog.Error("Hub: failed to marshal batch", "error", err, "type", batch.Type)
		return
	}

	sent := 0
	for c := range h.clients {
		select {
		case c.send <- data:
			sent++
		default:
			delete(h.clients, c)
			close(c.send)
			slog.Warn("Hub: slow client removed", "total", len(h.clients))
			if h.onConnectionChange != nil {
				h.onConnectionChange(len(h.clients))
			}
			if h.onSlowClientDrop != nil {
				h.onSlowClientDrop()
			}
		}
	}
	if sent > 0 && h.onMessageSent != nil {
		h.onMessageSent(batch.Type)
	}
}

// SetDevMode controls whether cross-origin WebSocket connections are accepted.
// Should be true only in development environments.
func (h *Hub) SetDevMode(devMode bool) {
	h.devMode = devMode
}

// SetWSMetrics wires WebSocket metric callbacks.
func (h *Hub) SetWSMetrics(onMessageSent func(string), onSlowClientDrop func()) {
	h.onMessageSent = onMessageSent
	h.onSlowClientDrop = onSlowClientDrop
}

// Broadcast adds a log entry to the broadcast buffer.
func (h *Hub) Broadcast(entry LogEntry) {
	select {
	case h.broadcast <- entry:
	default:
		// Drop if internal channel is full
	}
}

// BroadcastMetric adds a metric entry to the broadcast buffer.
func (h *Hub) BroadcastMetric(entry MetricEntry) {
	select {
	case h.metricsCh <- entry:
	default:
		// Drop if internal channel is full
	}
}

// Stop gracefully shuts down the hub.
func (h *Hub) Stop() {
	close(h.stopCh)
	h.wg.Wait()
	slog.Info("🛑 WebSocket hub stopped")
}

// HandleWebSocket is the HTTP handler that upgrades connections to WebSocket.
func (h *Hub) HandleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: h.devMode, // Allow cross-origin in dev mode only
	})
	if err != nil {
		slog.Error("WebSocket upgrade failed", "error", err)
		return
	}

	c := &client{
		conn: conn,
		send: make(chan []byte, 256),
	}

	h.register <- c

	// Writer goroutine
	go func() {
		defer func() {
			h.unregister <- c
			conn.Close(websocket.StatusNormalClosure, "closing")
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
}
