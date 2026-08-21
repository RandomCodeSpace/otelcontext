package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// baseValid returns a Config that passes Validate() — test functions mutate one field at a time.
// writeTLSPair writes a throwaway cert/key file pair into a temp dir and
// returns their paths.
func writeTLSPair(t *testing.T) (certFile, keyFile string) {
	t.Helper()
	dir := t.TempDir()
	certFile = filepath.Join(dir, "server.crt")
	keyFile = filepath.Join(dir, "server.key")
	if err := os.WriteFile(certFile, []byte("cert"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, []byte("key"), 0o600); err != nil {
		t.Fatal(err)
	}
	return certFile, keyFile
}

func baseValid() *Config {
	return &Config{
		HTTPPort:                 "8080",
		GRPCPort:                 "4317",
		DBDriver:                 "sqlite",
		HotRetentionDays:         7,
		MetricMaxCardinality:     10000,
		SamplingRate:             1.0,
		APIRateLimitRPS:          100,
		DBMaxOpenConns:           50,
		DBMaxIdleConns:           10,
		CompressionLevel:         "default",
		GRPCMaxRecvMB:            16,
		GRPCMaxConcurrentStreams: 1000,
		GraphRAGEventQueueSize:   100000,
		RetentionBatchSize:       50000,
		RetentionBatchSleepMs:    1,
		// Aggregate Engine defaults
		AggregateMode:                          "legacy",
		AggregateMaxSeries:                     6000,
		AggregateMaxSeriesMetrics:              2400,
		AggregateMaxSeriesTraces:               2400,
		AggregateMaxSeriesEdges:                500,
		AggregateMaxSeriesLogs:                 500,
		AggregateMaxSeriesSystem:               200,
		AggregateMaxOperationsPerService:       20,
		AggregateMaxTraceSeriesPerService:      50,
		AggregateMaxLogTemplatesPerService:     10,
		AggregateMaxMetricSeriesPerService:     50,
		AggregateSeriesPerTenantFraction:       0,
		AggregateMaxProducerBaselinesPerSeries: 8,
		AggregateMaxBaselines:                  0,
		AggregateMetricDims:                    make(map[string][]string),
		// Bounded exemplar retention defaults (#161)
		ExemplarTracesPerServiceWindow:    25,
		ExemplarTracesGlobalWindow:        1500,
		ExemplarBytesPerServiceWindow:     512 * 1024,
		ExemplarBytesGlobalWindow:         8 * 1024 * 1024,
		ExemplarHealthyRate:               0.005,
		ExemplarStratumTopK:               5,
		ExemplarLogsErrorPerServiceWindow: 50,
		ExemplarLogsWarnEnabled:           false,
		ExemplarLogsWarnPerServiceWindow:  20,
		ExemplarMaxSpansPerTrace:          500,
		ExemplarMaxBytesPerTrace:          256 * 1024,
	}
}

func TestValidate_BaseConfigOK(t *testing.T) {
	if err := baseValid().Validate(); err != nil {
		t.Fatalf("baseline config should validate: %v", err)
	}
}

func TestValidate_HotRetentionDays_LowerBound(t *testing.T) {
	c := baseValid()
	c.HotRetentionDays = 0
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "HOT_RETENTION_DAYS") {
		t.Fatalf("expected HOT_RETENTION_DAYS error, got %v", err)
	}
}

// TestValidate_HotRetentionDays_UpperBound_OverflowGuard guards against
// time.Duration(days) * 24 * time.Hour overflowing int64 nanoseconds.
// 36500 is 100y — generous and three orders of magnitude below the overflow
// threshold (~106751 days). Anything above must be rejected.
func TestValidate_HotRetentionDays_UpperBound_OverflowGuard(t *testing.T) {
	cases := []struct {
		days    int
		wantErr bool
	}{
		{days: 36500, wantErr: false},     // at the edge, allowed
		{days: 36501, wantErr: true},      // one over, rejected
		{days: 200000, wantErr: true},     // plausible typo
		{days: 10_000_000, wantErr: true}, // would overflow Duration
	}
	for _, tc := range cases {
		c := baseValid()
		c.HotRetentionDays = tc.days
		err := c.Validate()
		if tc.wantErr && err == nil {
			t.Fatalf("days=%d should be rejected (overflow guard)", tc.days)
		}
		if !tc.wantErr && err != nil {
			t.Fatalf("days=%d should pass, got %v", tc.days, err)
		}
		// Sanity: for the allowed upper bound, the resulting Duration must remain positive.
		if !tc.wantErr {
			d := time.Duration(tc.days) * 24 * time.Hour
			if d <= 0 {
				t.Fatalf("days=%d produced non-positive Duration %v", tc.days, d)
			}
		}
	}
}

