package config

import (
	"strings"
	"testing"
)

func TestRetainedNameLimitsIndependentOfActiveSeries(t *testing.T) {
	t.Setenv("AGGREGATE_MODE", "aggregate")
	t.Setenv("AGGREGATE_MAX_OPERATION_NAMES_PER_TENANT", "12000")
	t.Setenv("AGGREGATE_MAX_METRIC_NAMES_PER_TENANT", "6000")
	t.Setenv("AGGREGATE_MAX_LOG_TEMPLATES_PER_TENANT", "7000")
	cfg, err := Load("__no_such_env_file__")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AggregateMaxOperationNamesPerTenant != 12000 || cfg.AggregateMaxMetricNamesPerTenant != 6000 || cfg.AggregateMaxLogTemplatesPerTenant != 7000 {
		t.Fatal("retained name environment overrides were not loaded")
	}
	if cfg.AggregateMaxSeries != 6000 || cfg.AggregateMaxSeriesLogs != 500 {
		t.Fatal("retained name headroom changed active series limits")
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestRetainedNameLimitsRequireFiniteTenantAndInstanceBounds(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*Config)
	}{
		{"AGGREGATE_MAX_OPERATION_NAMES", func(c *Config) { c.AggregateMaxOperationNamesPerTenant = 0 }},
		{"AGGREGATE_MAX_METRIC_NAMES", func(c *Config) { c.AggregateMaxMetricNamesPerTenant = 1_000_001 }},
		{"AGGREGATE_MAX_LOG_TEMPLATES", func(c *Config) { c.AggregateMaxLogTemplates = 10_000_001 }},
		{"AGGREGATE_MAX_METRIC_NAMES", func(c *Config) { c.AggregateMaxMetricNames = c.AggregateMaxMetricNamesPerTenant - 1 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := baseValid()
			cfg.AggregateMode = "aggregate"
			tc.mutate(cfg)
			if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), tc.name) {
				t.Fatalf("wanted %s error, got %v", tc.name, err)
			}
		})
	}
}
