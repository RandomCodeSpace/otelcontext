package ingest

import (
	"errors"
	"testing"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/aggregate"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"
)

// stubApplier stands in for the durable group-commit writer.
type stubApplier struct{ err error }

func (s stubApplier) Apply(m aggregate.DeltaMap) uint64 { return 0 }

func (s stubApplier) ApplyErr(m aggregate.DeltaMap) (uint64, error) { return 0, s.err }

// engineWithApplier builds an engine in the given mode with a stubbed apply path.
func engineWithApplier(t *testing.T, mode string, err error) *aggregate.Engine {
	t.Helper()
	eng, buildErr := aggregate.NewEngine(aggregate.EngineConfig{Mode: mode})
	if buildErr != nil {
		t.Fatalf("NewEngine: %v", buildErr)
	}
	eng.SetApplier(stubApplier{err: err})
	return eng
}

// reducerWithOneSpan produces a reducer holding a single accepted span.
func reducerWithOneSpan(eng *aggregate.Engine) *aggregate.Reducer {
	now := time.Now()
	r := eng.NewReducer(now)
	r.ReduceSpan(aggregate.SpanInput{
		Tenant:         "default",
		Service:        "checkout",
		SpanName:       "GET /orders",
		Method:         "GET",
		HTTPStatusCode: 200,
		Timestamp:      now,
		DurationMicros: 1200,
	})
	return r
}

// TestApplyAggregateMapsSaturationToResourceExhausted covers the backpressure
// contract: a saturated group-commit writer answers like the raw pipeline's
// ErrQueueFull, which the HTTP OTLP handler already turns into 429.
func TestApplyAggregateMapsSaturationToResourceExhausted(t *testing.T) {
	saturated := &aggregate.SaturationError{Bound: "waiters", Current: 512, Limit: 512}
	for _, mode := range []string{aggregate.ModeShadow, aggregate.ModeAggregate} {
		t.Run(mode, func(t *testing.T) {
			eng := engineWithApplier(t, mode, saturated)
			err := applyAggregate(eng, reducerWithOneSpan(eng))
			if err == nil {
				t.Fatal("saturated writer was acknowledged as success")
			}
			st, ok := grpcstatus.FromError(err)
			if !ok || st.Code() != codes.ResourceExhausted {
				t.Fatalf("error = %v, want RESOURCE_EXHAUSTED", err)
			}
			if !isQueueFull(err) {
				t.Fatal("HTTP handler would not map this error to 429")
			}
		})
	}
}

// TestApplyAggregateCommitFailureByMode: in aggregate mode the store is the
// authoritative dataset and a failed commit must not be acknowledged; in shadow
// mode the legacy path is still the truth and raw telemetry must not be lost
// over an aggregate-side problem.
func TestApplyAggregateCommitFailureByMode(t *testing.T) {
	boom := errors.New("disk on fire")

	eng := engineWithApplier(t, aggregate.ModeAggregate, boom)
	err := applyAggregate(eng, reducerWithOneSpan(eng))
	st, ok := grpcstatus.FromError(err)
	if err == nil || !ok || st.Code() != codes.Unavailable {
		t.Fatalf("aggregate mode commit failure = %v, want UNAVAILABLE", err)
	}

	shadow := engineWithApplier(t, aggregate.ModeShadow, boom)
	if err := applyAggregate(shadow, reducerWithOneSpan(shadow)); err != nil {
		t.Fatalf("shadow mode commit failure = %v, want nil (legacy path is the source of truth)", err)
	}
}

func TestApplyAggregateNilEngineIsNoop(t *testing.T) {
	if err := applyAggregate(nil, nil); err != nil {
		t.Fatalf("legacy mode (nil engine) = %v, want nil", err)
	}
}
