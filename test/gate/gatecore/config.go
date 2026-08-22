package gatecore

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// Config is the gate's complete effective configuration. It is recorded
// verbatim in the report, so a run is reproducible from its own output.
//
// LoadConfigFile unmarshals an operator's JSON on top of DefaultConfig(), so a
// config file states only what it changes and every unstated field is the
// frozen default rather than a zero value.
type Config struct {
	RunID     string `json:"run_id"`
	RepoRoot  string `json:"repo_root"`
	WorkDir   string `json:"work_dir"`
	DataDir   string `json:"data_dir"`
	ReportDir string `json:"report_dir"`

	Binaries    Binaries          `json:"binaries"`
	HTTPAddr    string            `json:"http_addr"`
	GRPCAddr    string            `json:"grpc_addr"`
	MCPPath     string            `json:"mcp_path"`
	APIKey      string            `json:"api_key"`
	Confinement ConfinementConfig `json:"confinement"`
	Prefill     PrefillConfig     `json:"prefill"`
	Load        LoadConfig        `json:"load"`
	Sampling    SamplingConfig    `json:"sampling"`
	Queries     QueryConfig       `json:"queries"`
	Thresholds  Thresholds        `json:"thresholds"`

	// ServerEnv is the environment the server under test is started with. It
	// is recorded in full; AGGREGATE_SYNCHRONOUS in particular is a durability
	// claim and the report quotes it.
	ServerEnv map[string]string `json:"server_env"`

	ReadyTimeoutSec    float64        `json:"ready_timeout_sec"`
	ShutdownTimeoutSec float64        `json:"shutdown_timeout_sec"`
	Classify           TierSpecConfig `json:"tier_spec"`
}

// Binaries are the three executables the protocol drives.
type Binaries struct {
	Server  string `json:"server"`
	Loadsim string `json:"loadsim"`
	Prefill string `json:"prefill"`
}

// ConfinementConfig configures Q2.
type ConfinementConfig struct {
	// Enabled false skips confinement entirely. The gate refuses to certify
	// such a run; it exists for dry runs of the plumbing.
	Enabled bool `json:"enabled"`
	// AllowFallback permits the taskset path when delegated cgroup control is
	// unavailable. The run is then marked taskset-fallback.
	AllowFallback   bool   `json:"allow_fallback"`
	CPUQuotaPercent int    `json:"cpu_quota_percent"`
	MemoryMax       string `json:"memory_max"`
	// FallbackCPUs is the taskset -c argument.
	FallbackCPUs       string `json:"fallback_cpus"`
	FallbackGOMAXPROCS int    `json:"fallback_gomaxprocs"`
	UnitPrefix         string `json:"unit_prefix"`
}

// PrefillConfig configures the deterministic seven-day store-level prefill.
type PrefillConfig struct {
	Enabled bool   `json:"enabled"`
	Windows int    `json:"windows"`
	Workers int    `json:"workers"`
	DBPath  string `json:"db_path"`
}

// LoadConfig configures every loadsim invocation.
type LoadConfig struct {
	// Profile owns the service count and per-signal rates when set; Services
	// applies only when Profile is empty.
	Profile         string  `json:"profile"`
	Services        int     `json:"services"`
	SettleSec       float64 `json:"settle_sec"`
	SustainedSec    float64 `json:"sustained_sec"`
	BurstSpec       string  `json:"burst_spec"`
	BatchIntervalMs int     `json:"batch_interval_ms"`
	CallTimeoutSec  float64 `json:"call_timeout_sec"`
	TenantID        string  `json:"tenant_id"`

	// PostBurstAllowanceSec is the interval the contract allows the system to
	// recover in after the burst. The recovery probe spends it in loadsim's
	// settle phase, so its latencies are recorded as evidence but excluded
	// from the graded percentile.
	PostBurstAllowanceSec float64 `json:"post_burst_allowance_sec"`
	// PostBurstProofSec is the graded window that begins once the allowance
	// has elapsed.
	PostBurstProofSec float64 `json:"post_burst_proof_sec"`

	// QuietGapSec separates two load runs so no aggregate window carries
	// contributions from both. It must exceed one window.
	QuietGapSec float64 `json:"quiet_gap_sec"`

	// CrashRunSec is the length of the run during which the server is killed;
	// CrashAtSec is how far into its sustained phase the kill lands.
	CrashRunSec       float64 `json:"crash_run_sec"`
	CrashRunSettleSec float64 `json:"crash_run_settle_sec"`
	CrashAtSec        float64 `json:"crash_at_sec"`
	// LedgerFlushSec is how often the load generator fsyncs the ACK ledger,
	// so a copy predating the kill always exists on disk.
	LedgerFlushSec float64 `json:"ledger_flush_sec"`
}

