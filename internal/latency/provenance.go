// Package latency defines the shared provenance vocabulary for percentile
// values. Percentile algorithms remain with the packages that own the data.
package latency

// Status says what kind of claim a percentile value can support.
type Status string

const (
	StatusMeasured    Status = "measured"
	StatusApproximate Status = "approximate"
	StatusEstimated   Status = "estimated"
	StatusBounded     Status = "bounded"
	StatusUnavailable Status = "unavailable"
)

const (
	MethodPercentileDisc           = "percentile_disc"
	MethodOrderedRank              = "ordered_rank"
	MethodDDSketch                 = "ddsketch"
	MethodNearestRank              = "nearest_rank"
	MethodRetainedPrefix           = "retained_prefix"
	MethodAverageMultiplier        = "average_multiplier"
	MethodRollingObservationWindow = "rolling_observation_window"
)

const (
	ReasonNoObservations           = "no_observations"
	ReasonPercentileNotRecorded    = "percentile_not_recorded"
	ReasonInvalidDistribution      = "invalid_source_distribution"
	ReasonSketchCollapsed          = "sketch_collapsed"
	ReasonSketchSaturated          = "sketch_saturated"
	ReasonSketchCollapsedSaturated = "sketch_collapsed_and_saturated"
)

// LowSampleThreshold is descriptive, not suppressive. Empirical percentiles
// below this population remain visible with LowSample set.
const LowSampleThreshold uint64 = 100

// Provenance carries the claim independently for each percentile.
type Provenance struct {
	P50 *Percentile `json:"p50,omitempty"`
	P95 *Percentile `json:"p95,omitempty"`
	P99 *Percentile `json:"p99,omitempty"`
}

// Percentile describes how one numeric percentile was produced.
type Percentile struct {
	Status             Status  `json:"status"`
	Method             string  `json:"method"`
	SampleCount        uint64  `json:"sample_count"`
	PopulationCount    uint64  `json:"population_count,omitempty"`
	SampleLimit        uint64  `json:"sample_limit,omitempty"`
	LowSample          bool    `json:"low_sample,omitempty"`
	SketchScale        uint8   `json:"sketch_scale,omitempty"`
	RelativeErrorBound float64 `json:"relative_error_bound,omitempty"`
	Degraded           bool    `json:"degraded,omitempty"`
	Collapsed          bool    `json:"collapsed,omitempty"`
	Saturations        uint64  `json:"saturations,omitempty"`
	EstimateFactor     float64 `json:"estimate_factor,omitempty"`
	Reason             string  `json:"reason,omitempty"`
}