func TestValidate_InvalidDBDriver(t *testing.T) {
	c := baseValid()
	c.DBDriver = "mongodb"
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "DB_DRIVER") {
		t.Fatalf("expected driver error, got %v", err)
	}
}

func TestValidate_Ports(t *testing.T) {
	c := baseValid()
	c.HTTPPort = "70000"
	if err := c.Validate(); err == nil {
		t.Fatal("HTTP port out of range must error")
	}
}

func TestValidate_TLS_PairRequired(t *testing.T) {
	// Only cert set — must error.
	c := baseValid()
	c.TLSCertFile = "/tmp/some.crt"
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "TLS_") {
		t.Fatalf("expected TLS pair error, got %v", err)
	}
	// Only key set — must error.
	c = baseValid()
	c.TLSKeyFile = "/tmp/some.key"
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "TLS_") {
		t.Fatalf("expected TLS pair error, got %v", err)
	}
}

func TestValidate_TLS_FilesMustExist(t *testing.T) {
	c := baseValid()
	c.TLSCertFile = "/does/not/exist.crt"
	c.TLSKeyFile = "/does/not/exist.key"
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "TLS_CERT_FILE") {
		t.Fatalf("expected missing-file error, got %v", err)
	}
}

func TestValidate_TLS_ReadableFilesOK(t *testing.T) {
	cert, key := writeTLSPair(t)
	c := baseValid()
	c.TLSCertFile = cert
	c.TLSKeyFile = key
	if err := c.Validate(); err != nil {
		t.Fatalf("expected TLS pair to validate, got %v", err)
	}
	if !c.TLSEnabled() {
		t.Fatal("TLSEnabled should be true when both files are set")
	}
}

func TestLoad_RetentionFullVacuum(t *testing.T) {
	// Default: off — the daily SQLite maintenance must not full-VACUUM.
	t.Setenv("RETENTION_FULL_VACUUM", "")
	if err := os.Unsetenv("RETENTION_FULL_VACUUM"); err != nil {
		t.Fatalf("unset: %v", err)
	}
	cfg, err := Load("__no_such_env_file__")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.RetentionFullVacuum {
		t.Fatal("RetentionFullVacuum must default to false")
	}

	t.Setenv("RETENTION_FULL_VACUUM", "true")
	cfg, err = Load("__no_such_env_file__")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.RetentionFullVacuum {
		t.Fatal("RETENTION_FULL_VACUUM=true not loaded")
	}
}

func TestLoad_EnvVars_TLS_APIKey_OTel_Tenant(t *testing.T) {
	t.Setenv("TLS_CERT_FILE", "")
	t.Setenv("TLS_KEY_FILE", "")
	t.Setenv("API_KEY", "top-secret")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "localhost:4317")
	t.Setenv("DEFAULT_TENANT", "acme")

	cfg, err := Load("__no_such_env_file__")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.APIKey != "top-secret" {
		t.Errorf("APIKey not loaded: %q", cfg.APIKey)
	}
	if cfg.OTelExporterEndpoint != "localhost:4317" {
		t.Errorf("OTelExporterEndpoint not loaded: %q", cfg.OTelExporterEndpoint)
	}
	if cfg.DefaultTenant != "acme" {
		t.Errorf("DefaultTenant not loaded: %q", cfg.DefaultTenant)
	}
	if cfg.TLSEnabled() {
		t.Error("TLSEnabled should be false when env vars unset")
	}
}

func TestTLSAutoSelfsigned_EnvParsing(t *testing.T) {
	cases := []struct {
		val  string
		want bool
	}{
		{"true", true},
		{"TRUE", true},
		{"True", true},
		{"1", true},
		{"yes", true},
		{"YES", true},
		{"on", true},
		{" on ", true},
		{"false", false},
		{"0", false},
		{"no", false},
		{"", false},
		{"definitely", false},
	}
	for _, tc := range cases {
		t.Run(tc.val, func(t *testing.T) {
			t.Setenv("TLS_CERT_FILE", "")
			t.Setenv("TLS_KEY_FILE", "")
			t.Setenv("TLS_AUTO_SELFSIGNED", tc.val)
			cfg, err := Load("__no_such_env_file__")
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.TLSAutoSelfsigned != tc.want {
				t.Errorf("TLSAutoSelfsigned = %v, want %v (input %q)",
					cfg.TLSAutoSelfsigned, tc.want, tc.val)
			}
			if tc.want {
				if !cfg.TLSSelfsignedMode() {
					t.Error("expected TLSSelfsignedMode() to be true")
				}
				if !cfg.TLSEnabled() {
					t.Error("expected TLSEnabled() to be true under self-signed mode")
				}
			}
		})
	}
}