// SamplingConfig configures the Prometheus and disk sampling loop.
type SamplingConfig struct {
	IntervalSec float64 `json:"interval_sec"`
	// SteadyStartOffsetSec excludes the beginning of the sustained phase from
	// the projection fit: page cache, connection ramp and the first finalize
	// tick are not steady state.
	SteadyStartOffsetSec float64 `json:"steady_start_offset_sec"`
	// Metrics are the series recorded in every scrape.
	Metrics []string `json:"metrics"`
	// RequiredMetrics must be present in every scrape. A missing one fails
	// the gate rather than becoming a blank cell.
	RequiredMetrics []string `json:"required_metrics"`
	// BacklogMetric is the writer-backlog series the flatness rule reads.
	BacklogMetric string `json:"backlog_metric"`
	// ChargedBytesMetric is an optional counter of logical bytes charged to
	// the main tier. Used ONLY to report the amplification factor.
	ChargedBytesMetric string `json:"charged_bytes_metric"`
}

// QueryConfig names the completeness surfaces. The MCP tools are listed
// explicitly here rather than implied by a count, so the report can say which
// five answered.
type QueryConfig struct {
	API      []APICheck    `json:"api"`
	MCPTools []MCPToolSpec `json:"mcp_tools"`
	Timeout  float64       `json:"timeout_sec"`
}

// APICheck is one HTTP query surface.
type APICheck struct {
	Name string `json:"name"`
	Path string `json:"path"`
	// Range selects the time window: "seven_day" spans the prefill plus the
	// live run, "crash_run" spans the crash run, "none" sends no start/end.
	Range string `json:"range"`
	// PerWindow marks a surface that returns one point per aggregate window,
	// which the gate checks for window coverage.
	PerWindow bool `json:"per_window"`
	// ExpectCoverage is the aggregate coverage marker this surface must
	// declare. Empty records whatever arrived without gating on it.
	//
	// It is a string rather than a "must be full" flag because not every
	// aggregate-backed surface can honestly claim full coverage:
	// /api/metrics/service-map returns aggregate-derived NODES alongside
	// exemplar-derived EDGES and correctly declares "sampled". Demanding
	// "full" there would be demanding a lie.
	ExpectCoverage string `json:"expect_coverage"`
	// ScalarKeys are top-level numeric fields recorded from an object
	// response.
	ScalarKeys []string `json:"scalar_keys"`
}

// MCPToolSpec is one explicitly named aggregate-backed MCP tool.
type MCPToolSpec struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
	// RangeArgs, when set, injects the seven-day range into these argument
	// keys at call time (start/end style tools).
	StartArg string `json:"start_arg,omitempty"`
	EndArg   string `json:"end_arg,omitempty"`
	// SinceArg, when set, receives an RFC3339 timestamp seven days back.
	SinceArg string `json:"since_arg,omitempty"`
}

// TierSpecConfig is the serializable form of ClassifySpec.
type TierSpecConfig struct {
	MainDBFile      string `json:"main_db_file"`
	AggregateDBFile string `json:"aggregate_db_file"`
	DLQDir          string `json:"dlq_dir"`
	TLSDir          string `json:"tls_dir"`
}

// Spec converts to the classifier's input.
func (t TierSpecConfig) Spec() ClassifySpec {
	return ClassifySpec{
		MainDBFile:      t.MainDBFile,
		AggregateDBFile: t.AggregateDBFile,
		DLQDir:          t.DLQDir,
		TLSDir:          t.TLSDir,
	}
}

// DefaultMetrics is the series the gate records on every scrape. Anything the
// assertions read must be listed here.
func DefaultMetrics() []string {
	return []string{
		"otelcontext_aggregate_input_points_total",
		"otelcontext_aggregate_late_points_total",
		"otelcontext_aggregate_admission_rejected_total",
		"otelcontext_aggregate_identity_overflow_total",
		"otelcontext_aggregate_delta_log_rows",
		"otelcontext_aggregate_delta_log_age_seconds",
		"otelcontext_aggregate_deltas_total",
		"otelcontext_aggregate_commits_total",
		"otelcontext_aggregate_commit_bytes_total",
		"otelcontext_aggregate_finalize_rows_total",
		"otelcontext_aggregate_closed_windows",
		"otelcontext_aggregate_series_active",
		"otelcontext_aggregate_overflow_series_active",
		"otelcontext_aggregate_gc_swept_total",
		"otelcontext_aggregate_recovery_duration_seconds",
		"otelcontext_aggregate_recovery_rows",
		"otelcontext_disk_component_bytes",
		"otelcontext_disk_component_high_water_bytes",
		"otelcontext_disk_used_bytes",
		"otelcontext_disk_used_ratio",
		"otelcontext_disk_shedding_state",
		"otelcontext_ingest_pipeline_queue_depth",
		"otelcontext_ingest_pipeline_dropped_total",
		"otelcontext_exemplar_dropped_total",
		"otelcontext_exemplar_rows_purged_total",
	}
}

