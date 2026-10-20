package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/migrate"
	"github.com/RandomCodeSpace/otelcontext/internal/storage"
)

func TestSeedCorrelationAndRetention(t *testing.T) {
	end := time.Date(2026, 1, 10, 12, 0, 0, 0, time.UTC)
	out := filepath.Join(t.TempDir(), "raw")
	m, err := seed(options{out, 2, 2, "loadsim-svc-", end})
	if err != nil {
		t.Fatal(err)
	}
	if m.Traces != 18 || m.Spans != 36 || m.Logs != 36 || m.RetainedTraces != 16 || m.ExpiredTraces != 2 {
		t.Fatalf("wrong counts: %+v", m)
	}
	db, err := storage.NewDatabase("sqlite", m.Database)
	if err != nil {
		t.Fatal(err)
	}
	repo := storage.NewRepositoryFromDB(db, "sqlite")
	defer repo.Close()
	ctx := context.Background()
	if _, err := migrate.RequireExact(ctx, db, "sqlite"); err != nil {
		t.Fatal(err)
	}
	for service := 0; service < 2; service++ {
		for point, cp := range m.Checkpoints {
			trace, err := repo.GetTrace(ctx, traceID(end, service, point))
			if err != nil {
				t.Fatal(err)
			}
			if len(trace.Spans) != 2 || len(trace.Logs) != 2 || !trace.Timestamp.Equal(cp.Timestamp) {
				t.Fatalf("bad correlated exemplar: %+v", trace)
			}
			if trace.Truncated == nil || *trace.Truncated || *trace.RetainedSpanCount != 2 {
				t.Fatal("bad completeness metadata")
			}
			for _, log := range trace.Logs {
				if log.TraceID != trace.TraceID || log.Timestamp.After(end) {
					t.Fatal("invalid log correlation/time")
				}
				var attrs map[string]any
				if err := json.Unmarshal([]byte(log.AttributesJSON), &attrs); err != nil {
					t.Fatal(err)
				}
				if attrs["fixture.kind"] != cp.Kind || attrs["host.memory.total"] != float64(int64(32)<<30) {
					t.Fatalf("bad attributes: %v", attrs)
				}
			}
		}
	}
	n, err := repo.PurgeTracesBatched(ctx, m.Cutoff, 1, 0)
	if err != nil || n != 2 {
		t.Fatalf("purged %d traces: %v", n, err)
	}
	n, err = repo.PurgeLogsBatched(ctx, m.Cutoff, 1, 0)
	if err != nil || n != 4 {
		t.Fatalf("purged %d logs: %v", n, err)
	}
	for table, want := range map[string]int64{"traces": 16, "spans": 32, "logs": 32} {
		var got int64
		if err := db.Table(table).Count(&got).Error; err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("%s=%d want %d", table, got, want)
		}
	}
	if _, err := repo.GetTrace(ctx, m.Checkpoints[0].ExampleTraceID); err == nil {
		t.Fatal("expired sentinel survived retention")
	}
	if _, err := repo.GetTrace(ctx, m.Checkpoints[1].ExampleTraceID); err != nil {
		t.Fatal("oldest retained exemplar disappeared:", err)
	}
}

func TestSeedRefusesExistingOutput(t *testing.T) {
	out := t.TempDir()
	sentinel := filepath.Join(out, "user-data")
	if err := os.WriteFile(sentinel, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := seed(options{out, 2, 1, "loadsim-svc-", time.Now().Add(-time.Minute).Truncate(time.Second)})
	if err == nil {
		t.Fatal("accepted existing fixture directory")
	}
	data, err := os.ReadFile(sentinel)
	if err != nil || string(data) != "keep" {
		t.Fatalf("existing data changed: %q %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(out, "raw.db")); !os.IsNotExist(err) {
		t.Fatal("created DB inside existing output")
	}
}

func TestSeedRejectsInvalidOptionsBeforeCreatingFiles(t *testing.T) {
	for _, o := range []options{
		{services: 0}, {services: 201}, {services: 2, hosts: 0},
		{services: 2, hosts: 1, prefix: "loadsim-svc-", end: time.Now().Add(time.Hour)},
	} {
		o.out = filepath.Join(t.TempDir(), "raw")
		if _, err := seed(o); err == nil {
			t.Fatalf("accepted %+v", o)
		}
		if _, err := os.Stat(o.out); !os.IsNotExist(err) {
			t.Fatal("created output for invalid options")
		}
	}
}
