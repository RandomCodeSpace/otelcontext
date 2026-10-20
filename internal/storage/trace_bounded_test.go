package storage

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"
)

func TestGetTraceBoundedLimitsAndIsolation(t *testing.T) {
	repo := newTestRepo(t)
	now := time.Now().UTC()
	for _, tenant := range []string{"alpha", "beta"} {
		trace := Trace{TenantID: tenant, TraceID: "shared", Timestamp: now}
		spans := []Span{
			{TenantID: tenant, TraceID: "shared", SpanID: "b", ParentSpanID: "root", StartTime: now.Add(time.Second)},
			{TenantID: tenant, TraceID: "shared", SpanID: "a", ParentSpanID: "root", StartTime: now.Add(time.Second)},
			{TenantID: tenant, TraceID: "shared", SpanID: "root", StartTime: now},
		}
		logs := []Log{
			{TenantID: tenant, TraceID: "shared", Body: tenant + " later", Timestamp: now.Add(time.Second)},
			{TenantID: tenant, TraceID: "shared", Body: tenant + " first", Timestamp: now},
			{TenantID: tenant, TraceID: "shared", Body: tenant + " second", Timestamp: now},
		}
		if err := repo.BatchCreateAll([]Trace{trace}, spans, logs); err != nil {
			t.Fatal(err)
		}
	}
	ctx := WithTenantContext(context.Background(), "alpha")
	limits := TraceReadLimits{Spans: 2, Logs: 2, SpanBytes: 4096, LogBytes: 4096}
	queries := 0
	if err := repo.db.Callback().Query().After("gorm:query").Register("test:bounded_payload", func(tx *gorm.DB) {
		// Observe rows actually scanned by GORM, before the caller can trim.
		if strings.Contains(tx.Statement.SQL.String(), "trace_budget") {
			queries++
			if tx.RowsAffected > 2 {
				t.Errorf("materialized %d rows before limiting", tx.RowsAffected)
			}
		}
	}); err != nil {
		t.Fatal(err)
	}
	out, err := repo.GetTraceBounded(ctx, "shared", limits)
	if err != nil {
		t.Fatal(err)
	}
	if queries != 2 {
		t.Fatalf("expected two bounded payload reads, got %d", queries)
	}
	if out.TotalSpans != 3 || out.TotalLogs != 3 || !out.SpansTruncated || !out.LogsTruncated {
		t.Fatalf("wrong tenant counts or bounds: %+v", out)
	}
	if len(out.Spans) != 2 || out.Spans[0].SpanID != "root" || out.Spans[1].SpanID != "a" {
		t.Fatalf("unstable span prefix: %+v", out.Spans)
	}
	if len(out.Logs) != 2 || out.Logs[0].Body != "alpha first" || out.Logs[1].Body != "alpha second" {
		t.Fatalf("unstable or cross-tenant logs: %+v", out.Logs)
	}
	full, err := repo.GetTrace(ctx, "shared")
	if err != nil {
		t.Fatal(err)
	}
	if len(full.Spans) != 3 || len(full.Logs) != 3 {
		t.Fatal("changed the existing GetTrace contract")
	}
	missing := WithTenantContext(ctx, "absent")
	if _, err := repo.GetTraceBounded(missing, "shared", limits); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("cross-tenant trace lookup: %v", err)
	}
}

