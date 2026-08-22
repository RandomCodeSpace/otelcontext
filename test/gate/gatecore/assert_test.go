package gatecore

import (
	"strings"
	"testing"
	"time"
)

// passingResult builds a Result in which every threshold in the frozen
// contract is met. Each test then breaks exactly one thing and checks that the
// corresponding assertion — and only sensible collateral — flips to FAIL.
func passingResult() *Result {
	cfg := DefaultConfig()
	t0 := time.Date(2026, 8, 22, 0, 0, 0, 0, time.UTC)
	th := cfg.Thresholds

	sustainedSent := int64(th.SustainedHours * 3600 * th.SustainedPointsPerSec)
	burstSent := int64(th.BurstSeconds * th.BurstPointsPerSec)

	r := &Result{
		Schema:          Schema,
		GateVersion:     "test",
		RunID:           "test-run",
		StartedAt:       t0,
		EndedAt:         t0.Add(5 * time.Hour),
		Config:          cfg,
		ServerEnv:       cfg.ServerEnv,
		DurabilityClaim: "Crash-durable on a surviving volume.",
		Phases: []Phase{
			{Name: "prefill", Completed: true, StartedAt: t0, EndedAt: t0.Add(time.Hour)},
			{Name: "server_start", Completed: true, StartedAt: t0, EndedAt: t0.Add(time.Minute)},
			{Name: "main_load", Completed: true, StartedAt: t0, EndedAt: t0.Add(3 * time.Hour)},
			{Name: "post_burst", Completed: true, StartedAt: t0, EndedAt: t0.Add(time.Minute)},
			{Name: "quiet_gap", Completed: true, StartedAt: t0, EndedAt: t0.Add(time.Minute)},
			{Name: "crash_run", Completed: true, StartedAt: t0, EndedAt: t0.Add(10 * time.Minute)},
			{Name: "measure", Completed: true, StartedAt: t0, EndedAt: t0.Add(time.Minute)},
		},
		Confinement: Confinement{
			Mode: ConfinementCgroup, Unit: "u.scope", ScopePath: "/sys/fs/cgroup/u.scope",
			CPUMaxRaw: "200000 100000", EffectiveCPUs: 2,
			MemoryMaxRaw: "4294967296", MemoryMaxByte: 4 * GiB, Note: CgroupNote,
		},
		Load: LoadResults{
			Sustained: LoadPhase{
				Present: true, Phase: "sustained", DurationSec: th.SustainedHours * 3600,
				Samples: 500000, P50Ms: 20, P90Ms: 60, P99Ms: 120, P999Ms: 200, MaxMs: 400,
				PointsSent: sustainedSent, PointsAcked: sustainedSent,
				PointsPerSec: th.SustainedPointsPerSec,
			},
			Burst: LoadPhase{
				Present: true, Phase: "burst", DurationSec: th.BurstSeconds,
				Samples: 20000, P99Ms: 900,
				PointsSent: burstSent, PointsAcked: burstSent - 1000,
			},
			PostBurstAllowance: LoadPhase{Present: true, Phase: "settle", DurationSec: 120, Samples: 100, P99Ms: 700},
			PostBurstProof:     LoadPhase{Present: true, Phase: "sustained", DurationSec: 120, Samples: 100, P99Ms: 130},
			CrashRun:           LoadPhase{Present: true, Phase: "sustained", DurationSec: 600, Samples: 100, P99Ms: 150},
			LedgerPath:         "/tmp/ack-ledger.json",
			Ledger: LedgerSummary{
				Present: true, Final: true, WindowSecs: 300, Windows: 3,
				PreKillCopyAt: t0.Add(3 * time.Hour), PreKillCopyPath: "/tmp/prekill.json", PreKillCopyBytes: 4096,
				Totals: LedgerCounts{AttemptedPoints: 1000, AckedPoints: 990},
			},
		},
		Recovery: RecoveryResult{
			KillSignal: "SIGKILL", KilledPID: 4242, KilledAt: t0.Add(3 * time.Hour),
			ReadyObserved: true, TimeToReadySec: 18, CrashIntervalSec: 20,
			StatsSource: "server-restarted.log", StatsFound: true,
			FinalizedWindows: 2, ReplayedRows: 1841, ReplayedSeries: 904,
			SeededBaselines: 1200, SkippedSeries: 0, DurationSec: 0.4,
			Bounds: CrashBoundReport{
				Evaluated: true, Pass: true, Signal: "spans", WindowSecs: 300,
				WindowsCompared: 3, WindowsExact: 2, WindowsCrashAffected: 1,
				AmbiguityPoints: 60, AckedLossPoints: 0,
				Windows: []WindowBound{
					{WindowStart: 1000, Attempted: 500, Acked: 500, Observed: 500, ObservedFound: true, Exact: true, Pass: true},
					{WindowStart: 1300, CrashAffected: true, Attempted: 500, Acked: 440, Observed: 470, ObservedFound: true, Pass: true},
					{WindowStart: 1600, Attempted: 500, Acked: 500, Observed: 500, ObservedFound: true, Exact: true, Pass: true},
				},
			},
		},
		Memory: MemoryResult{
			Basis: string(ConfinementCgroup), PeakBytes: 2 * GiB, PeakSource: "memory.peak",
			LimitBytes: th.MemoryPeakMaxBytes, OOMKills: 0, OOMSource: "memory.events",
			OOMObserved: true, VmHWMBytes: 1900 * MiB,
			PerIncarnation: []MemoryIncarnate{{Label: "initial", PID: 1, PeakBytes: 2 * GiB}},
		},
		Disk: DiskResult{
			DataDir: "/data", MeasuredAt: t0, TotalBytes: 6 * GiB, TotalLimit: th.DiskTotalMaxBytes,
			FreeBytes: 3 * GiB, FreeMinBytes: th.DiskFreeMinBytes,
			Tiers: []DiskTier{
				{Name: TierMain, Bytes: 3 * GiB, LimitBytes: th.DiskMainMaxBytes, Projected: true},
				{Name: TierAggregate, Bytes: GiB, LimitBytes: th.DiskAggregateMaxBytes},
				{Name: TierDLQ, Bytes: 0, LimitBytes: th.DiskDLQMaxBytes},
				{Name: TierWALTempTLS, Bytes: 100 * MiB, LimitBytes: th.DiskWALTempTLSMaxBytes},
			},
			GaugeBytes: map[string]float64{"main_db": float64(3 * GiB)},
		},
		Backlog: BacklogTrend{
			Metric: cfg.Sampling.BacklogMetric, Samples: 700, Evaluated: true, Flat: true,
			First: 10000, Last: 10200, Min: 9000, Max: 12000,
			SlopePerMinute: 0.5, SpanMinutes: 170, FittedGrowth: 85, AllowanceRows: 5000, R2: 0.02,
		},
		Queries: QueryResults{
			PrefillRangeStart: t0.Add(-7 * 24 * time.Hour), PrefillRangeEnd: t0,
			PrefillWindows: 2016,
			Checks: []QueryCheck{
				{Name: "traffic_seven_day", URL: "/api/metrics/traffic", Status: 200,
					Coverage: "full", CoverageSource: "header", CoverageExpected: "full",
					WindowsReturned: 2016, WindowsExpected: 2016, MissingWindows: 0, BodyBytes: 500000},
				{Name: "dashboard_seven_day", URL: "/api/metrics/dashboard", Status: 200,
					Coverage: "full", CoverageSource: "body", CoverageExpected: "full", BodyBytes: 900},
			},
			MCPTools: []MCPToolCall{
				{Tool: "get_anomaly_timeline", Status: 200, ResultBytes: 900},
				{Tool: "get_service_map", Status: 200, ResultBytes: 5000},
				{Tool: "get_service_health", Status: 200, ResultBytes: 400},
				{Tool: "root_cause_analysis", Status: 200, ResultBytes: 700},
				{Tool: "impact_analysis", Status: 200, ResultBytes: 300},
			},
		},
		Projection: FitProjection(synthSamples(24, 1*GiB, 4*MiB, 0), 576, 2, 6),
	}

	// Sustained-phase counter samples: the three silent-drop counters do not
	// move, and the backlog oscillates without trending.
	for i := 0; i < 20; i++ {
		r.MetricSeries = append(r.MetricSeries, MetricSample{
			At: t0.Add(time.Duration(i) * time.Minute), Phase: "sustained",
			Values: map[string]float64{
				"otelcontext_aggregate_late_points_total":        0,
				"otelcontext_aggregate_admission_rejected_total": 0,
				"otelcontext_aggregate_identity_overflow_total":  0,
				cfg.Sampling.BacklogMetric:                       10000,
			},
		})
	}
	r.MetricSeries = append(r.MetricSeries, MetricSample{
		At: t0.Add(4 * time.Hour), Phase: "post_burst_proof",
		Values: map[string]float64{cfg.Sampling.BacklogMetric: 11000},
	})
	return r
}

