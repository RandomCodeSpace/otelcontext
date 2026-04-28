package storage

import (
	"fmt"
	"testing"
	"time"

	"gorm.io/gorm"
)

// newTestRepo builds a Repository backed by an in-memory SQLite DB with all models migrated.
// Tests live in the same package so they can poke unexported fields.
func newTestRepo(t *testing.T) *Repository {
	t.Helper()
	db, err := NewDatabase("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("NewDatabase: %v", err)
	}
	if err := AutoMigrateModels(db, "sqlite"); err != nil {
		t.Fatalf("AutoMigrateModels: %v", err)
	}
	repo := &Repository{db: db, driver: "sqlite"}
	t.Cleanup(func() { _ = repo.Close() })
	return repo
}

// seedLogs inserts N logs at the given timestamp.
func seedLogs(t *testing.T, db *gorm.DB, n int, ts time.Time, service string) {
	t.Helper()
	logs := make([]Log, n)
	for i := range n {
		logs[i] = Log{
			TraceID:     "trace-xxxx",
			SpanID:      "span-yyyy",
			Severity:    "INFO",
			Body:        "test log body " + service,
			ServiceName: service,
			Timestamp:   ts,
		}
	}
	if err := db.CreateInBatches(logs, 500).Error; err != nil {
		t.Fatalf("seedLogs: %v", err)
	}
}

// seedTrace inserts a trace and its spans. Span timestamps are independent of the
// trace timestamp to allow clock-skew scenarios.
func seedTrace(t *testing.T, db *gorm.DB, traceID string, traceTS time.Time, spanStartTimes []time.Time) {
	t.Helper()
	tr := Trace{
		TraceID:     traceID,
		ServiceName: "svc",
		Duration:    1000,
		Status:      "OK",
		Timestamp:   traceTS,
	}
	if err := db.Create(&tr).Error; err != nil {
		t.Fatalf("seedTrace trace: %v", err)
	}
	spans := make([]Span, len(spanStartTimes))
	for i, st := range spanStartTimes {
		spans[i] = Span{
			TraceID: traceID,
			// Each span needs a distinct SpanID — the composite uniqueIndex
			// idx_spans_tenant_trace_span on (tenant_id, trace_id, span_id)
			// would otherwise collapse all spans for this trace into one row.
			SpanID:        fmt.Sprintf("%s-span-%d", traceID, i),
			OperationName: "op",
			StartTime:     st,
			EndTime:       st.Add(time.Millisecond),
			Duration:      1000,
			ServiceName:   "svc",
		}
	}
	if len(spans) > 0 {
		if err := db.CreateInBatches(spans, 500).Error; err != nil {
			t.Fatalf("seedTrace spans: %v", err)
		}
	}
}

func mustCount(t *testing.T, db *gorm.DB, model any) int64 {
	t.Helper()
	var n int64
	if err := db.Model(model).Count(&n).Error; err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}
