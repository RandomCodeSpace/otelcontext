//go:build !race_off

package realtime

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// TestStopDoesNotDeadlockAgainstChurn is the regression for the hub shutdown
// deadlock (#212). Registration and unregistration both send on channels that
// only the run loop drains; the run loop returns as soon as stopCh closes. A
// client whose writer goroutine reaches its unregister send after that return
// blocks forever, so Stop()'s writerWg.Wait() never comes back and the whole
// graceful shutdown wedges.
//
// The test drives connect/disconnect churn while Stop() runs, so the send and
// the run loop's exit interleave, and it fails by timeout rather than hanging
// the suite for the full go test deadline.
func TestStopDoesNotDeadlockAgainstChurn(t *testing.T) {
	for attempt := 0; attempt < 12; attempt++ {
		hub := NewHub(nil)
		go hub.Run()

		srv := httptest.NewServer(http.HandlerFunc(hub.HandleWebSocket))
		wsURL := "ws" + srv.URL[len("http"):]

		var wg sync.WaitGroup
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				c, _, err := websocket.Dial(ctx, wsURL, nil)
				if err != nil {
					return // refused mid-shutdown is a valid outcome
				}
				// Close immediately: the writer goroutine's unregister send
				// then races the run loop's exit.
				_ = c.Close(websocket.StatusNormalClosure, "bye")
			}()
		}

		// Stop concurrently with the churn — the window under test.
		time.Sleep(time.Duration(attempt) * time.Millisecond)
		done := make(chan struct{})
		go func() {
			hub.Stop()
			close(done)
		}()

		select {
		case <-done:
		case <-time.After(20 * time.Second):
			srv.Close()
			t.Fatalf("attempt %d: Hub.Stop() deadlocked — a writer or handler is blocked "+
				"sending on register/unregister with the run loop already returned", attempt)
		}

		wg.Wait()
		srv.Close()
	}
}