func TestTLSAutoSelfsigned_DefaultCacheDir(t *testing.T) {
	t.Setenv("TLS_CERT_FILE", "")
	t.Setenv("TLS_KEY_FILE", "")
	if err := os.Unsetenv("TLS_CACHE_DIR"); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load("__no_such_env_file__")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.TLSCacheDir != "./data/tls" {
		t.Errorf("TLSCacheDir default = %q, want ./data/tls", cfg.TLSCacheDir)
	}
}

// TestTLSAutoSelfsigned_IgnoredWhenCertFilesSet verifies the precedence rule:
// explicit TLSCertFile + TLSKeyFile win over TLSAutoSelfsigned. The resulting
// Config must report cert-file mode, not self-signed mode.
func TestTLSAutoSelfsigned_IgnoredWhenCertFilesSet(t *testing.T) {
	cert, key := writeTLSPair(t)

	c := baseValid()
	c.TLSCertFile = cert
	c.TLSKeyFile = key
	c.TLSAutoSelfsigned = true

	if err := c.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if !c.TLSCertFileMode() {
		t.Error("expected cert-file mode to be active")
	}
	if c.TLSSelfsignedMode() {
		t.Error("self-signed mode must yield to explicit cert-file mode")
	}
	if !c.TLSEnabled() {
		t.Error("TLSEnabled should be true in cert-file mode")
	}
}

func TestValidateDBForEnv_RefusesSQLiteInProduction(t *testing.T) {
	c := baseValid()
	c.DBDriver = "sqlite"
	c.Env = "production"
	c.AllowSqliteProd = false
	err := c.ValidateDBForEnv()
	if err == nil || !strings.Contains(err.Error(), "SQLite is unsuitable") {
		t.Fatalf("expected SQLite-in-prod rejection, got %v", err)
	}
}

func TestValidateDBForEnv_AllowsSQLiteWhenOptIn(t *testing.T) {
	c := baseValid()
	c.DBDriver = "sqlite"
	c.Env = "production"
	c.AllowSqliteProd = true
	if err := c.ValidateDBForEnv(); err != nil {
		t.Fatalf("opt-in should allow SQLite in prod, got %v", err)
	}
}

func TestValidateDBForEnv_AllowsSQLiteInDev(t *testing.T) {
	c := baseValid()
	c.DBDriver = "sqlite"
	c.Env = "development"
	if err := c.ValidateDBForEnv(); err != nil {
		t.Fatalf("SQLite in dev must pass, got %v", err)
	}
}

func TestValidateDBForEnv_AllowsPostgresInProd(t *testing.T) {
	c := baseValid()
	c.DBDriver = "postgres"
	c.Env = "production"
	if err := c.ValidateDBForEnv(); err != nil {
		t.Fatalf("Postgres in prod must pass, got %v", err)
	}
}

func TestLoad_DefaultTenant_FallsBackToDefault(t *testing.T) {
	// Ensure var is absent — Setenv("", "") would leave it set-but-empty, which
	// the getEnv helper treats as a present value.
	if err := os.Unsetenv("DEFAULT_TENANT"); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load("__no_such_env_file__")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.DefaultTenant != "default" {
		t.Errorf("expected default tenant to be 'default', got %q", cfg.DefaultTenant)
	}
}

// TestValidate_GraphRAGEventQueueSize_Bounds: the event channel buffer is
// allocated up front and each event embeds a Span/Log by value, so the env
// value must be range-checked (also breaks the CodeQL
// go/uncontrolled-allocation-size taint path env -> make(chan, n)).
func TestValidate_GraphRAGEventQueueSize_Bounds(t *testing.T) {
	for _, bad := range []int{0, -1, 1_000_001} {
		c := baseValid()
		c.GraphRAGEventQueueSize = bad
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "GRAPHRAG_EVENT_QUEUE_SIZE") {
			t.Fatalf("size %d: expected GRAPHRAG_EVENT_QUEUE_SIZE error, got %v", bad, err)
		}
	}
	c := baseValid()
	c.GraphRAGEventQueueSize = 1_000_000
	if err := c.Validate(); err != nil {
		t.Fatalf("size 1000000 should validate: %v", err)
	}
}

func TestValidate_AggregateMode_Valid(t *testing.T) {
	cases := []string{"legacy", "aggregate-shadow", "aggregate"}
	for _, mode := range cases {
		c := baseValid()
		c.AggregateMode = mode
		if err := c.Validate(); err != nil {
			t.Errorf("mode %q should be valid, got %v", mode, err)
		}
	}
}

