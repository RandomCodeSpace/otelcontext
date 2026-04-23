package storage

import (
	"context"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"
)

func TestLikeOpFor_Dialects(t *testing.T) {
	cases := []struct {
		driver string
		want   string
	}{
		{"postgres", "ILIKE"},
		{"Postgres", "ILIKE"},
		{"postgresql", "ILIKE"},
		{"POSTGRESQL", "ILIKE"},
		{"sqlite", "LIKE"},
		{"", "LIKE"},
		{"mysql", "LIKE"},
		{"mssql", "LIKE"},
		{"sqlserver", "LIKE"},
		{"unknown", "LIKE"},
	}
	for _, c := range cases {
		t.Run(c.driver, func(t *testing.T) {
			got := likeOpFor(c.driver)
			if got != c.want {
				t.Fatalf("likeOpFor(%q) = %q, want %q", c.driver, got, c.want)
			}
		})
	}
}

func TestRepository_likeOp(t *testing.T) {
	r := &Repository{driver: "postgres"}
	if r.likeOp() != "ILIKE" {
		t.Fatal("Repository.likeOp() for postgres")
	}
	r.driver = "sqlite"
	if r.likeOp() != "LIKE" {
		t.Fatal("Repository.likeOp() for sqlite")
	}
}

func TestSearchLogs_PlainBody_Roundtrip(t *testing.T) {
	repo := newTestRepo(t)
	seed := []Log{
		{Severity: "ERROR", Body: "database connection refused", ServiceName: "api", Timestamp: time.Now().UTC()},
		{Severity: "INFO", Body: "user login succeeded", ServiceName: "auth", Timestamp: time.Now().UTC()},
		{Severity: "WARN", Body: "cache miss for key xyz", ServiceName: "cache", Timestamp: time.Now().UTC()},
	}
	if err := repo.db.Create(&seed).Error; err != nil {
		t.Fatalf("seed: %v", err)
	}

	logs, err := repo.SearchLogs(context.Background(), "connection", 10)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(logs) != 1 {
		t.Fatalf("want 1 match, got %d", len(logs))
	}
	if !strings.Contains(logs[0].Body, "connection") {
		t.Fatalf("wrong row returned: %q", logs[0].Body)
	}
}

func TestSearchLogs_EmptyQuery_ReturnsRecent(t *testing.T) {
	repo := newTestRepo(t)
	for i := 0; i < 5; i++ {
		repo.db.Create(&Log{Severity: "INFO", Body: "x", ServiceName: "s", Timestamp: time.Now().UTC().Add(time.Duration(i) * time.Second)})
	}

	logs, err := repo.SearchLogs(context.Background(), "", 3)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(logs) != 3 {
		t.Fatalf("want 3 rows, got %d", len(logs))
	}
}

func TestSearchLogs_NoMatches(t *testing.T) {
	repo := newTestRepo(t)
	repo.db.Create(&Log{Severity: "INFO", Body: "alpha", ServiceName: "s", Timestamp: time.Now().UTC()})
	logs, _ := repo.SearchLogs(context.Background(), "zzzz-no-such-term", 10)
	if len(logs) != 0 {
		t.Fatalf("expected 0 matches, got %d", len(logs))
	}
}

func TestGetLogsV2_SearchFilter(t *testing.T) {
	repo := newTestRepo(t)
	now := time.Now().UTC()
	repo.db.Create(&[]Log{
		{Severity: "ERROR", Body: "panic: nil pointer", ServiceName: "api", TraceID: "t1", Timestamp: now},
		{Severity: "INFO", Body: "ok", ServiceName: "api", TraceID: "t2", Timestamp: now},
	})
	logs, total, err := repo.GetLogsV2(context.Background(), LogFilter{Search: "panic", Limit: 10})
	if err != nil {
		t.Fatalf("GetLogsV2: %v", err)
	}
	if total != 1 || len(logs) != 1 {
		t.Fatalf("want 1/1 got %d/%d", len(logs), total)
	}
}

