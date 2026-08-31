package gatecore

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// The Markdown report is rendered from the same Result value that is
// serialized to JSON. There is no second source of numbers, so the two cannot
// disagree; if a field is missing from the JSON it is missing from the
// Markdown too, and the assertion that needed it has already failed.

// ReportBaseName is the file stem both artefacts share.
func ReportBaseName(day time.Time) string {
	return day.UTC().Format("2006-01-02") + "-aggregate-7day-gate"
}

// WriteReports writes <dir>/<date>-aggregate-7day-gate.{json,md} and returns
// the two paths.
func WriteReports(dir string, day time.Time, r *Result) (jsonPath, mdPath string, err error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", "", err
	}
	base := ReportBaseName(day)
	jsonPath = filepath.Join(dir, base+".json")
	mdPath = filepath.Join(dir, base+".md")

	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return "", "", err
	}
	b = append(b, '\n')
	if err := os.WriteFile(jsonPath, b, 0o600); err != nil {
		return "", "", err
	}
	if err := os.WriteFile(mdPath, []byte(RenderMarkdown(r, filepath.Base(jsonPath))), 0o600); err != nil {
		return "", "", err
	}
	return jsonPath, mdPath, nil
}

// RenderMarkdown renders the report.
func RenderMarkdown(r *Result, jsonName string) string {
	var b strings.Builder
	renderHeader(&b, r, jsonName)
	renderVerdict(&b, r)
	renderProvenance(&b, r)
	renderConfinement(&b, r)
	renderAssertionTables(&b, r)
	renderPhases(&b, r)
	renderLoad(&b, r)
	renderRecovery(&b, r)
	renderMemory(&b, r)
	renderDisk(&b, r)
	renderProjection(&b, r)
	renderQueries(&b, r)
	renderDurability(&b, r)
	renderGaps(&b, r)
	renderCommands(&b, r)
	return b.String()
}

func mark(ok bool) string {
	if ok {
		return "PASS"
	}
	return "FAIL"
}

func renderHeader(b *strings.Builder, r *Result, jsonName string) {
	fmt.Fprintf(b, "# Aggregate seven-day gate — %s\n\n", r.StartedAt.UTC().Format("2006-01-02"))
	fmt.Fprintf(b, "Rendered from `%s`. That JSON is the source of truth; every number below is read "+
		"straight out of it.\n\n", jsonName)
	fmt.Fprintf(b, "| | |\n|---|---|\n")
	fmt.Fprintf(b, "| Run id | `%s` |\n", r.RunID)
	fmt.Fprintf(b, "| Schema | `%s` |\n", r.Schema)
	fmt.Fprintf(b, "| Gate version | `%s` |\n", r.GateVersion)
	fmt.Fprintf(b, "| Started | %s |\n", r.StartedAt.UTC().Format(time.RFC3339))
	fmt.Fprintf(b, "| Ended | %s |\n", r.EndedAt.UTC().Format(time.RFC3339))
	fmt.Fprintf(b, "| Wall time | %s |\n\n", Dur(time.Duration(r.DurationSec)*time.Second))
}

func renderVerdict(b *strings.Builder, r *Result) {
	fmt.Fprintf(b, "## Verdict: %s\n\n", mark(r.Passed))
	passed, failed := 0, 0
	for _, a := range r.Assertions {
		if a.Pass {
			passed++
		} else {
			failed++
		}
	}
	fmt.Fprintf(b, "%d assertions, %d passed, %d failed.\n\n", len(r.Assertions), passed, failed)
	if len(r.Failures) == 0 {
		b.WriteString("No failures.\n\n")
		return
	}
	b.WriteString("Failures, in full:\n\n")
	for _, f := range r.Failures {
		fmt.Fprintf(b, "- %s\n", f)
	}
	b.WriteString("\n")
}

