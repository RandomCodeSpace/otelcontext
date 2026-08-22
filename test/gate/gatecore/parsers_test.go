package gatecore

import (
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// --- cgroup / proc --------------------------------------------------------

func TestParseCPUMax(t *testing.T) {
	c, err := ParseCPUMax("200000 100000\n")
	if err != nil {
		t.Fatalf("ParseCPUMax: %v", err)
	}
	if c.CPUs != 2 {
		t.Errorf("effective CPUs = %v, want 2", c.CPUs)
	}
	if c.QuotaUsec != 200000 || c.PeriodUsec != 100000 {
		t.Errorf("quota/period = %d/%d", c.QuotaUsec, c.PeriodUsec)
	}
	if c.Unbounded {
		t.Error("a quota of 200000 is not unbounded")
	}
}

func TestParseCPUMaxUnbounded(t *testing.T) {
	c, err := ParseCPUMax("max 100000")
	if err != nil {
		t.Fatalf("ParseCPUMax: %v", err)
	}
	if !c.Unbounded || c.CPUs != 0 {
		t.Errorf("'max' must read as unbounded with no CPU figure; got %+v", c)
	}
}

func TestParseCPUMaxRejectsGarbage(t *testing.T) {
	for _, in := range []string{"", "200000", "200000 0", "abc 100000", "200000 100000 extra"} {
		if _, err := ParseCPUMax(in); err == nil {
			t.Errorf("ParseCPUMax(%q) must fail", in)
		}
	}
}

func TestParseMemoryMax(t *testing.T) {
	v, bounded, err := ParseMemoryMax("4294967296\n")
	if err != nil || !bounded || v != 4*GiB {
		t.Fatalf("ParseMemoryMax = %d, %t, %v", v, bounded, err)
	}
	v, bounded, err = ParseMemoryMax("max")
	if err != nil || bounded || v != -1 {
		t.Fatalf("'max' must read as unbounded: %d, %t, %v", v, bounded, err)
	}
	if _, _, err := ParseMemoryMax("four gigs"); err == nil {
		t.Error("garbage memory.max must fail")
	}
}

func TestParseMemoryEvents(t *testing.T) {
	ev, err := ParseMemoryEvents("low 0\nhigh 0\nmax 12\noom 0\noom_kill 0\n")
	if err != nil {
		t.Fatalf("ParseMemoryEvents: %v", err)
	}
	if v, ok := ev["oom_kill"]; !ok || v != 0 {
		t.Errorf("oom_kill = %d, present %t", v, ok)
	}
	if ev["max"] != 12 {
		t.Errorf("max = %d, want 12", ev["max"])
	}
	if _, err := ParseMemoryEvents(""); err == nil {
		t.Error("an empty memory.events must fail rather than read as zero events")
	}
}

func TestParseMemoryPeakAndVmHWM(t *testing.T) {
	v, err := ParseMemoryPeak(" 1073741824 \n")
	if err != nil || v != GiB {
		t.Fatalf("ParseMemoryPeak = %d, %v", v, err)
	}
	status := "Name:\totelcontext\nVmPeak:\t 9000000 kB\nVmHWM:\t 2097152 kB\nVmRSS:\t 1048576 kB\n"
	hwm, err := ParseVmHWM(status)
	if err != nil {
		t.Fatalf("ParseVmHWM: %v", err)
	}
	if hwm != 2*GiB {
		t.Errorf("VmHWM = %d, want %d", hwm, 2*GiB)
	}
	if _, err := ParseVmHWM("Name:\tx\n"); err == nil {
		t.Error("a status with no VmHWM line must fail")
	}
}

func TestParseCgroupProcsAndSelfCgroup(t *testing.T) {
	pids, err := ParseCgroupProcs("1234\n1235\n\n")
	if err != nil || len(pids) != 2 || pids[0] != 1234 {
		t.Fatalf("ParseCgroupProcs = %v, %v", pids, err)
	}
	path, err := ParseProcSelfCgroup("0::/user.slice/user-1000.slice/session-3.scope\n")
	if err != nil || path != "/user.slice/user-1000.slice/session-3.scope" {
		t.Fatalf("ParseProcSelfCgroup = %q, %v", path, err)
	}
	if _, err := ParseProcSelfCgroup("1:name=systemd:/\n"); err == nil {
		t.Error("a cgroup-v1 body must be refused")
	}
}

func TestParseMemTotal(t *testing.T) {
	v, err := ParseMemTotal("MemTotal:       32819612 kB\nMemFree:  100 kB\n")
	if err != nil || v != 32819612*1024 {
		t.Fatalf("ParseMemTotal = %d, %v", v, err)
	}
}

// --- prometheus -----------------------------------------------------------

const promFixture = `# HELP otelcontext_aggregate_input_points_total Points accepted.
# TYPE otelcontext_aggregate_input_points_total counter
otelcontext_aggregate_input_points_total{signal="spans"} 1.234567e+06
otelcontext_aggregate_input_points_total{signal="logs"} 4000
otelcontext_aggregate_delta_log_rows 12345
otelcontext_disk_component_bytes{component="main_db"} 1.073741824e+09
otelcontext_disk_component_bytes{component="aggregate_db"} 5.24288e+08
otelcontext_aggregate_late_points_total 0
weird_metric{label="a=b,c"} +Inf
`

func TestParsePrometheusText(t *testing.T) {
	ps, err := ParsePrometheusText(promFixture)
	if err != nil {
		t.Fatalf("ParsePrometheusText: %v", err)
	}
	if total, ok := ps.Sum("otelcontext_aggregate_input_points_total"); !ok || total != 1234567+4000 {
		t.Errorf("input points sum = %v, ok %t", total, ok)
	}
	if v, ok := ps.Get("otelcontext_aggregate_delta_log_rows", nil); !ok || v != 12345 {
		t.Errorf("delta_log_rows = %v, ok %t", v, ok)
	}
	comps := ps.ByLabel("otelcontext_disk_component_bytes", "component")
	if comps["main_db"] != float64(GiB) || comps["aggregate_db"] != float64(500*MiB) {
		t.Errorf("component bytes = %v", comps)
	}
	if _, ok := ps.Sum("otelcontext_absent_metric"); ok {
		t.Error("an absent metric must report found=false, not zero")
	}
	if v, ok := ps.Get("weird_metric", map[string]string{"label": "a=b,c"}); !ok || !math.IsInf(v, 1) {
		t.Errorf("escaped label / +Inf value not handled: %v %t", v, ok)
	}
}

func TestParsePrometheusTextRejectsMalformed(t *testing.T) {
	for _, in := range []string{
		"metric_without_value\n",
		"metric{unterminated=\"x\n",
		"metric{noequals} 1\n",
		"metric 1.2.3\n",
	} {
		if _, err := ParsePrometheusText(in); err == nil {
			t.Errorf("ParsePrometheusText(%q) must fail; a scrape the gate cannot read must not be scored", in)
		}
	}
}

func TestPromSampleKeyIsStable(t *testing.T) {
	s := PromSample{Name: "m", Labels: map[string]string{"b": "2", "a": "1"}}
	if got := s.Key(); got != `m{a="1",b="2"}` {
		t.Errorf("Key() = %q, want labels sorted", got)
	}
}

func TestFlattenKeepsOnlyRequestedMetrics(t *testing.T) {
	ps, err := ParsePrometheusText(promFixture)
	if err != nil {
		t.Fatal(err)
	}
	flat := ps.Flatten([]string{"otelcontext_aggregate_delta_log_rows"})
	if len(flat) != 1 {
		t.Fatalf("flatten kept %d keys, want 1: %v", len(flat), flat)
	}
	if flat["otelcontext_aggregate_delta_log_rows"] != 12345 {
		t.Errorf("flatten value = %v", flat["otelcontext_aggregate_delta_log_rows"])
	}
}

// --- server log -----------------------------------------------------------

func TestParseRecoveryLog(t *testing.T) {
	body := `time=2026-08-22T10:00:00.000Z level=INFO msg="starting"
time=2026-08-22T10:00:01.000Z level=INFO msg="🔁 Aggregate store recovered" path=/data/aggregate.db finalized_windows=3 replayed_rows=1841 replayed_series_windows=904 seeded_baselines=1200 unresolved_series=0 duration=412.5ms
time=2026-08-22T10:00:02.000Z level=INFO msg="ready"
`
	s, err := ParseRecoveryLog(body)
	if err != nil {
		t.Fatalf("ParseRecoveryLog: %v", err)
	}
	if !s.Found || s.FinalizedWindows != 3 || s.ReplayedRows != 1841 ||
		s.ReplayedSeries != 904 || s.SeededBaselines != 1200 || s.SkippedSeries != 0 {
		t.Errorf("parsed stats = %+v", s)
	}
	if s.Duration != 412500*time.Microsecond {
		t.Errorf("duration = %v, want 412.5ms", s.Duration)
	}
	if s.Path != "/data/aggregate.db" {
		t.Errorf("path = %q", s.Path)
	}
}

func TestParseRecoveryLogTakesTheLastIncarnation(t *testing.T) {
	body := `msg="🔁 Aggregate store recovered" unresolved_series=7 replayed_rows=1 duration=1ms
msg="🔁 Aggregate store recovered" unresolved_series=0 replayed_rows=99 duration=2ms
`
	s, err := ParseRecoveryLog(body)
	if err != nil {
		t.Fatalf("ParseRecoveryLog: %v", err)
	}
	if s.SkippedSeries != 0 || s.ReplayedRows != 99 {
		t.Errorf("must report the most recent recovery; got %+v", s)
	}
}

func TestParseRecoveryLogAbsentOrIncomplete(t *testing.T) {
	if _, err := ParseRecoveryLog("nothing here\n"); err == nil {
		t.Error("an absent recovery line must be an error, not a zero-valued success")
	}
	if _, err := ParseRecoveryLog(`msg="🔁 Aggregate store recovered" replayed_rows=1`); err == nil {
		t.Error("a line without unresolved_series must fail: that field is the gated one")
	}
}

// --- disk classification --------------------------------------------------

func TestClassifyPartitionsTheDataDirectory(t *testing.T) {
	entries := []FileEntry{
		{RelPath: "otelcontext.db", Bytes: 4 * GiB},
		{RelPath: "otelcontext.db-wal", Bytes: 64 * MiB},
		{RelPath: "otelcontext.db-shm", Bytes: 32 * KiBTest},
		{RelPath: "aggregate.db", Bytes: GiB},
		{RelPath: "aggregate.db-wal", Bytes: 16 * MiB},
		{RelPath: "dlq/batch-1.json", Bytes: 10 * MiB},
		{RelPath: "dlq/nested/batch-2.json", Bytes: 5 * MiB},
		{RelPath: "tls/cert.pem", Bytes: 4096},
		{RelPath: "etilqs_abc123", Bytes: 1 * MiB},
		{RelPath: "vectordb.snapshot", Bytes: 7 * MiB},
	}
	c := Classify(entries, DefaultClassifySpec())
	if c.Bytes[TierMain] != 4*GiB {
		t.Errorf("main = %d, want %d (sidecars belong to the WAL tier)", c.Bytes[TierMain], 4*GiB)
	}
	if c.Bytes[TierAggregate] != GiB {
		t.Errorf("aggregate = %d", c.Bytes[TierAggregate])
	}
	if c.Bytes[TierDLQ] != 15*MiB {
		t.Errorf("dlq = %d, want %d", c.Bytes[TierDLQ], 15*MiB)
	}
	wantWAL := int64(64*MiB + 32*KiBTest + 16*MiB + 4096 + 1*MiB)
	if c.Bytes[TierWALTempTLS] != wantWAL {
		t.Errorf("wal/temp/tls = %d, want %d", c.Bytes[TierWALTempTLS], wantWAL)
	}
	if c.Unclassified != 7*MiB || len(c.UnclassifiedFiles) != 1 {
		t.Errorf("unclassified = %d across %v", c.Unclassified, c.UnclassifiedFiles)
	}
	var sum int64
	for _, e := range entries {
		sum += e.Bytes
	}
	if c.Total != sum {
		t.Errorf("total = %d, want %d: unclassified bytes still cost the volume", c.Total, sum)
	}
}

// KiBTest keeps the fixture readable without exporting a KiB constant nothing
// else needs.
const KiBTest int64 = 1024

func TestMainTierPhysicalBytesIncludesSidecars(t *testing.T) {
	entries := []FileEntry{
		{RelPath: "otelcontext.db", Bytes: 100},
		{RelPath: "otelcontext.db-wal", Bytes: 20},
		{RelPath: "otelcontext.db-shm", Bytes: 3},
		{RelPath: "aggregate.db", Bytes: 999},
		{RelPath: "aggregate.db-wal", Bytes: 999},
	}
	if got := MainTierPhysicalBytes(entries, DefaultClassifySpec()); got != 123 {
		t.Errorf("main-tier physical = %d, want 123", got)
	}
}

// --- ledger ---------------------------------------------------------------

func TestLedgerRecorderBucketsBySignalAndWindow(t *testing.T) {
	base := time.Unix(1_755_000_000, 0).UTC()
	r := NewLedgerRecorder(300, time.Second, base)
	r.Attempt(Contribution{base.Unix(): 100}, "spans")
	r.Ack(Contribution{base.Unix(): 100}, "spans")
	r.Attempt(Contribution{base.Unix(): 40}, "logs")
	next := base.Add(5 * time.Minute).Unix()
	r.Attempt(Contribution{next: 70}, "spans") // next window
	r.Ack(Contribution{next: 70}, "spans")

	l := r.Snapshot(base.Add(6*time.Minute), true)
	if len(l.Windows) != 2 {
		t.Fatalf("windows = %d, want 2", len(l.Windows))
	}
	if l.Windows[0].WindowStart >= l.Windows[1].WindowStart {
		t.Error("windows must be sorted by start")
	}
	w0 := l.Windows[0]
	if w0.SignalCounts("spans").AckedPoints != 100 {
		t.Errorf("window 0 spans acked = %d", w0.SignalCounts("spans").AckedPoints)
	}
	if w0.SignalCounts("logs").AttemptedPoints != 40 || w0.SignalCounts("logs").AckedPoints != 0 {
		t.Errorf("an unacknowledged attempt must stay unacknowledged: %+v", w0.SignalCounts("logs"))
	}
	if !l.Windows[1].SignalCounts("spans").Exact() {
		t.Error("a fully acknowledged window must read as exact")
	}
	if l.Totals.AttemptedPoints != 210 || l.Totals.AckedPoints != 170 {
		t.Errorf("totals = %+v", l.Totals)
	}
	if l.TotalsBySignal["spans"].AckedPoints != 170 {
		t.Errorf("per-signal totals = %+v", l.TotalsBySignal)
	}
}

func TestLedgerRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ack-ledger.json")
	base := time.Unix(1_755_000_000, 0).UTC()
	r := NewLedgerRecorder(300, 2*time.Second, base)
	r.Attempt(Contribution{base.Unix(): 10}, "spans")
	r.Ack(Contribution{base.Unix(): 10}, "spans")

	if err := WriteLedger(path, r.Snapshot(base, false)); err != nil {
		t.Fatalf("WriteLedger: %v", err)
	}
	l, err := LoadLedger(path)
	if err != nil {
		t.Fatalf("LoadLedger: %v", err)
	}
	if l.Final {
		t.Error("a mid-run flush must not claim to be final")
	}
	if l.FlushIntervalSec != 2 {
		t.Errorf("flush interval = %v", l.FlushIntervalSec)
	}
	s := l.Summary(path)
	if !s.Present || s.Windows != 1 || s.Totals.AckedPoints != 10 {
		t.Errorf("summary = %+v", s)
	}
	// No temporary files may survive the atomic write.
	names, _ := os.ReadDir(dir)
	if len(names) != 1 {
		t.Errorf("write left %d files behind, want 1", len(names))
	}
}

