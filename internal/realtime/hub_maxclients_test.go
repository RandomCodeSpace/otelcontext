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

// TestHub_MaxClientsCap verifies HandleWebSocket rejects new connections
// once the cap is reached and the cap returns to enforcing again as old
// connections drop. Without the cap, a flood of connects exhausts file
// descriptors and per-client send-channel memory.
func TestHub_MaxClientsCap(t *testing.T) {
	hub := NewHub(nil)
	hub.SetMaxClients(2)
	go hub.Run()
	defer hub.Stop()

	srv := httptest.NewServer(http.HandlerFunc(hub.HandleWebSocket))
	defer srv.Close()

	// Registered after srv.Close's defer so it runs FIRST (LIFO): any conn
	// still open when the test bails via Fatalf must be closed before
	// srv.Close, which blocks forever on open hijacked connections.
	var openConns []*websocket.Conn
	defer func() {
		for _, c := range openConns {
			_ = c.Close(websocket.StatusNormalClosure, "test")
		}
	}()

	wsURL := "ws" + srv.URL[len("http"):]

	dial := func(t *testing.T) (*websocket.Conn, *http.Response, error) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		c, resp, err := websocket.Dial(ctx, wsURL, nil)
		if err == nil {
			openConns = append(openConns, c)
		}
		return c, resp, err
	}

	// First two connections should succeed.
	c1, _, err := dial(t)
	if err != nil {
		t.Fatalf("client 1 dial: %v", err)
	}
	c2, _, err := dial(t)
	if err != nil {
		t.Fatalf("client 2 dial: %v", err)
	}

	// Wait for the hub goroutine to register the connections — ActiveClients
	// is incremented in HandleWebSocket BEFORE the upgrade, so the count is
	// already accurate by the time dial returns.
	if got := hub.ActiveClients(); got != 2 {
		t.Fatalf("ActiveClients after 2 connects: got %d, want 2", got)
	}

	// Third connection MUST be rejected with 503.
	_, resp, err := dial(t)
	if err == nil {
		t.Fatalf("client 3 dial: expected error from 503 rejection, got success")
	}
	if resp == nil {
		t.Fatalf("client 3: expected non-nil response on rejection, got nil — err=%v", err)
	}
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("client 3: got status %d, want %d", resp.StatusCode, http.StatusServiceUnavailable)
	}
	resp.Body.Close()

	// ActiveClients must NOT have leaked into the count for the rejected one.
	if got := hub.ActiveClients(); got != 2 {
		t.Fatalf("ActiveClients after rejection: got %d, want 2 (slot must be released on reject)", got)
	}

	// Drop client 1, wait for the writer goroutine to release the slot, then
	// verify a new connect succeeds.
	c1.Close(websocket.StatusNormalClosure, "test")
	deadline := time.Now().Add(2 * time.Second)
	for hub.ActiveClients() > 1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := hub.ActiveClients(); got != 1 {
		t.Fatalf("ActiveClients after client 1 drop: got %d, want 1", got)
	}

	c3, _, err := dial(t)
	if err != nil {
		t.Fatalf("client 3 (retry after drop): %v", err)
	}

	_, _ = c2, c3
}

// TestHub_MaxClientsZeroIsUnlimited verifies the legacy unlimited path
// still works when no cap is configured.
func TestHub_MaxClientsZeroIsUnlimited(t *testing.T) {
	hub := NewHub(nil)
	// SetMaxClients NOT called → maxClients=0 → unlimited
	go hub.Run()
	defer hub.Stop()

	srv := httptest.NewServer(http.HandlerFunc(hub.HandleWebSocket))
	defer srv.Close()
	wsURL := "ws" + srv.URL[len("http"):]

	const N = 10
	conns := make([]*websocket.Conn, 0, N)
	var mu sync.Mutex
	// Runs before srv.Close (LIFO): a Fatalf below must not leave open
	// hijacked conns for srv.Close to wait on forever.
	defer func() {
		mu.Lock()
		defer mu.Unlock()
		for _, c := range conns {
			_ = c.Close(websocket.StatusNormalClosure, "test")
		}
	}()
	var wg sync.WaitGroup
	for range N {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			c, _, err := websocket.Dial(ctx, wsURL, nil)
			if err != nil {
				t.Errorf("dial: %v", err)
				return
			}
			mu.Lock()
			conns = append(conns, c)
			mu.Unlock()
		}()
	}
	wg.Wait()

	if got := hub.ActiveClients(); got != int64(N) {
		t.Fatalf("ActiveClients: got %d, want %d", got, N)
	}
}

func TestHub_SetMaxClients_NegativeCoercesToZero(t *testing.T) {
	hub := NewHub(nil)
	hub.SetMaxClients(-5)
	if hub.maxClients != 0 {
		t.Fatalf("negative cap should coerce to 0 (unlimited), got %d", hub.maxClients)
	}
}