func renderProvenance(b *strings.Builder, r *Result) {
	p := r.Provenance
	b.WriteString("## Provenance\n\n| | |\n|---|---|\n")
	fmt.Fprintf(b, "| Commit | `%s` |\n", p.CommitSHA)
	if p.CandidateTag != "" {
		fmt.Fprintf(b, "| Candidate | `%s` -> `%s` |\n", p.CandidateTag, p.TagCommitSHA)
		fmt.Fprintf(b, "| Expected commit | `%s` |\n", p.ExpectedCommitSHA)
	}
	fmt.Fprintf(b, "| Branch | `%s` |\n", p.Branch)
	fmt.Fprintf(b, "| Dirty tree | %t |\n", p.DirtyTree)
	if len(p.DirtyFiles) > 0 {
		fmt.Fprintf(b, "| Dirty files | `%s` |\n", strings.Join(p.DirtyFiles, "`, `"))
	}
	fmt.Fprintf(b, "| Go | `%s` |\n", p.GoVersion)
	for _, name := range sortedKeys(p.BinarySHA256) {
		fmt.Fprintf(b, "| sha256 `%s` | `%s` |\n", name, p.BinarySHA256[name])
	}
	if p.ArchivePath != "" {
		fmt.Fprintf(b, "| Archive | `%s` |\n", p.ArchivePath)
		fmt.Fprintf(b, "| Archive sha256 | `%s` |\n", p.ArchiveSHA256)
	}
	if p.ConfigPath != "" {
		fmt.Fprintf(b, "| Config | `%s` |\n", p.ConfigPath)
		fmt.Fprintf(b, "| Config sha256 | `%s` |\n", p.ConfigSHA256)
	}
	if p.ServerVersion != "" {
		fmt.Fprintf(b, "| Candidate version | `%s` |\n", p.ServerVersion)
	}
	h := r.Host
	fmt.Fprintf(b, "| Host | `%s` (%s/%s, %d CPU, %s RAM) |\n",
		h.Hostname, h.OS, h.Arch, h.NumCPU, HumanBytes(h.TotalMemBytes))
	fmt.Fprintf(b, "| Kernel | `%s` |\n", h.Kernel)
	fmt.Fprintf(b, "| cgroup v2 | %t |\n", h.CgroupV2)
	fmt.Fprintf(b, "| Data dir | `%s` on `%s` (%s), mounted at `%s` |\n",
		h.DataDir, h.DataDirDevice, h.DataDirFSType, h.DataDirMount)
	fmt.Fprintf(b, "| Volume size | %s |\n\n", HumanBytes(h.DataDirTotal))

	b.WriteString("### Effective server environment\n\n| Variable | Value |\n|---|---|\n")
	for _, k := range sortedKeys(r.ServerEnv) {
		fmt.Fprintf(b, "| `%s` | `%s` |\n", k, r.ServerEnv[k])
	}
	b.WriteString("\n")
}

func renderConfinement(b *strings.Builder, r *Result) {
	c := r.Confinement
	b.WriteString("## Confinement\n\n| | |\n|---|---|\n")
	fmt.Fprintf(b, "| Mode | `%s` |\n", c.Mode)
	if c.Unit != "" {
		fmt.Fprintf(b, "| Unit | `%s` |\n", c.Unit)
	}
	if c.ScopePath != "" {
		fmt.Fprintf(b, "| Scope path | `%s` |\n", c.ScopePath)
	}
	if c.CPUMaxRaw != "" {
		fmt.Fprintf(b, "| `cpu.max` | `%s` (%.2f CPUs) |\n", c.CPUMaxRaw, c.EffectiveCPUs)
	}
	if c.MemoryMaxRaw != "" {
		fmt.Fprintf(b, "| `memory.max` | `%s` (%s) |\n", c.MemoryMaxRaw, HumanBytes(c.MemoryMaxByte))
	}
	if c.TasksetCPUs != "" {
		fmt.Fprintf(b, "| taskset CPUs | `%s` (GOMAXPROCS=%d) |\n", c.TasksetCPUs, c.GOMAXPROCS)
	}
	fmt.Fprintf(b, "\n%s\n\n", c.Note)
}