func TestGetTraceBoundedBytesBeforeMaterialization(t *testing.T) {
	repo := newTestRepo(t)
	now := time.Now().UTC()
	trace := Trace{TenantID: "default", TraceID: "t"}
	spans := []Span{
		{TenantID: "default", TraceID: "t", SpanID: "root", StartTime: now},
		{TenantID: "default", TraceID: "t", SpanID: "large", ParentSpanID: "root", StartTime: now.Add(time.Second), OperationName: strings.Repeat("x", 4096)},
		{TenantID: "default", TraceID: "t", SpanID: "later", ParentSpanID: "root", StartTime: now.Add(2 * time.Second)},
	}
	// Byte counting must include UTF-8 and bytes after embedded NULs. Counting
	// characters or SQL text length would admit the second row incorrectly.
	logs := []Log{
		{TenantID: "default", TraceID: "t", Body: "first", Timestamp: now},
		{TenantID: "default", TraceID: "t", Body: strings.Repeat("界\x00", 200), Timestamp: now.Add(time.Second)},
		{TenantID: "default", TraceID: "t", Body: "later", Timestamp: now.Add(2 * time.Second)},
	}
	if err := repo.BatchCreateAll([]Trace{trace}, spans, logs); err != nil {
		t.Fatal(err)
	}
	// These fields are intentionally invalid compressed data. A full Log scan
	// would fail even if the caller discarded them after loading.
	if err := repo.db.Model(&Log{}).Where("trace_id = ?", "t").Updates(map[string]any{
		"attributes_json": []byte(zstdMagic + "invalid"), "ai_insight": []byte(zstdMagic + "invalid"),
	}).Error; err != nil {
		t.Fatal(err)
	}
	queries := 0
	if err := repo.db.Callback().Query().After("gorm:query").Register("test:byte_bound", func(tx *gorm.DB) {
		if strings.Contains(tx.Statement.SQL.String(), "trace_budget") {
			queries++
			if tx.RowsAffected != 1 {
				t.Errorf("payload query scanned %d rows, want 1", tx.RowsAffected)
			}
		}
	}); err != nil {
		t.Fatal(err)
	}
	out, err := repo.GetTraceBounded(context.Background(), "t", TraceReadLimits{Spans: 10, Logs: 10, SpanBytes: 512, LogBytes: 512})
	if err != nil {
		t.Fatal(err)
	}
	if queries != 2 || len(out.Spans) != 1 || len(out.Logs) != 1 || !out.SpansTruncated || !out.LogsTruncated {
		t.Fatalf("unbounded materialization: %+v; queries=%d", out, queries)
	}
	if out.Spans[0].SpanID != "root" || out.Logs[0].Body != "first" {
		t.Fatalf("not a stable prefix: %+v", out)
	}
	if out.TotalSpans != 3 || out.TotalLogs != 3 {
		t.Fatalf("wrong totals: %+v", out)
	}
}

func TestGetTraceBoundedAttributeExpansion(t *testing.T) {
	repo := newTestRepo(t)
	now := time.Now().UTC()
	trace := Trace{TenantID: "default", TraceID: "attrs"}
	spans := []Span{
		{TenantID: "default", TraceID: "attrs", SpanID: "root", StartTime: now, AttributesJSON: CompressedText(strings.Repeat("a", 1<<20))},
		{TenantID: "default", TraceID: "attrs", SpanID: "child", ParentSpanID: "root", StartTime: now.Add(time.Second), AttributesJSON: `{"small":true}`},
		{TenantID: "default", TraceID: "attrs", SpanID: "legacy", ParentSpanID: "root", StartTime: now.Add(2 * time.Second)},
	}
	if err := repo.BatchCreateAll([]Trace{trace}, spans, nil); err != nil {
		t.Fatal(err)
	}
	if err := repo.db.Model(&Span{}).Where("span_id = ?", "legacy").UpdateColumn("attributes_json", []byte(`{"legacy":true}`)).Error; err != nil {
		t.Fatal(err)
	}
	limits := TraceReadLimits{Spans: 10, Logs: 10, SpanBytes: 1024, LogBytes: 1024}
	out, err := repo.GetTraceBounded(context.Background(), "attrs", limits)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Spans) != 3 || !out.SpansTruncated || out.LogsTruncated {
		t.Fatalf("hidden attribute truncation: %+v", out)
	}
	if out.Spans[0].AttributesJSON != "" || out.Spans[1].AttributesJSON != `{"small":true}` || out.Spans[2].AttributesJSON != `{"legacy":true}` {
		t.Fatalf("attribute budget discarded the wrong fields: %+v", out.Spans)
	}
	// An exhausted decode budget must also omit attributes without failing.
	if err := repo.db.Model(&Span{}).Where("trace_id = ?", "attrs").UpdateColumn("attributes_json", CompressedText(strings.Repeat("b", 600))).Error; err != nil {
		t.Fatal(err)
	}
	out, err = repo.GetTraceBounded(context.Background(), "attrs", limits)
	if err != nil {
		t.Fatal(err)
	}
	total := 0
	for _, sp := range out.Spans {
		total += len(sp.AttributesJSON)
	}
	if total != 600 || !out.SpansTruncated {
		t.Fatalf("decoded attributes exceeded budget: bytes=%d truncated=%v", total, out.SpansTruncated)
	}
}
