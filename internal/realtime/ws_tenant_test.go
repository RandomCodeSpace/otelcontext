package realtime_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/api"
	"github.com/RandomCodeSpace/otelcontext/internal/authn"
	"github.com/RandomCodeSpace/otelcontext/internal/realtime"
	"github.com/coder/websocket"
)

// The tests live in package realtime_test because they compose the WebSocket
// handshake gate (internal/api) with the hubs, and internal/api already
// imports internal/realtime.

func keyAuth(t *testing.T, entries map[string]string) *authn.Authenticator {
	t.Helper()
	store, err := authn.NewKeyStoreFromMap(entries)
	if err != nil {
		t.Fatalf("NewKeyStoreFromMap: %v", err)
	}
	return authn.NewAuthenticator("operator-key", store, false)
}

// gatedServer serves one WebSocket handler behind the handshake gate.
func gatedServer(t *testing.T, opts api.WSGateOptions, h http.HandlerFunc) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.Handle("/ws/events", h)
	srv := httptest.NewServer(api.WebSocketGate(opts, mux))
	t.Cleanup(srv.Close)
	return "ws" + srv.URL[len("http"):] + "/ws/events"
}

// dialKey opens a socket authenticated with a bearer key. It returns the
// handshake status code rather than the response so the body is closed exactly
// once, here.
func dialKey(t *testing.T, url, key string) (*websocket.Conn, int, error) {
	t.Helper()
	hdr := http.Header{}
	if key != "" {
		hdr.Set("Authorization", "Bearer "+key)
	}
	return dialWith(t, url, hdr)
}

// dialWith dials with arbitrary handshake headers.
func dialWith(t *testing.T, url string, hdr http.Header) (*websocket.Conn, int, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, resp, err := websocket.Dial(ctx, url, &websocket.DialOptions{
		HTTPHeader:   hdr,
		Subprotocols: hdrSubprotocols(hdr),
	})
	status := 0
	if resp != nil {
		status = resp.StatusCode
		if resp.Body != nil {
			_ = resp.Body.Close()
		}
	}
	if err == nil {
		t.Cleanup(func() { _ = conn.Close(websocket.StatusNormalClosure, "test") })
	}
	return conn, status, err
}

// hdrSubprotocols lets a caller pass subprotocols through the same helper.
func hdrSubprotocols(hdr http.Header) []string {
	if v := hdr.Values("X-Test-Subprotocols"); len(v) > 0 {
		hdr.Del("X-Test-Subprotocols")
		return v
	}
	return nil
}

// readBatch waits for one hub batch and decodes the log bodies it carries.
func readBatch(t *testing.T, conn *websocket.Conn, within time.Duration) (string, []string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), within)
	defer cancel()
	_, msg, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var batch struct {
		Type string `json:"type"`
		Data []struct {
			Body   string `json:"body"`
			Tenant string `json:"tenant"`
		} `json:"data"`
	}
	if err := json.Unmarshal(msg, &batch); err != nil {
		t.Fatalf("decode batch: %v (%s)", err, msg)
	}
	bodies := make([]string, 0, len(batch.Data))
	for _, d := range batch.Data {
		if d.Tenant != "" {
			t.Errorf("tenant leaked into the wire payload: %s", msg)
		}
		bodies = append(bodies, d.Body)
	}
	return batch.Type, bodies
}

// TestHub_CrossTenantIsolation is the #194 blocker-7 acceptance test: a
// tenant-A key can never receive tenant-B events.
func TestHub_CrossTenantIsolation(t *testing.T) {
	hub := realtime.NewHub(nil)
	go hub.Run()
	t.Cleanup(hub.Stop)

	auth := keyAuth(t, map[string]string{"acme-key": "acme", "beta-key": "beta"})
	url := gatedServer(t, api.WSGateOptions{Auth: auth, DefaultTenant: "default"}, hub.HandleWebSocket)

	acme, _, err := dialKey(t, url, "acme-key")
	if err != nil {
		t.Fatalf("acme dial: %v", err)
	}
	beta, _, err := dialKey(t, url, "beta-key")
	if err != nil {
		t.Fatalf("beta dial: %v", err)
	}
	// Let both registrations land before broadcasting.
	waitFor(t, func() bool { return hub.ActiveClients() == 2 })

	hub.Broadcast(realtime.LogEntry{Tenant: "acme", Body: "acme-only", ServiceName: "svc"})
	hub.Broadcast(realtime.LogEntry{Tenant: "beta", Body: "beta-only", ServiceName: "svc"})

	typ, bodies := readBatch(t, acme, 3*time.Second)
	if typ != "logs" || len(bodies) != 1 || bodies[0] != "acme-only" {
		t.Fatalf("acme socket received %v (type %q), want [acme-only]", bodies, typ)
	}
	_, bodies = readBatch(t, beta, 3*time.Second)
	if len(bodies) != 1 || bodies[0] != "beta-only" {
		t.Fatalf("beta socket received %v, want [beta-only]", bodies)
	}
}