// assertionByID finds one row of the table.
func assertionByID(t *testing.T, r *Result, id string) Assertion {
	t.Helper()
	for _, a := range r.Assertions {
		if a.ID == id {
			return a
		}
	}
	t.Fatalf("no assertion %q in the table (%d rows)", id, len(r.Assertions))
	return Assertion{}
}

func TestPassingResultPasses(t *testing.T) {
	r := passingResult()
	r.Finalize()
	if !r.Passed {
		t.Fatalf("the reference passing run failed:\n  %s", strings.Join(r.Failures, "\n  "))
	}
	if len(r.Assertions) == 0 {
		t.Fatal("no assertions produced")
	}
}

func TestThresholdFailures(t *testing.T) {
	cases := []struct {
		name   string
		id     string
		mutate func(*Result)
	}{
		{"ack p99 over the bound", "sustained.ack_p99", func(r *Result) { r.Load.Sustained.P99Ms = 251 }},
		{"ack ratio below 99.9%", "sustained.ack_ratio", func(r *Result) {
			r.Load.Sustained.PointsAcked = r.Load.Sustained.PointsSent - r.Load.Sustained.PointsSent/500
		}},
		{"one RESOURCE_EXHAUSTED", "sustained.resource_exhausted", func(r *Result) { r.Load.Sustained.Exhausted = 1 }},
		{"offered rate below contract", "sustained.offered_rate", func(r *Result) { r.Load.Sustained.PointsSent /= 2 }},
		{"short sustained phase", "sustained.duration", func(r *Result) { r.Load.Sustained.DurationSec = 3600 }},
		{"late points moved", "sustained.late_points", func(r *Result) {
			r.MetricSeries[len(r.MetricSeries)-2].Values["otelcontext_aggregate_late_points_total"] = 5
		}},
		{"admission rejected moved", "sustained.admission_rejected", func(r *Result) {
			r.MetricSeries[len(r.MetricSeries)-2].Values["otelcontext_aggregate_admission_rejected_total"] = 1
		}},
		{"backlog walked up", "sustained.backlog_flat", func(r *Result) { r.Backlog.Flat = false }},
		{"burst offered too little", "burst.offered_rate", func(r *Result) { r.Load.Burst.PointsSent /= 3 }},
		{"post-burst p99 still high", "burst.recovery_ack_p99", func(r *Result) { r.Load.PostBurstProof.P99Ms = 400 }},
		{"post-burst backlog still high", "burst.recovery_backlog", func(r *Result) {
			r.MetricSeries[len(r.MetricSeries)-1].Values[r.Config.Sampling.BacklogMetric] = 99999
		}},
		{"OOM killed", "burst.no_crash_or_oom", func(r *Result) { r.Memory.OOMKills = 1 }},
		{"ready too slow", "recovery.ready", func(r *Result) { r.Recovery.TimeToReadySec = 61 }},
		{"never became ready", "recovery.ready", func(r *Result) { r.Recovery.ReadyObserved = false }},
		{"skipped series non-zero", "recovery.skipped_series", func(r *Result) { r.Recovery.SkippedSeries = 1 }},
		{"acknowledged loss", "recovery.no_acknowledged_loss", func(r *Result) {
			r.Recovery.Bounds.AckedLossPoints = 7
		}},
		{"no pre-kill ledger", "recovery.ledger_persisted_pre_kill", func(r *Result) {
			r.Load.Ledger.PreKillCopyBytes = 0
		}},
		{"graceful shutdown instead of kill", "recovery.kill_delivered", func(r *Result) {
			r.Recovery.KillSignal = "SIGTERM"
		}},
		{"memory peak over 4 GiB", "memory.peak", func(r *Result) { r.Memory.PeakBytes = 5 * GiB }},
		{"oom counter unreadable", "memory.oom_kills", func(r *Result) { r.Memory.OOMObserved = false }},
		{"aggregate tier too big", "disk.aggregate", func(r *Result) { r.Disk.Tiers[1].Bytes = 3 * GiB }},
		{"dlq tier too big", "disk.dlq", func(r *Result) { r.Disk.Tiers[2].Bytes = GiB }},
		{"wal tier too big", "disk.wal_temp_tls", func(r *Result) { r.Disk.Tiers[3].Bytes = GiB }},
		{"total over 7 GiB", "disk.total", func(r *Result) { r.Disk.TotalBytes = 8 * GiB }},
		{"free headroom under 1 GiB", "disk.free_headroom", func(r *Result) { r.Disk.FreeBytes = 512 * MiB }},
		{"projected main tier too big", "disk.main_projected", func(r *Result) {
			r.Projection = FitProjection(synthSamples(24, GiB, 16*MiB, 0), 576, 2, 6)
		}},
		{"projection refused", "projection.sample_count", func(r *Result) {
			r.Projection = FitProjection(synthSamples(2, GiB, MiB, 0), 576, 2, 6)
		}},
		{"query surface errored", "query.api.traffic_seven_day", func(r *Result) { r.Queries.Checks[0].Status = 500 }},
		{"query surface truncated", "query.api.traffic_seven_day", func(r *Result) { r.Queries.Checks[0].TruncatedTrue = true }},
		{"windows missing", "query.api.traffic_seven_day.windows", func(r *Result) {
			r.Queries.Checks[0].WindowsReturned = 2015
			r.Queries.Checks[0].MissingWindows = 1
		}},
		{"coverage not what the surface must declare", "query.api.dashboard_seven_day.coverage", func(r *Result) {
			r.Queries.Checks[1].Coverage = "exemplar"
		}},
		{"projection fitted over too short a span", "projection.window_span", func(r *Result) {
			r.Projection = FitProjection(shortSpanSamples(), 576, 2, 6)
		}},
		{"mcp tool rejected", "query.mcp.impact_analysis", func(r *Result) {
			r.Queries.MCPTools[4].RPCError = "-32601 unknown tool"
		}},
		{"phase did not complete", "phase.crash_run.completed", func(r *Result) {
			r.Phases[5].Completed = false
			r.Phases[5].Error = "server never restarted"
		}},
		{"cpu quota wrong", "confinement.cpu_max", func(r *Result) { r.Confinement.EffectiveCPUs = 8 }},
		{"memory bound absent", "confinement.memory_max", func(r *Result) { r.Confinement.MemoryMaxByte = 16 * GiB }},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := passingResult()
			c.mutate(r)
			r.Finalize()
			a := assertionByID(t, r, c.id)
			if a.Pass {
				t.Errorf("assertion %s passed despite %s (threshold %s %s, actual %s)",
					c.id, c.name, a.Comparator, a.Threshold, a.Actual)
			}
			if r.Passed {
				t.Error("the run was marked passed with a failing assertion")
			}
			if len(r.Failures) == 0 {
				t.Error("a failing run must list its failures")
			}
		})
	}
}

