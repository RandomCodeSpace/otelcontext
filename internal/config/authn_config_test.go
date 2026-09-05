package config

import (
	"os"
	"strings"
	"testing"
)

// productionConfig returns a Config that is valid apart from the
// production-hardening axis each test drives.
func productionConfig(t *testing.T) *Config {
	t.Helper()
	c := baseValid()
	c.Env = "production"
	c.DBDriver = "postgres"
	return c
}

// Production does not implicitly enable or require transport protection or
// authentication. Each remains independently opt-in.
func TestValidate_ProductionAllowsOptionalTransportAndAuth(t *testing.T) {
	certFile, keyFile := writeTLSPair(t)

	cases := []struct {
		name   string
		mutate func(*Config)
	}{
		{
			name:   "plaintext and unauthenticated",
			mutate: func(*Config) {},
		},
		{
			name:   "TLS without authentication",
			mutate: func(c *Config) { c.TLSCertFile, c.TLSKeyFile = certFile, keyFile },
		},
		{
			name:   "self-signed TLS without authentication",
			mutate: func(c *Config) { c.TLSAutoSelfsigned = true },
		},
		{
			name:   "API key without TLS",
			mutate: func(c *Config) { c.APIKey = "secret" },
		},
		{
			name:   "tenant keys without TLS",
			mutate: func(c *Config) { c.APITenantKeysFile = "/etc/otelcontext/keys.json" },
		},
		{
			name: "TLS plus API_KEY admitted",
			mutate: func(c *Config) {
				c.TLSCertFile, c.TLSKeyFile = certFile, keyFile
				c.APIKey = "secret"
			},
		},
		{
			name: "TLS plus tenant keys admitted",
			mutate: func(c *Config) {
				c.TLSCertFile, c.TLSKeyFile = certFile, keyFile
				c.APITenantKeysFile = "/etc/otelcontext/keys.json"
			},
		},
		{
			name:   "external trusted authentication",
			mutate: func(c *Config) { c.AuthTrustExternal = true; c.AuthExternalTenantHeader = "X-OtelContext-Tenant" },
		},
		{
			name:   "deprecated insecure flag true",
			mutate: func(c *Config) { c.AllowInsecureGRPC = true },
		},
		{
			name:   "deprecated insecure flag false",
			mutate: func(c *Config) { c.AllowInsecureGRPC = false },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := productionConfig(t)
			tc.mutate(c)
			if err := c.Validate(); err != nil {
				t.Fatalf("production opt-in combination refused: %v", err)
			}
		})
	}
}

func TestValidate_ProductionRejectsInvalidExplicitTLS(t *testing.T) {
	c := productionConfig(t)
	c.TLSCertFile = "/tmp/cert-without-key.crt"
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "TLS_CERT_FILE and TLS_KEY_FILE") {
		t.Fatalf("invalid explicit TLS pair must be rejected, got %v", err)
	}
}

