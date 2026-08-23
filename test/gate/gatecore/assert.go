package gatecore

import (
	"fmt"
	"sort"
	"strings"
)

// Threshold evaluation.
//
// Every criterion in the frozen contract produces exactly one Assertion,
// whether or not the evidence for it arrived. Missing evidence is a FAILED
// assertion carrying the reason — never an absent row, never a blank cell.

const (
	catPhase       = "phase"
	catConfinement = "confinement"
	catSustained   = "sustained"
	catBurst       = "burst"
	catRecovery    = "recovery"
	catMemory      = "memory"
	catDisk        = "disk"
	catProjection  = "projection"
	catQuery       = "query"
)

// Evaluate scores a filled-in Result and returns the assertion table.
func Evaluate(r *Result) []Assertion {
	t := r.Config.Thresholds
	var a []Assertion

	a = append(a, phaseAssertions(r)...)
	a = append(a, confinementAssertions(r, t)...)
	a = append(a, sustainedAssertions(r, t)...)
	a = append(a, burstAssertions(r, t)...)
	a = append(a, recoveryAssertions(r, t)...)
	a = append(a, memoryAssertions(r, t)...)
	a = append(a, diskAssertions(r, t)...)
	a = append(a, projectionAssertions(r, t)...)
	a = append(a, queryAssertions(r, t)...)
	return a
}

// Finalize stamps the verdict onto the result from its own assertion table.
// Passed is true only when every assertion passed and every phase completed.
func (r *Result) Finalize() {
	r.Schema = Schema
	r.Assertions = Evaluate(r)
	r.Failures = nil
	for _, as := range r.Assertions {
		if !as.Pass {
			r.Failures = append(r.Failures, fmt.Sprintf("%s: %s (threshold %s %s, actual %s)",
				as.ID, as.Description, as.Comparator, as.Threshold, as.Actual))
		}
	}
	sort.Strings(r.Failures)
	r.Passed = len(r.Failures) == 0
	if !r.EndedAt.IsZero() && !r.StartedAt.IsZero() {
		r.DurationSec = r.EndedAt.Sub(r.StartedAt).Seconds()
	}
}

// --- builders -------------------------------------------------------------

func pass(id, cat, desc, cmp, threshold, actual, basis string, ok bool, detail string) Assertion {
	return Assertion{
		ID: id, Category: cat, Description: desc, Comparator: cmp,
		Threshold: threshold, Actual: actual, Pass: ok, Basis: basis, Detail: detail,
	}
}

func lte(id, cat, desc, basis string, actual, limit float64, render func(float64) string, detail string) Assertion {
	return pass(id, cat, desc, "<=", render(limit), render(actual), basis, actual <= limit, detail)
}

func gte(id, cat, desc, basis string, actual, limit float64, render func(float64) string, detail string) Assertion {
	return pass(id, cat, desc, ">=", render(limit), render(actual), basis, actual >= limit, detail)
}

func eqInt(id, cat, desc, basis string, actual, want int64, detail string) Assertion {
	return pass(id, cat, desc, "==", Count(want), Count(actual), basis, actual == want, detail)
}

func boolAssert(id, cat, desc, basis string, ok bool, detail string) Assertion {
	actual := "false"
	if ok {
		actual = "true"
	}
	return pass(id, cat, desc, "==", "true", actual, basis, ok, detail)
}

func bytesLTE(id, cat, desc, basis string, actual, limit int64, detail string) Assertion {
	return pass(id, cat, desc, "<=", HumanBytes(limit), HumanBytes(actual), basis, actual <= limit, detail)
}

// --- phases ---------------------------------------------------------------

func phaseAssertions(r *Result) []Assertion {
	out := make([]Assertion, 0, len(r.Phases)+1)
	if len(r.Phases) == 0 {
		out = append(out, boolAssert("phase.any", catPhase,
			"the protocol recorded at least one phase", "orchestrator", false,
			"no phase ran: there is nothing to certify"))
		return out
	}
	for _, p := range r.Phases {
		detail := p.Detail
		if p.Error != "" {
			detail = p.Error
		}
		out = append(out, boolAssert("phase."+p.Name+".completed", catPhase,
			"phase "+p.Name+" ran to completion", "orchestrator", p.Completed, detail))
	}
	return out
}