func renderAssertionTables(b *strings.Builder, r *Result) {
	b.WriteString("## Thresholds versus actuals\n\n")
	cats := []string{catPhase, catConfinement, catSustained, catBurst, catRecovery,
		catMemory, catDisk, catProjection, catQuery}
	seen := make(map[string]bool)
	for _, c := range cats {
		seen[c] = true
	}
	for _, a := range r.Assertions {
		if !seen[a.Category] {
			cats = append(cats, a.Category)
			seen[a.Category] = true
		}
	}
	for _, cat := range cats {
		rows := assertionsIn(r.Assertions, cat)
		if len(rows) == 0 {
			continue
		}
		fmt.Fprintf(b, "### %s\n\n", cat)
		b.WriteString("| Result | Assertion | Threshold | Actual | Basis | Notes |\n")
		b.WriteString("|---|---|---|---|---|---|\n")
		for _, a := range rows {
			res := mark(a.Pass)
			if a.Degraded {
				res += " (degraded basis)"
			}
			fmt.Fprintf(b, "| %s | `%s` — %s | %s %s | %s | %s | %s |\n",
				res, a.ID, mdEscape(a.Description), a.Comparator, mdEscape(a.Threshold),
				mdEscape(a.Actual), mdEscape(a.Basis), mdEscape(a.Detail))
		}
		b.WriteString("\n")
	}
}

func assertionsIn(all []Assertion, cat string) []Assertion {
	var out []Assertion
	for _, a := range all {
		if a.Category == cat {
			out = append(out, a)
		}
	}
	return out
}

func renderPhases(b *strings.Builder, r *Result) {
	b.WriteString("## Phases\n\n| Phase | Started | Duration | Completed | Detail |\n|---|---|---|---|---|\n")
	for _, p := range r.Phases {
		detail := p.Detail
		if p.Error != "" {
			detail = "ERROR: " + p.Error
		}
		fmt.Fprintf(b, "| `%s` | %s | %s | %t | %s |\n",
			p.Name, p.StartedAt.UTC().Format(time.RFC3339), Secs(p.DurationSec), p.Completed, mdEscape(detail))
	}
	b.WriteString("\n")
}

func renderLoad(b *strings.Builder, r *Result) {
	b.WriteString("## Load phases\n\n")
	b.WriteString("| Phase | Duration | Offered | ACKed | ACK ratio | p50 | p99 | p99.9 | max | RESOURCE_EXHAUSTED | UNAVAILABLE | other |\n")
	b.WriteString("|---|---|---|---|---|---|---|---|---|---|---|---|\n")
	rows := []struct {
		name string
		p    LoadPhase
	}{
		{"sustained", r.Load.Sustained},
		{"burst", r.Load.Burst},
		{"post-burst allowance (0-120s, evidence only)", r.Load.PostBurstAllowance},
		{"post-burst proof (120-240s, gated)", r.Load.PostBurstProof},
		{"crash run (ledger source)", r.Load.CrashRun},
	}
	for _, row := range rows {
		p := row.p
		if !p.Present {
			fmt.Fprintf(b, "| %s | — | — | — | — | — | — | — | — | — | — | — |\n", row.name)
			continue
		}
		offered := float64(0)
		if p.DurationSec > 0 {
			offered = float64(p.PointsSent) / p.DurationSec
		}
		fmt.Fprintf(b, "| %s | %s | %s | %s | %s | %s | %s | %s | %s | %d | %d | %d |\n",
			row.name, Secs(p.DurationSec), Rate(offered), Rate(p.PointsPerSec), Pct(p.AckRatio()),
			Ms(p.P50Ms), Ms(p.P99Ms), Ms(p.P999Ms), Ms(p.MaxMs),
			p.Exhausted, p.Unavailable, p.OtherErrors)
	}
	b.WriteString("\n")

	t := r.Backlog
	b.WriteString("### Writer backlog trend (sustained phase)\n\n| | |\n|---|---|\n")
	fmt.Fprintf(b, "| Metric | `%s` |\n", t.Metric)
	fmt.Fprintf(b, "| Samples | %d over %.1f min |\n", t.Samples, t.SpanMinutes)
	fmt.Fprintf(b, "| First / last | %.0f / %.0f rows |\n", t.First, t.Last)
	fmt.Fprintf(b, "| Min / max | %.0f / %.0f rows |\n", t.Min, t.Max)
	fmt.Fprintf(b, "| Slope | %.2f rows/min (R2 %.3f) |\n", t.SlopePerMinute, t.R2)
	fmt.Fprintf(b, "| Fitted growth / allowance | %.0f / %.0f rows |\n", t.FittedGrowth, t.AllowanceRows)
	fmt.Fprintf(b, "| Flat | %t |\n", t.Flat)
	if t.Error != "" {
		fmt.Fprintf(b, "| Error | %s |\n", mdEscape(t.Error))
	}
	b.WriteString("\n")
}

