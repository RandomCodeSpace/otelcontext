package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// baseValid returns a Config that passes Validate() — test functions mutate one field at a time.
func baseValid() *Config {
	return &Config{
		HTTPPort:             "8080",
		GRPCPort:             "4317",
		DBDriver:             "sqlite",
		HotRetentionDays:     7,
		MetricMaxCardinality: 10000,
		SamplingRate:         1.0,
		APIRateLimitRPS:      100,
		DBMaxOpenConns:       50,
		DBMaxIdleConns:       10,
		CompressionLevel:     "default",
		GRPCMaxRecvMB:            16,
		GRPCMaxConcurrentStreams: 1000,
		RetentionBatchSize:    50000,
		RetentionBatchSleepMs: 1,
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
	dir := t.TempDir()
	cert := filepath.Join(dir, "server.crt")
	key := filepath.Join(dir, "server.key")
	if err := os.WriteFile(cert, []byte("cert"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(key, []byte("key"), 0o600); err != nil {
		t.Fatal(err)
	}
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
	dir := t.TempDir()
	cert := filepath.Join(dir, "server.crt")
	key := filepath.Join(dir, "server.key")
	if err := os.WriteFile(cert, []byte("cert"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(key, []byte("key"), 0o600); err != nil {
		t.Fatal(err)
	}

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
