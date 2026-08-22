//go:build gate

// Command gate runs the seven-day aggregate release gate (#202).
//
// It owns the whole protocol: deterministic seven-day prefill, a confined
// server, three hours of sustained churn, a burst, a kill -9 with recovery
// verification against a client-side ACK ledger, disk and memory measurement,
// query-completeness checks, threshold assertions and the report.
//
// It is deliberately manual. CI compiles this binary and unit-tests the
// calculations in ./gatecore; it never runs the protocol on a shared runner.
//
// Build and run:
//
//	make gate-build
//	make gate-run
//
// Exit status is 0 only when every assertion passed.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/RandomCodeSpace/otelcontext/test/gate/gatecore"
)

// gateVersion identifies the orchestrator itself, independent of the schema.
const gateVersion = "1.0.0"

// phaseGuard keeps a metric sample near a phase boundary from being attributed
// to the wrong side of it. Process start, gRPC dial and the load generator's
// own phase clock do not switch in the same millisecond the gate does.
const phaseGuard = 5 * time.Second

type gate struct {
	cfg gatecore.Config
	res *gatecore.Result

	http *http.Client
	mode gatecore.ConfinementMode

	sampler *sampler
	server  *serverProc

	crashRunStart time.Time
	crashRunEnd   time.Time
	killedAt      time.Time
	readyAt       time.Time

	mu sync.Mutex
}

func main() {
	cfgPath := flag.String("config", "", "gate configuration JSON; unstated fields keep the frozen defaults")
	outDir := flag.String("out", "", "report directory override (default: report_dir from the config)")
	runID := flag.String("run-id", "", "run identifier (default: UTC timestamp)")
	printCfg := flag.Bool("print-config", false, "print the effective configuration and exit")
	flag.Parse()

	cfg, err := gatecore.LoadConfigFile(*cfgPath)
	if err != nil {
		log.Fatalf("gate: %v", err)
	}
	if *outDir != "" {
		cfg.ReportDir = *outDir
	}
	if cfg.RunID == "" {
		cfg.RunID = *runID
	}
	if cfg.RunID == "" {
		cfg.RunID = time.Now().UTC().Format("20060102T150405Z")
	}
	if cfg.RepoRoot == "" {
		if wd, err := os.Getwd(); err == nil {
			cfg.RepoRoot = wd
		}
	}
	if *printCfg {
		fmt.Println(mustJSON(cfg))
		return
	}
	if err := cfg.Validate(); err != nil {
		log.Fatalf("gate: %v", err)
	}

	g := newGate(cfg)
	runErr := g.run()

	g.res.EndedAt = time.Now().UTC()
	g.res.Finalize()

	jsonPath, mdPath, werr := gatecore.WriteReports(g.abs(cfg.ReportDir), g.res.StartedAt, g.res)
	if werr != nil {
		log.Printf("gate: writing the report failed: %v", werr)
	} else {
		log.Printf("gate: report written to %s and %s", jsonPath, mdPath)
	}

	if runErr != nil {
		log.Printf("gate: protocol error: %v", runErr)
	}
	if !g.res.Passed {
		log.Printf("gate: FAILED with %d failing assertions", len(g.res.Failures))
		for _, f := range g.res.Failures {
			log.Printf("  - %s", f)
		}
		os.Exit(1)
	}
	log.Printf("gate: PASSED %d assertions", len(g.res.Assertions))
}

func newGate(cfg gatecore.Config) *gate {
	g := &gate{
		cfg:  cfg,
		http: &http.Client{Timeout: time.Duration(cfg.Queries.Timeout * float64(time.Second))},
		res: &gatecore.Result{
			Schema:      gatecore.Schema,
			GateVersion: gateVersion,
			RunID:       cfg.RunID,
			StartedAt:   time.Now().UTC(),
			Config:      cfg,
			ServerEnv:   cfg.ServerEnv,
			DurabilityClaim: "Crash-durable on a surviving volume: committed aggregate data survives " +
				"a process or container kill -9 while the underlying volume persists. " +
				"This is not a host-power-loss claim and not a Pod-reschedule or node-loss claim.",
		},
	}
	g.sampler = newSampler(g)
	return g
}