func renderRecovery(b *strings.Builder, r *Result) {
	rec := r.Recovery
	b.WriteString("## Recovery — kill -9 on a surviving volume\n\n| | |\n|---|---|\n")
	fmt.Fprintf(b, "| Killed | pid %d with %s at %s |\n", rec.KilledPID, rec.KillSignal,
		rec.KilledAt.UTC().Format(time.RFC3339))
	fmt.Fprintf(b, "| Restarted | %s |\n", rec.RestartedAt.UTC().Format(time.RFC3339))
	fmt.Fprintf(b, "| Ready | %s (%.1f s after restart) |\n", rec.ReadyAt.UTC().Format(time.RFC3339), rec.TimeToReadySec)
	fmt.Fprintf(b, "| Crash interval | %.1f s |\n", rec.CrashIntervalSec)
	fmt.Fprintf(b, "| Recovery stats source | `%s` |\n", rec.StatsSource)
	fmt.Fprintf(b, "| Finalized windows | %d |\n", rec.FinalizedWindows)
	fmt.Fprintf(b, "| Replayed rows / series-windows | %d / %d |\n", rec.ReplayedRows, rec.ReplayedSeries)
	fmt.Fprintf(b, "| Seeded baselines | %d |\n", rec.SeededBaselines)
	fmt.Fprintf(b, "| Skipped (unresolved) series | %d |\n", rec.SkippedSeries)
	fmt.Fprintf(b, "| Recovery duration | %.3f s |\n\n", rec.DurationSec)

	l := r.Load.Ledger
	b.WriteString("### ACK ledger\n\n| | |\n|---|---|\n")
	fmt.Fprintf(b, "| Path | `%s` |\n", r.Load.LedgerPath)
	fmt.Fprintf(b, "| Pre-kill snapshot | `%s` (%s, taken %s) |\n", l.PreKillCopyPath,
		HumanBytes(l.PreKillCopyBytes), l.PreKillCopyAt.UTC().Format(time.RFC3339))
	fmt.Fprintf(b, "| Flush interval | %.1f s |\n", l.FlushIntervalSec)
	fmt.Fprintf(b, "| Windows | %d (%d..%d) |\n", l.Windows, l.FirstWindow, l.LastWindow)
	fmt.Fprintf(b, "| Attempted / ACKed points | %d / %d |\n\n", l.Totals.AttemptedPoints, l.Totals.AckedPoints)

	bd := rec.Bounds
	b.WriteString("### Crash-interval bound\n\n")
	if !bd.Evaluated {
		fmt.Fprintf(b, "Not evaluated: %s\n\n", mdEscape(bd.Error))
		return
	}
	b.WriteString("The aggregate write path is at-least-once across a crash, so a crash-affected window " +
		"is bounded rather than fixed: post-restart totals must be at least the confirmed-ACKed " +
		"contributions and at most all attempted contributions. Windows the crash did not touch carry " +
		"attempted == ACKed, so the same rule is an exact equality there.\n\n")
	b.WriteString("| | |\n|---|---|\n")
	fmt.Fprintf(b, "| Signal compared | `%s` |\n", bd.Signal)
	fmt.Fprintf(b, "| Comparison range | %d..%d |\n", bd.CompareFromUnix, bd.CompareToUnix)
	fmt.Fprintf(b, "| Windows compared | %d (%d exact, %d crash-affected) |\n",
		bd.WindowsCompared, bd.WindowsExact, bd.WindowsCrashAffected)
	fmt.Fprintf(b, "| Attempted / ACKed / observed | %d / %d / %d |\n",
		bd.TotalAttempted, bd.TotalAcked, bd.TotalObserved)
	fmt.Fprintf(b, "| Permitted ambiguity | %d points |\n", bd.AmbiguityPoints)
	fmt.Fprintf(b, "| Acknowledged loss | %d points |\n", bd.AckedLossPoints)
	fmt.Fprintf(b, "| Windows outside bounds | %d below, %d above, %d missing |\n\n",
		bd.WindowsBelowLower, bd.WindowsAboveUpper, bd.WindowsMissing)

	b.WriteString("| Window | Crash-affected | ACKed (lower) | Observed | Attempted (upper) | Result |\n")
	b.WriteString("|---|---|---|---|---|---|\n")
	for _, w := range bd.Windows {
		observed := Count(w.Observed)
		if !w.ObservedFound {
			observed = "absent"
		}
		fmt.Fprintf(b, "| %d | %t | %d | %s | %d | %s %s |\n",
			w.WindowStart, w.CrashAffected, w.Acked, observed, w.Attempted, mark(w.Pass), mdEscape(w.Reason))
	}
	b.WriteString("\n")
}