// --- confinement (Q2) -----------------------------------------------------

func confinementAssertions(r *Result, t Thresholds) []Assertion {
	c := r.Confinement
	out := []Assertion{
		pass("confinement.mode", catConfinement,
			"the server ran inside a recorded resource boundary", "==",
			string(ConfinementCgroup)+" (or sanctioned "+string(ConfinementTaskset)+")",
			string(c.Mode), "orchestrator",
			c.Mode == ConfinementCgroup || c.Mode == ConfinementTaskset, c.Note),
	}

	if c.Mode == ConfinementCgroup {
		wantCPUs := float64(r.Config.Confinement.CPUQuotaPercent) / 100
		out = append(out,
			boolAssert("confinement.scope_path", catConfinement,
				"the transient scope's cgroup path was located and read", "cgroup-v2 files",
				c.ScopePath != "", "scope path: "+c.ScopePath),
			pass("confinement.cpu_max", catConfinement,
				"effective cpu.max matches the configured CPU quota", "==",
				fmt.Sprintf("%.2f CPUs", wantCPUs), fmt.Sprintf("%.2f CPUs", c.EffectiveCPUs),
				"cgroup cpu.max", c.EffectiveCPUs == wantCPUs, "raw: "+c.CPUMaxRaw),
			bytesLTE("confinement.memory_max", catConfinement,
				"effective memory.max is at or below the memory threshold", "cgroup memory.max",
				c.MemoryMaxByte, t.MemoryPeakMaxBytes, "raw: "+c.MemoryMaxRaw),
		)
		return out
	}

	deg := boolAssert("confinement.fallback_declared", catConfinement,
		"the taskset fallback is declared and its narrower claim is stated", "orchestrator",
		c.TasksetCPUs != "" && c.Note != "",
		"cpus: "+c.TasksetCPUs+"; "+c.Note)
	deg.Degraded = true
	out = append(out, deg)
	return out
}

// --- sustained (Q3) -------------------------------------------------------

