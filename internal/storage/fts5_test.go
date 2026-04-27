package storage

import (
	"context"
	"strings"
	"testing"
	"time"
)

// withTenant attaches a tenant id to ctx — tests use this so that the
// tenant_id WHERE clause matches the explicit tenant we set on each row,
// rather than the implicit "default".
func withTenant(ctx context.Context, t string) context.Context {
	return WithTenantContext(ctx, t)
}

// TestFTS5MatchExpr_Empty verifies the empty/whitespace short-circuit so the
// caller knows to skip MATCH attachment.
func TestFTS5MatchExpr_Empty(t *testing.T) {
	cases := []string{"", "   ", "\t\n"}
	for _, c := range cases {
		if got := fts5MatchExpr(c); got != "" {
			t.Fatalf("fts5MatchExpr(%q) = %q, want empty", c, got)
		}
	}
}

// TestFTS5MatchExpr_Quoting verifies that user input is quoted and prefix-suffixed.
func TestFTS5MatchExpr_Quoting(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"connection", `"connection"*`},
		{"foo bar", `"foo"* "bar"*`},
		{`he said "hi"`, `"he"* "said"* """hi"""*`},
		{"  panic  ", `"panic"*`},
	}
	for _, c := range cases {
		if got := fts5MatchExpr(c.in); got != c.want {
			t.Fatalf("fts5MatchExpr(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestFTS5Available_DriverGate verifies that only sqlite uses the FTS5 path.
func TestFTS5Available_DriverGate(t *testing.T) {
	cases := []struct {
		driver string
		want   bool
	}{
		{"sqlite", true},
		{"SQLITE", true},
		{"", false},
		{"postgres", false},
		{"mysql", false},
		{"mssql", false},
	}
	for _, c := range cases {
		if got := fts5Available(c.driver); got != c.want {
			t.Fatalf("fts5Available(%q) = %v, want %v", c.driver, got, c.want)
		}
	}
}

// TestSearchLogs_FTS5_BM25_Ordering verifies that BM25 puts the more relevant
// row first (more occurrences of the query token = lower BM25 score = higher rank).
func TestSearchLogs_FTS5_BM25_Ordering(t *testing.T) {
	repo := newTestRepo(t)
	now := time.Now().UTC()
	rows := []Log{
		{TenantID: "default", Severity: "ERROR", Body: "connection error connection lost connection refused", ServiceName: "api", Timestamp: now.Add(-3 * time.Second)},
		{TenantID: "default", Severity: "INFO", Body: "service started", ServiceName: "api", Timestamp: now.Add(-2 * time.Second)},
		{TenantID: "default", Severity: "WARN", Body: "lost connection to upstream", ServiceName: "api", Timestamp: now.Add(-1 * time.Second)},
	}
	if err := repo.db.Create(&rows).Error; err != nil {
		t.Fatalf("seed: %v", err)
	}
	logs, err := repo.SearchLogs(context.Background(), "connection", 10)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(logs) != 2 {
		t.Fatalf("want 2 BM25 matches, got %d", len(logs))
	}
	if !strings.Contains(logs[0].Body, "connection error connection lost connection refused") {
		t.Fatalf("BM25 should rank triple-occurrence row first; got %q", logs[0].Body)
	}
}

// TestSearchLogs_FTS5_PrefixMatch verifies that "conn" matches "connection"
// thanks to the trailing `*` wildcard the helper appends.
func TestSearchLogs_FTS5_PrefixMatch(t *testing.T) {
	repo := newTestRepo(t)
	repo.db.Create(&[]Log{
		{TenantID: "default", Severity: "ERROR", Body: "connection refused", ServiceName: "api", Timestamp: time.Now().UTC()},
		{TenantID: "default", Severity: "INFO", Body: "kernel panic", ServiceName: "k", Timestamp: time.Now().UTC()},
	})
	logs, err := repo.SearchLogs(context.Background(), "conn", 10)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(logs) != 1 {
		t.Fatalf("want 1 prefix match for 'conn', got %d", len(logs))
	}
}

// TestSearchLogs_FTS5_PorterStemming verifies the porter tokenizer treats
// "panic" and "panicked" as the same stem.
func TestSearchLogs_FTS5_PorterStemming(t *testing.T) {
	repo := newTestRepo(t)
	repo.db.Create(&[]Log{
		{TenantID: "default", Severity: "ERROR", Body: "the worker panicked at startup", ServiceName: "w", Timestamp: time.Now().UTC()},
		{TenantID: "default", Severity: "INFO", Body: "all good", ServiceName: "w", Timestamp: time.Now().UTC()},
	})
	logs, err := repo.SearchLogs(context.Background(), "panic", 10)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(logs) != 1 {
		t.Fatalf("porter stem 'panic' should match 'panicked'; got %d rows", len(logs))
	}
}

// TestSearchLogs_FTS5_TenantIsolation verifies the tenant_id WHERE is honored
// so a tenant cannot read another tenant's matching rows via FTS5.
func TestSearchLogs_FTS5_TenantIsolation(t *testing.T) {
	repo := newTestRepo(t)
	repo.db.Create(&[]Log{
		{TenantID: "alpha", Severity: "ERROR", Body: "secret to alpha: connection lost", ServiceName: "api", Timestamp: time.Now().UTC()},
		{TenantID: "beta", Severity: "ERROR", Body: "secret to beta: connection lost", ServiceName: "api", Timestamp: time.Now().UTC()},
	})

	logs, err := repo.SearchLogs(withTenant(context.Background(), "alpha"), "connection", 10)
	if err != nil {
		t.Fatalf("search alpha: %v", err)
	}
	if len(logs) != 1 || logs[0].TenantID != "alpha" {
		t.Fatalf("tenant alpha should see only its row; got %d rows (first tenant=%q)", len(logs), firstTenant(logs))
	}
	logs, err = repo.SearchLogs(withTenant(context.Background(), "beta"), "connection", 10)
	if err != nil {
		t.Fatalf("search beta: %v", err)
	}
	if len(logs) != 1 || logs[0].TenantID != "beta" {
		t.Fatalf("tenant beta should see only its row; got %d rows", len(logs))
	}
}

func firstTenant(l []Log) string {
	if len(l) == 0 {
		return ""
	}
	return l[0].TenantID
}

// TestSearchLogs_FTS5_DeleteTriggerKeepsIndexInSync verifies that a hard
// DELETE on logs propagates through the AFTER DELETE trigger so the row no
// longer appears in MATCH results — required for retention purges.
func TestSearchLogs_FTS5_DeleteTriggerKeepsIndexInSync(t *testing.T) {
	repo := newTestRepo(t)
	old := time.Now().UTC().Add(-30 * 24 * time.Hour)
	row := Log{TenantID: "default", Severity: "ERROR", Body: "rare error string xyzzy", ServiceName: "s", Timestamp: old}
	if err := repo.db.Create(&row).Error; err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Confirm row is searchable via FTS5.
	logs, err := repo.SearchLogs(context.Background(), "xyzzy", 10)
	if err != nil {
		t.Fatalf("pre-delete search: %v", err)
	}
	if len(logs) != 1 {
		t.Fatalf("want 1 row before delete, got %d", len(logs))
	}
	// Drop via the retention path — exercises GORM hard delete and the AFTER
	// DELETE trigger.
	if _, err := repo.PurgeLogsBatched(context.Background(), time.Now().UTC().Add(-1*time.Hour), 100, time.Millisecond); err != nil {
		t.Fatalf("purge: %v", err)
	}
	logs, err = repo.SearchLogs(context.Background(), "xyzzy", 10)
	if err != nil {
		t.Fatalf("post-delete search: %v", err)
	}
	if len(logs) != 0 {
		t.Fatalf("FTS index should have dropped row after delete trigger; got %d rows", len(logs))
	}
}

// TestSearchLogs_FTS5_SpecialCharsDoNotPanic verifies that user input
// containing FTS5 query operators is escaped and does not produce a syntax
// error or fall through to a 500.
func TestSearchLogs_FTS5_SpecialCharsDoNotPanic(t *testing.T) {
	repo := newTestRepo(t)
	repo.db.Create(&Log{TenantID: "default", Severity: "INFO", Body: "ok", ServiceName: "s", Timestamp: time.Now().UTC()})
	cases := []string{
		`AND OR NOT`,
		`"`,
		`* + -`,
		`{ } ( )`,
		`zzzz-no-such-term`,
	}
	for _, q := range cases {
		t.Run(q, func(t *testing.T) {
			if _, err := repo.SearchLogs(context.Background(), q, 10); err != nil {
				t.Fatalf("special-char query %q errored: %v", q, err)
			}
		})
	}
}

// TestGetLogsV2_FTS5_OrdersByBM25 verifies that GetLogsV2's search path also
// orders results by BM25 relevance on SQLite.
func TestGetLogsV2_FTS5_OrdersByBM25(t *testing.T) {
	repo := newTestRepo(t)
	now := time.Now().UTC()
	repo.db.Create(&[]Log{
		{TenantID: "default", Severity: "ERROR", Body: "panic: nil pointer dereference at panic at panic", ServiceName: "api", TraceID: "t1", Timestamp: now},
		{TenantID: "default", Severity: "INFO", Body: "no issues", ServiceName: "api", TraceID: "t2", Timestamp: now},
		{TenantID: "default", Severity: "WARN", Body: "panic recovered", ServiceName: "api", TraceID: "t3", Timestamp: now},
	})
	logs, total, err := repo.GetLogsV2(context.Background(), LogFilter{Search: "panic", Limit: 10})
	if err != nil {
		t.Fatalf("GetLogsV2: %v", err)
	}
	if total != 2 || len(logs) != 2 {
		t.Fatalf("want 2/2 got %d/%d", total, len(logs))
	}
	if logs[0].TraceID != "t1" {
		t.Fatalf("BM25 should rank triple-occurrence row first; got TraceID=%q", logs[0].TraceID)
	}
}

// TestGetLogsV2_FTS5_FiltersStillApply verifies that ServiceName and Severity
// filters compose correctly on top of FTS5 MATCH.
func TestGetLogsV2_FTS5_FiltersStillApply(t *testing.T) {
	repo := newTestRepo(t)
	now := time.Now().UTC()
	repo.db.Create(&[]Log{
		{TenantID: "default", Severity: "ERROR", Body: "connection lost", ServiceName: "api", Timestamp: now},
		{TenantID: "default", Severity: "INFO", Body: "connection ok", ServiceName: "api", Timestamp: now},
		{TenantID: "default", Severity: "ERROR", Body: "connection lost", ServiceName: "auth", Timestamp: now},
	})
	logs, total, err := repo.GetLogsV2(context.Background(), LogFilter{Search: "connection", Severity: "ERROR", ServiceName: "api", Limit: 10})
	if err != nil {
		t.Fatalf("GetLogsV2: %v", err)
	}
	if total != 1 || len(logs) != 1 {
		t.Fatalf("want 1 ERROR+api+'connection' row; got %d/%d", total, len(logs))
	}
	if logs[0].ServiceName != "api" || logs[0].Severity != "ERROR" {
		t.Fatalf("filter mismatch: service=%q severity=%q", logs[0].ServiceName, logs[0].Severity)
	}
}

// TestSetupSQLiteFTS5_Idempotent verifies the setup is safe to re-run on a DB
// that already has the table + triggers.
func TestSetupSQLiteFTS5_Idempotent(t *testing.T) {
	repo := newTestRepo(t)
	// newTestRepo already ran setupSQLiteFTS5 via AutoMigrateModels; running
	// it a second time must not error or duplicate triggers.
	if err := setupSQLiteFTS5(repo.db); err != nil {
		t.Fatalf("re-run setupSQLiteFTS5: %v", err)
	}
	repo.db.Create(&Log{TenantID: "default", Severity: "INFO", Body: "second-run sentinel", ServiceName: "s", Timestamp: time.Now().UTC()})
	logs, err := repo.SearchLogs(context.Background(), "sentinel", 10)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(logs) != 1 {
		t.Fatalf("expected 1 row after idempotent re-setup; got %d", len(logs))
	}
}
