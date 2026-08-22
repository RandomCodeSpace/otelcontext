package ingest

import (
	"context"
	"fmt"
	"io/fs"
	"syscall"
	"testing"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/aggregate"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"
)

// Raw exemplar shedding never turns a successful aggregate Export into a
// retryable failure. The authoritative aggregate commit failing on ENOSPC or
// SQLITE_FULL is the one exception (#201 Q5): a durable ACK asserts the deltas
// are committed, and answering OK for data that hit a full disk is data loss
// with better branding.
//
// The stub applier and reducer helpers are shared with
// aggregate_saturation_test.go.

func TestAggregateCommitDiskFullFailsTheExport(t *testing.T) {
	diskFull := []struct {
		name string
		err  error
	}{
		{"ENOSPC", fmt.Errorf("group commit: %w", syscall.ENOSPC)},
		{"path ENOSPC", &fs.PathError{Op: "write", Path: "/data/aggregate.db-wal", Err: syscall.ENOSPC}},
		{"SQLITE_FULL", fmt.Errorf("commit: %s", "database or disk is full")},
	}
	for _, tc := range diskFull {
		t.Run(tc.name, func(t *testing.T) {
			eng := engineWithApplier(t, aggregate.ModeAggregate, tc.err)
			err := applyAggregate(eng, reducerWithOneSpan(eng))
			if err == nil {
				t.Fatal("a commit that hit a full disk was acknowledged as success")
			}
			st, ok := grpcstatus.FromError(err)
			if !ok || st.Code() != codes.ResourceExhausted {
				t.Fatalf("error = %v, want RESOURCE_EXHAUSTED so the client backs off and retries", err)
			}
			if !isQueueFull(err) {
				t.Fatal("the HTTP OTLP handler would not map this to a retryable 429")
			}
		})
	}
}

// TestTraceExportFailsOnDiskFullCommit is the same contract at the Export
// boundary, which is the surface an OTLP client actually sees.
func TestTraceExportFailsOnDiskFullCommit(t *testing.T) {
	now := time.Now().UTC()
	eng := engineWithApplier(t, aggregate.ModeAggregate, fmt.Errorf("commit deltas: %w", syscall.ENOSPC))
	srv := NewTraceServer(nil, nil, aggTestConfig())
	srv.SetAggregateEngine(eng)

	resp, err := srv.Export(context.Background(), ackErrorSpanRequest(3, now))
	if err == nil {
		t.Fatalf("Export succeeded (resp=%v) on a commit that hit a full disk", resp)
	}
	st, ok := grpcstatus.FromError(err)
	if !ok || st.Code() != codes.ResourceExhausted {
		t.Fatalf("Export error = %v, want RESOURCE_EXHAUSTED", err)
	}
}

// TestShadowModeDiskFullDoesNotFailTheExport: in shadow mode the legacy raw
// path is still the source of truth. Failing the Export there would convert a
// shadow-side disk problem into raw telemetry loss.
func TestShadowModeDiskFullDoesNotFailTheExport(t *testing.T) {
	eng := engineWithApplier(t, aggregate.ModeShadow, fmt.Errorf("group commit: %w", syscall.ENOSPC))
	if err := applyAggregate(eng, reducerWithOneSpan(eng)); err != nil {
		t.Fatalf("shadow-mode disk-full commit failed the Export: %v", err)
	}
}