func (g *gate) abs(p string) string {
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(g.cfg.RepoRoot, p)
}

func (g *gate) baseURL() string { return "http://" + g.cfg.HTTPAddr }

func (g *gate) recordCommand(phase string, argv []string, dir string, started time.Time, dur float64, exit int, logPath, errMsg string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.res.Commands = append(g.res.Commands, gatecore.Command{
		Phase: phase, Argv: argv, Dir: dir, StartedAt: started.UTC(),
		DurationSec: dur, ExitCode: exit, LogPath: logPath, Error: errMsg,
	})
}

// phase runs one protocol step, recording its outcome whether or not it worked.
func (g *gate) phase(name string, fn func() error) error {
	p := gatecore.Phase{Name: name, StartedAt: time.Now().UTC()}
	g.sampler.setPhase(name)
	log.Printf("gate: phase %s started", name)
	err := fn()
	p.EndedAt = time.Now().UTC()
	p.DurationSec = p.EndedAt.Sub(p.StartedAt).Seconds()
	p.Completed = err == nil
	if err != nil {
		p.Error = err.Error()
		log.Printf("gate: phase %s FAILED after %.0fs: %v", name, p.DurationSec, err)
	} else {
		log.Printf("gate: phase %s completed in %.0fs", name, p.DurationSec)
	}
	g.mu.Lock()
	g.res.Phases = append(g.res.Phases, p)
	g.mu.Unlock()
	return err
}

// note records a gap between what the contract needs and what the platform
// exposes. Gaps are evidence, not excuses.
func (g *gate) note(format string, args ...any) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.res.Gaps = append(g.res.Gaps, fmt.Sprintf(format, args...))
}

// run drives the whole protocol.
func (g *gate) run() error {
	if err := os.MkdirAll(g.abs(g.cfg.WorkDir), 0o750); err != nil {
		return err
	}
	if err := os.MkdirAll(g.abs(g.cfg.DataDir), 0o750); err != nil {
		return err
	}
	g.cfg.WorkDir = g.abs(g.cfg.WorkDir)
	g.cfg.DataDir = g.abs(g.cfg.DataDir)
	if err := g.pinServerPaths(); err != nil {
		return err
	}
	g.res.Config = g.cfg
	g.res.ServerEnv = g.cfg.ServerEnv

	g.res.Provenance = collectProvenance(g.cfg.RepoRoot, g.cfg.Binaries)
	g.res.Host = collectHost(g.cfg.DataDir)
	if err := g.preflightDisk(); err != nil {
		return err
	}
	g.recordKnownGaps()

	g.mode = gatecore.ConfinementCgroup
	if err := g.cgroupProbe(); err != nil {
		if !g.cfg.Confinement.AllowFallback {
			return fmt.Errorf("cgroup confinement unavailable and fallback disallowed: %w", err)
		}
		log.Printf("gate: cgroup confinement unavailable (%v); falling back to taskset", err)
		g.mode = gatecore.ConfinementTaskset
		g.note("cgroup-v2 delegated control was unavailable (%v); the run is taskset-fallback "+
			"and validates dedicated-core behavior rather than quota throttling.", err)
	}

	go g.sampler.run()
	defer g.sampler.shutdown()
	defer func() {
		if g.server != nil {
			_ = g.stopServer(g.server)
		}
	}()

	prefill, err := g.runPrefill()
	if err != nil {
		return err
	}

	if err := g.phase("server_start", func() error { return g.bootServer("initial") }); err != nil {
		return err
	}

	burstEnd, err := g.runMainLoad()
	if err != nil {
		return err
	}
	if err := g.runPostBurstProbe(burstEnd); err != nil {
		return err
	}
	if err := g.runQuietGap(); err != nil {
		return err
	}
	if err := g.runCrashPhase(); err != nil {
		return err
	}
	return g.measure(prefill)
}

