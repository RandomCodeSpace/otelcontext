package storage

import (
	"testing"
	"time"
)

func TestMigrateTimeoutFromEnv(t *testing.T) {
	cases := []struct {
		name string
		env  string
		want time.Duration
	}{
		{"default when unset", "", 60 * time.Second},
		{"explicit 30s", "30", 30 * time.Second},
		{"explicit 0 = opt out", "0", 0},
		{"negative falls back", "-5", 60 * time.Second},
		{"malformed falls back", "abc", 60 * time.Second},
		{"caps at 1h", "7200", time.Hour},
		{"surrounding whitespace", "  120  ", 120 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.env == "" {
				t.Setenv("DB_MIGRATE_TIMEOUT_SECS", "")
			} else {
				t.Setenv("DB_MIGRATE_TIMEOUT_SECS", tc.env)
			}
			if got := migrateTimeoutFromEnv(); got != tc.want {
				t.Errorf("env=%q -> got %v, want %v", tc.env, got, tc.want)
			}
		})
	}
}

// TestAutoMigrateModels_RespectsTimeout exercises the timeout-bounded path
// against an in-memory SQLite DB. SQLite migrations are too fast to actually
// trip the deadline, so this test asserts: (a) Timeout > 0 doesn't break
// the happy path, (b) Timeout=0 still works (legacy unbounded behaviour).
//
// A real Postgres lock-contention test would need a live Postgres + advisory
// lock and lives in pg_integration_test.go behind an external-DB build tag.
func TestAutoMigrateModels_RespectsTimeout(t *testing.T) {
	t.Run("WithTimeout", func(t *testing.T) {
		db, err := NewDatabase("sqlite", ":memory:")
		if err != nil {
			t.Fatalf("NewDatabase: %v", err)
		}
		defer func() { sqlDB, _ := db.DB(); _ = sqlDB.Close() }()
		opts := MigrateOptions{Timeout: 30 * time.Second}
		if err := AutoMigrateModelsWithOptions(db, "sqlite", opts); err != nil {
			t.Fatalf("AutoMigrate with timeout: %v", err)
		}
		if !db.Migrator().HasTable("traces") {
			t.Fatalf("expected traces table to be created")
		}
	})

	t.Run("ZeroTimeoutLegacyPath", func(t *testing.T) {
		db, err := NewDatabase("sqlite", ":memory:")
		if err != nil {
			t.Fatalf("NewDatabase: %v", err)
		}
		defer func() { sqlDB, _ := db.DB(); _ = sqlDB.Close() }()
		opts := MigrateOptions{Timeout: 0}
		if err := AutoMigrateModelsWithOptions(db, "sqlite", opts); err != nil {
			t.Fatalf("AutoMigrate without timeout: %v", err)
		}
		if !db.Migrator().HasTable("traces") {
			t.Fatalf("expected traces table to be created")
		}
	})
}