func sustainedAssertions(r *Result, t Thresholds) []Assertion {
	p := r.Load.Sustained
	basis := "loadsim ACK ledger and report"
	if !p.Present {
		return []Assertion{missing("sustained", catSustained,
			"the sustained phase produced no loadsim report")}
	}

	offered := float64(0)
	if p.DurationSec > 0 {
		offered = float64(p.PointsSent) / p.DurationSec
	}
	minRate := t.SustainedPointsPerSec * (1 - t.SustainedRateTolerance)
	minDur := t.SustainedHours * 3600 * 0.99

	out := []Assertion{
		gte("sustained.duration", catSustained, "the sustained phase ran for the contracted time",
			basis, p.DurationSec, minDur, Secs,
			fmt.Sprintf("contract: %.1f h", t.SustainedHours)),
		gte("sustained.offered_rate", catSustained,
			"the offered load reached the contracted points/second", basis, offered, minRate, Rate,
			fmt.Sprintf("acked rate %.0f pts/s; a phase that ran below the contracted load cannot certify it", p.PointsPerSec)),
		lte("sustained.ack_p99", catSustained, "ACK p99 stayed inside the latency bound",
			basis, p.P99Ms, t.AckP99MaxMs, Ms,
			fmt.Sprintf("p50 %.1f ms, p90 %.1f ms, p99.9 %.1f ms, max %.1f ms over %d samples",
				p.P50Ms, p.P90Ms, p.P999Ms, p.MaxMs, p.Samples)),
		gte("sustained.ack_ratio", catSustained, "acknowledged points as a fraction of points sent",
			basis, p.AckRatio(), t.AckRatioMin, Pct,
			fmt.Sprintf("%d acked of %d sent", p.PointsAcked, p.PointsSent)),
		eqInt("sustained.resource_exhausted", catSustained,
			"no RESOURCE_EXHAUSTED refusal at sustained load", basis,
			p.Exhausted, t.MaxResourceExhausted, p.FirstErr),
		eqInt("sustained.transport_errors", catSustained,
			"no UNAVAILABLE or other transport error at sustained load", basis,
			p.Unavailable+p.OtherErrors, 0, p.FirstErr),
	}

	// Silent aggregate drops: counter deltas across the phase must be zero.
	for _, m := range []struct {
		id, key string
		limit   float64
	}{
		{"sustained.late_points", "otelcontext_aggregate_late_points_total", t.MaxLatePointsDelta},
		{"sustained.admission_rejected", "otelcontext_aggregate_admission_rejected_total", t.MaxAdmissionRejectedDelta},
		{"sustained.identity_overflow", "otelcontext_aggregate_identity_overflow_total", t.MaxIdentityOverflowDelta},
	} {
		delta, verdict := MetricDeltaWitnessed(r.MetricSeries, "sustained", m.key, DropCounterWitness)
		if verdict == MetricAbsent {
			out = append(out, missing(m.id, catSustained,
				"metric "+m.key+" was absent from the sustained-phase scrapes AND so was its witness "+
					DropCounterWitness+", so the gate cannot tell a counter that never fired from a "+
					"metric that no longer exists"))
			continue
		}
		a := lte(m.id, catSustained,
			"no silent aggregate drops: "+m.key+" did not move", "prometheus counter delta",
			delta, m.limit, Float, "delta across the sustained phase")
		if verdict == MetricEmptyVector {
			a.Basis = "prometheus counter vector with no children"
			a.Degraded = true
			a.Detail = "the counter vector had no child series in any sustained-phase scrape, which for a " +
				"drop counter means it never fired; the sibling " + DropCounterWitness + " was present, so " +
				"the metric family is registered. The Prometheus text format cannot distinguish an empty " +
				"vector from a deleted metric, which is why this basis is marked degraded."
		}
		out = append(out, a)
	}

	// Backlog flatness.
	b := r.Backlog
	if !b.Evaluated {
		out = append(out, missing("sustained.backlog_flat", catSustained,
			"writer backlog trend could not be established: "+b.Error))
	} else {
		out = append(out, boolAssert("sustained.backlog_flat", catSustained,
			"no sustained backlog growth", "prometheus "+b.Metric,
			b.Flat,
			fmt.Sprintf("fitted growth %.0f rows and endpoint growth %.0f rows over %.1f min, allowance %.0f rows (peak %.0f, R2 %.3f)",
				b.FittedGrowth, b.EndpointGrowth, b.SpanMinutes, b.AllowanceRows, b.Max, b.R2)))
	}
	return out
}

// --- burst (Q3) -----------------------------------------------------------

