//go:build readproof

package readproof

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/aggregate"
)

const (
	aggprefillEnv = "OTELCONTEXT_AGGPREFILL_BINARY"
	// prefillWindowsEnv sizes the seeded history. The decision's shape is the
	// full seven days (2016 windows); hosted CI cannot write 12M bucket rows
	// inside its budget, so the workflow passes a smaller count and the
	// artifact records both numbers.
	prefillWindowsEnv     = "OTELCONTEXT_READPROOF_PREFILL_WINDOWS"
	sevenDayWindows       = 2016
	aggprefillSeries      = 6000
	aggprefillServices    = 120
	aggregateRCAService   = "checkout-svc-000"
	aggregateReadyTimeout = 4 * time.Minute
)

func TestReadLatencyAggregate(t *testing.T) {
	binary := requireBinary(t)
	prefillBinary := os.Getenv(aggprefillEnv)
	if prefillBinary == "" {
		t.Fatalf("%s is required for the aggregate shape", aggprefillEnv)
	}
	windows := sevenDayWindows
	if raw := os.Getenv(prefillWindowsEnv); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > sevenDayWindows {
			t.Fatalf("%s=%q: want 1..%d", prefillWindowsEnv, raw, sevenDayWindows)
		}
		windows = n
	}

	started := time.Now()
	objectives := Objectives{Requests: 200, WarmP99MS: 500, ColdMS: 2000}
	proof := newProof(t, "aggregate", binary, objectives)
	proof.Prefill = Prefill{RequestedWindows: sevenDayWindows, Windows: windows, Series: aggprefillSeries, Services: aggprefillServices}
	if windows != sevenDayWindows {
		proof.Notes = append(proof.Notes, "prefill is shorter than the decision's seven days: "+strconv.Itoa(windows)+" five-minute windows were seeded ("+prefillWindowsEnv+")")
	}

	dir := stateDir(t)
	app := newAppProcess(t, binary, dir, "aggregate")
	proof.ServerEnv = app.env
	// The prefill seeds `windows` aligned five-minute windows ending at the
	// current one, so the full horizon starts (windows-1) windows before the
	// current window's start; ending at now keeps a seven-day request inside
	// the engine's read-range cap.
	fullEnd := time.Now().UTC()
	fullStart := time.Unix(aggregate.WindowStart(fullEnd), 0).UTC().Add(-time.Duration(windows-1) * aggregate.WindowSize)
	plan := endpoints(app, aggregateRCAService, fullStart, fullEnd)
	for _, ep := range plan {
		proof.Measurements = append(proof.Measurements, ep.m)
	}
	defer writeProof(t, proof, started)

	workers := runtime.NumCPU()
	if workers > 8 {
		workers = 8
	}
	prefillStarted := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, prefillBinary,
		"-db", filepath.Join(dir, "aggregate.db"),
		"-windows", strconv.Itoa(windows),
		"-workers", strconv.Itoa(workers))
	out, err := cmd.CombinedOutput()
	proof.Prefill.Seconds = round3(time.Since(prefillStarted).Seconds())
	proof.Prefill.AggregateDBBytes = fileSize(filepath.Join(dir, "aggregate.db"))
	if err != nil {
		proof.Prefill.Error = err.Error() + ": " + tail(string(out), 2000)
		markUnmeasured(proof, "aggregate prefill failed: "+err.Error())
		t.Fatalf("aggprefill: %v\n%s", err, tail(string(out), 4000))
	}
	t.Logf("prefill: %d windows in %.1f s, aggregate.db %d bytes", windows, proof.Prefill.Seconds, proof.Prefill.AggregateDBBytes)

	if err := app.start(); err != nil {
		markUnmeasured(proof, "server start failed: "+err.Error())
		t.Fatalf("start: %v", err)
	}
	defer app.stop()
	sampler := startRSSSampler(app)
	readyCtx, cancelReady := context.WithTimeout(context.Background(), aggregateReadyTimeout)
	defer cancelReady()
	if err := app.waitReady(readyCtx); err != nil {
		proof.RSS = sampler.finish(0)
		markUnmeasured(proof, "server never became ready")
		t.Fatalf("ready: %v", err)
	}
	proof.ReadySeconds = round3(time.Since(app.started).Seconds())
	t.Logf("ready after %.1f s", proof.ReadySeconds)

	measureFrom := time.Since(app.started).Seconds()
	sampler.sample()
	for _, ep := range plan {
		measure(t, ep.m, ep.fn, objectives)
	}
	proof.RSS = sampler.finish(measureFrom)
}