func TestMissingMetricFailsRatherThanBlanks(t *testing.T) {
	r := passingResult()
	for i := range r.MetricSeries {
		delete(r.MetricSeries[i].Values, "otelcontext_aggregate_late_points_total")
	}
	r.Finalize()
	a := assertionByID(t, r, "sustained.late_points")
	if a.Pass {
		t.Error("an absent counter must fail the gate, not pass it")
	}
	if a.Actual != "absent" {
		t.Errorf("actual = %q, want the row to say the evidence is absent", a.Actual)
	}
}

func TestTasksetFallbackIsDeclaredAndDegraded(t *testing.T) {
	r := passingResult()
	r.Confinement = Confinement{
		Mode: ConfinementTaskset, TasksetCPUs: "0,1", GOMAXPROCS: 2,
		EffectiveCPUs: 2, Note: TasksetNote,
	}
	r.Memory.Basis = string(ConfinementTaskset)
	r.Memory.PeakSource = "/proc/1/status VmHWM"
	r.Finalize()

	a := assertionByID(t, r, "confinement.fallback_declared")
	if !a.Pass || !a.Degraded {
		t.Errorf("the fallback must pass while being marked degraded: %+v", a)
	}
	if !strings.Contains(a.Detail, "dedicated-core") {
		t.Errorf("the fallback must state its narrower claim; detail = %q", a.Detail)
	}
	mem := assertionByID(t, r, "memory.peak")
	if !mem.Degraded {
		t.Error("the memory assertion must be marked degraded in taskset-fallback mode")
	}
}