func burstAssertions(r *Result, t Thresholds) []Assertion {
	basis := "loadsim report"
	p := r.Load.Burst
	if !p.Present {
		return []Assertion{missing("burst", catBurst, "the burst phase produced no loadsim report")}
	}

	offered := float64(0)
	if p.DurationSec > 0 {
		offered = float64(p.PointsSent) / p.DurationSec
	}
	out := []Assertion{
		gte("burst.duration", catBurst, "the burst ran for the contracted time",
			basis, p.DurationSec, t.BurstSeconds*0.95, Secs, ""),
		gte("burst.offered_rate", catBurst, "the burst offered the contracted points/second",
			basis, offered, t.BurstPointsPerSec*(1-t.SustainedRateTolerance), Rate,
			"backpressure during the burst is permitted; offering less load than contracted is not"),
	}

	// No crash, no OOM. Backpressure is allowed, dying is not.
	crashed := false
	detail := "the server process survived the burst"
	for _, ph := range r.Phases {
		if ph.Name == "burst" && !ph.Completed {
			crashed = true
			detail = "burst phase did not complete: " + ph.Error
		}
	}
	if r.Memory.OOMKills > 0 {
		crashed = true
		detail = fmt.Sprintf("%d oom_kill events recorded", r.Memory.OOMKills)
	}
	out = append(out, boolAssert("burst.no_crash_or_oom", catBurst,
		"no crash and no OOM kill during the burst", "orchestrator and cgroup memory.events",
		!crashed, detail))

	// Return to sustained bounds within the allowance.
	proof := r.Load.PostBurstProof
	allow := r.Load.PostBurstAllowance
	if !proof.Present {
		out = append(out, missing("burst.recovery_ack_p99", catBurst,
			"the post-burst recovery probe produced no report"))
	} else {
		out = append(out, lte("burst.recovery_ack_p99", catBurst,
			fmt.Sprintf("ACK p99 back inside the sustained bound within %.0f s of burst end", t.BurstRecoverySeconds),
			"loadsim recovery probe, graded window", proof.P99Ms, t.AckP99MaxMs, Ms,
			fmt.Sprintf("graded window is %.0f-%.0f s after burst end; the allowance window (0-%.0f s) measured p99 %.1f ms and is reported, not gated",
				t.BurstRecoverySeconds, t.BurstRecoverySeconds+r.Config.Load.PostBurstProofSec,
				t.BurstRecoverySeconds, allow.P99Ms)))
	}

	// Backlog back inside sustained bounds.
	last, found := MetricLast(r.MetricSeries, "post_burst_proof", r.Config.Sampling.BacklogMetric)
	switch {
	case !r.Backlog.Evaluated:
		out = append(out, missing("burst.recovery_backlog", catBurst,
			"no sustained-phase backlog baseline to compare the post-burst backlog against"))
	case !found:
		out = append(out, missing("burst.recovery_backlog", catBurst,
			"metric "+r.Config.Sampling.BacklogMetric+" was not scraped during the post-burst graded window"))
	default:
		out = append(out, lte("burst.recovery_backlog", catBurst,
			fmt.Sprintf("writer backlog back inside the sustained peak within %.0f s of burst end", t.BurstRecoverySeconds),
			"prometheus "+r.Config.Sampling.BacklogMetric, last, r.Backlog.Max, Float,
			"sustained-phase peak is the bound"))
	}
	return out
}

// --- recovery (Q3) --------------------------------------------------------

