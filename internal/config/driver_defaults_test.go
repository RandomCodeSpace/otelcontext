package config

import (
	"os"
	"testing"
)

// sqliteEnvKeys is the set of env vars whose defaults applyDriverDefaults
// flips when the driver is SQLite. Cleared via t.Setenv before each test so a
// stray host-env value doesn't leak in.
var sqliteEnvKeys = []string{
	"DB_MAX_OPEN_CONNS",
	"DB_MAX_IDLE_CONNS",
	"INGEST_PIPELINE_WORKERS",
	"INGEST_PIPELINE_QUEUE_SIZE",
	"METRIC_MAX_CARDINALITY",
	"STORE_MIN_SEVERITY",
	"SAMPLING_RATE",
	"GRPC_MAX_CONCURRENT_STREAMS",
	"LOG_FTS_ENABLED",
}

// clearSQLiteEnv unsets every env var consulted by applyDriverDefaults so
// the test starts from a deterministic "operator set nothing" baseline.
func clearSQLiteEnv(t *testing.T) {
	t.Helper()
	for _, k := range sqliteEnvKeys {
		// Unsetenv is reverted by the Go runtime when the test ends only when
		// paired with Setenv("") first. Use Setenv("") then explicit Unsetenv
		// via a deferred cleanup so concurrent tests do not see leaked state.
		if _, ok := os.LookupEnv(k); ok {
			old := os.Getenv(k)
			t.Setenv(k, old) // record original for revert
			if err := os.Unsetenv(k); err != nil {
				t.Fatalf("unset %s: %v", k, err)
			}
		}
	}
}

// TestApplyDriverDefaults_SQLite_FlipsAllWhenNoEnv proves the post-Load()
// override fires when the driver is SQLite and the operator did not set
// any of the overridable env vars.
func TestApplyDriverDefaults_SQLite_FlipsAllWhenNoEnv(t *testing.T) {
	clearSQLiteEnv(t)
	cfg := &Config{
		DBDriver:                 "sqlite",
		DBMaxOpenConns:           50,    // Postgres default
		DBMaxIdleConns:           10,    // Postgres default
		IngestPipelineWorkers:    8,     // Postgres default
		IngestPipelineQueueSize:  50000, // Postgres default
		MetricMaxCardinality:     10000, // Postgres default
		StoreMinSeverity:         "",    // same-as-ingest default
		SamplingRate:             1.0,   // keep-all default
		GRPCMaxConcurrentStreams: 1000,  // Postgres default
		LogFTSEnabled:            false, // FTS5 opt-in default
	}
	applyDriverDefaults(cfg)

	cases := []struct {
		name string
		got  any
		want any
	}{
		{"DBMaxOpenConns", cfg.DBMaxOpenConns, 1},
		{"DBMaxIdleConns", cfg.DBMaxIdleConns, 1},
		{"IngestPipelineWorkers", cfg.IngestPipelineWorkers, 2},
		{"IngestPipelineQueueSize", cfg.IngestPipelineQueueSize, 10000},
		{"MetricMaxCardinality", cfg.MetricMaxCardinality, 3000},
		{"StoreMinSeverity", cfg.StoreMinSeverity, "WARN"},
		{"SamplingRate", cfg.SamplingRate, 0.05},
		{"GRPCMaxConcurrentStreams", cfg.GRPCMaxConcurrentStreams, 240},
		{"LogFTSEnabled", cfg.LogFTSEnabled, true},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s: SQLite override = %v, want %v", c.name, c.got, c.want)
		}
	}
}

// TestApplyDriverDefaults_SQLite_RespectsExplicitOverride proves that an
// operator-set env var is preserved even when its value equals the Postgres
// default. The presence check is via os.LookupEnv, not a value comparison.
func TestApplyDriverDefaults_SQLite_RespectsExplicitOverride(t *testing.T) {
	clearSQLiteEnv(t)
	t.Setenv("DB_MAX_OPEN_CONNS", "50") // explicit override, equal to Postgres default
	t.Setenv("SAMPLING_RATE", "1.0")    // explicit "keep all"

	cfg := &Config{
		DBDriver:       "sqlite",
		DBMaxOpenConns: 50,  // operator-set value
		SamplingRate:   1.0, // operator-set value
		// rest unset so we can confirm the others still flip
	}
	applyDriverDefaults(cfg)

	if cfg.DBMaxOpenConns != 50 {
		t.Errorf("explicit DB_MAX_OPEN_CONNS=50 was clobbered to %d", cfg.DBMaxOpenConns)
	}
	if cfg.SamplingRate != 1.0 {
		t.Errorf("explicit SAMPLING_RATE=1.0 was clobbered to %f", cfg.SamplingRate)
	}
	// And a field with no env override still flips
	if cfg.MetricMaxCardinality != 3000 {
		t.Errorf("MetricMaxCardinality should have flipped to 3000, got %d", cfg.MetricMaxCardinality)
	}
}

// TestApplyDriverDefaults_Postgres_NoChange proves the Postgres / Postgresql
// drivers are untouched by this override regardless of env state.
func TestApplyDriverDefaults_Postgres_NoChange(t *testing.T) {
	clearSQLiteEnv(t)
	for _, drv := range []string{"postgres", "postgresql", "Postgres", "POSTGRES"} {
		t.Run(drv, func(t *testing.T) {
			cfg := &Config{
				DBDriver:                 drv,
				DBMaxOpenConns:           50,
				DBMaxIdleConns:           10,
				IngestPipelineWorkers:    8,
				IngestPipelineQueueSize:  50000,
				MetricMaxCardinality:     10000,
				StoreMinSeverity:         "",
				SamplingRate:             1.0,
				GRPCMaxConcurrentStreams: 1000,
				LogFTSEnabled:            false,
			}
			before := *cfg
			applyDriverDefaults(cfg)
			if *cfg != before {
				t.Errorf("Postgres driver %q was mutated by SQLite override: %+v → %+v", drv, before, *cfg)
			}
		})
	}
}

// TestApplyDriverDefaults_SQLite_CaseInsensitive proves the driver-name
// match is case-insensitive so SQLite / sqlite / SQLITE all trip the
// override.
func TestApplyDriverDefaults_SQLite_CaseInsensitive(t *testing.T) {
	clearSQLiteEnv(t)
	for _, drv := range []string{"sqlite", "SQLite", "SQLITE"} {
		t.Run(drv, func(t *testing.T) {
			cfg := &Config{
				DBDriver:       drv,
				DBMaxOpenConns: 50,
			}
			applyDriverDefaults(cfg)
			if cfg.DBMaxOpenConns != 1 {
				t.Errorf("driver=%q SQLite override missed; DBMaxOpenConns=%d", drv, cfg.DBMaxOpenConns)
			}
		})
	}
}
