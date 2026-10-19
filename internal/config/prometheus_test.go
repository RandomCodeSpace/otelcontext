package config

import (
	"os"
	"strings"
	"testing"
)

func TestPrometheusConfig(t *testing.T) {
	t.Setenv("PROMETHEUS_URL", "http://prometheus:9090")
	t.Setenv("PROMETHEUS_REQUIRED", "true")
	t.Setenv("PROMETHEUS_TENANT_ID", "default")
	c, err := Load("__no_such_env_file__")
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if !c.PrometheusRequired || c.PrometheusMaxIngestWaiters < 200 {
		t.Fatal("Prometheus configuration not loaded")
	}
	for _, tc := range []struct {
		name   string
		mutate func(*Config)
	}{
		{"PROMETHEUS_REQUIRED", func(c *Config) { c.PrometheusURL = "" }},
		{"PROMETHEUS_URL", func(c *Config) { c.PrometheusURL = "http://user:secret@prometheus:9090" }},
		{"PROMETHEUS_TIMEOUT_MS", func(c *Config) { c.PrometheusTimeoutMs = 60000 }},
		{"concurrency", func(c *Config) { c.PrometheusMaxIngestWaiters = 100000 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bad := *c
			tc.mutate(&bad)
			if err := bad.Validate(); err == nil || !strings.Contains(err.Error(), tc.name) {
				t.Fatalf("wanted %s failure, got %v", tc.name, err)
			}
		})
	}
}

func TestPrometheusDisabledByDefault(t *testing.T) {
	for _, key := range []string{"PROMETHEUS_URL", "PROMETHEUS_REQUIRED", "PROMETHEUS_TENANT_ID"} {
		t.Setenv(key, "")
		if err := os.Unsetenv(key); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("DEFAULT_TENANT", "alpha")
	c, err := Load("__no_such_env_file__")
	if err != nil {
		t.Fatal(err)
	}
	if c.PrometheusURL != "" || c.PrometheusRequired || c.PrometheusTenantID != "alpha" {
		t.Fatalf("unexpected backend defaults: URL=%q required=%v tenant=%q", c.PrometheusURL, c.PrometheusRequired, c.PrometheusTenantID)
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
}