func TestValidate_AggregateMode_Invalid(t *testing.T) {
	c := baseValid()
	c.AggregateMode = "invalid-mode"
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "AGGREGATE_MODE") {
		t.Fatalf("expected AGGREGATE_MODE error, got %v", err)
	}
}

func TestValidate_AggregateMaxSeries_LowerBound(t *testing.T) {
	c := baseValid()
	c.AggregateMaxSeries = 0
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "AGGREGATE_MAX_SERIES") {
		t.Fatalf("expected AGGREGATE_MAX_SERIES error, got %v", err)
	}
}

func TestValidate_AggregateSeriesSubCaps_LowerBound(t *testing.T) {
	caps := []struct {
		name  string
		field string
	}{
		{"AggregateMaxSeriesMetrics", "AGGREGATE_MAX_SERIES_METRICS"},
		{"AggregateMaxSeriesTraces", "AGGREGATE_MAX_SERIES_TRACES"},
		{"AggregateMaxSeriesEdges", "AGGREGATE_MAX_SERIES_EDGES"},
		{"AggregateMaxSeriesLogs", "AGGREGATE_MAX_SERIES_LOGS"},
		{"AggregateMaxSeriesSystem", "AGGREGATE_MAX_SERIES_SYSTEM"},
	}
	for _, tc := range caps {
		c := baseValid()
		switch tc.name {
		case "AggregateMaxSeriesMetrics":
			c.AggregateMaxSeriesMetrics = 0
		case "AggregateMaxSeriesTraces":
			c.AggregateMaxSeriesTraces = 0
		case "AggregateMaxSeriesEdges":
			c.AggregateMaxSeriesEdges = 0
		case "AggregateMaxSeriesLogs":
			c.AggregateMaxSeriesLogs = 0
		case "AggregateMaxSeriesSystem":
			c.AggregateMaxSeriesSystem = 0
		}
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), tc.field) {
			t.Errorf("field %s = 0 should fail validation, got %v", tc.name, err)
		}
	}
}

func TestValidate_AggregatePerServiceCaps_LowerBound(t *testing.T) {
	caps := []struct {
		name  string
		field string
	}{
		{"AggregateMaxOperationsPerService", "AGGREGATE_MAX_OPERATIONS_PER_SERVICE"},
		{"AggregateMaxTraceSeriesPerService", "AGGREGATE_MAX_TRACE_SERIES_PER_SERVICE"},
		{"AggregateMaxLogTemplatesPerService", "AGGREGATE_MAX_LOG_TEMPLATES_PER_SERVICE"},
		{"AggregateMaxMetricSeriesPerService", "AGGREGATE_MAX_METRIC_SERIES_PER_SERVICE"},
	}
	for _, tc := range caps {
		c := baseValid()
		switch tc.name {
		case "AggregateMaxOperationsPerService":
			c.AggregateMaxOperationsPerService = 0
		case "AggregateMaxTraceSeriesPerService":
			c.AggregateMaxTraceSeriesPerService = 0
		case "AggregateMaxLogTemplatesPerService":
			c.AggregateMaxLogTemplatesPerService = 0
		case "AggregateMaxMetricSeriesPerService":
			c.AggregateMaxMetricSeriesPerService = 0
		}
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), tc.field) {
			t.Errorf("field %s = 0 should fail validation, got %v", tc.name, err)
		}
	}
}

func TestValidate_AggregateTenantFraction_OutOfRange(t *testing.T) {
	cases := []struct {
		val     float64
		wantErr bool
	}{
		{-0.1, true},
		{0.0, false},
		{0.5, false},
		{1.0, false},
		{1.1, true},
	}
	for _, tc := range cases {
		c := baseValid()
		c.AggregateSeriesPerTenantFraction = tc.val
		err := c.Validate()
		if tc.wantErr && err == nil {
			t.Errorf("fraction %.1f should fail, got nil", tc.val)
		}
		if !tc.wantErr && err != nil {
			t.Errorf("fraction %.1f should pass, got %v", tc.val, err)
		}
	}
}

func TestValidate_AggregateProducerBaselinesPerSeries_LowerBound(t *testing.T) {
	c := baseValid()
	c.AggregateMaxProducerBaselinesPerSeries = 0
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "AGGREGATE_MAX_PRODUCER_BASELINES_PER_SERIES") {
		t.Fatalf("expected error, got %v", err)
	}
}