// An entry with no tenant is invisible to a scoped socket: fail closed.
func TestHub_UntaggedEntryNeverReachesScopedSocket(t *testing.T) {
	hub := realtime.NewHub(nil)
	go hub.Run()
	t.Cleanup(hub.Stop)

	auth := keyAuth(t, map[string]string{"acme-key": "acme"})
	url := gatedServer(t, api.WSGateOptions{Auth: auth, DefaultTenant: "default"}, hub.HandleWebSocket)
	acme, _, err := dialKey(t, url, "acme-key")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	waitFor(t, func() bool { return hub.ActiveClients() == 1 })

	hub.Broadcast(realtime.LogEntry{Body: "untagged", ServiceName: "svc"})
	hub.Broadcast(realtime.LogEntry{Tenant: "acme", Body: "tagged", ServiceName: "svc"})

	_, bodies := readBatch(t, acme, 3*time.Second)
	for _, b := range bodies {
		if b == "untagged" {
			t.Fatalf("untagged entry reached a scoped socket: %v", bodies)
		}
	}
}

// The handshake is refused outright once authentication is configured.
func TestHub_UnauthenticatedHandshakeRefused(t *testing.T) {
	// The hub loop is deliberately NOT started: no handshake gets far enough
	// to register a client, and starting it would race Run's WaitGroup.Add
	// against Stop's Wait for no benefit.
	hub := realtime.NewHub(nil)

	auth := keyAuth(t, map[string]string{"acme-key": "acme"})
	url := gatedServer(t, api.WSGateOptions{Auth: auth, DefaultTenant: "default"}, hub.HandleWebSocket)

	if _, status, err := dialKey(t, url, ""); err == nil {
		t.Fatal("unauthenticated handshake succeeded")
	} else if status != http.StatusUnauthorized {
		t.Fatalf("want 401, got status=%d err=%v", status, err)
	}
	if _, status, err := dialKey(t, url, "wrong-key"); err == nil {
		t.Fatal("bad-key handshake succeeded")
	} else if status != http.StatusUnauthorized {
		t.Fatalf("want 401, got status=%d err=%v", status, err)
	}
	if hub.ActiveClients() != 0 {
		t.Fatalf("refused handshakes still occupy %d admission slots", hub.ActiveClients())
	}
}

// Production origin policy: a cross-origin browser handshake is refused.
func TestHub_OriginViolationRefusedInProduction(t *testing.T) {
	hub := realtime.NewHub(nil) // no client ever registers — see the test above

	auth := keyAuth(t, map[string]string{"acme-key": "acme"})
	url := gatedServer(t, api.WSGateOptions{
		Auth: auth, DefaultTenant: "default",
		AllowedOrigins: []string{"https://app.example.com"}, EnforceOrigin: true,
	}, hub.HandleWebSocket)

	hdr := http.Header{}
	hdr.Set("Authorization", "Bearer acme-key")
	hdr.Set("Origin", "https://evil.example.com")
	conn, status, err := dialWith(t, url, hdr)
	if err == nil {
		_ = conn.Close(websocket.StatusNormalClosure, "test")
		t.Fatal("cross-origin handshake succeeded")
	}
	if status != http.StatusForbidden {
		t.Fatalf("want 403, got status=%d err=%v", status, err)
	}
}

