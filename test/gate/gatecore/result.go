// Package gatecore holds the pure logic of the seven-day aggregate release
// gate (#202): configuration, parsers, threshold evaluation, the projection
// fit, the result schema, and the Markdown renderer.
//
// Nothing here talks to a process, a socket, or a clock it did not receive as
// an argument. The orchestrator (../main.go, build tag `gate`) drives the
// protocol and fills these structures in; CI compiles and unit-tests this
// package on the normal build so the calculations and the renderer are
// covered without running the three-hour protocol.
//
// JSON is the source of truth. The Markdown report is rendered from the same
// Result value that is serialized to JSON, so the two cannot disagree.
package gatecore

import "time"

// Schema is the identifier stamped into every emitted report. Bump it when a
// field changes meaning, never when one is added.
const Schema = "otelcontext.aggregate-7day-gate/v1"

// HorizonWindows is the two-day main-tier horizon, in completed five-minute
// windows: 2 days * 24 h * 12 windows/h.
const HorizonWindows = 576

// WindowSecs is the aggregate window width the whole platform is built on.
const WindowSecs int64 = 300

// Result is the whole gate run. It is the only thing serialized to JSON and
// the only thing the Markdown renderer reads.
type Result struct {
	Schema      string    `json:"schema"`
	GateVersion string    `json:"gate_version"`
	RunID       string    `json:"run_id"`
	StartedAt   time.Time `json:"started_at"`
	EndedAt     time.Time `json:"ended_at"`
	DurationSec float64   `json:"duration_sec"`

	// Passed is the gate verdict: every assertion passed and every phase
	// completed. It is never true while Failures is non-empty.
	Passed   bool     `json:"passed"`
	Failures []string `json:"failures"`

	Provenance  Provenance        `json:"provenance"`
	Host        HostInfo          `json:"host"`
	Confinement Confinement       `json:"confinement"`
	Config      Config            `json:"effective_config"`
	ServerEnv   map[string]string `json:"server_env"`

	Phases   []Phase   `json:"phases"`
	Commands []Command `json:"commands"`

	Load       LoadResults    `json:"load"`
	Recovery   RecoveryResult `json:"recovery"`
	Memory     MemoryResult   `json:"memory"`
	Disk       DiskResult     `json:"disk"`
	Projection Projection     `json:"main_tier_projection"`
	Queries    QueryResults   `json:"queries"`
	Backlog    BacklogTrend   `json:"backlog_trend"`

	// MetricSeries is the sampled Prometheus evidence behind the trend and
	// projection assertions. Keys are `name` or `name{label="value"}`.
	MetricSeries []MetricSample `json:"metric_series"`

	// Gaps records things the gate needed that the server does not expose.
	// A gap is evidence about the platform, not an excuse: an assertion that
	// depends on a gap fails unless it has a sanctioned degraded basis.
	Gaps []string `json:"metric_gaps"`

	Assertions []Assertion `json:"assertions"`

	// DurabilityClaim is the one sentence this run is allowed to support.
	DurabilityClaim string `json:"durability_claim"`
}

// Provenance identifies exactly what was measured.
type Provenance struct {
	CommitSHA       string            `json:"commit_sha"`
	Branch          string            `json:"branch"`
	DirtyTree       bool              `json:"dirty_tree"`
	DirtyFiles      []string          `json:"dirty_files,omitempty"`
	GoVersion       string            `json:"go_version"`
	BinarySHA256    map[string]string `json:"binary_sha256"`
	BuiltAt         time.Time         `json:"built_at"`
	OrchestratorPID int               `json:"orchestrator_pid"`
}

// HostInfo is the machine the numbers came off.
type HostInfo struct {
	Hostname       string `json:"hostname"`
	Kernel         string `json:"kernel"`
	OS             string `json:"os"`
	Arch           string `json:"arch"`
	NumCPU         int    `json:"num_cpu"`
	TotalMemBytes  int64  `json:"total_mem_bytes"`
	DataDir        string `json:"data_dir"`
	DataDirFSType  string `json:"data_dir_fs_type"`
	DataDirDevice  string `json:"data_dir_device"`
	DataDirMount   string `json:"data_dir_mountpoint"`
	DataDirTotal   int64  `json:"data_dir_total_bytes"`
	DataDirFreeMin int64  `json:"data_dir_free_bytes_min"`
	CgroupV2       bool   `json:"cgroup_v2"`
}

// ConfinementMode names how the server was bounded.
type ConfinementMode string