// pinServerPaths forces every file the server writes into the gate's own data
// directory. Without this the main DB lands in the working directory and the
// DLQ under a relative ./data, and the disk walk that asserts the budget table
// would be measuring a directory the server never wrote to.
func (g *gate) pinServerPaths() error {
	httpPort, err := portOf(g.cfg.HTTPAddr)
	if err != nil {
		return fmt.Errorf("http_addr: %w", err)
	}
	grpcPort, err := portOf(g.cfg.GRPCAddr)
	if err != nil {
		return fmt.Errorf("grpc_addr: %w", err)
	}
	if g.cfg.ServerEnv == nil {
		g.cfg.ServerEnv = map[string]string{}
	}
	spec := g.cfg.Classify.Spec()
	aggPath := filepath.Join(g.cfg.DataDir, spec.AggregateDBFile)
	for k, v := range map[string]string{
		"HTTP_PORT":         httpPort,
		"GRPC_PORT":         grpcPort,
		"DB_DSN":            filepath.Join(g.cfg.DataDir, spec.MainDBFile),
		"AGGREGATE_DB_PATH": aggPath,
		"DATA_DISK_PATH":    g.cfg.DataDir,
		"DLQ_PATH":          filepath.Join(g.cfg.DataDir, spec.DLQDir),
		"TLS_CACHE_DIR":     filepath.Join(g.cfg.DataDir, spec.TLSDir),
	} {
		g.cfg.ServerEnv[k] = v
	}
	// The prefill writes the same file the server will open. One path, set
	// once, so the two cannot diverge.
	g.cfg.Prefill.DBPath = aggPath
	return nil
}

// portOf extracts the port from a host:port address.
func portOf(addr string) (string, error) {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", err
	}
	if port == "" {
		return "", fmt.Errorf("%q carries no port", addr)
	}
	return port, nil
}

// preflightDisk refuses to start a five-hour run that the disk watchdog will
// immediately shed.
//
// The watchdog's used-bytes figure is statfs on the WHOLE filesystem
// (total - available), not the size of the data directory. On a shared root
// filesystem that number is already far above DATA_DISK_BUDGET_MB, the
// watchdog enters raw_off at 95% of the budget, and /ready answers 503 for the
// rest of the run. Failing here with the arithmetic on screen beats a readiness
// timeout three hours later.
func (g *gate) preflightDisk() error {
	budgetMB, err := strconv.ParseInt(g.cfg.ServerEnv["DATA_DISK_BUDGET_MB"], 10, 64)
	if err != nil || budgetMB <= 0 {
		return fmt.Errorf("server_env DATA_DISK_BUDGET_MB is %q: the disk watchdog needs a positive budget",
			g.cfg.ServerEnv["DATA_DISK_BUDGET_MB"])
	}
	budget := budgetMB * 1024 * 1024
	total, free, err := statfs(g.cfg.DataDir)
	if err != nil {
		return fmt.Errorf("statfs %s: %w", g.cfg.DataDir, err)
	}
	used := total - free
	// The watchdog sheds at 95% and only leaves that state at 90%, so anything
	// at or above 85% of the budget is already too close to run against.
	if float64(used) >= 0.85*float64(budget) {
		return fmt.Errorf(
			"the data volume backing %s already reports %s used against a DATA_DISK_BUDGET_MB of %s; "+
				"the disk watchdog measures the whole filesystem (statfs total-available), enters raw_off "+
				"at 95%% of the budget and holds /ready at 503 there. Point data_dir at a dedicated volume, "+
				"or raise server_env.DATA_DISK_BUDGET_MB above the volume's existing usage — but say so in "+
				"the report, because it is no longer the budget #201 specified",
			g.cfg.DataDir, gatecore.HumanBytes(used), gatecore.HumanBytes(budget))
	}
	return nil
}

