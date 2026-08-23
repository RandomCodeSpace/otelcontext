package gatecore

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultConfigIsValid(t *testing.T) {
	if err := DefaultConfig().Validate(); err != nil {
		t.Fatalf("the frozen default configuration does not validate: %v", err)
	}
}

func TestDefaultThresholdsMatchTheFrozenContract(t *testing.T) {
	th := DefaultThresholds()
	cases := []struct {
		name      string
		got, want float64
	}{
		{"sustained hours", th.SustainedHours, 3},
		{"sustained rate", th.SustainedPointsPerSec, 10000},
		{"ack p99 ms", th.AckP99MaxMs, 300},
		{"ack ratio", th.AckRatioMin, 0.999},
		{"burst rate", th.BurstPointsPerSec, 20000},
		{"burst seconds", th.BurstSeconds, 60},
		{"burst recovery seconds", th.BurstRecoverySeconds, 120},
		{"ready seconds", th.ReadySeconds, 60},
		{"memory bytes", float64(th.MemoryPeakMaxBytes), 4 * 1024 * 1024 * 1024},
		{"main tier bytes", float64(th.DiskMainMaxBytes), 4 * 1024 * 1024 * 1024},
		{"aggregate tier bytes", float64(th.DiskAggregateMaxBytes), 2.25 * 1024 * 1024 * 1024},
		{"dlq tier bytes", float64(th.DiskDLQMaxBytes), 0.5 * 1024 * 1024 * 1024},
		{"wal tier bytes", float64(th.DiskWALTempTLSMaxBytes), 0.25 * 1024 * 1024 * 1024},
		{"total bytes", float64(th.DiskTotalMaxBytes), 7 * 1024 * 1024 * 1024},
		{"free headroom bytes", float64(th.DiskFreeMinBytes), 1024 * 1024 * 1024},
		{"projection horizon", float64(th.ProjectionHorizonWindows), 576},
		{"prefill windows", float64(th.PrefillWindows), 2016},
		{"prefill series", float64(th.PrefillSeries), 6000},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
	if th.MaxResourceExhausted != 0 || th.MaxSkippedSeries != 0 || th.MaxOOMKills != 0 {
		t.Error("the zero-tolerance thresholds must be zero")
	}
	if th.RequiredCoverage != "full" {
		t.Errorf("required coverage = %q, want full", th.RequiredCoverage)
	}
}

func TestAggregateMCPToolsAreNamedNotCounted(t *testing.T) {
	want := map[string]bool{
		"get_anomaly_timeline": false,
		"get_service_map":      false,
		"get_service_health":   false,
		"root_cause_analysis":  false,
		"impact_analysis":      false,
	}
	for _, tool := range AggregateMCPTools() {
		if _, ok := want[tool.Name]; !ok {
			t.Errorf("unexpected tool %q in the aggregate-backed list", tool.Name)
			continue
		}
		want[tool.Name] = true
	}
	for name, seen := range want {
		if !seen {
			t.Errorf("aggregate-backed tool %q is missing from the gate configuration", name)
		}
	}
	// search_logs is 24h-clamped and is deliberately not a seven-day
	// completeness target.
	for _, tool := range AggregateMCPTools() {
		if tool.Name == "search_logs" {
			t.Error("search_logs is 24h-clamped and must not be a seven-day completeness target")
		}
	}
}

func TestLoadConfigFileOverlaysDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gate.json")
	body := `{"http_addr":"127.0.0.1:19090","thresholds":{"ack_p99_max_ms":150},"load":{"sustained_sec":600}}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfigFile(path)
	if err != nil {
		t.Fatalf("LoadConfigFile: %v", err)
	}
	if cfg.HTTPAddr != "127.0.0.1:19090" {
		t.Errorf("http addr = %q", cfg.HTTPAddr)
	}
	if cfg.Thresholds.AckP99MaxMs != 150 {
		t.Errorf("overridden threshold = %v", cfg.Thresholds.AckP99MaxMs)
	}
	if cfg.Load.SustainedSec != 600 {
		t.Errorf("overridden duration = %v", cfg.Load.SustainedSec)
	}
	// Unstated fields keep the frozen defaults rather than becoming zero.
	if cfg.Thresholds.MemoryPeakMaxBytes != 4*GiB {
		t.Errorf("unstated threshold was zeroed: %d", cfg.Thresholds.MemoryPeakMaxBytes)
	}
	if len(cfg.Queries.MCPTools) != 5 {
		t.Errorf("unstated MCP tool list was zeroed: %d entries", len(cfg.Queries.MCPTools))
	}
	if cfg.Load.QuietGapSec != DefaultConfig().Load.QuietGapSec {
		t.Errorf("unstated load field was zeroed: %v", cfg.Load.QuietGapSec)
	}
}

func TestLoadConfigFileRejectsUnknownFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gate.json")
	if err := os.WriteFile(path, []byte(`{"htttp_addr":"oops"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfigFile(path); err == nil {
		t.Error("a typo'd key must be refused, not silently ignored")
	}
}

func TestValidateRejectsUnscoreableRuns(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*Config)
		wantSub string
	}{
		{"no confinement", func(c *Config) { c.Confinement.Enabled = false }, "confinement.enabled"},
		{"quiet gap under a window", func(c *Config) { c.Load.QuietGapSec = 60 }, "quiet_gap_sec"},
		{"kill outside the crash run", func(c *Config) { c.Load.CrashAtSec = 99999 }, "crash_at_sec"},
		{"no ledger flush", func(c *Config) { c.Load.LedgerFlushSec = 0 }, "ledger_flush_sec"},
		{"no mcp tools", func(c *Config) { c.Queries.MCPTools = nil }, "mcp_tools"},
		{"no loadsim", func(c *Config) { c.Binaries.Loadsim = "" }, "binaries.loadsim"},
		{"prefill without a binary", func(c *Config) { c.Binaries.Prefill = "" }, "binaries.prefill"},
		{"no sampling", func(c *Config) { c.Sampling.IntervalSec = 0 }, "interval_sec"},
		{"steady window opens too late", func(c *Config) { c.Sampling.SteadyStartOffsetSec = c.Load.SustainedSec }, "steady_start_offset_sec"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := DefaultConfig()
			c.mutate(&cfg)
			err := cfg.Validate()
			if err == nil {
				t.Fatalf("%s must be refused", c.name)
			}
			if !strings.Contains(err.Error(), c.wantSub) {
				t.Errorf("error %q does not mention %q", err, c.wantSub)
			}
		})
	}
}