func TestValidate_AggregateMaxBaselines_OverrideValidation(t *testing.T) {
	c := baseValid()
	c.AggregateMaxProducerBaselinesPerSeries = 8
	c.AggregateMaxBaselines = 7 // less than per-series cap
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "AGGREGATE_MAX_BASELINES") {
		t.Fatalf("expected override validation error, got %v", err)
	}
	// Valid override case: >= per-series cap
	c.AggregateMaxBaselines = 8
	if err := c.Validate(); err != nil {
		t.Errorf("override = 8 should be valid, got %v", err)
	}
}

func TestValidate_AggregateSubCaps_SumExceedsGlobal(t *testing.T) {
	c := baseValid()
	c.AggregateMaxSeries = 1000
	c.AggregateMaxSeriesMetrics = 2400
	c.AggregateMaxSeriesTraces = 500
	c.AggregateMaxSeriesEdges = 501
	c.AggregateMaxSeriesLogs = 100
	c.AggregateMaxSeriesSystem = 100
	// Sum: 2400 + 500 + 501 + 100 + 100 = 3601 > 1000
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "AGGREGATE_MAX_SERIES") {
		t.Fatalf("expected sum-exceeds error, got %v", err)
	}
}

func TestLoad_AggregateDefaults(t *testing.T) {
	// Unset all aggregate env vars to test defaults
	vars := []string{
		"AGGREGATE_MODE",
		"AGGREGATE_MAX_SERIES",
		"AGGREGATE_MAX_SERIES_METRICS",
		"AGGREGATE_MAX_SERIES_TRACES",
		"AGGREGATE_MAX_SERIES_EDGES",
		"AGGREGATE_MAX_SERIES_LOGS",
		"AGGREGATE_MAX_SERIES_SYSTEM",
		"AGGREGATE_MAX_OPERATIONS_PER_SERVICE",
		"AGGREGATE_MAX_TRACE_SERIES_PER_SERVICE",
		"AGGREGATE_MAX_LOG_TEMPLATES_PER_SERVICE",
		"AGGREGATE_MAX_METRIC_SERIES_PER_SERVICE",
		"AGGREGATE_SERIES_PER_TENANT_FRACTION",
		"AGGREGATE_MAX_PRODUCER_BASELINES_PER_SERIES",
		"AGGREGATE_MAX_BASELINES",
	}
	for _, v := range vars {
		if err := os.Unsetenv(v); err != nil {
			t.Fatal(err)
		}
	}

	cfg, err := Load("__no_such_env_file__")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.AggregateMode != "legacy" {
		t.Errorf("AggregateMode default = %q, want legacy", cfg.AggregateMode)
	}
	if cfg.AggregateMaxSeries != 6000 {
		t.Errorf("AggregateMaxSeries default = %d, want 6000", cfg.AggregateMaxSeries)
	}
	if cfg.AggregateMaxSeriesMetrics != 2400 {
		t.Errorf("AggregateMaxSeriesMetrics default = %d, want 2400", cfg.AggregateMaxSeriesMetrics)
	}
	if cfg.AggregateMaxSeriesTraces != 2400 {
		t.Errorf("AggregateMaxSeriesTraces default = %d, want 2400", cfg.AggregateMaxSeriesTraces)
	}
	if cfg.AggregateMaxSeriesEdges != 500 {
		t.Errorf("AggregateMaxSeriesEdges default = %d, want 500", cfg.AggregateMaxSeriesEdges)
	}
	if cfg.AggregateMaxSeriesLogs != 500 {
		t.Errorf("AggregateMaxSeriesLogs default = %d, want 500", cfg.AggregateMaxSeriesLogs)
	}
	if cfg.AggregateMaxSeriesSystem != 200 {
		t.Errorf("AggregateMaxSeriesSystem default = %d, want 200", cfg.AggregateMaxSeriesSystem)
	}
	if cfg.AggregateMaxOperationsPerService != 20 {
		t.Errorf("AggregateMaxOperationsPerService default = %d, want 20", cfg.AggregateMaxOperationsPerService)
	}
	if cfg.AggregateMaxTraceSeriesPerService != 50 {
		t.Errorf("AggregateMaxTraceSeriesPerService default = %d, want 50", cfg.AggregateMaxTraceSeriesPerService)
	}
	if cfg.AggregateMaxLogTemplatesPerService != 10 {
		t.Errorf("AggregateMaxLogTemplatesPerService default = %d, want 10", cfg.AggregateMaxLogTemplatesPerService)
	}
	if cfg.AggregateMaxMetricSeriesPerService != 50 {
		t.Errorf("AggregateMaxMetricSeriesPerService default = %d, want 50", cfg.AggregateMaxMetricSeriesPerService)
	}
	if cfg.AggregateSeriesPerTenantFraction != 0 {
		t.Errorf("AggregateSeriesPerTenantFraction default = %v, want 0", cfg.AggregateSeriesPerTenantFraction)
	}
	if cfg.AggregateMaxProducerBaselinesPerSeries != 8 {
		t.Errorf("AggregateMaxProducerBaselinesPerSeries default = %d, want 8", cfg.AggregateMaxProducerBaselinesPerSeries)
	}
	if cfg.AggregateMaxBaselines != 0 {
		t.Errorf("AggregateMaxBaselines default = %d, want 0", cfg.AggregateMaxBaselines)
	}
}

