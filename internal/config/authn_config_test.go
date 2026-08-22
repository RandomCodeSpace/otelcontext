package config

import (
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

// The production startup matrix from the #198 resolution: plaintext OR
// unauthenticated gRPC is refused, and each waiver flag admits it with a
// message that names the flag.
func TestValidate_ProductionRequiresTransportAndAuth(t *testing.T) {
	certFile, keyFile := writeTLSPair(t)

	cases := []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{
			name:    "plaintext and unauthenticated refused",
			mutate:  func(*Config) {},
			wantErr: "transport protection",
		},
		{
			name:    "TLS without authentication refused",
			mutate:  func(c *Config) { c.TLSCertFile, c.TLSKeyFile = certFile, keyFile },
			wantErr: "requires authentication",
		},
		{
			name:    "self-signed TLS counts as transport protection",
			mutate:  func(c *Config) { c.TLSAutoSelfsigned = true },
			wantErr: "requires authentication",
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
			name:   "AUTH_TRUST_EXTERNAL waives both",
			mutate: func(c *Config) { c.AuthTrustExternal = true; c.AuthExternalTenantHeader = "X-OtelContext-Tenant" },
		},
		{
			name:   "OTELCONTEXT_ALLOW_INSECURE_GRPC waives both",
			mutate: func(c *Config) { c.AllowInsecureGRPC = true },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := productionConfig(t)
			tc.mutate(c)
			err := c.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("want admitted, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("want refusal, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q should mention %q", err, tc.wantErr)
			}
			// Every refusal names both waivers so the operator can see the
			// acknowledgement they would be making.
			for _, flag := range []string{"AUTH_TRUST_EXTERNAL", "OTELCONTEXT_ALLOW_INSECURE_GRPC"} {
				if !strings.Contains(err.Error(), flag) {
					t.Errorf("refusal %q does not name %s", err, flag)
				}
			}
		})
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