func recoveryAssertions(r *Result, t Thresholds) []Assertion {
	rec := r.Recovery
	out := []Assertion{
		boolAssert("recovery.ledger_persisted_pre_kill", catRecovery,
			"an ACK ledger existed on disk before SIGKILL was sent", "orchestrator snapshot",
			r.Load.Ledger.PreKillCopyBytes > 0 && !r.Load.Ledger.PreKillCopyAt.IsZero(),
			fmt.Sprintf("snapshot %s (%d bytes) taken at %s", r.Load.Ledger.PreKillCopyPath,
				r.Load.Ledger.PreKillCopyBytes, r.Load.Ledger.PreKillCopyAt.Format("15:04:05Z07:00"))),
		boolAssert("recovery.kill_delivered", catRecovery,
			"the server was killed with SIGKILL, not asked to shut down", "orchestrator",
			rec.KillSignal == "SIGKILL" && rec.KilledPID > 0,
			fmt.Sprintf("signal %s to pid %d", rec.KillSignal, rec.KilledPID)),
	}

	if !rec.ReadyObserved {
		out = append(out, missing("recovery.ready", catRecovery,
			"the restarted server never reported ready"))
	} else {
		out = append(out, lte("recovery.ready", catRecovery,
			"the restarted server reported ready inside the readiness bound",
			"GET /ready", rec.TimeToReadySec, t.ReadySeconds, Secs,
			fmt.Sprintf("crash interval %.1f s", rec.CrashIntervalSec)))
	}

	if !rec.StatsFound {
		out = append(out, missing("recovery.skipped_series", catRecovery,
			"the server's recovery summary was not found in "+rec.StatsSource+
				"; SkippedSeries has no Prometheus gauge, so the log is the only source"))
	} else {
		out = append(out, lte("recovery.skipped_series", catRecovery,
			"startup recovery resolved every delta-log series", rec.StatsSource,
			float64(rec.SkippedSeries), float64(t.MaxSkippedSeries), Float,
			fmt.Sprintf("finalized %d windows, replayed %d rows into %d series-windows, seeded %d baselines in %.2f s",
				rec.FinalizedWindows, rec.ReplayedRows, rec.ReplayedSeries, rec.SeededBaselines, rec.DurationSec)))
	}

	b := rec.Bounds
	if !b.Evaluated {
		out = append(out, missing("recovery.crash_bounds", catRecovery,
			"the crash-interval bound could not be evaluated: "+b.Error))
		return out
	}
	out = append(out,
		boolAssert("recovery.no_acknowledged_loss", catRecovery,
			"no acknowledged aggregate loss: every window's post-restart total is at least its ACKed contributions",
			"ACK ledger vs "+b.Signal+" totals",
			b.AckedLossPoints == 0,
			fmt.Sprintf("%d points below the acknowledged lower bound across %d compared windows",
				b.AckedLossPoints, b.WindowsCompared)),
		boolAssert("recovery.within_attempted_upper_bound", catRecovery,
			"no window exceeded its attempted contributions",
			"ACK ledger vs "+b.Signal+" totals",
			b.WindowsAboveUpper == 0 && b.WindowsMissing == 0,
			fmt.Sprintf("%d of %d windows above the attempted upper bound, %d windows had no point at all",
				b.WindowsAboveUpper, b.WindowsCompared, b.WindowsMissing)),
		boolAssert("recovery.exact_outside_crash", catRecovery,
			"windows the crash did not touch matched exactly",
			"ACK ledger vs "+b.Signal+" totals",
			exactOutsideCrashHeld(b),
			fmt.Sprintf("%d exact windows, %d crash-affected windows carrying %d points of permitted ambiguity",
				b.WindowsExact, b.WindowsCrashAffected, b.AmbiguityPoints)),
	)
	return out
}

// exactOutsideCrashHeld reports whether every unambiguous window matched its
// single expected total.
func exactOutsideCrashHeld(b CrashBoundReport) bool {
	for _, w := range b.Windows {
		if w.Exact && !w.Pass {
			return false
		}
	}
	return true
}

// --- memory (Q3) ----------------------------------------------------------

func memoryAssertions(r *Result, t Thresholds) []Assertion {
	m := r.Memory
	degraded := r.Confinement.Mode != ConfinementCgroup

	peak := lte("memory.peak", catMemory, "peak memory stayed inside the bound",
		m.PeakSource, float64(m.PeakBytes), float64(t.MemoryPeakMaxBytes),
		func(v float64) string { return HumanBytes(int64(v)) },
		fmt.Sprintf("VmHWM secondary evidence: %s", HumanBytes(m.VmHWMBytes)))
	peak.Degraded = degraded
	if m.PeakBytes <= 0 {
		peak.Pass = false
		peak.Detail = "no peak memory figure was collected"
	}

	var oom Assertion
	if !m.OOMObserved {
		oom = missing("memory.oom_kills", catMemory,
			"no oom_kill counter was readable; in cgroup mode this comes from memory.events")
		oom.Degraded = degraded
	} else {
		oom = eqInt("memory.oom_kills", catMemory, "no OOM kill occurred", m.OOMSource,
			m.OOMKills, t.MaxOOMKills, "")
		oom.Degraded = degraded
	}
	return []Assertion{peak, oom}
}

// --- disk (Q3) ------------------------------------------------------------