func TestCompressedText_RoundTrip(t *testing.T) {
	repo := newTestRepo(t)
	// Log body is now plain text; AttributesJSON and AIInsight remain CompressedText.
	aiInsightText := "this is a long AI insight: " + strings.Repeat("deadbeef ", 100)
	attrJSON := `{"foo":"bar","baz":42}`

	orig := Log{
		Severity:       "INFO",
		Body:           "body",
		ServiceName:    "s",
		AttributesJSON: CompressedText(attrJSON),
		AIInsight:      CompressedText(aiInsightText),
		Timestamp:      time.Now().UTC(),
	}
	if err := repo.db.Create(&orig).Error; err != nil {
		t.Fatalf("create: %v", err)
	}

	var got Log
	if err := repo.db.First(&got, orig.ID).Error; err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got.AttributesJSON) != attrJSON {
		t.Fatalf("attrs mismatch:\n got=%q\nwant=%q", got.AttributesJSON, attrJSON)
	}
	if string(got.AIInsight) != aiInsightText {
		t.Fatalf("ai insight mismatch (compression round-trip broken)")
	}
}

func TestCompressedText_EmptyString(t *testing.T) {
	repo := newTestRepo(t)
	orig := Log{Severity: "INFO", Body: "b", ServiceName: "s", AttributesJSON: "", Timestamp: time.Now().UTC()}
	if err := repo.db.Create(&orig).Error; err != nil {
		t.Fatalf("create: %v", err)
	}
	var got Log
	repo.db.First(&got, orig.ID)
	if string(got.AttributesJSON) != "" {
		t.Fatalf("empty round-trip: %q", got.AttributesJSON)
	}
}

func TestCompressedText_GormDBDataType(t *testing.T) {
	var ct CompressedText
	// Construct a fake gorm.DB per dialect to query DB data type mapping.
	// Use the dialector Name via an open DB.
	cases := []struct {
		driver string
		want   string
	}{
		{"sqlite", "blob"},
	}
	for _, c := range cases {
		db, err := NewDatabase(c.driver, ":memory:")
		if err != nil {
			t.Fatalf("open %s: %v", c.driver, err)
		}
		got := ct.GormDBDataType(db, nil)
		if got != c.want {
			t.Fatalf("%s GormDBDataType = %s want %s", c.driver, got, c.want)
		}
		_ = closeDB(db)
	}
}

func TestAutoMigrateEnabled_EnvParsing(t *testing.T) {
	cases := []struct {
		env   string
		unset bool
		want  bool
	}{
		{env: "", unset: true, want: true},
		{env: "", want: true},
		{env: "true", want: true},
		{env: "TRUE", want: true},
		{env: "1", want: true},
		{env: "yes", want: true},
		{env: "on", want: true},
		{env: "false", want: false},
		{env: "FALSE", want: false},
		{env: "0", want: false},
		{env: "off", want: false},
		{env: "no", want: false},
		{env: "  false  ", want: false},
		{env: "garbage", want: true}, // unknown values default to true (safe)
	}
	for _, c := range cases {
		name := c.env
		if c.unset {
			name = "(unset)"
		}
		t.Run(name, func(t *testing.T) {
			if c.unset {
				t.Setenv("DB_AUTOMIGRATE", "")
				// t.Setenv with empty sets env var to "" which is not "unset";
				// LookupEnv returns true. Treat as present-but-empty → default true.
			} else {
				t.Setenv("DB_AUTOMIGRATE", c.env)
			}
			got := autoMigrateEnabled()
			if got != c.want {
				t.Fatalf("DB_AUTOMIGRATE=%q → %v, want %v", c.env, got, c.want)
			}
		})
	}
}

func closeDB(db *gorm.DB) error {
	sqlDB, err := db.DB()
	if err != nil {
		return err
	}
	return sqlDB.Close()
}

// Verify LIKE behaviour on SQLite is case-insensitive for ASCII text by default.
func TestSQLite_LIKE_CaseInsensitivityForASCII(t *testing.T) {
	repo := newTestRepo(t)
	repo.db.Create(&Log{Severity: "ERROR", Body: "Connection Refused", ServiceName: "s", Timestamp: time.Now().UTC()})

	logs, _ := repo.SearchLogs(context.Background(), "connection", 10)
	if len(logs) != 1 {
		t.Fatalf("SQLite ASCII LIKE should match case-insensitively; got %d rows", len(logs))
	}
}

// PurgeLogsBatched must respect a cancelled context without spinning indefinitely on the
// SQLite single-shot path when the deletion is large.
func TestPurgeLogs_DeadlineExceeded_ReturnsPromptly(t *testing.T) {
	repo := newTestRepo(t)
	old := time.Now().UTC().Add(-100 * 24 * time.Hour)
	seedLogs(t, repo.db, 1000, old, "svc")

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	done := make(chan struct{})
	go func() {
		_, _ = repo.PurgeLogsBatched(ctx, time.Now(), 10, 5*time.Millisecond)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("purge did not honour ctx timeout")
	}
}
