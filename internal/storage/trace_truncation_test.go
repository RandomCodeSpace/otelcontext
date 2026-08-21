package storage

import (
	"testing"
	"time"
)

// Exemplar truncation metadata (#163) round-trips through the real migration
// and upsert paths. The columns are nullable on purpose: NULL means "no
// truncation claim was made", which a reader must be able to tell apart from an
// explicit truncated=false.

func TestTraceTruncationMetadataRoundTrips(t *testing.T) {
	repo := newTestRepo(t)
	ts := time.Now().UTC()

	truncated := true
	retained, observed := 5, 137
	rows := []Trace{
		{TenantID: DefaultTenantID, TraceID: "aaaa", ServiceName: "checkout", Status: StatusCodeError, Timestamp: ts,
			Truncated: &truncated, RetainedSpanCount: &retained, ObservedSpanCount: &observed},
		{TenantID: DefaultTenantID, TraceID: "bbbb", ServiceName: "checkout", Status: "STATUS_CODE_OK", Timestamp: ts},
	}
	if err := repo.BatchCreateTraces(rows); err != nil {
		t.Fatalf("BatchCreateTraces: %v", err)
	}

	var got Trace
	if err := repo.db.Where("trace_id = ?", "aaaa").First(&got).Error; err != nil {
		t.Fatalf("load truncated trace: %v", err)
	}
	if got.Truncated == nil || !*got.Truncated {
		t.Fatalf("truncated = %v, want true", got.Truncated)
	}
	if got.RetainedSpanCount == nil || *got.RetainedSpanCount != retained {
		t.Fatalf("retained_span_count = %v, want %d", got.RetainedSpanCount, retained)
	}
	if got.ObservedSpanCount == nil || *got.ObservedSpanCount != observed {
		t.Fatalf("observed_span_count = %v, want %d", got.ObservedSpanCount, observed)
	}

	var plain Trace
	if err := repo.db.Where("trace_id = ?", "bbbb").First(&plain).Error; err != nil {
		t.Fatalf("load untruncated trace: %v", err)
	}
	if plain.Truncated != nil || plain.RetainedSpanCount != nil || plain.ObservedSpanCount != nil {
		t.Fatalf("trace with no truncation claim carries one: %+v", plain)
	}
}

// TestTraceTruncationLandsOnAlreadyInsertedRow is the case that matters in
// practice: a trace long enough to be truncated arrives across several batches,
// so the truncation only becomes known after the row already exists. The insert
// paths are first-writer-wins, so without the dedicated update pass the row
// would keep its NULL forever.
func TestTraceTruncationLandsOnAlreadyInsertedRow(t *testing.T) {
	repo := newTestRepo(t)
	ts := time.Now().UTC()

	first := []Trace{{TenantID: DefaultTenantID, TraceID: "cccc", ServiceName: "orders", Status: "STATUS_CODE_UNSET", Timestamp: ts}}
	if err := repo.BatchCreateTraces(first); err != nil {
		t.Fatalf("first BatchCreateTraces: %v", err)
	}

	truncated := true
	retained, observed := 500, 4212
	second := []Trace{{TenantID: DefaultTenantID, TraceID: "cccc", ServiceName: "orders", Status: StatusCodeError, Timestamp: ts,
		Truncated: &truncated, RetainedSpanCount: &retained, ObservedSpanCount: &observed}}
	if err := repo.BatchCreateTraces(second); err != nil {
		t.Fatalf("second BatchCreateTraces: %v", err)
	}

	var got Trace
	if err := repo.db.Where("trace_id = ?", "cccc").First(&got).Error; err != nil {
		t.Fatalf("load trace: %v", err)
	}
	if got.Truncated == nil || !*got.Truncated {
		t.Fatalf("truncated = %v, want true after the later batch", got.Truncated)
	}
	if got.ObservedSpanCount == nil || *got.ObservedSpanCount != observed {
		t.Fatalf("observed_span_count = %v, want %d", got.ObservedSpanCount, observed)
	}
	// Status upgrade must still work alongside the truncation update.
	if got.Status != StatusCodeError {
		t.Fatalf("status = %q, want %q", got.Status, StatusCodeError)
	}

	var count int64
	if err := repo.db.Model(&Trace{}).Where("trace_id = ?", "cccc").Count(&count).Error; err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Fatalf("trace row duplicated: %d rows", count)
	}
}
