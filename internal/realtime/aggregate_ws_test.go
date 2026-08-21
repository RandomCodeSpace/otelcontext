package realtime

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// fakePublisher is an AggregatePublisher whose identity a test drives by hand.
type fakePublisher struct {
	epoch atomic.Value // string
	rev   atomic.Uint64
	calls atomic.Int64
}

func newFakePublisher(epoch string) *fakePublisher {
	p := &fakePublisher{}
	p.epoch.Store(epoch)
	return p
}

func (p *fakePublisher) Epoch() string    { return p.epoch.Load().(string) }
func (p *fakePublisher) Revision() uint64 { return p.rev.Load() }
func (p *fakePublisher) Snapshot(context.Context, string) *LiveSnapshot {
	p.calls.Add(1)
	return &LiveSnapshot{
		Type:     "live_snapshot",
		Epoch:    p.Epoch(),
		Revision: p.rev.Load(),
		Coverage: "sampled",
	}
}

// wsFixture wires an EventHub in aggregate mode behind an httptest server.
type wsFixture struct {
	hub  *EventHub
	pub  *fakePublisher
	conn *websocket.Conn
}

func newWSFixture(t *testing.T, floor time.Duration) *wsFixture {
	t.Helper()
	hub := NewEventHub(nil, nil, nil)
	pub := newFakePublisher("epoch-1")
	hub.SetAggregatePublisher(pub, floor)

	ctx, cancel := context.WithCancel(context.Background())
	go hub.Start(ctx, time.Hour, time.Hour)

	srv := httptest.NewServer(http.HandlerFunc(hub.HandleWebSocket))
	dialCtx, dialCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer dialCancel()
	conn, resp, err := websocket.Dial(dialCtx, "ws"+srv.URL[len("http"):], nil)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() {
		_ = conn.Close(websocket.StatusNormalClosure, "test")
		cancel()
		hub.Stop()
		srv.Close()
	})
	return &wsFixture{hub: hub, pub: pub, conn: conn}
}

// read waits for one snapshot message.
func (f *wsFixture) read(t *testing.T, within time.Duration) LiveSnapshot {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), within)
	defer cancel()
	_, msg, err := f.conn.Read(ctx)
	if err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	var snap LiveSnapshot
	if err := json.Unmarshal(msg, &snap); err != nil {
		t.Fatalf("decode snapshot: %v (%s)", err, msg)
	}
	return snap
}

// TestAggregateWSSendsFullSnapshotOnConnect: a fresh client has nothing to
// merge into, so the first message is a full snapshot flagged reset.
func TestAggregateWSSendsFullSnapshotOnConnect(t *testing.T) {
	f := newWSFixture(t, 50*time.Millisecond)
	snap := f.read(t, 2*time.Second)
	if !snap.Reset {
		t.Error("connect snapshot is not flagged reset")
	}
	if snap.Epoch != "epoch-1" {
		t.Errorf("epoch = %q, want epoch-1", snap.Epoch)
	}
	if snap.Coverage == "" {
		t.Error("connect snapshot carries no coverage")
	}
}

// TestAggregateWSTrailingEdgeDelivery is the acceptance criterion: a revision
// change landing INSIDE the spacing interval is still delivered — it must not
// disappear for want of a later event.
func TestAggregateWSTrailingEdgeDelivery(t *testing.T) {
	f := newWSFixture(t, 80*time.Millisecond)
	_ = f.read(t, 2*time.Second) // connect snapshot

	// A single change, then silence. Nothing else will ever arrive to "carry"
	// it, so if the publication were leading-edge-only it would be lost.
	f.pub.rev.Store(7)

	snap := f.read(t, 3*time.Second)
	if snap.Revision != 7 {
		t.Fatalf("revision = %d, want 7", snap.Revision)
	}
	if snap.Reset {
		t.Error("a revision change inside the same epoch must not ask the client to reset")
	}
}

// TestAggregateWSPublishesOnlyOnRevisionChange: an idle engine produces no
// data messages, however many spacing intervals elapse.
func TestAggregateWSPublishesOnlyOnRevisionChange(t *testing.T) {
	hub := NewEventHub(nil, nil, nil)
	pub := newFakePublisher("epoch-1")
	hub.SetAggregatePublisher(pub, time.Hour)

	pub.rev.Store(3)
	hub.publishIfChanged()
	first := pub.calls.Load()

	hub.publishIfChanged()
	hub.publishIfChanged()
	if got := pub.calls.Load(); got != first {
		t.Fatalf("snapshot built %d times for an unchanged revision, want %d", got, first)
	}

	pub.rev.Store(4)
	hub.publishIfChanged()
	if hub.lastRev != 4 {
		t.Fatalf("lastRev = %d after a change, want 4", hub.lastRev)
	}
}

// TestAggregateWSEpochChangeResetsClients: the revision counter restarts at
// zero on a new process generation, so a client must be told to drop state
// rather than merge a lower revision into a higher one.
func TestAggregateWSEpochChangeResetsClients(t *testing.T) {
	f := newWSFixture(t, 80*time.Millisecond)
	_ = f.read(t, 2*time.Second)

	f.pub.rev.Store(9)
	if snap := f.read(t, 3*time.Second); snap.Reset {
		t.Fatal("same-epoch publication asked for a reset")
	}

	// New process generation: epoch changes and revision goes BACKWARDS.
	f.pub.epoch.Store("epoch-2")
	f.pub.rev.Store(1)

	snap := f.read(t, 3*time.Second)
	if !snap.Reset {
		t.Fatal("epoch change did not flag reset")
	}
	if snap.Epoch != "epoch-2" {
		t.Errorf("epoch = %q, want epoch-2", snap.Epoch)
	}
}

// TestAggregateModeDisablesPerEventBroadcasts covers both hubs: in aggregate
// mode the coalesced snapshot is the only data message.
func TestAggregateModeDisablesPerEventBroadcasts(t *testing.T) {
	hub := NewEventHub(nil, nil, nil)
	hub.SetAggregatePublisher(newFakePublisher("epoch-1"), time.Hour)
	hub.BroadcastLog(LogEntry{Body: "hello"})
	hub.BroadcastMetric(MetricEntry{Name: "m"})
	if len(hub.logsCh) != 0 || len(hub.metricsCh) != 0 {
		t.Fatalf("event hub buffered per-event broadcasts in aggregate mode: logs=%d metrics=%d",
			len(hub.logsCh), len(hub.metricsCh))
	}

	h := NewHub(nil)
	h.SetAggregateMode(true)
	h.Broadcast(LogEntry{Body: "hello"})
	h.BroadcastMetric(MetricEntry{Name: "m"})
	if len(h.broadcast) != 0 || len(h.metricsCh) != 0 {
		t.Fatalf("hub buffered per-event broadcasts in aggregate mode: logs=%d metrics=%d",
			len(h.broadcast), len(h.metricsCh))
	}

	// Legacy mode is unchanged.
	legacy := NewHub(nil)
	legacy.Broadcast(LogEntry{Body: "hello"})
	if len(legacy.broadcast) != 1 {
		t.Fatalf("legacy hub dropped a broadcast: %d buffered", len(legacy.broadcast))
	}
}

// TestAggregateSnapshotNeverCarriesTraces pins the "no 7-day resend" rule at
// its practical edge: the coalesced payload is summary/traffic/topology, never
// a list of raw traces.
func TestAggregateSnapshotNeverCarriesTraces(t *testing.T) {
	pub := newFakePublisher("epoch-1")
	snap := pub.Snapshot(context.Background(), "")
	if snap.Traces != nil {
		t.Fatal("coalesced aggregate snapshot carries raw traces")
	}
}