const (
	// ConfinementCgroup is the sanctioned mode: a cgroup-v2 transient scope
	// with CPUQuota and MemoryMax, verified from the cgroup files.
	ConfinementCgroup ConfinementMode = "cgroup-scope"
	// ConfinementTaskset is the fallback: dedicated cores, no quota
	// throttling and no memory bound. It validates a different thing and the
	// report says so.
	ConfinementTaskset ConfinementMode = "taskset-fallback"
)

// CgroupNote and TasksetNote are the fixed statements each mode carries into
// the report. They are constants so no run can soften the fallback wording.
const (
	CgroupNote = "cgroup-v2 transient scope with CPUQuota and MemoryMax enforced by the kernel; " +
		"cpu.max, memory.max, memory.peak and memory.events read back from the scope's cgroup files. " +
		"The load generator ran outside the scope."
	TasksetNote = "taskset-fallback: delegated cgroup control was unavailable, so the server was pinned " +
		"to dedicated cores with GOMAXPROCS matched. This run validates dedicated-core behavior, " +
		"NOT Kubernetes-style CPU quota throttling, and no kernel memory bound was applied — " +
		"the memory evidence is /proc VmHWM, not cgroup memory.peak."
)

// Confinement records the boundary the server actually ran inside.
type Confinement struct {
	Mode          ConfinementMode `json:"mode"`
	Unit          string          `json:"unit,omitempty"`
	ScopePath     string          `json:"scope_path,omitempty"`
	CPUMaxRaw     string          `json:"cpu_max_raw,omitempty"`
	CPUQuotaUsec  int64           `json:"cpu_quota_usec,omitempty"`
	CPUPeriodUsec int64           `json:"cpu_period_usec,omitempty"`
	EffectiveCPUs float64         `json:"effective_cpus"`
	MemoryMaxRaw  string          `json:"memory_max_raw,omitempty"`
	MemoryMaxByte int64           `json:"memory_max_bytes,omitempty"`
	TasksetCPUs   string          `json:"taskset_cpus,omitempty"`
	GOMAXPROCS    int             `json:"gomaxprocs,omitempty"`
	// Note states what this mode does and does not validate. Always present.
	Note string `json:"note"`
}

// Phase is one step of the protocol.
type Phase struct {
	Name        string    `json:"name"`
	StartedAt   time.Time `json:"started_at"`
	EndedAt     time.Time `json:"ended_at"`
	DurationSec float64   `json:"duration_sec"`
	Completed   bool      `json:"completed"`
	Detail      string    `json:"detail,omitempty"`
	Error       string    `json:"error,omitempty"`
}

// Command is one external invocation, recorded verbatim.
type Command struct {
	Phase       string    `json:"phase"`
	Argv        []string  `json:"argv"`
	Dir         string    `json:"dir,omitempty"`
	StartedAt   time.Time `json:"started_at"`
	DurationSec float64   `json:"duration_sec"`
	ExitCode    int       `json:"exit_code"`
	LogPath     string    `json:"log_path,omitempty"`
	Error       string    `json:"error,omitempty"`
}

// LoadResults collects every loadsim invocation the protocol makes.
type LoadResults struct {
	// Sustained and Burst come from the main run (settle -> sustained ->
	// burst). PostBurst* come from the recovery probe whose settle window IS
	// the two-minute recovery allowance.
	Sustained LoadPhase `json:"sustained"`
	Burst     LoadPhase `json:"burst"`
	// PostBurstAllowance is the 0-120s window after burst end. Reported as
	// evidence, deliberately NOT gated: it is the interval the contract
	// allows the system to still be recovering in.
	PostBurstAllowance LoadPhase `json:"post_burst_allowance"`
	// PostBurstProof is the 120-240s window after burst end. Gated against
	// the sustained bounds.
	PostBurstProof LoadPhase `json:"post_burst_proof"`
	// CrashRun is the run during which the server is killed. Its latency
	// numbers are not gated; its ACK ledger is the recovery evidence.
	CrashRun LoadPhase `json:"crash_run"`

	ReportPaths map[string]string `json:"report_paths"`
	LedgerPath  string            `json:"ack_ledger_path"`
	Ledger      LedgerSummary     `json:"ack_ledger_summary"`
}