// recordKnownGaps states, up front, the places where the contract asks for
// something main does not expose. They are re-stated in the report so a reader
// never has to guess why a number came from a log line.
func (g *gate) recordKnownGaps() {
	g.note("internal/aggregate publishes only recovery duration and four row classes " +
		"(otelcontext_aggregate_recovery_duration_seconds, otelcontext_aggregate_recovery_rows{kind} " +
		"for replayed, finalized_windows, topology_restored_rows, topology_restored_windows). " +
		"promStoreRecorder.RecordRecovery receives the whole RecoveryStats but publishes none of " +
		"SkippedSeries — the corruption signal this gate asserts at zero — or SeededBaselines, " +
		"so the gate parses the server's own slog line.")
	g.note("The aggregate query API carries no `truncated` field: internal/aggregate pages every " +
		"store read to completion, so truncation never reaches the wire on /api/metrics/*. " +
		"Completeness there is asserted via the coverage marker and exact window coverage; the " +
		"literal truncated=false check applies only where the field exists (exemplar-backed responses).")
	g.note("test/aggprefill reports windows, bucket rows and delta rows but not per-window " +
		"observation totals, so the prefill tier's exact scalar check is window coverage " +
		"(every seeded window answered) rather than span-count equality.")
	g.note("There is no process-resident-memory collector in the Prometheus surface; memory " +
		"evidence comes from cgroup memory.peak / memory.events with /proc VmHWM as secondary.")
	g.note("No metric reports logical charged bytes for the main (exemplar) tier, so the " +
		"physical/charged amplification factor is unmeasured. The projection does not need it.")
}

// --- phases ---------------------------------------------------------------

func (g *gate) runPrefill() (prefillFacts, error) {
	var facts prefillFacts
	if !g.cfg.Prefill.Enabled {
		return facts, errors.New("prefill is disabled: there is no seven-day dataset to certify")
	}
	err := g.phase("prefill", func() error {
		argv := []string{
			g.abs(g.cfg.Binaries.Prefill),
			"-db", g.abs(g.cfg.Prefill.DBPath),
			"-workers", strconv.Itoa(g.cfg.Prefill.Workers),
			"-windows", strconv.Itoa(g.cfg.Prefill.Windows),
		}
		out, err := g.runCommand("prefill", argv, "prefill.log")
		if err != nil {
			return err
		}
		facts, err = parsePrefillOutput(out)
		if err != nil {
			return err
		}
		if facts.WindowsFinalized != g.cfg.Prefill.Windows {
			return fmt.Errorf("prefill finalized %d windows, expected %d",
				facts.WindowsFinalized, g.cfg.Prefill.Windows)
		}
		return nil
	})
	return facts, err
}

func (g *gate) bootServer(label string) error {
	p, err := g.startServer(label, g.mode)
	if err != nil {
		return err
	}
	g.server = p
	g.sampler.setProc(p)

	conf, cerr := readConfinement(p, g.mode, g.cfg.Confinement)
	g.res.Confinement = conf
	if cerr != nil {
		return fmt.Errorf("verify confinement: %w", cerr)
	}

	ready, err := g.waitReady(time.Duration(g.cfg.ReadyTimeoutSec * float64(time.Second)))
	if err != nil {
		return err
	}
	g.readyAt = ready
	return nil
}

// runMainLoad drives settle -> sustained -> burst in one loadsim invocation,
// switching the sampler's phase label on the gate's own clock so the
// sustained-phase counter deltas are not polluted by the settle ramp or the
// burst.
func (g *gate) runMainLoad() (time.Time, error) {
	var burstEnd time.Time
	err := g.phase("main_load", func() error {
		reportPath := filepath.Join(g.cfg.WorkDir, "loadsim-main.json")
		g.recordReportPath("main", reportPath)
		argv := g.loadsimArgv(reportPath, g.cfg.Load.SettleSec, g.cfg.Load.SustainedSec, g.cfg.Load.BurstSpec, "")

		b, err := g.startCommand("main_load", argv, "loadsim-main.log")
		if err != nil {
			return err
		}
		t0 := time.Now()

		settle := dur(g.cfg.Load.SettleSec)
		sustained := dur(g.cfg.Load.SustainedSec)

		g.sampler.setPhase("settle")
		sleepUntil(t0.Add(settle + phaseGuard))

		g.sampler.setPhase("sustained")
		steadyStart := t0.Add(settle + dur(g.cfg.Sampling.SteadyStartOffsetSec))
		go func() {
			sleepUntil(steadyStart)
			g.sampler.beginSteady(time.Now())
		}()
		sleepUntil(t0.Add(settle + sustained - phaseGuard))
		g.sampler.endSteady()

		g.sampler.setPhase("pre_burst_transition")
		sleepUntil(t0.Add(settle + sustained + phaseGuard))
		g.sampler.setPhase("burst")

		if _, err := g.waitCommand(b); err != nil {
			return fmt.Errorf("main load run: %w", err)
		}
		burstEnd = time.Now()

		rep, err := gatecore.LoadLoadsimReport(reportPath)
		if err != nil {
			return err
		}
		g.res.Load.Sustained = rep.PhaseNamed("loadsim-main.json", "sustained")
		g.res.Load.Burst = rep.PhaseNamed("loadsim-main.json", "burst")
		return nil
	})
	return burstEnd, err
}