// DefaultRequiredMetrics is the subset whose absence fails the gate.
func DefaultRequiredMetrics() []string {
	return []string{
		"otelcontext_aggregate_input_points_total",
		"otelcontext_aggregate_late_points_total",
		"otelcontext_aggregate_admission_rejected_total",
		"otelcontext_aggregate_delta_log_rows",
		"otelcontext_disk_component_bytes",
	}
}

// AggregateMCPTools is the explicit list of aggregate-backed MCP tools the
// gate exercises over the full seven-day range. Named, not counted.
func AggregateMCPTools() []MCPToolSpec {
	return []MCPToolSpec{
		{Name: "get_anomaly_timeline", Arguments: map[string]any{}, SinceArg: "since"},
		{Name: "get_service_map", Arguments: map[string]any{"depth": 3}},
		{Name: "get_service_health", Arguments: map[string]any{"service_name": "loadsim-svc-000"}},
		{Name: "root_cause_analysis", Arguments: map[string]any{"service": "loadsim-svc-000", "time_range": "7d"}},
		{Name: "impact_analysis", Arguments: map[string]any{"service": "loadsim-svc-000", "depth": 3}},
	}
}

// DefaultAPIChecks is the HTTP completeness surface.
func DefaultAPIChecks() []APICheck {
	return []APICheck{
		{
			Name: "traffic_seven_day", Path: "/api/metrics/traffic", Range: "seven_day",
			PerWindow: true, ExpectCoverage: "full",
		},
		{
			Name: "dashboard_seven_day", Path: "/api/metrics/dashboard", Range: "seven_day",
			ExpectCoverage: "full",
			ScalarKeys: []string{"total_traces", "total_logs", "total_errors", "active_services",
				"requests", "request_errors", "spans", "span_errors", "p99_latency_ms"},
		},
		{
			// Nodes are aggregate-derived, edges come from the exemplar-backed
			// topology, and the handler says so. "sampled" is the honest
			// declaration here, so it is what the gate asserts.
			Name: "service_map_seven_day", Path: "/api/metrics/service-map", Range: "seven_day",
			ExpectCoverage: "sampled",
		},
		{Name: "stats", Path: "/api/stats", Range: "none"},
		{Name: "ready", Path: "/ready", Range: "none"},
		{Name: "live", Path: "/live", Range: "none"},
	}
}

// DefaultConfig returns the frozen protocol.
func DefaultConfig() Config {
	return Config{
		WorkDir:   "./data/gate-run",
		DataDir:   "./data/gate-run/data",
		ReportDir: "./docs/gates",
		Binaries: Binaries{
			Server:  "./bin/otelcontext",
			Loadsim: "./bin/loadsim",
			Prefill: "./bin/aggprefill",
		},
		HTTPAddr: "127.0.0.1:8080",
		GRPCAddr: "127.0.0.1:4317",
		MCPPath:  "/mcp",
		Confinement: ConfinementConfig{
			Enabled:            true,
			AllowFallback:      true,
			CPUQuotaPercent:    200,
			MemoryMax:          "4G",
			FallbackCPUs:       "0,1",
			FallbackGOMAXPROCS: 2,
			UnitPrefix:         "otelcontext-gate",
		},
		Prefill: PrefillConfig{
			Enabled: true,
			Windows: 2016,
			Workers: 8,
			DBPath:  "./data/gate-run/data/aggregate.db",
		},
		Load: LoadConfig{
			Profile:               "aggregate-acceptance",
			Services:              150,
			SettleSec:             120,
			SustainedSec:          3 * 60 * 60,
			BurstSpec:             "2x60s",
			BatchIntervalMs:       250,
			CallTimeoutSec:        30,
			PostBurstAllowanceSec: 120,
			PostBurstProofSec:     120,
			QuietGapSec:           360,
			CrashRunSec:           900,
			CrashRunSettleSec:     60,
			CrashAtSec:            450,
			LedgerFlushSec:        2,
		},
		Sampling: SamplingConfig{
			IntervalSec:          15,
			SteadyStartOffsetSec: 900,
			Metrics:              DefaultMetrics(),
			RequiredMetrics:      DefaultRequiredMetrics(),
			BacklogMetric:        "otelcontext_aggregate_delta_log_rows",
		},
		Queries: QueryConfig{
			API:      DefaultAPIChecks(),
			MCPTools: AggregateMCPTools(),
			Timeout:  120,
		},
		Thresholds: DefaultThresholds(),
		ServerEnv:  DefaultServerEnv(),
		Classify: TierSpecConfig{
			MainDBFile:      "otelcontext.db",
			AggregateDBFile: "aggregate.db",
			DLQDir:          "dlq",
			TLSDir:          "tls",
		},
		ReadyTimeoutSec:    180,
		ShutdownTimeoutSec: 60,
	}
}