// LoadPhase is one measured phase pulled out of a loadsim report.
type LoadPhase struct {
	Present      bool    `json:"present"`
	Source       string  `json:"source,omitempty"`
	Phase        string  `json:"phase,omitempty"`
	DurationSec  float64 `json:"duration_sec"`
	Samples      int64   `json:"ack_samples"`
	P50Ms        float64 `json:"ack_p50_ms"`
	P90Ms        float64 `json:"ack_p90_ms"`
	P99Ms        float64 `json:"ack_p99_ms"`
	P999Ms       float64 `json:"ack_p999_ms"`
	MaxMs        float64 `json:"ack_max_ms"`
	PointsSent   int64   `json:"points_sent"`
	PointsAcked  int64   `json:"points_acked"`
	PointsPerSec float64 `json:"points_acked_per_sec"`
	RequestsOK   int64   `json:"requests_ok"`
	RequestsErr  int64   `json:"requests_err"`
	Exhausted    int64   `json:"resource_exhausted"`
	Unavailable  int64   `json:"unavailable"`
	OtherErrors  int64   `json:"other_errors"`
	FirstErr     string  `json:"first_error,omitempty"`
}

// AckRatio is acked/sent. Zero sent is reported as 0, never as 1: a phase
// that sent nothing did not achieve 100% delivery.
func (p LoadPhase) AckRatio() float64 {
	if p.PointsSent <= 0 {
		return 0
	}
	return float64(p.PointsAcked) / float64(p.PointsSent)
}

// LedgerSummary is the digest of the ACK ledger carried into the report.
type LedgerSummary struct {
	Present          bool         `json:"present"`
	Schema           string       `json:"schema,omitempty"`
	Final            bool         `json:"final"`
	FlushedAt        time.Time    `json:"flushed_at"`
	FlushIntervalSec float64      `json:"flush_interval_sec"`
	WindowSecs       int64        `json:"window_secs"`
	Windows          int          `json:"window_count"`
	FirstWindow      int64        `json:"first_window"`
	LastWindow       int64        `json:"last_window"`
	Totals           LedgerCounts `json:"totals"`

	// PreKillCopyAt and PreKillCopyPath record the snapshot the orchestrator
	// took of the on-disk ledger immediately before sending SIGKILL. Its
	// existence is the evidence that the ledger was persisted before the
	// crash rather than reconstructed after it.
	PreKillCopyAt    time.Time `json:"pre_kill_copy_at"`
	PreKillCopyPath  string    `json:"pre_kill_copy_path,omitempty"`
	PreKillCopyBytes int64     `json:"pre_kill_copy_bytes"`
}

// RecoveryResult is the kill -9 phase outcome.
type RecoveryResult struct {
	KilledAt         time.Time `json:"killed_at"`
	KillSignal       string    `json:"kill_signal"`
	KilledPID        int       `json:"killed_pid"`
	RestartedAt      time.Time `json:"restarted_at"`
	ReadyAt          time.Time `json:"ready_at"`
	TimeToReadySec   float64   `json:"time_to_ready_sec"`
	ReadyObserved    bool      `json:"ready_observed"`
	CrashIntervalSec float64   `json:"crash_interval_sec"`

	// Stats come from the server's own recovery log line. SkippedSeries has
	// no Prometheus gauge (see Gaps), so the log is the only source.
	StatsSource      string  `json:"stats_source"`
	StatsFound       bool    `json:"stats_found"`
	FinalizedWindows int     `json:"finalized_windows"`
	ReplayedRows     int     `json:"replayed_rows"`
	ReplayedSeries   int     `json:"replayed_series_windows"`
	SeededBaselines  int     `json:"seeded_baselines"`
	SkippedSeries    int     `json:"skipped_series"`
	DurationSec      float64 `json:"recovery_duration_sec"`

	// Bounds is the per-window at-least-once comparison across the whole
	// crash run: exact where the ledger says acked == attempted, a range
	// across the crash interval.
	Bounds CrashBoundReport `json:"crash_bounds"`
}

// MemoryResult is the memory evidence. Basis names which number is load
// bearing in this confinement mode.
type MemoryResult struct {
	Basis          string            `json:"basis"`
	PeakBytes      int64             `json:"peak_bytes"`
	PeakSource     string            `json:"peak_source"`
	LimitBytes     int64             `json:"limit_bytes"`
	OOMKills       int64             `json:"oom_kills"`
	OOMSource      string            `json:"oom_source"`
	OOMObserved    bool              `json:"oom_counter_observed"`
	VmHWMBytes     int64             `json:"vmhwm_bytes"`
	PerIncarnation []MemoryIncarnate `json:"per_incarnation"`
}

// MemoryIncarnate is one server process lifetime's memory evidence. The gate
// restarts the server once (after the kill), so there are two of these.
type MemoryIncarnate struct {
	Label      string `json:"label"`
	PID        int    `json:"pid"`
	PeakBytes  int64  `json:"peak_bytes"`
	VmHWMBytes int64  `json:"vmhwm_bytes"`
	OOMKills   int64  `json:"oom_kills"`
	ScopePath  string `json:"scope_path,omitempty"`
}