// runPostBurstProbe measures the return to sustained bounds. Its settle window
// IS the contract's two-minute recovery allowance, so its latencies are
// recorded but excluded from the graded percentile; the graded window begins
// once the allowance has elapsed.
func (g *gate) runPostBurstProbe(burstEnd time.Time) error {
	return g.phase("post_burst", func() error {
		reportPath := filepath.Join(g.cfg.WorkDir, "loadsim-postburst.json")
		g.recordReportPath("post_burst", reportPath)
		argv := g.loadsimArgv(reportPath,
			g.cfg.Load.PostBurstAllowanceSec, g.cfg.Load.PostBurstProofSec, "", "")

		b, err := g.startCommand("post_burst", argv, "loadsim-postburst.log")
		if err != nil {
			return err
		}
		t0 := time.Now()
		g.sampler.setPhase("post_burst_allowance")
		sleepUntil(t0.Add(dur(g.cfg.Load.PostBurstAllowanceSec) + phaseGuard))
		g.sampler.setPhase("post_burst_proof")

		if _, err := g.waitCommand(b); err != nil {
			return fmt.Errorf("post-burst probe: %w", err)
		}
		rep, err := gatecore.LoadLoadsimReport(reportPath)
		if err != nil {
			return err
		}
		g.res.Load.PostBurstAllowance = rep.PhaseNamed("loadsim-postburst.json", "settle")
		g.res.Load.PostBurstProof = rep.PhaseNamed("loadsim-postburst.json", "sustained")
		log.Printf("gate: burst ended at %s; allowance %.0fs then a graded %.0fs window",
			burstEnd.Format(time.RFC3339), g.cfg.Load.PostBurstAllowanceSec, g.cfg.Load.PostBurstProofSec)
		return nil
	})
}

// runQuietGap keeps the crash run from sharing an aggregate window with the
// phases before it, so the per-window comparison has a single contributor.
func (g *gate) runQuietGap() error {
	return g.phase("quiet_gap", func() error {
		sleepUntil(time.Now().Add(dur(g.cfg.Load.QuietGapSec)))
		return nil
	})
}

