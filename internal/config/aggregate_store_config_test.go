package config

import (
	"path/filepath"
	"strings"
	"testing"
)

// Durable aggregate store configuration (#173).

func TestValidate_AggregateStore_LegacyModeSkipsStoreChecks(t *testing.T) {
	c := baseValid()
	c.AggregateMode = "legacy"
	c.AggregateDBPath = ""
	c.AggregateSynchronous = "nonsense"
	c.AggregateCommitMaxWaiters = 0
	if err := c.Validate(); err != nil {
		t.Fatalf("legacy mode must not read the store config, got %v", err)
	}
}

func TestValidate_AggregateStore_Bounds(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{"empty path", func(c *Config) { c.AggregateDBPath = "" }, "AGGREGATE_DB_PATH"},
		{"bad synchronous", func(c *Config) { c.AggregateSynchronous = "OFF" }, "AGGREGATE_SYNCHRONOUS"},
		{"zero coalesce", func(c *Config) { c.AggregateCommitCoalesceMs = 0 }, "AGGREGATE_COMMIT_COALESCE_MS"},
		{"zero waiters", func(c *Config) { c.AggregateCommitMaxWaiters = 0 }, "AGGREGATE_COMMIT_MAX_WAITERS"},
		{"zero finalize", func(c *Config) { c.AggregateFinalizeIntervalSec = 0 }, "AGGREGATE_FINALIZE_INTERVAL_SEC"},
		{
			"pending deltas below batch target",
			func(c *Config) { c.AggregateCommitMaxPendingDeltas = 10 },
			"AGGREGATE_COMMIT_MAX_PENDING_DELTAS",
		},
		{
			"pending bytes below batch target",
			func(c *Config) { c.AggregateCommitMaxPendingBytes = 1024 },
			"AGGREGATE_COMMIT_MAX_PENDING_BYTES",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := baseValid()
			c.AggregateMode = "aggregate-shadow"
			tc.mutate(c)
			err := c.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected %s error, got %v", tc.want, err)
			}
		})
	}
}

func TestLoad_AggregateStoreEnv(t *testing.T) {
	path := filepath.Join(t.TempDir(), "aggregate.db")
	t.Setenv("AGGREGATE_MODE", "aggregate-shadow")
	t.Setenv("AGGREGATE_DB_PATH", path)
	t.Setenv("AGGREGATE_ALLOW_REBUILD", "true")
	t.Setenv("AGGREGATE_SYNCHRONOUS", "full")
	t.Setenv("AGGREGATE_COMMIT_COALESCE_MS", "9")
	t.Setenv("AGGREGATE_COMMIT_MAX_WAITERS", "64")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.AggregateDBPath != path {
		t.Errorf("AggregateDBPath = %q, want %q", cfg.AggregateDBPath, path)
	}
	if !cfg.AggregateAllowRebuild {
		t.Error("AggregateAllowRebuild = false, want true")
	}
	if cfg.AggregateSynchronous != "FULL" {
		t.Errorf("AggregateSynchronous = %q, want FULL (upper-cased)", cfg.AggregateSynchronous)
	}
	if cfg.AggregateCommitCoalesceMs != 9 || cfg.AggregateCommitMaxWaiters != 64 {
		t.Errorf("commit knobs = %d/%d, want 9/64", cfg.AggregateCommitCoalesceMs, cfg.AggregateCommitMaxWaiters)
	}
}