func diskAssertions(r *Result, t Thresholds) []Assertion {
	d := r.Disk
	byName := make(map[string]DiskTier, len(d.Tiers))
	for _, tier := range d.Tiers {
		byName[tier.Name] = tier
	}

	out := make([]Assertion, 0, 8)

	// Main tier is gated on the conservative PROJECTION, per Q4.
	if !r.Projection.Evaluated {
		out = append(out, missing("disk.main_projected", catDisk,
			"the main-tier projection was not produced: "+r.Projection.Error))
	} else {
		out = append(out, bytesLTE("disk.main_projected", catDisk,
			"projected two-day main-tier footprint (conservative upper estimate)",
			"projection from filesystem samples",
			r.Projection.ProjectedUpperBytes, t.DiskMainMaxBytes,
			fmt.Sprintf("point estimate %s from %d samples over %.1f windows; measured main tier at report time %s",
				HumanBytes(r.Projection.ProjectedBytes), r.Projection.SampleCount,
				r.Projection.WindowsSpan, HumanBytes(byName[TierMain].Bytes))))
	}

	for _, spec := range []struct {
		id, tier, desc string
		limit          int64
	}{
		{"disk.aggregate", TierAggregate, "demonstrated aggregate.db tier", t.DiskAggregateMaxBytes},
		{"disk.dlq", TierDLQ, "demonstrated DLQ tier", t.DiskDLQMaxBytes},
		{"disk.wal_temp_tls", TierWALTempTLS, "demonstrated WAL, temp and TLS tier", t.DiskWALTempTLSMaxBytes},
	} {
		tier, ok := byName[spec.tier]
		if !ok {
			out = append(out, missing(spec.id, catDisk, "tier "+spec.tier+" was not measured"))
			continue
		}
		out = append(out, bytesLTE(spec.id, catDisk, spec.desc, "filesystem walk of the data directory",
			tier.Bytes, spec.limit, fmt.Sprintf("%d files", len(tier.Files))))
	}

	out = append(out,
		bytesLTE("disk.total", catDisk, "total allocated data-directory usage",
			"filesystem walk of the data directory", d.TotalBytes, t.DiskTotalMaxBytes,
			fmt.Sprintf("%s unclassified across %d files", HumanBytes(d.UnclassifiedB), len(d.UnclassifiedFs))),
		pass("disk.free_headroom", catDisk, "free headroom on the data volume", ">=",
			HumanBytes(t.DiskFreeMinBytes), HumanBytes(d.FreeBytes), "statfs on the data volume",
			d.FreeBytes >= t.DiskFreeMinBytes,
			fmt.Sprintf("minimum observed during the run: %s", HumanBytes(r.Host.DataDirFreeMin))),
	)
	return out
}

// --- projection (Q4) ------------------------------------------------------

func projectionAssertions(r *Result, t Thresholds) []Assertion {
	p := r.Projection
	out := []Assertion{
		gte("projection.sample_count", catProjection,
			"the projection was fitted over enough steady-portion samples", "orchestrator sampler",
			float64(p.SampleCount), float64(t.ProjectionMinSamples), Float, p.Error),
	}
	if !p.Evaluated {
		out = append(out, missing("projection.fit", catProjection,
			"no slope was fitted: "+p.Error))
		return out
	}
	out = append(out, gte("projection.window_span", catProjection,
		"the steady samples span enough completed windows for the slope to mean anything",
		"orchestrator sampler", p.WindowsSpan, t.ProjectionMinWindowSpan,
		func(v float64) string { return fmt.Sprintf("%.1f windows", v) },
		"a slope fitted across less than this is a startup transient extrapolated across two days"))

	// The projection must be the slope times the horizon and nothing else.
	// If an amplification factor was measured it is report-only; multiplying
	// it back in would charge the indexes twice.
	wantUpper := int64(p.UpperBytesPerWinds * float64(p.HorizonWinds))
	if wantUpper < 0 {
		wantUpper = 0
	}
	out = append(out,
		boolAssert("projection.single_application", catProjection,
			"the projection is the physical slope times the horizon, with no amplification re-applied",
			"projection arithmetic",
			p.ProjectedUpperBytes == wantUpper,
			fmt.Sprintf("%s/window upper x %d windows = %s; amplification measured: %t (%.2fx)",
				HumanBytes(int64(p.UpperBytesPerWinds)), p.HorizonWinds,
				HumanBytes(p.ProjectedUpperBytes), p.AmplificationMeasured, p.AmplificationFactor)),
		boolAssert("projection.labelled", catProjection,
			"the projection is labelled as a projection with its sample count and observed range",
			"report renderer", p.Label != "" && p.SampleCount > 0,
			fmt.Sprintf("observed %s..%s over %.1f windows, R2 %.3f",
				HumanBytes(p.ObservedMin), HumanBytes(p.ObservedMax), p.WindowsSpan, p.Fit.R2)),
	)
	return out
}