// DefaultServerEnv is the environment the server under test runs with.
//
// AGGREGATE_SYNCHRONOUS is recorded in the report because it is exactly the
// knob that decides what the durability claim may say. NORMAL survives process
// and container kill on a surviving volume; it does not claim host power loss.
func DefaultServerEnv() map[string]string {
	return map[string]string{
		"APP_ENV":                       "development",
		"OTELCONTEXT_ALLOW_SQLITE_PROD": "false",
		"DB_DRIVER":                     "sqlite",
		"AGGREGATE_MODE":                "aggregate",
		"AGGREGATE_SYNCHRONOUS":         "NORMAL",
		"INGEST_ASYNC_ENABLED":          "true",
		"SAMPLING_RATE":                 "1.0",
		// HOT_RETENTION_DAYS is 8, not 7, deliberately. The aggregate purge cuts
		// at now - HOT_RETENTION_DAYS and runs once at startup, so a seven-day
		// prefill measured by a five-hour protocol would have its oldest windows
		// deleted out from under the completeness check. One extra day keeps the
		// full seeded range queryable for the whole run.
		"HOT_RETENTION_DAYS":      "8",
		"EXEMPLAR_RETENTION_DAYS": "2",
		"DATA_DISK_BUDGET_MB":     "8192",
		"MCP_ENABLED":             "true",
		"LOG_FTS_ENABLED":         "true",
		"API_KEY":                 "",
		"TLS_AUTO_SELFSIGNED":     "false",
	}
}

// LoadConfigFile overlays an operator's JSON on top of the defaults.
func LoadConfigFile(path string) (Config, error) {
	cfg := DefaultConfig()
	if path == "" {
		return cfg, nil
	}
	b, err := os.ReadFile(path) // #nosec G304 -- operator-supplied gate config
	if err != nil {
		return cfg, err
	}
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return cfg, fmt.Errorf("parse gate config %s: %w", path, err)
	}
	return cfg, nil
}

// Validate refuses a configuration that cannot produce a scoreable run.
func (c Config) Validate() error {
	var problems []string
	if c.Binaries.Server == "" {
		problems = append(problems, "binaries.server is empty")
	}
	if c.Binaries.Loadsim == "" {
		problems = append(problems, "binaries.loadsim is empty")
	}
	if c.Prefill.Enabled && c.Binaries.Prefill == "" {
		problems = append(problems, "prefill is enabled but binaries.prefill is empty")
	}
	if c.DataDir == "" {
		problems = append(problems, "data_dir is empty")
	}
	if c.ReportDir == "" {
		problems = append(problems, "report_dir is empty")
	}
	if c.Sampling.IntervalSec <= 0 {
		problems = append(problems, "sampling.interval_sec must be positive")
	}
	if c.Load.QuietGapSec <= float64(WindowSecs) {
		problems = append(problems, fmt.Sprintf(
			"load.quiet_gap_sec (%.0f) must exceed one %ds aggregate window, or two phases share a window",
			c.Load.QuietGapSec, WindowSecs))
	}
	if c.Load.CrashAtSec <= 0 || c.Load.CrashAtSec >= c.Load.CrashRunSec {
		problems = append(problems, "load.crash_at_sec must fall strictly inside load.crash_run_sec")
	}
	if c.Sampling.SteadyStartOffsetSec >= c.Load.SustainedSec {
		problems = append(problems, fmt.Sprintf(
			"sampling.steady_start_offset_sec (%.0f) must be shorter than load.sustained_sec (%.0f), "+
				"or the projection's sample window opens after the sustained phase has closed",
			c.Sampling.SteadyStartOffsetSec, c.Load.SustainedSec))
	}
	if c.Load.LedgerFlushSec <= 0 {
		problems = append(problems, "load.ledger_flush_sec must be positive: the ledger must reach disk before the kill")
	}
	if len(c.Queries.MCPTools) == 0 {
		problems = append(problems, "queries.mcp_tools is empty: the contract requires explicitly named tools")
	}
	if c.Thresholds.ProjectionHorizonWindows <= 0 {
		problems = append(problems, "thresholds.projection_horizon_windows must be positive")
	}
	if !c.Confinement.Enabled {
		problems = append(problems, "confinement.enabled is false: a run without a resource boundary cannot certify one")
	}
	if len(problems) > 0 {
		return fmt.Errorf("gate config invalid:\n  - %s", strings.Join(problems, "\n  - "))
	}
	return nil
}