func TestLoadLedgerRejectsForeignSchema(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x.json")
	if err := os.WriteFile(path, []byte(`{"schema":"something/v9","window_secs":300}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadLedger(path); err == nil {
		t.Error("a foreign schema must be refused")
	}
}

func TestWindowStartForAligns(t *testing.T) {
	got := WindowStartFor(time.Unix(1_755_000_123, 0), 300)
	if got%300 != 0 || got > 1_755_000_123 || 1_755_000_123-got >= 300 {
		t.Errorf("WindowStartFor = %d, not the containing aligned window", got)
	}
}

func TestLedgerContributionStraddlesTwoWindows(t *testing.T) {
	// internal/aggregate picks a span's window from its START time, which the
	// load generator backdates. One batch therefore lands in two windows, and
	// the ledger must split it rather than charge the whole batch to whichever
	// window the Export happened in.
	base := time.Unix(1_755_000_000, 0).UTC() // aligned
	r := NewLedgerRecorder(300, time.Second, base)

	c := Contribution{}
	c.Add(base.Add(299*time.Second), 300) // late in window 0
	c.Add(base.Add(299*time.Second), 300)
	c.Add(base.Add(301*time.Second), 300) // early in window 1
	if got := c.Points(); got != 3 {
		t.Fatalf("Points() = %d, want 3", got)
	}
	r.Attempt(c, "spans")
	r.Ack(c, "spans")

	l := r.Snapshot(base.Add(10*time.Minute), true)
	if len(l.Windows) != 2 {
		t.Fatalf("a straddling batch produced %d windows, want 2", len(l.Windows))
	}
	if l.Windows[0].SignalCounts("spans").AckedPoints != 2 {
		t.Errorf("window 0 acked = %d, want 2", l.Windows[0].SignalCounts("spans").AckedPoints)
	}
	if l.Windows[1].SignalCounts("spans").AckedPoints != 1 {
		t.Errorf("window 1 acked = %d, want 1", l.Windows[1].SignalCounts("spans").AckedPoints)
	}
	// The request touched both windows, so both count it.
	if l.Windows[0].Counts.AckedRequests != 1 || l.Windows[1].Counts.AckedRequests != 1 {
		t.Errorf("request counts = %d / %d, want 1 each",
			l.Windows[0].Counts.AckedRequests, l.Windows[1].Counts.AckedRequests)
	}
	if l.Totals.AckedPoints != 3 || l.Totals.AttemptedPoints != 3 {
		t.Errorf("totals = %+v", l.Totals)
	}
}

func TestLedgerRecorderIgnoresEmptyContribution(t *testing.T) {
	base := time.Unix(1_755_000_000, 0).UTC()
	r := NewLedgerRecorder(300, time.Second, base)
	r.Attempt(Contribution{}, "spans")
	r.Attempt(Contribution{base.Unix(): 0}, "spans")
	l := r.Snapshot(base, true)
	if l.Totals.AttemptedRequests != 0 {
		t.Errorf("an empty contribution recorded a request: %+v", l.Totals)
	}
}