// --- query completeness (Q3) ---------------------------------------------

func queryAssertions(r *Result, t Thresholds) []Assertion {
	q := r.Queries
	out := make([]Assertion, 0, len(q.Checks)*2+len(q.MCPTools)+1)

	if len(q.Checks) == 0 {
		out = append(out, missing("query.api", catQuery, "no HTTP query surface was exercised"))
	}
	for _, c := range q.Checks {
		id := "query.api." + c.Name
		if c.Error != "" || c.Status != 200 {
			out = append(out, pass(id, catQuery, "query surface "+c.Name+" answered", "==",
				"HTTP 200", fmt.Sprintf("HTTP %d", c.Status), c.URL, false, c.Error))
			continue
		}
		out = append(out, boolAssert(id, catQuery,
			"query surface "+c.Name+" answered over the requested range without a truncation flag",
			c.URL, !c.TruncatedTrue,
			fmt.Sprintf("%.2f s, %d bytes, coverage %q (%s), truncated flag present: %t",
				c.DurationSec, c.BodyBytes, c.Coverage, c.CoverageSource, c.TruncatedFound)))

		if c.WindowsExpected > 0 {
			out = append(out, eqInt(id+".windows", catQuery,
				"query surface "+c.Name+" returned every seeded window",
				c.URL, int64(c.WindowsReturned), int64(c.WindowsExpected),
				fmt.Sprintf("%d windows missing", c.MissingWindows)))
			out = append(out, eqInt(id+".windows_extra", catQuery,
				"query surface "+c.Name+" returned no windows outside the seeded interval",
				c.URL, int64(c.ExtraWindows), 0,
				fmt.Sprintf("%d extra windows", c.ExtraWindows)))
		}
		if c.CoverageExpected != "" {
			out = append(out, pass(id+".coverage", catQuery,
				"query surface "+c.Name+" declared the aggregate coverage the contract expects of it",
				"==", c.CoverageExpected, c.Coverage, c.CoverageSource,
				c.Coverage == c.CoverageExpected, ""))
		}
	}

	if len(q.MCPTools) == 0 {
		out = append(out, missing("query.mcp", catQuery,
			"no aggregate-backed MCP tool was exercised; the contract requires them named explicitly"))
	}
	for _, m := range q.MCPTools {
		id := "query.mcp." + m.Tool
		ok := m.Status == 200 && m.RPCError == "" && m.Error == "" && !m.TruncatedTrue
		detail := fmt.Sprintf("%.2f s, %d result bytes, args %s", m.DurationSec, m.ResultBytes, m.Arguments)
		if m.RPCError != "" {
			detail = "JSON-RPC error: " + m.RPCError
		} else if m.Error != "" {
			detail = m.Error
		}
		out = append(out, boolAssert(id, catQuery,
			"MCP tool "+m.Tool+" answered over the full seven-day range", "POST "+r.Config.MCPPath,
			ok, detail))
	}
	return out
}