// runCrashPhase is the kill -9 and the recovery verification.
func (g *gate) runCrashPhase() error {
	return g.phase("crash_run", func() error {
		reportPath := filepath.Join(g.cfg.WorkDir, "loadsim-crash.json")
		ledgerPath := filepath.Join(g.cfg.WorkDir, "ack-ledger.json")
		g.recordReportPath("crash_run", reportPath)
		g.res.Load.LedgerPath = ledgerPath

		argv := g.loadsimArgv(reportPath,
			g.cfg.Load.CrashRunSettleSec, g.cfg.Load.CrashRunSec, "", ledgerPath)

		b, err := g.startCommand("crash_run", argv, "loadsim-crash.log")
		if err != nil {
			return err
		}
		g.crashRunStart = time.Now()

		// Let the ledger accumulate, then snapshot the on-disk copy BEFORE the
		// kill. That snapshot is the evidence the ledger predates the crash.
		sleepUntil(g.crashRunStart.Add(dur(g.cfg.Load.CrashRunSettleSec + g.cfg.Load.CrashAtSec)))
		snapPath := filepath.Join(g.cfg.WorkDir, "ack-ledger-prekill.json")
		if n, at, err := snapshotFile(ledgerPath, snapPath); err == nil {
			g.res.Load.Ledger.PreKillCopyPath = snapPath
			g.res.Load.Ledger.PreKillCopyBytes = n
			g.res.Load.Ledger.PreKillCopyAt = at
		} else {
			log.Printf("gate: pre-kill ledger snapshot failed: %v", err)
		}

		killed := g.server
		if err := kill9(killed); err != nil {
			killCommand(b)
			return fmt.Errorf("SIGKILL: %w", err)
		}
		g.killedAt = time.Now()
		g.res.Recovery.KillSignal = "SIGKILL"
		g.res.Recovery.KilledPID = killed.pid
		g.res.Recovery.KilledAt = g.killedAt.UTC()
		g.sampler.setProc(nil)
		reap(killed)
		g.server = nil

		restartedAt := time.Now()
		g.res.Recovery.RestartedAt = restartedAt.UTC()
		if err := g.bootServer("restarted"); err != nil {
			killCommand(b)
			return fmt.Errorf("restart after kill: %w", err)
		}
		g.res.Recovery.ReadyAt = g.readyAt.UTC()
		g.res.Recovery.ReadyObserved = true
		g.res.Recovery.TimeToReadySec = g.readyAt.Sub(restartedAt).Seconds()
		g.res.Recovery.CrashIntervalSec = g.readyAt.Sub(g.killedAt).Seconds()

		if _, err := g.waitCommand(b); err != nil {
			log.Printf("gate: crash-run load generator exited with %v (expected: it kept "+
				"emitting into a dead endpoint)", err)
		}
		g.crashRunEnd = time.Now()

		if rep, err := gatecore.LoadLoadsimReport(reportPath); err == nil {
			g.res.Load.CrashRun = rep.PhaseNamed("loadsim-crash.json", "sustained")
		}
		return nil
	})
}

// --- measurement ----------------------------------------------------------

func (g *gate) measure(prefill prefillFacts) error {
	return g.phase("measure", func() error {
		g.collectSamplerEvidence()
		g.collectRecoveryStats()
		g.collectDisk()
		g.collectQueries(prefill)
		g.collectCrashBounds()
		return nil
	})
}

func (g *gate) collectSamplerEvidence() {
	ev := g.sampler.snapshot()
	samples := ev.Samples
	g.res.MetricSeries = samples
	g.res.Host.DataDirFreeMin = ev.FreeMin

	for name, count := range ev.Missing {
		g.note("required metric %s was absent from %d scrapes", name, count)
	}

	g.res.Backlog = gatecore.EvaluateBacklog(
		g.cfg.Sampling.BacklogMetric,
		gatecore.MetricSeriesIn(samples, "sustained", g.cfg.Sampling.BacklogMetric),
		g.cfg.Thresholds.BacklogAllowanceFraction,
		g.cfg.Thresholds.BacklogAllowanceFloorRows,
		g.cfg.Thresholds.BacklogMinSamples)

	g.res.Projection = gatecore.FitProjection(ev.DiskSamples,
		g.cfg.Thresholds.ProjectionHorizonWindows,
		g.cfg.Thresholds.ProjectionZ,
		g.cfg.Thresholds.ProjectionMinSamples)

	g.res.Memory = gatecore.MemoryResult{
		Basis:          string(g.mode),
		LimitBytes:     g.cfg.Thresholds.MemoryPeakMaxBytes,
		PeakSource:     ev.PeakSource,
		OOMSource:      ev.OOMSource,
		OOMObserved:    ev.OOMObserved,
		PerIncarnation: ev.Memory,
	}
	for _, m := range ev.Memory {
		if m.PeakBytes > g.res.Memory.PeakBytes {
			g.res.Memory.PeakBytes = m.PeakBytes
		}
		if m.VmHWMBytes > g.res.Memory.VmHWMBytes {
			g.res.Memory.VmHWMBytes = m.VmHWMBytes
		}
		g.res.Memory.OOMKills += m.OOMKills
	}
}

