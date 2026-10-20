package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/storage"
)

func TestRetainedTraceNoisyLogsKeepGraph(t *testing.T) {
	repo := diskRepo(t, filepath.Join(t.TempDir(), "raw.db"))
	now := time.Now().UTC()
	trace := storage.Trace{TenantID: "alpha", TraceID: "noisy", Timestamp: now}
	root := storage.Span{TenantID: "alpha", TraceID: "noisy", SpanID: "root", StartTime: now}
	if err := repo.DB().Create(&trace).Error; err != nil {
		t.Fatal(err)
	}
	if err := repo.DB().Create(&root).Error; err != nil {
		t.Fatal(err)
	}
	logs := make([]storage.Log, 1100)
	for i := range logs {
		logs[i] = storage.Log{TenantID: "alpha", TraceID: "noisy", SpanID: "root", Timestamp: now, Body: strings.Repeat("x", 4096)}
	}
	if err := repo.BatchCreateLogs(logs); err != nil {
		t.Fatal(err)
	}
	if err := repo.DB().Create(&storage.Log{TenantID: "beta", TraceID: "noisy", Timestamp: now, Body: "private beta evidence"}).Error; err != nil {
		t.Fatal(err)
	}
	srv := New("alpha", repo, nil, nil)
	ctx := storage.WithTenantContext(context.Background(), "alpha")
	res := srv.toolTraceGraph(ctx, map[string]any{"trace_id": "noisy"})
	out := decodeTraceGraph(t, res)
	if len(out.Spans) != 1 || out.Spans[0].ID != "root" || !out.Coverage.Complete {
		t.Fatalf("lost root graph: %+v", out)
	}
	body := res.Content[0].Resource.Text
	var payload struct {
		Logs          []logSummary `json:"logs"`
		LogsTruncated bool         `json:"logs_truncated"`
		LogsTotal     int64        `json:"logs_total"`
		LogsNote      string       `json:"logs_note"`
	}
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		t.Fatal(err)
	}
	if !payload.LogsTruncated || payload.LogsTotal != 1100 || len(payload.Logs) == 0 || len(payload.Logs) >= 1100 || !strings.Contains(payload.LogsNote, "search_logs") {
		t.Fatalf("missing bounded log coverage: count=%d total=%d truncated=%v note=%q", len(payload.Logs), payload.LogsTotal, payload.LogsTruncated, payload.LogsNote)
	}
	if len(body) > MaxToolResponseBytes || strings.Contains(body, "beta") {
		t.Fatal("response size or tenant isolation violated")
	}
	for i, log := range payload.Logs {
		if log.ID != logs[i].ID {
			t.Fatalf("unstable log order at %d: id=%d want=%d", i, log.ID, logs[i].ID)
		}
	}
	t.Logf("root graph retained; %d/%d logs; response %d bytes", len(payload.Logs), payload.LogsTotal, len(body))
}

func TestRetainedTraceBoundsSpansAndEscapedResponse(t *testing.T) {
	repo := diskRepo(t, filepath.Join(t.TempDir(), "raw.db"))
	now := time.Now().UTC()
	trace := storage.Trace{TenantID: "default", TraceID: "large"}
	spans := make([]storage.Span, retainedTraceLimits.Spans+1)
	for i := range spans {
		spans[i] = storage.Span{TenantID: "default", TraceID: "large", SpanID: fmt.Sprintf("%04d", i), ParentSpanID: "0000", StartTime: now, OperationName: strings.Repeat("\x00", 220)}
	}
	spans[0].ParentSpanID = ""
	logs := make([]storage.Log, retainedTraceLimits.Logs+1)
	for i := range logs {
		logs[i] = storage.Log{TenantID: "default", TraceID: "large", Timestamp: now, Body: strings.Repeat("\x00", 4096)}
	}
	if err := repo.BatchCreateAll([]storage.Trace{trace}, spans, logs); err != nil {
		t.Fatal(err)
	}
	srv := New("default", repo, nil, nil)
	res := srv.toolTraceGraph(context.Background(), map[string]any{"trace_id": "large"})
	out := decodeTraceGraph(t, res)
	if len(out.Spans) != retainedTraceLimits.Spans || out.Coverage.Complete || !out.Coverage.Truncated {
		t.Fatalf("bounded graph reported complete: spans=%d coverage=%+v", len(out.Spans), out.Coverage)
	}
	if out.Spans[0].ID != "0000" || len(res.Content[0].Resource.Text) > MaxToolResponseBytes {
		t.Fatal("root or escaped response bound lost")
	}
	t.Logf("bounded spans=%d; escaped response=%d bytes", len(out.Spans), len(res.Content[0].Resource.Text))
}

func TestRetainedTraceOversizedAttributesArePartial(t *testing.T) {
	repo := diskRepo(t, filepath.Join(t.TempDir(), "raw.db"))
	now := time.Now().UTC()
	trace := storage.Trace{TenantID: "default", TraceID: "attrs"}
	root := storage.Span{TenantID: "default", TraceID: "attrs", SpanID: "root", StartTime: now, AttributesJSON: storage.CompressedText(strings.Repeat("x", 2*retainedTraceLimits.SpanBytes))}
	if err := repo.BatchCreateAll([]storage.Trace{trace}, []storage.Span{root}, nil); err != nil {
		t.Fatal(err)
	}
	srv := New("default", repo, nil, nil)
	res := srv.toolTraceGraph(context.Background(), map[string]any{"trace_id": "attrs"})
	out := decodeTraceGraph(t, res)
	if len(out.Spans) != 1 || out.Spans[0].ID != "root" || out.Coverage.Complete || !out.Coverage.Truncated || !strings.Contains(out.Coverage.Note, "read limits") {
		t.Fatalf("hidden attribute loss: %+v", out)
	}
}