func TestLoad_ProductionDefaultsAllowSQLiteWithoutAuthOrTLS(t *testing.T) {
	for _, key := range []string{
		"API_KEY",
		"API_TENANT_KEYS_FILE",
		"AUTH_TRUST_EXTERNAL",
		"TLS_CERT_FILE",
		"TLS_KEY_FILE",
		"TLS_AUTO_SELFSIGNED",
		"OTELCONTEXT_ALLOW_INSECURE_GRPC",
		"OTELCONTEXT_ALLOW_SQLITE_PROD",
		"LOG_FTS_ENABLED",
	} {
		t.Setenv(key, "")
		if err := os.Unsetenv(key); err != nil {
			t.Fatalf("unset %s: %v", key, err)
		}
	}
	t.Setenv("APP_ENV", "production")
	t.Setenv("DB_DRIVER", "sqlite")

	c, err := Load("__no_such_env_file__")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.AuthEnabled() || c.TLSEnabled() {
		t.Fatalf("production defaults unexpectedly enabled auth or TLS: auth=%t tls=%t", c.AuthEnabled(), c.TLSEnabled())
	}
	if c.AllowInsecureGRPC || c.AllowSqliteProd {
		t.Fatalf("deprecated compatibility flags unexpectedly enabled: grpc=%t sqlite=%t", c.AllowInsecureGRPC, c.AllowSqliteProd)
	}
	if !c.LogFTSEnabled {
		t.Fatal("SQLite FTS must default on")
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if err := c.ValidateDBForEnv(); err != nil {
		t.Fatalf("ValidateDBForEnv: %v", err)
	}
}

// Development is untouched: plaintext, unauthenticated, no keys file — the
// configuration that every existing deployment runs today still validates.
func TestValidate_DevelopmentUnchanged(t *testing.T) {
	c := baseValid()
	if err := c.Validate(); err != nil {
		t.Fatalf("default development config refused: %v", err)
	}
	if c.AuthEnabled() {
		t.Error("AuthEnabled() must be false with no credential source")
	}
	if c.EnforceWSOrigin() {
		t.Error("WebSocket origin policy must be off in an unauthenticated development deployment")
	}
	if c.IsProduction() {
		t.Error("IsProduction() must be false for the default env")
	}
}

// The origin policy switches on with authentication, and always in production.
func TestEnforceWSOrigin(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Config)
		want   bool
	}{
		{"unauthenticated dev", func(*Config) {}, false},
		{"API_KEY set", func(c *Config) { c.APIKey = "secret" }, true},
		{"tenant keys set", func(c *Config) { c.APITenantKeysFile = "keys.json" }, true},
		{"trust external", func(c *Config) { c.AuthTrustExternal = true }, true},
		{"production without auth", func(c *Config) { c.Env = "production" }, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := baseValid()
			tc.mutate(c)
			if got := c.EnforceWSOrigin(); got != tc.want {
				t.Fatalf("EnforceWSOrigin() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestValidate_ExternalTenantHeader(t *testing.T) {
	cases := []struct {
		name    string
		header  string
		wantErr string
	}{
		{"default accepted", "X-OtelContext-Tenant", ""},
		{"empty refused", "", "must not be empty"},
		{"space refused", "X Tenant", "invalid character"},
		{"newline refused", "X-Tenant\n", "whitespace"},
		{"embedded colon refused", "X:Tenant", "invalid character"},
		{"client header refused", "X-Tenant-ID", "must not be X-Tenant-ID"},
		{"client header refused case-insensitively", "x-tenant-id", "must not be X-Tenant-ID"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := baseValid()
			c.AuthTrustExternal = true
			c.AuthExternalTenantHeader = tc.header
			err := c.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("want accepted, got %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %v should mention %q", err, tc.wantErr)
			}
		})
	}

	// The header is only validated when it is actually trusted.
	c := baseValid()
	c.AuthExternalTenantHeader = "not a header"
	if err := c.Validate(); err != nil {
		t.Fatalf("unused header validated: %v", err)
	}
}

// Defaults: reflection follows APP_ENV, everything else is off.
func TestLoad_AuthenticationDefaults(t *testing.T) {
	t.Setenv("APP_ENV", "development")
	c, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.AuthTrustExternal || c.AllowInsecureGRPC || c.APITenantKeysFile != "" || len(c.WSAllowedOrigins) != 0 {
		t.Fatalf("development defaults are not inert: %+v", struct {
			Trust    bool
			Insecure bool
			Keys     string
			Origins  []string
		}{c.AuthTrustExternal, c.AllowInsecureGRPC, c.APITenantKeysFile, c.WSAllowedOrigins})
	}
	if !c.GRPCReflectionEnabled() {
		t.Error("reflection should stay on outside production")
	}
	if c.AuthExternalTenantHeader != "X-OtelContext-Tenant" {
		t.Errorf("AuthExternalTenantHeader = %q", c.AuthExternalTenantHeader)
	}

	t.Setenv("APP_ENV", "production")
	c, err = Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.GRPCReflectionEnabled() {
		t.Error("reflection must default to off in production")
	}

	t.Setenv("GRPC_REFLECTION", "true")
	c, err = Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !c.GRPCReflectionEnabled() {
		t.Error("GRPC_REFLECTION=true must re-enable reflection in production")
	}
}

func TestLoad_WSAllowedOrigins(t *testing.T) {
	t.Setenv("WS_ALLOWED_ORIGINS", " https://app.example.com , , dash.example.com ")
	c, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := []string{"https://app.example.com", "dash.example.com"}
	if len(c.WSAllowedOrigins) != len(want) {
		t.Fatalf("WSAllowedOrigins = %v, want %v", c.WSAllowedOrigins, want)
	}
	for i := range want {
		if c.WSAllowedOrigins[i] != want[i] {
			t.Errorf("[%d] = %q, want %q", i, c.WSAllowedOrigins[i], want[i])
		}
	}
}
