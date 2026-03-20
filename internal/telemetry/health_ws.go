package telemetry

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/coder/websocket"
)

// HealthWSHandler returns an HTTP handler that upgrades to WebSocket and
// pushes HealthStats snapshots every 3 seconds. An immediate snapshot is
// sent on connection so the client never has to wait for the first tick.
func (m *Metrics) HealthWSHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			InsecureSkipVerify: true, // Allow cross-origin for dev mode
		})
		if err != nil {
			slog.Error("Health WS upgrade failed", "error", err)
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "closing")

		// Track this connection in active_connections metric
		m.IncrementActiveConns()
		defer m.DecrementActiveConns()

		slog.Info("📊 Health WS client connected")

		// Send immediate snapshot so client doesn't wait for first tick
		if err := m.sendHealthSnapshot(conn); err != nil {
			slog.Debug("Health WS initial send failed", "error", err)
			return
		}

		ticker := time.NewTicker(3 * time.Second)
		defer ticker.Stop()

		// Read goroutine — detects client disconnect
		// Use request context so goroutine exits when connection drops
		connCtx, connCancel := context.WithCancel(r.Context())
		defer connCancel()
		disconnected := make(chan struct{})
		go func() {
			defer close(disconnected)
			for {
				_, _, err := conn.Read(connCtx)
				if err != nil {
					return
				}
			}
		}()

		for {
			select {
			case <-disconnected:
				slog.Info("📊 Health WS client disconnected")
				return
			case <-ticker.C:
				if err := m.sendHealthSnapshot(conn); err != nil {
					slog.Debug("Health WS send failed", "error", err)
					return
				}
			}
		}
	}
}

// sendHealthSnapshot serializes the current HealthStats and writes it to the WebSocket.
func (m *Metrics) sendHealthSnapshot(conn *websocket.Conn) error {
	data, err := json.Marshal(m.GetHealthStats())
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return conn.Write(ctx, websocket.MessageText, data)
}