func renderMemory(b *strings.Builder, r *Result) {
	m := r.Memory
	b.WriteString("## Memory\n\n| | |\n|---|---|\n")
	fmt.Fprintf(b, "| Basis | %s |\n", m.Basis)
	fmt.Fprintf(b, "| Peak | %s (from `%s`) |\n", HumanBytes(m.PeakBytes), m.PeakSource)
	fmt.Fprintf(b, "| Limit | %s |\n", HumanBytes(m.LimitBytes))
	fmt.Fprintf(b, "| oom_kill | %d (from `%s`, observed: %t) |\n", m.OOMKills, m.OOMSource, m.OOMObserved)
	fmt.Fprintf(b, "| VmHWM (secondary) | %s |\n\n", HumanBytes(m.VmHWMBytes))
	if len(m.PerIncarnation) == 0 {
		return
	}
	b.WriteString("| Server incarnation | PID | Peak | VmHWM | oom_kill | Scope |\n|---|---|---|---|---|---|\n")
	for _, i := range m.PerIncarnation {
		fmt.Fprintf(b, "| %s | %d | %s | %s | %d | `%s` |\n",
			i.Label, i.PID, HumanBytes(i.PeakBytes), HumanBytes(i.VmHWMBytes), i.OOMKills, i.ScopePath)
	}
	b.WriteString("\n")
}

func renderDisk(b *strings.Builder, r *Result) {
	d := r.Disk
	b.WriteString("## Disk — every partition\n\n")
	fmt.Fprintf(b, "Filesystem walk of `%s` at %s. The server's own attribution gauges are shown "+
		"alongside; the walk is what the assertions read.\n\n", d.DataDir, d.MeasuredAt.UTC().Format(time.RFC3339))
	b.WriteString("| Tier | Measured | Budget | Basis | Server gauge | Gauge high-water |\n")
	b.WriteString("|---|---|---|---|---|---|\n")
	gaugeFor := map[string][]string{
		TierMain:       {"main_db"},
		TierAggregate:  {"aggregate_db"},
		TierDLQ:        {"dlq"},
		TierWALTempTLS: {"wal"},
	}
	for _, t := range d.Tiers {
		basis := "demonstrated"
		if t.Projected {
			basis = "projected"
		}
		var g, hw string
		for _, name := range gaugeFor[t.Name] {
			if v, ok := d.GaugeBytes[name]; ok {
				g = HumanBytes(int64(v))
			}
			if v, ok := d.GaugeHighWater[name]; ok {
				hw = HumanBytes(int64(v))
			}
		}
		if g == "" {
			g = "—"
		}
		if hw == "" {
			hw = "—"
		}
		fmt.Fprintf(b, "| `%s` | %s | %s | %s | %s | %s |\n",
			t.Name, HumanBytes(t.Bytes), HumanBytes(t.LimitBytes), basis, g, hw)
	}
	fmt.Fprintf(b, "| **total data dir** | %s | %s | demonstrated | — | — |\n",
		HumanBytes(d.TotalBytes), HumanBytes(d.TotalLimit))
	fmt.Fprintf(b, "| free headroom | %s | >= %s | statfs | — | — |\n\n",
		HumanBytes(d.FreeBytes), HumanBytes(d.FreeMinBytes))
	if d.UnclassifiedB > 0 {
		fmt.Fprintf(b, "Unclassified: %s across `%s`. Unclassified bytes count toward the "+
			"data-directory total but belong to no tier.\n\n",
			HumanBytes(d.UnclassifiedB), strings.Join(d.UnclassifiedFs, "`, `"))
	}
}