func TestExactWindowFailureIsCalledOut(t *testing.T) {
	r := passingResult()
	r.Recovery.Bounds.Windows[0].Exact = true
	r.Recovery.Bounds.Windows[0].Pass = false
	r.Finalize()
	if a := assertionByID(t, r, "recovery.exact_outside_crash"); a.Pass {
		t.Error("an unambiguous window that did not match exactly must fail")
	}
}

func TestMetricDeltaAndLast(t *testing.T) {
	samples := []MetricSample{
		{Phase: "sustained", Values: map[string]float64{"m": 10}},
		{Phase: "burst", Values: map[string]float64{"m": 500}},
		{Phase: "sustained", Values: map[string]float64{"m": 14}},
	}
	d, ok := MetricDelta(samples, "sustained", "m")
	if !ok || d != 4 {
		t.Errorf("delta = %v, ok %t; the burst sample must not leak into the sustained delta", d, ok)
	}
	if _, ok := MetricDelta(samples, "sustained", "absent"); ok {
		t.Error("an absent key must report not-found")
	}
	last, ok := MetricLast(samples, "sustained", "m")
	if !ok || last != 14 {
		t.Errorf("last = %v, ok %t", last, ok)
	}
}

func TestAckRatioOfAnIdlePhaseIsZeroNotOne(t *testing.T) {
	if got := (LoadPhase{}).AckRatio(); got != 0 {
		t.Errorf("AckRatio of a phase that sent nothing = %v, want 0", got)
	}
}