func TestLoad_AggregateEnvVars(t *testing.T) {
	t.Setenv("AGGREGATE_MODE", "aggregate-shadow")
	t.Setenv("AGGREGATE_MAX_SERIES", "5000")
	t.Setenv("AGGREGATE_MAX_PRODUCER_BASELINES_PER_SERIES", "10")
	t.Setenv("AGGREGATE_SERIES_PER_TENANT_FRACTION", "0.5")

	cfg, err := Load("__no_such_env_file__")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.AggregateMode != "aggregate-shadow" {
		t.Errorf("AggregateMode = %q, want aggregate-shadow", cfg.AggregateMode)
	}
	if cfg.AggregateMaxSeries != 5000 {
		t.Errorf("AggregateMaxSeries = %d, want 5000", cfg.AggregateMaxSeries)
	}
	if cfg.AggregateMaxProducerBaselinesPerSeries != 10 {
		t.Errorf("AggregateMaxProducerBaselinesPerSeries = %d, want 10", cfg.AggregateMaxProducerBaselinesPerSeries)
	}
	if cfg.AggregateSeriesPerTenantFraction != 0.5 {
		t.Errorf("AggregateSeriesPerTenantFraction = %v, want 0.5", cfg.AggregateSeriesPerTenantFraction)
	}
}

func TestResolvedAggregateMaxBaselines_Default(t *testing.T) {
	c := baseValid()
	c.AggregateMaxSeriesMetrics = 2400
	c.AggregateMaxProducerBaselinesPerSeries = 8
	c.AggregateMaxBaselines = 0 // use derived default

	resolved := c.ResolvedAggregateMaxBaselines()
	expected := 2400 * 8
	if resolved != expected {
		t.Errorf("resolved = %d, want %d (2400 × 8)", resolved, expected)
	}
}

func TestResolvedAggregateMaxBaselines_Override(t *testing.T) {
	c := baseValid()
	c.AggregateMaxBaselines = 10000

	resolved := c.ResolvedAggregateMaxBaselines()
	if resolved != 10000 {
		t.Errorf("resolved = %d, want 10000 (explicit override)", resolved)
	}
}

func TestParseAggregateMetricDims_Empty(t *testing.T) {
	result, err := ParseAggregateMetricDims("")
	if err != nil {
		t.Fatalf("ParseAggregateMetricDims: %v", err)
	}
	if len(result) != 0 {
		t.Errorf("empty string should return empty map, got %v", result)
	}
}

func TestParseAggregateMetricDims_SingleMetricSingleKey(t *testing.T) {
	result, err := ParseAggregateMetricDims("http.requests:method")
	if err != nil {
		t.Fatalf("ParseAggregateMetricDims: %v", err)
	}
	if len(result) != 1 {
		t.Fatalf("expected 1 metric, got %d", len(result))
	}
	keys, ok := result["http.requests"]
	if !ok {
		t.Fatalf("metric 'http.requests' not found")
	}
	if len(keys) != 1 || keys[0] != "method" {
		t.Errorf("expected ['method'], got %v", keys)
	}
}

func TestParseAggregateMetricDims_SingleMetricMultipleKeys(t *testing.T) {
	result, err := ParseAggregateMetricDims("http.requests:method,status_code")
	if err != nil {
		t.Fatalf("ParseAggregateMetricDims: %v", err)
	}
	keys := result["http.requests"]
	if len(keys) != 2 {
		t.Fatalf("expected 2 keys, got %d", len(keys))
	}
	// Keys should be sorted
	if keys[0] != "method" || keys[1] != "status_code" {
		t.Errorf("expected [method, status_code], got %v", keys)
	}
}

func TestParseAggregateMetricDims_MultipleMetrics(t *testing.T) {
	result, err := ParseAggregateMetricDims("http.requests:method,status_code;db.calls:database,operation")
	if err != nil {
		t.Fatalf("ParseAggregateMetricDims: %v", err)
	}
	if len(result) != 2 {
		t.Fatalf("expected 2 metrics, got %d", len(result))
	}
	if len(result["http.requests"]) != 2 || len(result["db.calls"]) != 2 {
		t.Errorf("unexpected key counts: %v", result)
	}
}