// The browser carrier: the credential travels as a subprotocol entry and the
// server echoes ONLY otelcontext.v1 — never the token-bearing entry.
func TestHub_SubprotocolEchoesOnlyOtelcontextV1(t *testing.T) {
	hub := realtime.NewHub(nil)
	go hub.Run()
	t.Cleanup(hub.Stop)

	auth := keyAuth(t, map[string]string{"acme-key": "acme"})
	url := gatedServer(t, api.WSGateOptions{Auth: auth, DefaultTenant: "default"}, hub.HandleWebSocket)

	authEntry := "auth." + base64.RawURLEncoding.EncodeToString([]byte("acme-key"))
	hdr := http.Header{}
	hdr.Add("X-Test-Subprotocols", authn.WSSubprotocol)
	hdr.Add("X-Test-Subprotocols", authEntry)
	conn, _, err := dialWith(t, url, hdr)
	if err != nil {
		t.Fatalf("subprotocol dial: %v", err)
	}

	if got := conn.Subprotocol(); got != authn.WSSubprotocol {
		t.Fatalf("negotiated subprotocol = %q, want %q", got, authn.WSSubprotocol)
	}
	waitFor(t, func() bool { return hub.ActiveClients() == 1 })
	hub.Broadcast(realtime.LogEntry{Tenant: "acme", Body: "scoped", ServiceName: "svc"})
	hub.Broadcast(realtime.LogEntry{Tenant: "beta", Body: "other", ServiceName: "svc"})
	_, bodies := readBatch(t, conn, 3*time.Second)
	if len(bodies) != 1 || bodies[0] != "scoped" {
		t.Fatalf("subprotocol-authenticated socket received %v, want [scoped]", bodies)
	}
}

// EventHub carries the same guarantee on the snapshot/batch stream.
func TestEventHub_CrossTenantIsolation(t *testing.T) {
	hub := realtime.NewEventHub(nil, nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	go hub.Start(ctx, time.Hour, 25*time.Millisecond)
	t.Cleanup(func() { cancel(); hub.Stop() })

	auth := keyAuth(t, map[string]string{"acme-key": "acme", "beta-key": "beta"})
	url := gatedServer(t, api.WSGateOptions{Auth: auth, DefaultTenant: "default"}, hub.HandleWebSocket)

	acme, _, err := dialKey(t, url, "acme-key")
	if err != nil {
		t.Fatalf("acme dial: %v", err)
	}
	beta, _, err := dialKey(t, url, "beta-key")
	if err != nil {
		t.Fatalf("beta dial: %v", err)
	}
	waitFor(t, func() bool { return hub.ActiveClients() == 2 })

	hub.BroadcastLog(realtime.LogEntry{Tenant: "acme", Body: "acme-only", ServiceName: "svc"})
	hub.BroadcastLog(realtime.LogEntry{Tenant: "beta", Body: "beta-only", ServiceName: "svc"})

	_, bodies := readBatch(t, acme, 3*time.Second)
	if len(bodies) != 1 || bodies[0] != "acme-only" {
		t.Fatalf("acme socket received %v, want [acme-only]", bodies)
	}
	_, bodies = readBatch(t, beta, 3*time.Second)
	if len(bodies) != 1 || bodies[0] != "beta-only" {
		t.Fatalf("beta socket received %v, want [beta-only]", bodies)
	}
}

// EventHub gained the admission cap the realtime Hub already had.
func TestEventHub_MaxClientsCap(t *testing.T) {
	hub := realtime.NewEventHub(nil, nil, nil)
	hub.SetMaxClients(1)
	ctx, cancel := context.WithCancel(context.Background())
	go hub.Start(ctx, time.Hour, time.Hour)
	t.Cleanup(func() { cancel(); hub.Stop() })

	url := gatedServer(t, api.WSGateOptions{Auth: authn.NewAuthenticator("", nil, false)}, hub.HandleWebSocket)

	if _, _, err := dialKey(t, url, ""); err != nil {
		t.Fatalf("first dial: %v", err)
	}
	waitFor(t, func() bool { return hub.ActiveClients() == 1 })

	_, status, err := dialKey(t, url, "")
	if err == nil {
		t.Fatal("second dial succeeded past the cap")
	}
	if status != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got status=%d err=%v", status, err)
	}
}

// waitFor polls a condition for up to 3s; test helpers elsewhere in this
// package use the same shape.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met within 3s")
}