func (g *gate) collectRecoveryStats() {
	logPath := filepath.Join(g.cfg.WorkDir, "server-restarted.log")
	g.res.Recovery.StatsSource = logPath
	body, err := os.ReadFile(logPath) // #nosec G304 -- gate work dir
	if err != nil {
		g.note("could not read the restarted server's log (%v); the recovery summary is unavailable", err)
		return
	}
	stats, err := gatecore.ParseRecoveryLog(string(body))
	if err != nil {
		g.note("recovery summary not found in %s: %v", logPath, err)
		return
	}
	g.res.Recovery.StatsFound = true
	g.res.Recovery.FinalizedWindows = stats.FinalizedWindows
	g.res.Recovery.ReplayedRows = stats.ReplayedRows
	g.res.Recovery.ReplayedSeries = stats.ReplayedSeries
	g.res.Recovery.SeededBaselines = stats.SeededBaselines
	g.res.Recovery.SkippedSeries = stats.SkippedSeries
	g.res.Recovery.DurationSec = stats.Duration.Seconds()
}

func (g *gate) collectDisk() {
	t := g.cfg.Thresholds
	d := gatecore.DiskResult{
		DataDir:      g.cfg.DataDir,
		MeasuredAt:   time.Now().UTC(),
		TotalLimit:   t.DiskTotalMaxBytes,
		FreeMinBytes: t.DiskFreeMinBytes,
	}
	entries, err := walkDataDir(g.cfg.DataDir)
	if err != nil {
		g.note("data-directory walk failed: %v", err)
	}
	c := gatecore.Classify(entries, g.cfg.Classify.Spec())
	d.TotalBytes = c.Total
	d.UnclassifiedB = c.Unclassified
	d.UnclassifiedFs = c.UnclassifiedFiles
	for _, spec := range []struct {
		name      string
		limit     int64
		projected bool
	}{
		{gatecore.TierMain, t.DiskMainMaxBytes, true},
		{gatecore.TierAggregate, t.DiskAggregateMaxBytes, false},
		{gatecore.TierDLQ, t.DiskDLQMaxBytes, false},
		{gatecore.TierWALTempTLS, t.DiskWALTempTLSMaxBytes, false},
	} {
		d.Tiers = append(d.Tiers, gatecore.DiskTier{
			Name: spec.name, Bytes: c.Bytes[spec.name], LimitBytes: spec.limit,
			Files: c.Files[spec.name], Projected: spec.projected,
		})
	}
	if _, free, err := statfs(g.cfg.DataDir); err == nil {
		d.FreeBytes = free
	}
	if body, err := g.scrape(); err == nil {
		if parsed, perr := gatecore.ParsePrometheusText(body); perr == nil {
			d.GaugeBytes = parsed.ByLabel("otelcontext_disk_component_bytes", "component")
			d.GaugeHighWater = parsed.ByLabel("otelcontext_disk_component_high_water_bytes", "component")
		}
	}
	g.res.Disk = d
}

func (g *gate) collectQueries(prefill prefillFacts) {
	start := time.Unix(prefill.FirstWindow, 0).UTC()
	end := time.Now().UTC()
	expected := windowRange(prefill.FirstWindow, prefill.LastWindow)

	g.res.Queries.PrefillRangeStart = start
	g.res.Queries.PrefillRangeEnd = end
	g.res.Queries.PrefillWindows = len(expected)
	g.res.Queries.Checks = g.runAPIChecks(start, end, expected)
	g.res.Queries.MCPTools = g.runMCPTools(start, end)
}

