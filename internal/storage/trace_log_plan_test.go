package storage_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/migrate"
	"github.com/RandomCodeSpace/otelcontext/internal/storage"
	"gorm.io/gorm"
)

func TestTraceLogSQLitePlan(t *testing.T) {
	t.Setenv("LOG_FTS_ENABLED", "true")
	db, err := storage.NewDatabase("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	repo := storage.NewRepositoryFromDB(db, "sqlite")
	t.Cleanup(func() { _ = repo.Close() })
	if _, err := migrate.Up(context.Background(), db, "sqlite"); err != nil {
		t.Fatal(err)
	}
	var version string
	if err := db.Raw("SELECT sqlite_version()").Scan(&version).Error; err != nil {
		t.Fatal(err)
	}
	t.Logf("Go=%s SQLite=%s canonical migration=4", runtime.Version(), version)

	const traceID = "826ad292590416212a4666581991cb1b"
	now := time.Date(2026, 10, 12, 21, 30, 0, 671483847, time.UTC)
	if err := repo.BatchCreateAll([]storage.Trace{{TenantID: "default", TraceID: traceID, Timestamp: now}}, nil, nil); err != nil {
		t.Fatal(err)
	}
	logs := make([]storage.Log, 512)
	for i := range logs {
		logs[i] = storage.Log{TenantID: "default", TraceID: fmt.Sprintf("unrelated-%04d", i), ServiceName: "loadsim-svc-100", Severity: "ERROR", Timestamp: now.Add(time.Duration(i-256) * time.Second), Body: "unrelated"}
	}
	if err := repo.BatchCreateLogs(logs); err != nil {
		t.Fatal(err)
	}
	logs = []storage.Log{
		{ID: 1<<53 + 3, TenantID: "default", TraceID: traceID, ServiceName: "loadsim-svc-100", Severity: "ERROR", Timestamp: now.Add(time.Nanosecond), Body: "third"},
		{ID: 1<<53 + 1, TenantID: "default", TraceID: traceID, ServiceName: "loadsim-svc-100", Severity: "ERROR", Timestamp: now, Body: "first"},
		{ID: 1<<53 + 2, TenantID: "default", TraceID: traceID, ServiceName: "loadsim-svc-100", Severity: "ERROR", Timestamp: now, Body: "second"},
		{ID: 1<<53 + 4, TenantID: "other", TraceID: traceID, ServiceName: "loadsim-svc-100", Severity: "ERROR", Timestamp: now.Add(-time.Nanosecond), Body: "other tenant"},
	}
	if err := repo.BatchCreateLogs(logs); err != nil {
		t.Fatal(err)
	}
	type query struct {
		dryRun bool
		sql    string
		vars   []any
	}
	var mu sync.Mutex
	var queries []query
	if err := db.Callback().Query().After("gorm:query").Register("test:capture_trace_log", func(tx *gorm.DB) {
		mu.Lock()
		defer mu.Unlock()
		queries = append(queries, query{tx.DryRun, tx.Statement.SQL.String(), append([]any(nil), tx.Statement.Vars...)})
	}); err != nil {
		t.Fatal(err)
	}
	for _, analyzed := range []bool{false, true} {
		t.Run(fmt.Sprintf("analyzed=%t", analyzed), func(t *testing.T) {
			if analyzed {
				if err := db.Exec("ANALYZE").Error; err != nil {
					t.Fatal(err)
				}
			}
			queries = nil
			out, err := repo.GetTraceBounded(context.Background(), traceID, storage.TraceReadLimits{Spans: 100, Logs: 100, SpanBytes: 262144, LogBytes: 262144})
			if err != nil {
				t.Fatal(err)
			}
			if out.TotalLogs != 3 || len(out.Logs) != 3 || out.LogsTruncated {
				t.Fatalf("unexpected logs: %+v", out)
			}
			for i, l := range out.Logs {
				if l.ID != uint(1<<53+1+i) {
					t.Fatalf("unstable integer ID prefix: %+v", out.Logs)
				}
			}
			if !out.Logs[2].Timestamp.Equal(now.Add(time.Nanosecond)) {
				t.Fatalf("lost nanoseconds: %v", out.Logs[2].Timestamp)
			}
			found, total, err := repo.GetLogsV2(context.Background(), storage.LogFilter{TraceID: traceID, ServiceName: "loadsim-svc-100", StartTime: now.Add(-7 * 24 * time.Hour), EndTime: now.Add(time.Hour), Limit: 10})
			if err != nil {
				t.Fatal(err)
			}
			if total != 3 || len(found) != 3 || found[0].Body != "third" {
				t.Fatalf("filtered search total=%d logs=%+v", total, found)
			}
			sqlDB, err := db.DB()
			if err != nil {
				t.Fatal(err)
			}
			explained, counts, payload := 0, 0, false
			for _, q := range queries {
				if !strings.Contains(q.sql, "logs") {
					continue
				}
				explained++
				if strings.HasPrefix(q.sql, "SELECT count(*)") {
					counts++
				}
				payload = payload || strings.HasPrefix(q.sql, "SELECT logs.id,")
				vars, err := json.Marshal(q.vars)
				if err != nil {
					t.Fatal(err)
				}
				t.Logf("SQL (builder_only=%t): %s\nARGS: %s", q.dryRun, q.sql, vars)
				rows, err := sqlDB.QueryContext(context.Background(), "EXPLAIN QUERY PLAN "+q.sql, q.vars...)
				if err != nil {
					t.Fatal(err)
				}
				var details []string
				for rows.Next() {
					var id, parent, unused int
					var detail string
					if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
						rows.Close()
						t.Fatal(err)
					}
					details = append(details, detail)
					t.Logf("PLAN %d %d %d %s", id, parent, unused, detail)
				}
				err = rows.Err()
				rows.Close()
				if err != nil {
					t.Fatal(err)
				}
				plan := strings.Join(details, "\n")
				if !strings.Contains(plan, "idx_logs_trace_id (trace_id=?)") {
					t.Errorf("missing selective trace lookup: %s", plan)
				}
				for _, d := range details {
					if strings.Contains(d, "SCAN logs") || (strings.Contains(d, "SEARCH logs") &&
						!strings.Contains(d, "idx_logs_trace_id (trace_id=?)") &&
						!strings.Contains(d, "INTEGER PRIMARY KEY (rowid=?)")) {
						t.Errorf("trace lookup walks unrelated logs: %s", d)
					}
				}
				if strings.HasPrefix(q.sql, "SELECT logs.id,") &&
					!strings.Contains(plan, "SEARCH logs USING INTEGER PRIMARY KEY (rowid=?)") {
					t.Errorf("payload lookup must seek by bounded candidate ID: %s", plan)
				}
			}
			if explained < 4 || counts != 2 || !payload {
				t.Fatalf("missing repository query coverage: explained=%d counts=%d payload=%t", explained, counts, payload)
			}
		})
	}
	if err := db.Callback().Query().Remove("test:capture_trace_log"); err != nil {
		t.Fatal(err)
	}

	t.Run("filtered_limits_and_cancellation", func(t *testing.T) {
		filter := storage.LogFilter{TraceID: traceID, ServiceName: "loadsim-svc-100", Severity: "ERROR", StartTime: now.Add(time.Nanosecond), EndTime: now.Add(time.Nanosecond), Limit: 1}
		for _, fts := range []bool{false, true} {
			t.Setenv("LOG_FTS_ENABLED", fmt.Sprint(fts))
			filter.Search = "third"
			found, total, err := repo.GetLogsV2(context.Background(), filter)
			if err != nil {
				t.Fatal(err)
			}
			if total != 1 || len(found) != 1 || found[0].ID != 1<<53+3 {
				t.Fatalf("FTS=%t filtered nanosecond boundary: total=%d logs=%+v", fts, total, found)
			}
		}
		filter.Search = ""
		filter.Offset = 1
		found, total, err := repo.GetLogsV2(context.Background(), filter)
		if err != nil || total != 1 || len(found) != 0 {
			t.Fatalf("offset changed count: %d %+v %v", total, found, err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := repo.GetTraceBounded(ctx, traceID, storage.TraceReadLimits{Spans: 1, Logs: 1, SpanBytes: 262144, LogBytes: 262144}); !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled trace read: %v", err)
		}
		if _, _, err := repo.GetLogsV2(ctx, filter); !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled log read: %v", err)
		}
		sqlDB, err := db.DB()
		if err != nil {
			t.Fatal(err)
		}
		if stats := sqlDB.Stats(); stats.MaxOpenConnections != 1 || stats.InUse != 0 {
			t.Fatalf("single connection not released: %+v", stats)
		}
		if err := sqlDB.PingContext(context.Background()); err != nil {
			t.Fatal(err)
		}
	})
}