// DiskResult is the measured data-directory footprint, by tier.
type DiskResult struct {
	DataDir        string     `json:"data_dir"`
	MeasuredAt     time.Time  `json:"measured_at"`
	Tiers          []DiskTier `json:"tiers"`
	TotalBytes     int64      `json:"total_bytes"`
	TotalLimit     int64      `json:"total_limit_bytes"`
	FreeBytes      int64      `json:"free_bytes"`
	FreeMinBytes   int64      `json:"free_bytes_required"`
	UnclassifiedB  int64      `json:"unclassified_bytes"`
	UnclassifiedFs []string   `json:"unclassified_files,omitempty"`
	// GaugeBytes is the server's own attribution, read from
	// otelcontext_disk_component_bytes. It corroborates the filesystem walk;
	// the walk is what the gate asserts on.
	GaugeBytes     map[string]float64 `json:"gauge_component_bytes"`
	GaugeHighWater map[string]float64 `json:"gauge_component_high_water_bytes"`
}

// DiskTier is one asserted partition of the data directory.
type DiskTier struct {
	Name       string   `json:"name"`
	Bytes      int64    `json:"bytes"`
	LimitBytes int64    `json:"limit_bytes"`
	Files      []string `json:"files,omitempty"`
	// Projected marks a tier whose limit is checked against the projection
	// rather than against the demonstrated bytes.
	Projected bool `json:"projected"`
}

// QueryResults is the completeness evidence.
type QueryResults struct {
	PrefillRangeStart time.Time     `json:"prefill_range_start"`
	PrefillRangeEnd   time.Time     `json:"prefill_range_end"`
	PrefillWindows    int           `json:"prefill_windows_expected"`
	Checks            []QueryCheck  `json:"checks"`
	MCPTools          []MCPToolCall `json:"mcp_tools"`
}

// QueryCheck is one HTTP query surface answered over the seven-day range.
type QueryCheck struct {
	Name           string  `json:"name"`
	URL            string  `json:"url"`
	Status         int     `json:"status"`
	DurationSec    float64 `json:"duration_sec"`
	Coverage       string  `json:"coverage,omitempty"`
	CoverageSource string  `json:"coverage_source,omitempty"`
	// CoverageExpected is the marker this surface was required to declare.
	// Empty means the marker was recorded but not gated.
	CoverageExpected string `json:"coverage_expected,omitempty"`
	TruncatedFound   bool   `json:"truncated_flag_found"`
	TruncatedTrue    bool   `json:"truncated_true"`
	// WindowsReturned and WindowsExpected apply to the per-window surfaces.
	WindowsReturned int                `json:"windows_returned,omitempty"`
	WindowsExpected int                `json:"windows_expected,omitempty"`
	MissingWindows  int                `json:"missing_windows,omitempty"`
	Scalars         map[string]float64 `json:"scalars,omitempty"`
	BodyBytes       int                `json:"body_bytes"`
	Error           string             `json:"error,omitempty"`
}

// MCPToolCall is one aggregate-backed MCP tool answered over the full range.
type MCPToolCall struct {
	Tool           string  `json:"tool"`
	Arguments      string  `json:"arguments"`
	Status         int     `json:"status"`
	DurationSec    float64 `json:"duration_sec"`
	RPCError       string  `json:"rpc_error,omitempty"`
	TruncatedFound bool    `json:"truncated_flag_found"`
	TruncatedTrue  bool    `json:"truncated_true"`
	ResultBytes    int     `json:"result_bytes"`
	Error          string  `json:"error,omitempty"`
}

// MetricSample is one Prometheus scrape, reduced to the metrics the gate
// asserts on.
type MetricSample struct {
	At     time.Time          `json:"t"`
	Phase  string             `json:"phase"`
	Values map[string]float64 `json:"v"`
}

// Assertion is one threshold decision. Every threshold in the frozen contract
// produces exactly one of these, pass or fail, never absent.
type Assertion struct {
	ID          string `json:"id"`
	Category    string `json:"category"`
	Description string `json:"description"`
	Comparator  string `json:"comparator"`
	Threshold   string `json:"threshold"`
	Actual      string `json:"actual"`
	Pass        bool   `json:"pass"`
	// Basis names the evidence source. A basis that is not the contract's
	// primary source (a taskset-fallback memory number, say) sets Degraded.
	Basis    string `json:"basis"`
	Degraded bool   `json:"degraded"`
	Detail   string `json:"detail,omitempty"`
}
