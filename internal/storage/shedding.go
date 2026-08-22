package storage

// SheddingState is the staged raw-retention shedding level driven by the disk
// watchdog (#201 Q5). It lives in this package because the watchdog owns it;
// internal/ingest already imports storage, so the exemplar policy can read the
// same type without a new dependency edge.
//
// The ladder is deliberately short. Every extra rung is another state an
// operator has to reason about at 3am while the volume fills.
type SheddingState int32

const (
	// SheddingNone is normal operation: the raw exemplar policy's own budgets
	// are the only gate.
	SheddingNone SheddingState = iota
	// SheddingErrorsOnly (>=90% of the enforcement ceiling) keeps error trace
	// and log exemplars and refuses healthy/slow/WARN raw retention. Aggregate
	// accounting is untouched — it is the thing that stays complete.
	SheddingErrorsOnly
	// SheddingRawOff (>=95%) refuses ALL new raw exemplar admission and the
	// exemplar DLQ fallback with it. Writing an exemplar to the DLQ at 95% is
	// still writing to the disk that is about to fill.
	SheddingRawOff
)

// String renders the state as its metric label.
func (s SheddingState) String() string {
	switch s {
	case SheddingErrorsOnly:
		return "errors_only"
	case SheddingRawOff:
		return "raw_off"
	default:
		return "none"
	}
}