// missing renders a FAILED assertion for evidence that never arrived. The
// contract is explicit: a missing metric fails the gate rather than becoming a
// blank report cell.
func missing(id, cat, why string) Assertion {
	return Assertion{
		ID: id, Category: cat,
		Description: "evidence required by the contract was not collected",
		Comparator:  "==", Threshold: "present", Actual: "absent",
		Pass: false, Basis: "orchestrator", Detail: why,
	}
}

// --- metric-series helpers ------------------------------------------------

// DropCounterWitness is a metric registered in the same place as the aggregate
// drop counters and always non-empty under load. Its presence is what lets the
// gate argue that an absent drop counter is an empty vector rather than a
// metric that was renamed out from under it.
const DropCounterWitness = "otelcontext_aggregate_input_points_total"

// MetricVerdict says how a metric key was found, or why it was not.
type MetricVerdict int

const (
	// MetricPresent means at least one sample carried the key.
	MetricPresent MetricVerdict = iota
	// MetricEmptyVector means the key was absent but its witness was present:
	// a registered counter vector with no children, i.e. one that never fired.
	MetricEmptyVector
	// MetricAbsent means neither the key nor its witness was found. The gate
	// cannot reason about it and must fail.
	MetricAbsent
)

// MetricDeltaWitnessed is MetricDelta with the empty-vector argument attached.
//
// client_golang emits nothing at all — not even HELP or TYPE — for a
// CounterVec with no child series. A drop counter that correctly never fired
// is therefore indistinguishable from one that was deleted, unless something
// else proves the family is still registered. The witness is that something.
func MetricDeltaWitnessed(samples []MetricSample, phase, key, witness string) (float64, MetricVerdict) {
	if delta, ok := MetricDelta(samples, phase, key); ok {
		return delta, MetricPresent
	}
	if witness != "" {
		if _, ok := MetricDeltaPrefix(samples, phase, witness); ok {
			return 0, MetricEmptyVector
		}
	}
	return 0, MetricAbsent
}

// MetricDeltaPrefix answers the same question as MetricDelta for a labelled
// metric family: keys are rendered as `name{...}`, so an exact lookup misses.
func MetricDeltaPrefix(samples []MetricSample, phase, name string) (float64, bool) {
	var first, last float64
	var have bool
	for _, s := range samples {
		if s.Phase != phase {
			continue
		}
		var total float64
		var hit bool
		for k, v := range s.Values {
			if k == name || strings.HasPrefix(k, name+"{") {
				total += v
				hit = true
			}
		}
		if !hit {
			continue
		}
		if !have {
			first, have = total, true
		}
		last = total
	}
	if !have {
		return 0, false
	}
	return last - first, true
}

// MetricDelta returns last-minus-first for one flattened metric key across the
// samples of one phase.
func MetricDelta(samples []MetricSample, phase, key string) (float64, bool) {
	var first, last float64
	var have bool
	for _, s := range samples {
		if s.Phase != phase {
			continue
		}
		v, ok := s.Values[key]
		if !ok {
			continue
		}
		if !have {
			first, have = v, true
		}
		last = v
	}
	if !have {
		return 0, false
	}
	return last - first, true
}

// MetricLast returns the last observed value of a key within a phase.
func MetricLast(samples []MetricSample, phase, key string) (float64, bool) {
	var last float64
	var have bool
	for _, s := range samples {
		if s.Phase != phase {
			continue
		}
		if v, ok := s.Values[key]; ok {
			last, have = v, true
		}
	}
	return last, have
}

// MetricSeriesIn extracts one key's samples within a phase as offsets from the
// first sample, ready for the trend fit.
func MetricSeriesIn(samples []MetricSample, phase, key string) []TimedValue {
	var out []TimedValue
	var base int64
	for _, s := range samples {
		if s.Phase != phase {
			continue
		}
		v, ok := s.Values[key]
		if !ok {
			continue
		}
		if len(out) == 0 {
			base = s.At.Unix()
		}
		out = append(out, TimedValue{OffsetSec: float64(s.At.Unix() - base), Value: v})
	}
	return out
}
