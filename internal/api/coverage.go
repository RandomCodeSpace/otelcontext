package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/aggregate"
)

// setCoverage stamps the data-coverage header on a response.
//
// Endpoints that return a bare JSON array cannot carry an additive "coverage"
// field without changing their shape, and silently wrapping them in an
// envelope would break every existing client. The header is how those
// endpoints stay honest without breaking (#164).
func setCoverage(w http.ResponseWriter, c aggregate.Coverage) {
	if c == "" {
		return
	}
	w.Header().Set(aggregate.CoverageHeader, string(c))
}

// Range-clamp headers (#217). When an aggregate-mode request asks for more
// history than the engine's read-range cap allows, the handler clamps the
// range instead of refusing the request, and these headers say so: what the
// client asked for, and what the response actually covers.
const (
	// RequestedStartHeader restates the start the client requested.
	RequestedStartHeader = "OtelContext-Requested-Start"
	// EffectiveStartHeader carries the clamped start the response covers.
	EffectiveStartHeader = "OtelContext-Effective-Start"
)

// clampAggregateRange bounds a requested [start, end) range to the aggregate
// engine's read-range cap and stamps the clamp on the response, so a
// shortened answer is never mistaken for a complete one (#217).
//
// The comparison mirrors the window alignment the engine applies in plan():
// start rounds down to its window, end rounds up to the next boundary. The
// clamped start keeps the most recent MaxReadWindowSpan of the request —
// retention holds up to the cap plus purge lag, so "everything" queries only
// ever lose the oldest slice, which is the slice retention is about to delete
// anyway. Selector.Validate stays in place as the hard backstop.
func clampAggregateRange(w http.ResponseWriter, start, end time.Time) time.Time {
	alignedStart := aggregate.WindowStart(start)
	alignedEnd := aggregate.WindowStart(end.Add(aggregate.WindowSize - time.Second))
	if alignedEnd-alignedStart <= aggregate.MaxReadWindowSpan {
		return start
	}
	eff := time.Unix(alignedEnd-aggregate.MaxReadWindowSpan, 0).UTC()
	w.Header().Set(RequestedStartHeader, start.UTC().Format(time.RFC3339))
	w.Header().Set(EffectiveStartHeader, eff.Format(time.RFC3339))
	return eff
}

// aggregateReadStatus maps an aggregate read error to its HTTP status. A
// selector the engine refuses is a client-shaped problem — the requested
// range or scope was invalid — not a server fault (#217).
func aggregateReadStatus(err error) int {
	if errors.Is(err, aggregate.ErrSelectorUnbounded) {
		return http.StatusBadRequest
	}
	return http.StatusInternalServerError
}
