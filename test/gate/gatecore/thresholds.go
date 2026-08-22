package gatecore

// Thresholds are the frozen pass criteria from issue #202 Q3, encoded as
// asserted values rather than as prose in a runbook.
//
// Every field here produces at least one Assertion on every run. Changing a
// number here changes what the gate certifies, so it is a contract change and
// belongs in a commit that says so.
type Thresholds struct {
	// --- Sustained phase ---
	SustainedHours        float64 `json:"sustained_hours"`
	SustainedPointsPerSec float64 `json:"sustained_points_per_sec"`
	// SustainedRateTolerance is how far below the offered rate the achieved
	// rate may sit before the phase is judged not to have run at the
	// contracted load. A phase that quietly ran at 4k pts/s must not pass a
	// 10k pts/s gate on the strength of its excellent latency.
	SustainedRateTolerance    float64 `json:"sustained_rate_tolerance"`
	AckP99MaxMs               float64 `json:"ack_p99_max_ms"`
	AckRatioMin               float64 `json:"ack_ratio_min"`
	MaxResourceExhausted      int64   `json:"max_resource_exhausted"`
	MaxLatePointsDelta        float64 `json:"max_late_points_delta"`
	MaxAdmissionRejectedDelta float64 `json:"max_admission_rejected_delta"`
	MaxIdentityOverflowDelta  float64 `json:"max_identity_overflow_delta"`

	// BacklogAllowanceFraction and BacklogAllowanceFloorRows define "no
	// sustained backlog growth": the fitted growth across the phase and the
	// first-to-last delta must both stay inside
	// max(floor, fraction * peak observed).
	BacklogAllowanceFraction  float64 `json:"backlog_allowance_fraction"`
	BacklogAllowanceFloorRows float64 `json:"backlog_allowance_floor_rows"`
	BacklogMinSamples         int     `json:"backlog_min_samples"`

	// --- Burst phase ---
	BurstPointsPerSec    float64 `json:"burst_points_per_sec"`
	BurstSeconds         float64 `json:"burst_seconds"`
	BurstRecoverySeconds float64 `json:"burst_recovery_seconds"`

	// --- Recovery ---
	ReadySeconds     float64 `json:"ready_seconds"`
	MaxSkippedSeries int     `json:"max_skipped_series"`

	// --- Memory ---
	MemoryPeakMaxBytes int64 `json:"memory_peak_max_bytes"`
	MaxOOMKills        int64 `json:"max_oom_kills"`

	// --- Disk, every partition ---
	DiskMainMaxBytes       int64 `json:"disk_main_max_bytes"`
	DiskAggregateMaxBytes  int64 `json:"disk_aggregate_max_bytes"`
	DiskDLQMaxBytes        int64 `json:"disk_dlq_max_bytes"`
	DiskWALTempTLSMaxBytes int64 `json:"disk_wal_temp_tls_max_bytes"`
	DiskTotalMaxBytes      int64 `json:"disk_total_max_bytes"`
	DiskFreeMinBytes       int64 `json:"disk_free_min_bytes"`

	// --- Main-tier projection ---
	ProjectionMinSamples int `json:"projection_min_samples"`
	// ProjectionMinWindowSpan is how many completed five-minute windows the
	// steady samples must actually span. Samples are cheap and a short span is
	// how a startup transient gets extrapolated across two days.
	ProjectionMinWindowSpan  float64 `json:"projection_min_window_span"`
	ProjectionZ              float64 `json:"projection_upper_estimate_z"`
	ProjectionHorizonWindows int     `json:"projection_horizon_windows"`

	// --- Query completeness ---
	PrefillWindows int `json:"prefill_windows"`
	PrefillSeries  int `json:"prefill_series"`
	// RequiredCoverage is the marker a fully aggregate-derived surface must
	// declare. Per-surface expectations live in QueryConfig.
	RequiredCoverage string `json:"required_coverage"`
}

// Byte-size helpers, spelled out so the numbers in DefaultThresholds read the
// way the contract does.
const (
	MiB int64 = 1 << 20
	GiB int64 = 1 << 30
)

// DefaultThresholds returns the frozen contract.
func DefaultThresholds() Thresholds {
	return Thresholds{
		SustainedHours:            3,
		SustainedPointsPerSec:     10000,
		SustainedRateTolerance:    0.05,
		AckP99MaxMs:               250,
		AckRatioMin:               0.999,
		MaxResourceExhausted:      0,
		MaxLatePointsDelta:        0,
		MaxAdmissionRejectedDelta: 0,
		MaxIdentityOverflowDelta:  0,

		BacklogAllowanceFraction:  0.10,
		BacklogAllowanceFloorRows: 5000,
		BacklogMinSamples:         12,

		BurstPointsPerSec:    20000,
		BurstSeconds:         60,
		BurstRecoverySeconds: 120,

		ReadySeconds:     60,
		MaxSkippedSeries: 0,

		MemoryPeakMaxBytes: 4 * GiB,
		MaxOOMKills:        0,

		DiskMainMaxBytes:       4*GiB + GiB/2,
		DiskAggregateMaxBytes:  GiB + GiB/2,
		DiskDLQMaxBytes:        GiB / 2,
		DiskWALTempTLSMaxBytes: GiB / 2,
		DiskTotalMaxBytes:      7 * GiB,
		DiskFreeMinBytes:       GiB,

		ProjectionMinSamples:     6,
		ProjectionMinWindowSpan:  6,
		ProjectionZ:              2,
		ProjectionHorizonWindows: HorizonWindows,

		PrefillWindows:   2016,
		PrefillSeries:    6000,
		RequiredCoverage: "full",
	}
}
