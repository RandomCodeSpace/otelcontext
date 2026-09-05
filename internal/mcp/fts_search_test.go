package mcp

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/storage"
)

func TestSearchLogsDefaultFTSPreservesMCPFiltersAndPagination(t *testing.T) {
	t.Setenv("LOG_FTS_ENABLED", "")
	if err := os.Unsetenv("LOG_FTS_ENABLED"); err != nil {
		t.Fatalf("unset LOG_FTS_ENABLED: %v", err)
	}
	db, err := storage.NewDatabase("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("NewDatabase: %v", err)
	}
	if err := storage.AutoMigrateModels(db, "sqlite"); err != nil {
		t.Fatalf("AutoMigrateModels: %v", err)
	}
	var ftsTableCount int64
	if err := db.Raw("SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'logs_fts'").Scan(&ftsTableCount).Error; err != nil {
		t.Fatalf("inspect logs_fts: %v", err)
	}
	if ftsTableCount != 1 {
		t.Fatalf("default migration created %d logs_fts tables, want 1", ftsTableCount)
	}

	repo := storage.NewRepositoryFromDB(db, "sqlite")
	t.Cleanup(func() { _ = repo.Close() })
	now := time.Now().UTC().Truncate(time.Second)
	rows := []storage.Log{
		{TenantID: "alpha", Severity: "ERROR", Body: "worker connected alpha first", ServiceName: "checkout", TraceID: "trace-target", Timestamp: now.Add(-2 * time.Minute)},
		{TenantID: "alpha", Severity: "ERROR", Body: "worker connected alpha second", ServiceName: "checkout", TraceID: "trace-target", Timestamp: now.Add(-time.Minute)},
		{TenantID: "beta", Severity: "ERROR", Body: "worker connected beta", ServiceName: "checkout", TraceID: "trace-target", Timestamp: now.Add(-time.Minute)},
		{TenantID: "alpha", Severity: "ERROR", Body: "worker connected wrong service", ServiceName: "payments", TraceID: "trace-target", Timestamp: now.Add(-time.Minute)},
		{TenantID: "alpha", Severity: "WARN", Body: "worker connected wrong severity", ServiceName: "checkout", TraceID: "trace-target", Timestamp: now.Add(-time.Minute)},
		{TenantID: "alpha", Severity: "ERROR", Body: "worker connected wrong trace", ServiceName: "checkout", TraceID: "trace-other", Timestamp: now.Add(-time.Minute)},
		{TenantID: "alpha", Severity: "ERROR", Body: "worker connected too old", ServiceName: "checkout", TraceID: "trace-target", Timestamp: now.Add(-25 * time.Hour)},
	}
	if err := repo.DB().Create(&rows).Error; err != nil {
		t.Fatalf("seed logs: %v", err)
	}

	srv := New("", repo, nil, nil)
	ctx := storage.WithTenantContext(context.Background(), "alpha")
	result := srv.toolSearchLogs(ctx, map[string]any{
		"query": "connections", "severity": "ERROR", "service": "checkout", "trace_id": "trace-target",
		"start": now.Add(-2 * time.Hour).Format(time.RFC3339), "end": now.Format(time.RFC3339),
		"limit": float64(1), "page": float64(1),
	})
	if result.IsError || len(result.Content) != 1 || result.Content[0].Resource == nil {
		t.Fatalf("search_logs result changed shape or errored: %+v", result)
	}
	var body struct {
		Total   int64 `json:"total"`
		Page    int   `json:"page"`
		Limit   int   `json:"limit"`
		Count   int   `json:"count"`
		Entries []struct {
			Severity    string `json:"severity"`
			ServiceName string `json:"service_name"`
			Body        string `json:"body"`
			TraceID     string `json:"trace_id"`
		} `json:"entries"`
	}
	if err := json.Unmarshal([]byte(result.Content[0].Resource.Text), &body); err != nil {
		t.Fatalf("decode search_logs resource: %v", err)
	}
	if body.Total != 2 || body.Page != 1 || body.Limit != 1 || body.Count != 1 || len(body.Entries) != 1 {
		t.Fatalf("filtered page metadata = %+v, want total=2 page=1 limit=1 count=1", body)
	}
	entry := body.Entries[0]
	if entry.Severity != "ERROR" || entry.ServiceName != "checkout" || entry.TraceID != "trace-target" {
		t.Fatalf("search_logs filters changed: %+v", entry)
	}
	if entry.Body != "worker connected alpha first" && entry.Body != "worker connected alpha second" {
		t.Fatalf("unexpected paged FTS entry: %+v", entry)
	}
}