// collectCrashBounds compares the post-restart per-window span totals against
// the ACK ledger. Only windows entirely inside the crash run are compared: a
// window shared with the quiet gap or with the run's own tail would carry a
// second contributor and the comparison would be meaningless.
func (g *gate) collectCrashBounds() {
	ledger, err := gatecore.LoadLedger(g.res.Load.LedgerPath)
	if err != nil {
		g.res.Recovery.Bounds.Error = "load ACK ledger: " + err.Error()
		return
	}
	g.res.Load.Ledger = mergeLedgerSummary(g.res.Load.Ledger, ledger.Summary(g.res.Load.LedgerPath))

	compareFrom := gatecore.WindowStartFor(g.crashRunStart, ledger.WindowSecs) + ledger.WindowSecs
	compareTo := gatecore.WindowStartFor(g.crashRunEnd, ledger.WindowSecs)

	observed, err := g.windowTotals(
		time.Unix(compareFrom, 0).Add(-time.Minute),
		time.Unix(compareTo, 0).Add(time.Minute), "spans")
	if err != nil {
		g.res.Recovery.Bounds.Error = "read per-window span totals: " + err.Error()
		return
	}
	g.res.Recovery.Bounds = gatecore.EvaluateCrashBounds(&ledger, "spans", observed,
		g.killedAt.Unix(), g.readyAt.Unix(), compareFrom, compareTo)
}

// --- helpers --------------------------------------------------------------

func (g *gate) loadsimArgv(reportPath string, settleSec, durationSec float64, burst, ledgerPath string) []string {
	argv := []string{
		g.abs(g.cfg.Binaries.Loadsim),
		"--direct",
		"--endpoint", g.cfg.GRPCAddr,
		"--settle", dur(settleSec).String(),
		"--duration", dur(durationSec).String(),
		"--batch-interval", fmt.Sprintf("%dms", g.cfg.Load.BatchIntervalMs),
		"--call-timeout", dur(g.cfg.Load.CallTimeoutSec).String(),
		"--report", reportPath,
	}
	// A profile owns the service count and the per-signal rates: loadsim
	// applies it after flag parsing, so passing --services alongside one would
	// record a number the run does not use.
	if g.cfg.Load.Profile != "" {
		argv = append(argv, "--profile", g.cfg.Load.Profile)
	} else {
		argv = append(argv, "--services", strconv.Itoa(g.cfg.Load.Services))
	}
	if burst != "" {
		argv = append(argv, "--burst", burst)
	}
	if ledgerPath != "" {
		argv = append(argv,
			"--ack-ledger", ledgerPath,
			"--ack-ledger-flush", dur(g.cfg.Load.LedgerFlushSec).String())
	}
	if g.cfg.Load.TenantID != "" {
		argv = append(argv, "--tenant-id", g.cfg.Load.TenantID)
	}
	return argv
}

func (g *gate) recordReportPath(name, path string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.res.Load.ReportPaths == nil {
		g.res.Load.ReportPaths = map[string]string{}
	}
	g.res.Load.ReportPaths[name] = path
}

// mergeLedgerSummary keeps the pre-kill snapshot facts, which the loaded
// document does not carry.
func mergeLedgerSummary(pre, loaded gatecore.LedgerSummary) gatecore.LedgerSummary {
	loaded.PreKillCopyAt = pre.PreKillCopyAt
	loaded.PreKillCopyPath = pre.PreKillCopyPath
	loaded.PreKillCopyBytes = pre.PreKillCopyBytes
	return loaded
}

// snapshotFile copies src to dst and returns the byte count and the time.
func snapshotFile(src, dst string) (int64, time.Time, error) {
	body, err := os.ReadFile(src) // #nosec G304 -- gate work dir
	if err != nil {
		return 0, time.Time{}, err
	}
	if err := os.WriteFile(dst, body, 0o600); err != nil {
		return 0, time.Time{}, err
	}
	return int64(len(body)), time.Now().UTC(), nil
}

// windowRange lists every aligned window start in [first, last].
func windowRange(first, last int64) []int64 {
	if first <= 0 || last < first {
		return nil
	}
	out := make([]int64, 0, (last-first)/gatecore.WindowSecs+1)
	for w := first; w <= last; w += gatecore.WindowSecs {
		out = append(out, w)
	}
	return out
}

func dur(sec float64) time.Duration { return time.Duration(sec * float64(time.Second)) }

func sleepUntil(t time.Time) {
	d := time.Until(t)
	if d > 0 {
		time.Sleep(d)
	}
}

func mustJSON(v any) string {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return "{}"
	}
	return string(b)
}