func renderProjection(b *strings.Builder, r *Result) {
	p := r.Projection
	b.WriteString("## Main-tier projection\n\n")
	fmt.Fprintf(b, "%s\n\n", p.Label)
	if !p.Evaluated {
		fmt.Fprintf(b, "Not produced: %s\n\n", mdEscape(p.Error))
		return
	}
	b.WriteString("| | |\n|---|---|\n")
	fmt.Fprintf(b, "| Samples | %d, from %s to %s |\n", p.SampleCount,
		p.FirstAt.UTC().Format(time.RFC3339), p.LastAt.UTC().Format(time.RFC3339))
	fmt.Fprintf(b, "| Observed range | %s .. %s over %.1f completed windows |\n",
		HumanBytes(p.ObservedMin), HumanBytes(p.ObservedMax), p.WindowsSpan)
	fmt.Fprintf(b, "| Physical growth | %s per completed 5-minute window |\n", HumanBytes(int64(p.BytesPerWindow)))
	fmt.Fprintf(b, "| Fit quality | R2 %.4f, slope std err %s/window |\n", p.Fit.R2, HumanBytes(int64(p.Fit.SlopeStdErr)))
	fmt.Fprintf(b, "| Conservative slope | %s per window (point estimate + %.1f std err) |\n",
		HumanBytes(int64(p.UpperBytesPerWinds)), p.ZScore)
	fmt.Fprintf(b, "| Projected %d-window footprint | %s (point) / **%s (gated upper estimate)** |\n",
		p.HorizonWinds, HumanBytes(p.ProjectedBytes), HumanBytes(p.ProjectedUpperBytes))
	if p.AmplificationMeasured {
		fmt.Fprintf(b, "| Amplification (report only) | %.2fx (physical %s / charged %s per window) |\n",
			p.AmplificationFactor, HumanBytes(int64(p.BytesPerWindow)), HumanBytes(int64(p.ChargedBytesPerWindow)))
	} else {
		fmt.Fprintf(b, "| Amplification | not measured — %s |\n", mdEscape(p.MetricNote))
	}
	b.WriteString("\nThe slope is already physical: it is the difference between two filesystem " +
		"measurements of the same files, so it contains the indexes, the FTS shadow tables, the " +
		"WAL/SHM sidecars and the free pages. The amplification factor is reported and never " +
		"multiplied back in, because that would charge the indexes twice.\n\n")
}

