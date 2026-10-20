package api

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSQLiteReadinessProbeIndependentOfQueryPool(t *testing.T) {
	path := filepath.Join(t.TempDir(), "aggregate.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1)
	if _, err := db.Exec("PRAGMA journal_mode=WAL; CREATE TABLE sample (id INTEGER)"); err != nil {
		t.Fatal(err)
	}
	probe, err := NewSQLiteReadinessProbe(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = probe.Close() })
	// Holding every query-pool slot deterministically reproduces the wait a
	// long aggregate query causes, without a slow or machine-dependent scan.
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := db.PingContext(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("occupied query pool ping = %v, want deadline exceeded", err)
	}
	s := newTestServer(t)
	s.SetAggregateDBProbe(probe.PingContext)
	if code, checks := readyChecks(t, s); code != http.StatusOK || checks["aggregate_db"] != "ok" {
		t.Fatalf("ready with occupied query pool: code=%d checks=%v", code, checks)
	}
	// A successful reachability check cannot bypass an actual capacity or
	// persistence failure. Each still has its own readiness gate.
	s.SetAggregateRuntimeProbe(func() AggregateRuntime { return AggregateRuntime{AdmissionRatio: 1} })
	if code, checks := readyChecks(t, s); code != http.StatusServiceUnavailable || checks["aggregate_db"] != "ok" {
		t.Fatalf("ready with saturated writer: code=%d checks=%v", code, checks)
	}
	s.SetAggregateRuntimeProbe(func() AggregateRuntime { return AggregateRuntime{CommitFailureStreak: 3} })
	if code, checks := readyChecks(t, s); code != http.StatusServiceUnavailable || checks["aggregate_db"] != "ok" {
		t.Fatalf("ready with failing writer: code=%d checks=%v", code, checks)
	}
	s.SetAggregateRuntimeProbe(nil)
	if err := probe.Close(); err != nil {
		t.Fatal(err)
	}
	if code, checks := readyChecks(t, s); code != http.StatusServiceUnavailable || !strings.Contains(checks["aggregate_db"], "closed") {
		t.Fatalf("ready with closed probe: code=%d checks=%v", code, checks)
	}
}

func TestSQLiteReadinessProbeReadOnlyAndBounded(t *testing.T) {
	// URI escaping must preserve literal query/fragment characters in paths.
	path := filepath.Join(t.TempDir(), "aggregate ?#.db")
	uri := (&url.URL{Scheme: "file", Path: path}).String()
	db, err := sql.Open("sqlite", uri)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("CREATE TABLE sample (id INTEGER)"); err != nil {
		t.Fatal(err)
	}
	for _, source := range []string{path, uri} {
		probe, err := NewSQLiteReadinessProbe(source)
		if err != nil {
			t.Fatal(err)
		}
		defer probe.Close()
		if _, err := probe.db.Exec("INSERT INTO sample VALUES (1)"); err == nil {
			t.Fatal("probe connection accepted a write")
		}
		if got := probe.db.Stats().MaxOpenConnections; got != 1 {
			t.Fatalf("probe connections = %d, want 1", got)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := probe.PingContext(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled probe = %v", err)
		}
	}
}

func TestSQLiteReadinessProbeFailsForUnavailableDatabase(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing.db")
	if p, err := NewSQLiteReadinessProbe(missing); err == nil {
		_ = p.Close()
		t.Fatal("opened missing database")
	}
	if _, err := os.Stat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("readiness probe created missing database: %v", err)
	}
	broken := filepath.Join(t.TempDir(), "broken.db")
	if err := os.WriteFile(broken, []byte(strings.Repeat("not a database", 100)), 0600); err != nil {
		t.Fatal(err)
	}
	if p, err := NewSQLiteReadinessProbe(broken); err == nil {
		_ = p.Close()
		t.Fatal("probe accepted corrupt database")
	}
	for _, path := range []string{"", ":memory:", "file::memory:?cache=shared", "file:test?mode=memory&cache=shared"} {
		if p, err := NewSQLiteReadinessProbe(path); err == nil {
			_ = p.Close()
			t.Fatalf("probe accepted non-disk database %q", path)
		}
	}
}