func TestParseAggregateMetricDims_KeysSorted(t *testing.T) {
	result, err := ParseAggregateMetricDims("metric:zebra,apple,monkey")
	if err != nil {
		t.Fatalf("ParseAggregateMetricDims: %v", err)
	}
	keys := result["metric"]
	expected := []string{"apple", "monkey", "zebra"}
	for i, k := range keys {
		if k != expected[i] {
			t.Errorf("key at index %d: got %q, want %q", i, k, expected[i])
		}
	}
}

// --- Bounded exemplar retention (#176) ---

func TestValidate_ExemplarBudgets_LowerBounds(t *testing.T) {
	cases := []struct {
		name  string
		field string
		mut   func(*Config)
	}{
		{"traces per service", "EXEMPLAR_TRACES_PER_SERVICE_WINDOW", func(c *Config) { c.ExemplarTracesPerServiceWindow = 0 }},
		{"bytes per service", "EXEMPLAR_BYTES_PER_SERVICE_WINDOW", func(c *Config) { c.ExemplarBytesPerServiceWindow = 512 }},
		{"stratum top k", "EXEMPLAR_STRATUM_TOP_K", func(c *Config) { c.ExemplarStratumTopK = 0 }},
		{"error log budget", "EXEMPLAR_LOGS_ERROR_PER_SERVICE_WINDOW", func(c *Config) { c.ExemplarLogsErrorPerServiceWindow = 0 }},
		{"warn log budget", "EXEMPLAR_LOGS_WARN_PER_SERVICE_WINDOW", func(c *Config) { c.ExemplarLogsWarnPerServiceWindow = 0 }},
		{"max spans per trace", "EXEMPLAR_MAX_SPANS_PER_TRACE", func(c *Config) { c.ExemplarMaxSpansPerTrace = 0 }},
		{"max bytes per trace", "EXEMPLAR_MAX_BYTES_PER_TRACE", func(c *Config) { c.ExemplarMaxBytesPerTrace = 1 }},
	}
	for _, tc := range cases {
		c := baseValid()
		tc.mut(c)
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), tc.field) {
			t.Errorf("%s should fail validation with %s, got %v", tc.name, tc.field, err)
		}
	}
}

func TestParseAggregateMetricDims_Whitespace(t *testing.T) {
	result, err := ParseAggregateMetricDims("  metric : key1 , key2  ")
	if err != nil {
		t.Fatalf("ParseAggregateMetricDims: %v", err)
	}
	keys := result["metric"]
	if len(keys) != 2 || keys[0] != "key1" || keys[1] != "key2" {
		t.Errorf("whitespace handling failed: got %v", keys)
	}
}