func renderQueries(b *strings.Builder, r *Result) {
	q := r.Queries
	b.WriteString("## Query completeness\n\n")
	fmt.Fprintf(b, "Seven-day range: %s .. %s (%d seeded windows, %d series, %d services).\n\n",
		q.PrefillRangeStart.UTC().Format(time.RFC3339), q.PrefillRangeEnd.UTC().Format(time.RFC3339),
		q.PrefillWindows, q.PrefillSeries, q.PrefillServices)
	b.WriteString("| Surface | Status | Time | Coverage | Windows | truncated flag | Scalars |\n")
	b.WriteString("|---|---|---|---|---|---|---|\n")
	for _, c := range q.Checks {
		windows := "—"
		if c.WindowsExpected > 0 {
			windows = fmt.Sprintf("%d/%d (%d missing)", c.WindowsReturned, c.WindowsExpected, c.MissingWindows)
		}
		trunc := "absent"
		if c.TruncatedFound {
			trunc = fmt.Sprintf("present, true=%t", c.TruncatedTrue)
		}
		fmt.Fprintf(b, "| `%s` `%s` | %d | %.2f s | %s | %s | %s | %s |\n",
			c.Name, c.URL, c.Status, c.DurationSec, orDash(c.Coverage), windows, trunc, scalarList(c.Scalars))
	}
	b.WriteString("\n### Aggregate-backed MCP tools, named explicitly\n\n")
	b.WriteString("| Tool | Arguments | Status | Time | Result bytes | truncated flag | Error |\n")
	b.WriteString("|---|---|---|---|---|---|---|\n")
	for _, m := range q.MCPTools {
		trunc := "absent"
		if m.TruncatedFound {
			trunc = fmt.Sprintf("present, true=%t", m.TruncatedTrue)
		}
		e := m.RPCError
		if e == "" {
			e = m.Error
		}
		fmt.Fprintf(b, "| `%s` | `%s` | %d | %.2f s | %d | %s | %s |\n",
			m.Tool, mdEscape(m.Arguments), m.Status, m.DurationSec, m.ResultBytes, trunc, mdEscape(orDash(e)))
	}
	if len(q.LatencyChecks) > 0 {
		b.WriteString("\n### User-facing query latency\n\n")
		b.WriteString("| Surface | Cold | Cache | Warm samples | Warm p50 | Warm p95 | Warm max |\n")
		b.WriteString("|---|---:|---|---:|---:|---:|---:|\n")
		for _, check := range q.LatencyChecks {
			fmt.Fprintf(b, "| `%s` | %.3f s | %s | %d | %.3f s | %.3f s | %.3f s |\n",
				check.Name, check.ColdSeconds, orDash(check.ColdCache), len(check.WarmSeconds),
				check.WarmP50, check.WarmP95, check.WarmMax)
		}
	}
	if len(q.LatencySentinel.Surfaces) > 0 {
		s := q.LatencySentinel
		b.WriteString("\n### Contradictory latency sentinel\n\n")
		fmt.Fprintf(b, "Fixture: `%s`, %d × %.0f ms plus %d × %.0f ms.\n\n",
			s.Service, s.LowCount, s.LowMS, s.TailCount, s.TailMS)
		b.WriteString("| Consumer | P99 | Status | Method | Samples | Scale | Relative error | Degraded |\n")
		b.WriteString("|---|---:|---|---|---:|---:|---:|---|\n")
		for _, surface := range s.Surfaces {
			fmt.Fprintf(b, "| `%s` | %.3f ms | %s | %s | %d | %d | %.3f%% | %t |\n",
				surface.Name, surface.ValueMS, orDash(surface.Status), orDash(surface.Method),
				surface.SampleCount, surface.SketchScale, surface.RelativeErrorBound*100, surface.Degraded)
		}
	}
	b.WriteString("\n")
}

func renderDurability(b *strings.Builder, r *Result) {
	b.WriteString("## Durability claim demonstrated\n\n")
	fmt.Fprintf(b, "> %s\n\n", r.DurabilityClaim)
	fmt.Fprintf(b, "`AGGREGATE_SYNCHRONOUS=%s` for this run. This gate demonstrates committed-data "+
		"recovery after a process kill -9 while the underlying volume survives. It does not claim "+
		"host power-loss durability, and it does not claim Pod-reschedule or node-loss durability — "+
		"see the durability section of `docs/OPERATIONS.md` for what the deployment contract carries.\n\n",
		r.ServerEnv["AGGREGATE_SYNCHRONOUS"])
}

func renderGaps(b *strings.Builder, r *Result) {
	b.WriteString("## Metric gaps found by this gate\n\n")
	if len(r.Gaps) == 0 {
		b.WriteString("None recorded.\n\n")
		return
	}
	for _, g := range r.Gaps {
		fmt.Fprintf(b, "- %s\n", g)
	}
	b.WriteString("\n")
}

func renderCommands(b *strings.Builder, r *Result) {
	b.WriteString("## Commands invoked\n\n| Phase | Command | Started | Duration | Exit | Log |\n")
	b.WriteString("|---|---|---|---|---|---|\n")
	for _, c := range r.Commands {
		fmt.Fprintf(b, "| `%s` | `%s` | %s | %s | %d | `%s` |\n",
			c.Phase, mdEscape(strings.Join(c.Argv, " ")), c.StartedAt.UTC().Format(time.RFC3339),
			Secs(c.DurationSec), c.ExitCode, c.LogPath)
	}
	b.WriteString("\n")
}

func scalarList(m map[string]float64) string {
	if len(m) == 0 {
		return "—"
	}
	parts := make([]string, 0, len(m))
	for _, k := range sortedFloatKeys(m) {
		parts = append(parts, fmt.Sprintf("%s=%.0f", k, m[k]))
	}
	return strings.Join(parts, ", ")
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

// mdEscape keeps a pipe inside a cell from splitting the table.
func mdEscape(s string) string {
	s = strings.ReplaceAll(s, "|", "\\|")
	return strings.ReplaceAll(s, "\n", " ")
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedFloatKeys(m map[string]float64) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