// shortSpanSamples is eight samples crammed into two windows: enough samples
// to fit, nowhere near enough span to project two days from.
func shortSpanSamples() []DiskSample {
	start := time.Date(2026, 8, 22, 0, 0, 0, 0, time.UTC)
	out := make([]DiskSample, 0, 8)
	for i := 0; i < 8; i++ {
		w := float64(i) * 0.25
		out = append(out, DiskSample{
			At:            start.Add(time.Duration(i) * 75 * time.Second),
			PhysicalBytes: GiB + int64(w*float64(4*MiB)),
			Windows:       w,
		})
	}
	return out
}

func TestCoverageExpectationIsPerSurface(t *testing.T) {
	// The coverage check compares against the per-surface expectation, not a
	// hardcoded "full". A handler that legitimately declares something else
	// must be satisfiable by a config change rather than by loosening the
	// gate, so the mechanism is proved with a non-"full" expectation.
	r := passingResult()
	r.Queries.Checks = append(r.Queries.Checks,
		QueryCheck{
			Name: "honest_sampled", URL: "/api/metrics/sampled-surface", Status: 200,
			Coverage: "sampled", CoverageSource: "body", CoverageExpected: "sampled", BodyBytes: 400,
		},
		QueryCheck{
			Name: "downgraded", URL: "/api/metrics/traffic", Status: 200,
			Coverage: "sampled", CoverageSource: "body", CoverageExpected: "full", BodyBytes: 400,
		},
	)
	r.Finalize()
	if a := assertionByID(t, r, "query.api.honest_sampled.coverage"); !a.Pass {
		t.Errorf("a surface declaring exactly what the contract expects of it failed: %+v", a)
	}
	if a := assertionByID(t, r, "query.api.downgraded.coverage"); a.Pass {
		t.Errorf("a surface that silently downgraded its coverage marker passed: %+v", a)
	}
}

func TestServiceMapExpectationTracksTheHandler(t *testing.T) {
	// Nodes and edges both come from one engine topology query now that the
	// GraphRAG side-channel is retired, so a complete service-map answer
	// declares "full". This pins the config to the handler; if the handler
	// changes its marker again, this is where it is noticed.
	var svcMap APICheck
	for _, c := range DefaultAPIChecks() {
		if c.Name == "service_map_seven_day" {
			svcMap = c
		}
	}
	if svcMap.ExpectCoverage != "full" {
		t.Errorf("service-map expected coverage = %q, want full", svcMap.ExpectCoverage)
	}
}

func TestEmptyDropCounterPassesOnWitnessButIsDegraded(t *testing.T) {
	// A CounterVec that never fired emits nothing at all — not even HELP or
	// TYPE. Failing every healthy run on that would make the gate useless;
	// passing it silently would hide a deleted metric. The witness settles it.
	r := passingResult()
	for i := range r.MetricSeries {
		delete(r.MetricSeries[i].Values, "otelcontext_aggregate_late_points_total")
		if r.MetricSeries[i].Phase == "sustained" {
			r.MetricSeries[i].Values[DropCounterWitness+`{signal="trace_op"}`] = float64(1000 * (i + 1))
		}
	}
	r.Finalize()
	a := assertionByID(t, r, "sustained.late_points")
	if !a.Pass {
		t.Errorf("an empty drop-counter vector with a live witness must pass: %+v", a)
	}
	if !a.Degraded {
		t.Error("the empty-vector basis must be marked degraded")
	}
	if !strings.Contains(a.Detail, "never fired") {
		t.Errorf("the report must explain the basis; detail = %q", a.Detail)
	}
}

func TestAbsentDropCounterWithNoWitnessFails(t *testing.T) {
	r := passingResult()
	for i := range r.MetricSeries {
		delete(r.MetricSeries[i].Values, "otelcontext_aggregate_late_points_total")
	}
	r.Finalize()
	if a := assertionByID(t, r, "sustained.late_points"); a.Pass {
		t.Error("with neither the counter nor its witness present the gate must fail, not guess")
	}
}