func TestParseAggregateMetricDims_ErrorEmptyMetricName(t *testing.T) {
	_, err := ParseAggregateMetricDims(":key")
	if err == nil {
		t.Fatal("expected error for empty metric name")
	}
	if !strings.Contains(err.Error(), "empty metric name") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestParseAggregateMetricDims_ErrorEmptyKeyList(t *testing.T) {
	_, err := ParseAggregateMetricDims("metric:")
	if err == nil {
		t.Fatal("expected error for empty key list")
	}
	if !strings.Contains(err.Error(), "empty key list") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestParseAggregateMetricDims_ErrorEmptyKey(t *testing.T) {
	_, err := ParseAggregateMetricDims("metric:key1,,key2")
	if err == nil {
		t.Fatal("expected error for empty key")
	}
	if !strings.Contains(err.Error(), "empty key") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestParseAggregateMetricDims_ErrorDuplicateMetric(t *testing.T) {
	_, err := ParseAggregateMetricDims("metric:key1;metric:key2")
	if err == nil {
		t.Fatal("expected error for duplicate metric")
	}
	if !strings.Contains(err.Error(), "duplicate metric name") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestParseAggregateMetricDims_ErrorDuplicateKey(t *testing.T) {
	_, err := ParseAggregateMetricDims("metric:key1,key1")
	if err == nil {
		t.Fatal("expected error for duplicate key")
	}
	if !strings.Contains(err.Error(), "duplicate key") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestParseAggregateMetricDims_ErrorStraySemicolon(t *testing.T) {
	_, err := ParseAggregateMetricDims("metric:key1;;metric2:key2")
	if err == nil {
		t.Fatal("expected error for stray semicolon")
	}
	if !strings.Contains(err.Error(), "stray semicolon") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestParseAggregateMetricDims_ErrorMissingColon(t *testing.T) {
	_, err := ParseAggregateMetricDims("metric_no_colon")
	if err == nil {
		t.Fatal("expected error for missing colon")
	}
	if !strings.Contains(err.Error(), "exactly one colon") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestLoad_AggregateMetricDims(t *testing.T) {
	t.Setenv("AGGREGATE_METRIC_DIMS", "http.requests:method,status_code;db.calls:database")

	cfg, err := Load("__no_such_env_file__")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if len(cfg.AggregateMetricDims) != 2 {
		t.Fatalf("expected 2 metrics, got %d", len(cfg.AggregateMetricDims))
	}

	httpKeys := cfg.AggregateMetricDims["http.requests"]
	if len(httpKeys) != 2 || httpKeys[0] != "method" || httpKeys[1] != "status_code" {
		t.Errorf("unexpected http.requests keys: %v", httpKeys)
	}

	dbKeys := cfg.AggregateMetricDims["db.calls"]
	if len(dbKeys) != 1 || dbKeys[0] != "database" {
		t.Errorf("unexpected db.calls keys: %v", dbKeys)
	}
}

func TestLoad_AggregateMetricDims_InvalidConfig(t *testing.T) {
	t.Setenv("AGGREGATE_METRIC_DIMS", "metric:")

	_, err := Load("__no_such_env_file__")
	if err == nil {
		t.Fatal("expected Load to fail with invalid AGGREGATE_METRIC_DIMS")
	}
	if !strings.Contains(err.Error(), "parsing AGGREGATE_METRIC_DIMS") {
		t.Errorf("unexpected error: %v", err)
	}
}

// A global cap below the per-service cap makes the per-service budget
// unreachable — a misconfiguration worth refusing at startup rather than
// debugging during an incident.
func TestValidate_ExemplarGlobalBudgetsMustCoverPerService(t *testing.T) {
	c := baseValid()
	c.ExemplarTracesGlobalWindow = 10 // below the 25 per-service default
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "EXEMPLAR_TRACES_GLOBAL_WINDOW") {
		t.Errorf("expected global trace cap error, got %v", err)
	}

	c = baseValid()
	c.ExemplarBytesGlobalWindow = 4096
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "EXEMPLAR_BYTES_GLOBAL_WINDOW") {
		t.Errorf("expected global byte cap error, got %v", err)
	}
}

func TestValidate_ExemplarHealthyRate_Range(t *testing.T) {
	for _, tc := range []struct {
		val     float64
		wantErr bool
	}{{-0.1, true}, {0, false}, {0.005, false}, {1.0, false}, {1.1, true}} {
		c := baseValid()
		c.ExemplarHealthyRate = tc.val
		err := c.Validate()
		if tc.wantErr && err == nil {
			t.Errorf("healthy rate %.3f should fail, got nil", tc.val)
		}
		if !tc.wantErr && err != nil {
			t.Errorf("healthy rate %.3f should pass, got %v", tc.val, err)
		}
	}
}

func TestLoad_ExemplarDefaultsMatchResolution(t *testing.T) {
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	checks := []struct {
		name string
		got  int
		want int
	}{
		{"EXEMPLAR_TRACES_PER_SERVICE_WINDOW", cfg.ExemplarTracesPerServiceWindow, 25},
		{"EXEMPLAR_TRACES_GLOBAL_WINDOW", cfg.ExemplarTracesGlobalWindow, 1500},
		{"EXEMPLAR_BYTES_PER_SERVICE_WINDOW", cfg.ExemplarBytesPerServiceWindow, 512 * 1024},
		{"EXEMPLAR_BYTES_GLOBAL_WINDOW", cfg.ExemplarBytesGlobalWindow, 8 * 1024 * 1024},
		{"EXEMPLAR_LOGS_ERROR_PER_SERVICE_WINDOW", cfg.ExemplarLogsErrorPerServiceWindow, 50},
		{"EXEMPLAR_LOGS_WARN_PER_SERVICE_WINDOW", cfg.ExemplarLogsWarnPerServiceWindow, 20},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %d, want %d", c.name, c.got, c.want)
		}
	}
	if cfg.ExemplarHealthyRate != 0.005 {
		t.Errorf("EXEMPLAR_HEALTHY_RATE = %v, want 0.005", cfg.ExemplarHealthyRate)
	}
	if cfg.ExemplarLogsWarnEnabled {
		t.Error("EXEMPLAR_LOGS_WARN_ENABLED should default off — WARN raw retention is opt-in")
	}
}
