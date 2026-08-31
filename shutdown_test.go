package main

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

func TestExecuteShutdownRunsOwnersInOrder(t *testing.T) {
	var order []string
	steps := []shutdownStep{
		{name: "admission", run: func(context.Context) error { order = append(order, "admission"); return nil }},
		{name: "ingest", run: func(context.Context) error { order = append(order, "ingest"); return nil }},
		{name: "derived", run: func(context.Context) error { order = append(order, "derived"); return nil }},
		{name: "database", run: func(context.Context) error { order = append(order, "database"); return nil }},
	}

	report, err := executeShutdown(context.Background(), steps)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"admission", "ingest", "derived", "database"}; !reflect.DeepEqual(order, want) {
		t.Fatalf("order = %v, want %v", order, want)
	}
	if len(report.Steps) != len(steps) || report.CompletedAt.IsZero() {
		t.Fatalf("incomplete report: %#v", report)
	}
}

func TestExecuteShutdownReturnsOwnerFailureAndContinuesSafeStops(t *testing.T) {
	wantErr := errors.New("final flush failed")
	closed := false
	steps := []shutdownStep{
		{name: "tsdb", run: func(context.Context) error { return wantErr }},
		{name: "database", run: func(context.Context) error { closed = true; return nil }},
	}

	report, err := executeShutdown(context.Background(), steps)
	if !errors.Is(err, wantErr) {
		t.Fatalf("error = %v, want %v", err, wantErr)
	}
	if !closed {
		t.Fatal("later safe stop did not run after an owner error")
	}
	if report.Steps[0].Error == "" {
		t.Fatalf("failure missing from report: %#v", report)
	}
}

func TestExecuteShutdownBoundsBlockedOwner(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()

	started := time.Now()
	_, err := executeShutdown(ctx, []shutdownStep{{
		name: "blocked",
		run: func(context.Context) error {
			<-release
			return nil
		},
	}})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want deadline exceeded", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("bounded shutdown took %s", elapsed)
	}
}

func TestGracefulStopAdmissionForcesOnDeadline(t *testing.T) {
	blocked := make(chan struct{})
	forced := false
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	err := gracefulStopAdmission(ctx, func() { <-blocked }, func() {
		forced = true
		close(blocked)
	})
	if !errors.Is(err, context.DeadlineExceeded) || !forced {
		t.Fatalf("error=%v forced=%v, want deadline and force", err, forced)
	}
}