func TestServerEnvRecordsTheDurabilityKnob(t *testing.T) {
	env := DefaultServerEnv()
	if env["AGGREGATE_SYNCHRONOUS"] == "" {
		t.Error("AGGREGATE_SYNCHRONOUS must be pinned and recorded: it decides what the durability claim may say")
	}
	if env["AGGREGATE_MODE"] != "aggregate" {
		t.Errorf("AGGREGATE_MODE = %q, want aggregate", env["AGGREGATE_MODE"])
	}
	if env["HOT_RETENTION_DAYS"] != "8" {
		t.Errorf("HOT_RETENTION_DAYS = %q; a seven-day prefill measured over a multi-hour run "+
			"needs one extra day or the oldest windows are purged mid-gate", env["HOT_RETENTION_DAYS"])
	}
	if env["SAMPLING_RATE"] != "1.0" {
		t.Errorf("SAMPLING_RATE = %q, want 1.0 so the completeness check is not comparing against a sample",
			env["SAMPLING_RATE"])
	}
}

func TestRequiredMetricsAreASubsetOfSampledMetrics(t *testing.T) {
	sampled := map[string]bool{}
	for _, m := range DefaultMetrics() {
		sampled[m] = true
	}
	for _, m := range DefaultRequiredMetrics() {
		if !sampled[m] {
			t.Errorf("required metric %q is never sampled", m)
		}
	}
}

func TestConfigRoundTripsThroughJSON(t *testing.T) {
	cfg := DefaultConfig()
	b, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var back Config
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("the effective config recorded in the report does not parse back: %v", err)
	}
	if back.Thresholds != cfg.Thresholds {
		t.Error("thresholds did not survive the round trip")
	}
}
