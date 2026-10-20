//go:build loadtest

package main

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/aggregate"
	"github.com/joho/godotenv"
)

const identityServices = 200

// Read the deployment knobs, retaining the engine defaults for omitted values.
// Do not load the profile into the process environment or open its DB paths.
func productionIdentityConfig(t testing.TB) aggregate.EngineConfig {
	t.Helper()
	env, err := godotenv.Read("../../deploy/kubernetes/otelcontext.env")
	if err != nil {
		t.Fatal(err)
	}
	get := func(key string, fallback int) int {
		if env[key] == "" {
			return fallback
		}
		n, err := strconv.Atoi(env[key])
		if err != nil || n <= 0 {
			t.Fatalf("invalid %s=%q", key, env[key])
		}
		return n
	}
	return aggregate.EngineConfig{
		Mode: aggregate.ModeAggregate,
		Limiter: aggregate.LimiterConfig{
			MaxSeries:                 get("AGGREGATE_MAX_SERIES", aggregate.DefaultMaxSeries),
			MaxSeriesLogs:             get("AGGREGATE_MAX_SERIES_LOGS", aggregate.DefaultMaxSeriesLogs),
			MaxSeriesMetrics:          get("AGGREGATE_MAX_SERIES_METRICS", aggregate.DefaultMaxSeriesMetrics),
			MaxSeriesTraces:           get("AGGREGATE_MAX_SERIES_TRACES", aggregate.DefaultMaxSeriesTraces),
			MaxSeriesEdges:            get("AGGREGATE_MAX_SERIES_EDGES", aggregate.DefaultMaxSeriesEdges),
			MaxSeriesSystem:           get("AGGREGATE_MAX_SERIES_SYSTEM", aggregate.DefaultMaxSeriesSystem),
			MaxLogTemplatesPerService: get("AGGREGATE_MAX_LOG_TEMPLATES_PER_SERVICE", aggregate.DefaultMaxLogTemplatesPerService),
		},
		Bounds: aggregate.Bounds{
			PerTenantKind: map[aggregate.Kind]int{
				aggregate.KindOperation:   get("AGGREGATE_MAX_OPERATION_NAMES_PER_TENANT", 10000),
				aggregate.KindMetricName:  get("AGGREGATE_MAX_METRIC_NAMES_PER_TENANT", 5000),
				aggregate.KindLogTemplate: get("AGGREGATE_MAX_LOG_TEMPLATES_PER_TENANT", 5000),
			},
			InstanceKind: map[aggregate.Kind]int{
				aggregate.KindOperation:   get("AGGREGATE_MAX_OPERATION_NAMES", 100000),
				aggregate.KindMetricName:  get("AGGREGATE_MAX_METRIC_NAMES", 50000),
				aggregate.KindLogTemplate: get("AGGREGATE_MAX_LOG_TEMPLATES", 50000),
			},
		},
	}
}

func identityEngine(t testing.TB, cfg aggregate.EngineConfig, now *time.Time) *aggregate.Engine {
	t.Helper()
	cfg.Now = func() time.Time { return *now }
	if cfg.Registrar == nil {
		cfg.Registrar = aggregate.NewMemRegistrar(&aggregate.MemRegistrarOptions{Limits: cfg.Bounds.PerTenantKind})
	}
	e, err := aggregate.NewEngine(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

type patternIdentity struct{ service, pattern int }
type identityRun struct {
	patterns             map[patternIdentity]uint32
	expected             map[aggregate.SeriesKey]uint64
	unstable, minerOther uint64
	points               uint64
}

// Sixty sequential records cover the actual joint pattern/severity cycle.
// More cycles vary the random substitutions, exercising startup generalization.
// The original generator is deliberately used without repairing its fmt artifacts.
func runIdentityLogs(t testing.TB, e *aggregate.Engine, now time.Time, cycles, offset int, prior *identityRun) *identityRun {
	t.Helper()
	out := prior
	if out == nil {
		out = &identityRun{patterns: map[patternIdentity]uint32{}, expected: map[aggregate.SeriesKey]uint64{}}
	}
	var fact aggregate.TemplateFact
	e.SetTemplateFactSink(func(f aggregate.TemplateFact) { fact = f })
	defer e.SetTemplateFactSink(nil)
	emitters := make([]directEmitter, identityServices)
	for i := range emitters {
		emitters[i] = directEmitter{svcName: serviceName(i), rng: rand.New(rand.NewSource(int64(i + 1))), seq: offset}
	}
	for cycle := 0; cycle < cycles; cycle++ {
		r := e.NewReducer(now)
		for i := range emitters {
			em := &emitters[i]
			first := em.seq
			req, _ := em.buildLogs(60)
			for j, log := range req.ResourceLogs[0].ScopeLogs[0].LogRecords {
				r.ReduceLog(aggregate.LogInput{Tenant: "default", Service: em.svcName, Severity: log.SeverityText, SeverityNumber: int32(log.SeverityNumber), Body: log.Body.GetStringValue(), Timestamp: now})
				p := patternIdentity{i, (first + j) % len(logTemplates)}
				if id, ok := out.patterns[p]; ok && id != fact.TemplateID {
					out.unstable++
				}
				out.patterns[p] = fact.TemplateID
				if fact.IsOther {
					out.minerOther++
				}
				out.points++
			}
		}
		for k, d := range r.Deltas() {
			out.expected[k.Key] += d.Count
		}
		if _, err := e.ApplyReducerErr(r); err != nil {
			t.Fatal(err)
		}
	}
	return out
}

func checkIdentityLogs(t testing.TB, e *aggregate.Engine, now time.Time, out *identityRun, full bool) {
	t.Helper()
	stats := e.Limiter().Stats()
	templates, minerOver, converged := 0, uint64(0), uint64(0)
	for _, s := range e.Miner().Stats() {
		templates += s.Templates
		minerOver += s.Overflow
		converged += s.Converged
	}
	q, err := e.QueryLogSummary(context.Background(), aggregate.Query{Tenant: "default", Start: now.Truncate(aggregate.WindowSize), End: now.Add(time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	var count int64
	for _, p := range q.Points {
		count += p.Count
	}
	if uint64(count) != out.points {
		t.Fatalf("count=%d want=%d", count, out.points)
	}
	t.Logf("points=%d templates=%d miner_other=%d miner_overflow=%d converged=%d unstable=%d active=%d overflow_series=%d reasons=%v identity_lost=%d coverage=%s dictionary=%+v", out.points, templates, out.minerOther, minerOver, converged, out.unstable, stats.ActiveBySignal[aggregate.SignalLog], stats.OverflowSeries, stats.Overflow, q.IdentityCoverage.LogPoints, q.Coverage, e.Cache().Stats())
	if !full {
		if q.Coverage != aggregate.CoverageLimited || q.IdentityCoverage.LogPoints == 0 {
			t.Fatal("overflow must report limited identity coverage")
		}
		return
	}
	if q.Coverage != aggregate.CoverageFull || q.IdentityCoverage.Limited() || out.minerOther != 0 || out.unstable != 0 || minerOver != 0 || len(stats.Overflow) != 0 || stats.OverflowSeries != 0 {
		t.Fatal("expected full identity coverage without miner, limiter, or dictionary overflow")
	}
	cs := e.Cache().Stats()
	if cs.Overflows+cs.Errors+cs.OverLength+cs.TenantsRejected+cs.Fenced != 0 {
		t.Fatalf("dictionary lost identities: %+v", cs)
	}
	if templates != 4000 || len(out.patterns) != 4000 || len(out.expected) != 9000 || stats.ActiveBySignal[aggregate.SignalLog] != 9000 {
		t.Fatal("production-200 identity census changed")
	}
	ids := map[uint32]bool{}
	for _, id := range out.patterns {
		ids[id] = true
	}
	if len(ids) != 4000 {
		t.Fatal("distinct generated patterns merged")
	}
	tiers := map[aggregate.StatusClass]int{}
	got := map[aggregate.SeriesKey]uint64{}
	for _, w := range e.Snapshot().Windows {
		for k, d := range w.Series {
			if k.Signal == aggregate.SignalLog {
				got[k] += d.Count
				tiers[k.StatusClass]++
			}
		}
	}
	if len(got) != len(out.expected) {
		t.Fatalf("materialized keys=%d expected=%d", len(got), len(out.expected))
	}
	for k, n := range out.expected {
		if got[k] != n {
			t.Fatalf("identity %+v count=%d want=%d", k, got[k], n)
		}
	}
	want := map[aggregate.StatusClass]int{aggregate.SeverityTierDebug: 2000, aggregate.SeverityTierInfo: 4000, aggregate.SeverityTierWarn: 2000, aggregate.SeverityTierError: 1000}
	for tier, n := range want {
		if tiers[tier] != n {
			t.Fatalf("severity %v series=%d want=%d", tier, tiers[tier], n)
		}
	}
	t.Logf("materialized severity series=%v", tiers)
}

func TestProductionLogIdentityCensus(t *testing.T) {
	for _, offset := range []int{0, 17} {
		t.Run(fmt.Sprintf("startup_offset_%d", offset), func(t *testing.T) {
			cfg := productionIdentityConfig(t)
			// TEST ONLY: measure demand before choosing deployment ceilings.
			cfg.Limiter.MaxSeries = 100000
			cfg.Limiter.MaxSeriesLogs = 90000
			cfg.Limiter.MaxLogTemplatesPerService = 1000
			cfg.Bounds.PerTenantKind[aggregate.KindLogTemplate] = 100000
			cfg.Bounds.InstanceKind[aggregate.KindLogTemplate] = 100000
			now := time.Unix(1800000060, 0).UTC()
			e := identityEngine(t, cfg, &now)
			out := runIdentityLogs(t, e, now, 50, offset, nil)
			checkIdentityLogs(t, e, now, out, true)
		})
	}
}

func TestProductionLogIdentityProfile(t *testing.T) {
	cfg := productionIdentityConfig(t)
	// The dense historical fixture retains 500 additional template names.
	// Retained names consume dictionary capacity, never active series slots.
	reg := aggregate.NewMemRegistrar(&aggregate.MemRegistrarOptions{Limits: cfg.Bounds.PerTenantKind})
	tenant, err := reg.Register(aggregate.GlobalTenant, aggregate.KindTenant, []byte("default"))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 500; i++ {
		if _, err := reg.Register(tenant, aggregate.KindLogTemplate, []byte(fmt.Sprintf("template-%04d: request <*> failed with <*> after <*> ms", i))); err != nil {
			t.Fatal(err)
		}
	}
	cfg.Registrar = reg
	now := time.Unix(1800000060, 0).UTC()
	e := identityEngine(t, cfg, &now)
	out := runIdentityLogs(t, e, now, 10, 0, nil)
	checkIdentityLogs(t, e, now, out, true)
	if n := reg.Count(tenant, aggregate.KindLogTemplate); n != 4500 {
		t.Fatalf("retained log templates=%d want=4500", n)
	}
	t.Log("retained template names=4500: 4000 live + 500 historical; active log series=9000")
	// Reserve the other signals' original headroom, independent of log demand.
	if cfg.Limiter.MaxSeriesMetrics < 2400 || cfg.Limiter.MaxSeriesTraces < 2400 || cfg.Limiter.MaxSeriesEdges < 500 || cfg.Limiter.MaxSeriesSystem < 200 {
		t.Fatal("log capacity borrowed another signal's budget")
	}
	if cfg.Limiter.MaxSeriesLogs < 12000 || cfg.Limiter.MaxLogTemplatesPerService < 32 {
		t.Fatal("production profile lacks measured log headroom")
	}
}

func TestProductionLogIdentityLimits(t *testing.T) {
	for _, name := range []string{"original", "active_signal", "miner", "service_names", "retained_dictionary"} {
		t.Run(name, func(t *testing.T) {
			cfg := productionIdentityConfig(t)
			switch name {
			case "original":
				cfg.Limiter.MaxSeries = 6000
				cfg.Limiter.MaxSeriesLogs = 500
				cfg.Limiter.MaxLogTemplatesPerService = 10
			case "active_signal":
				cfg.Limiter.MaxSeriesLogs = 500
			case "service_names":
				cfg.Limiter.MaxLogTemplatesPerService = 10
			case "retained_dictionary":
				cfg.Bounds.PerTenantKind[aggregate.KindLogTemplate] = 100
			}
			now := time.Unix(1800000060, 0).UTC()
			if name == "service_names" || name == "miner" {
				// Isolate miner and active per-service name caps with a shared registrar.
				minerCap := 1000
				if name == "miner" {
					minerCap = 10
				}
				reg := aggregate.NewMemRegistrar(&aggregate.MemRegistrarOptions{Limits: cfg.Bounds.PerTenantKind})
				cfg.Registrar = reg
				cfg.Miner = aggregate.NewTemplateMiner(aggregate.TemplateMinerConfig{MaxTemplatesPerService: minerCap, Registrar: aggregate.TemplateRegistrarFunc(func(r aggregate.TemplateRegistration) (uint32, error) {
					tenant, err := reg.Register(0, aggregate.KindTenant, []byte(r.Tenant))
					if err != nil {
						return 0, err
					}
					if r.IsOther {
						return reg.OtherID(tenant, aggregate.KindLogTemplate), nil
					}
					return reg.Register(tenant, aggregate.KindLogTemplate, []byte(r.Service+"\x00"+r.Template))
				})})
			}
			e := identityEngine(t, cfg, &now)
			out := runIdentityLogs(t, e, now, 2, 0, nil)
			checkIdentityLogs(t, e, now, out, false)
			stats := e.Limiter().Stats()
			switch name {
			case "original", "active_signal":
				if stats.Overflow[aggregate.OverflowSignal] == 0 {
					t.Fatal("signal cap did not bind")
				}
			case "miner":
				if out.minerOther == 0 || len(stats.Overflow) != 0 {
					t.Fatal("miner cap was not isolated")
				}
			case "service_names":
				if out.minerOther != 0 || stats.Overflow[aggregate.OverflowServiceNames] == 0 {
					t.Fatal("per-service name cap was not isolated")
				}
			case "retained_dictionary":
				if e.Cache().Stats().Overflows == 0 {
					t.Fatal("retained dictionary cap did not bind")
				}
			}
		})
	}
}

func openIdentityStore(t testing.TB, path string, cfg aggregate.EngineConfig, now *time.Time) (*aggregate.Engine, *aggregate.SQLiteStore, *aggregate.Writer) {
	t.Helper()
	store, err := aggregate.OpenSQLiteStore(aggregate.StoreConfig{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	reg, err := aggregate.NewDurableRegistrarWithBounds(store, cfg.Bounds)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Registrar = reg
	e := identityEngine(t, cfg, now)
	w, err := aggregate.NewWriter(aggregate.WriterConfig{Store: store, Engine: e, Registrar: reg, Now: func() time.Time { return *now }, CoalesceWindow: time.Microsecond, FinalizeInterval: -1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := aggregate.RestoreMiner(store, e.Miner()); err != nil {
		t.Fatal(err)
	}
	if _, err := aggregate.Recover(store, e, w, *now, aggregate.RecoverOptions{}); err != nil {
		t.Fatal(err)
	}
	e.SetApplier(w)
	w.Start()
	return e, store, w
}

func TestProductionLogIdentityRestart(t *testing.T) {
	cfg := productionIdentityConfig(t)
	path := filepath.Join(t.TempDir(), "aggregate.db")
	now := time.Unix(1800000060, 0).UTC()
	e, store, w := openIdentityStore(t, path, cfg, &now)
	closeStore := func() {
		w.Stop()
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
	}
	defer func() { closeStore() }()
	out := runIdentityLogs(t, e, now, 3, 0, nil)
	checkIdentityLogs(t, e, now, out, true)
	rows, err := store.LoadDict(0)
	if err != nil {
		t.Fatal(err)
	}
	before := len(rows)
	closeStore()
	e, store, w = openIdentityStore(t, path, cfg, &now)
	out = runIdentityLogs(t, e, now, 3, 0, out)
	checkIdentityLogs(t, e, now, out, true)
	rows, err = store.LoadDict(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != before {
		t.Fatalf("dictionary grew after identical replay: %d -> %d", before, len(rows))
	}
	// Finalized history must not consume the next live window's admission budget.
	now = now.Add(aggregate.WindowSize + aggregate.AllowedLateness + time.Minute)
	if w.FinalizeDue(now) != 1 {
		t.Fatal("expected one finalized window")
	}
	e.Rollover(now)
	if e.Limiter().Stats().Active != 0 {
		t.Fatal("finalized history spends active slots")
	}
	closeStore()
	e, store, w = openIdentityStore(t, path, cfg, &now)
	if e.Limiter().Stats().Active != 0 {
		t.Fatal("restart rehydrated finalized series into active budget")
	}
	next := runIdentityLogs(t, e, now, 1, 0, nil)
	checkIdentityLogs(t, e, now, next, true)
	for pattern, id := range out.patterns {
		if next.patterns[pattern] != id {
			t.Fatal("finalization/restart changed a repeated body identity")
		}
	}
	t.Logf("dictionary_rows=%d stable across mutable and finalized restart", before)
}

// Diagnostic, opt-in benchmarks. Run with -benchtime=1x. Heap is measured after
// GC with the engine live; it excludes concurrent ingest buffers, SQLite caches,
// network handlers, and cgroup page cache. It is not a production RSS guarantee.
func BenchmarkProductionLogIdentityHeap(b *testing.B) {
	for _, profile := range []string{"original", "production"} {
		b.Run(profile, func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				cfg := productionIdentityConfig(b)
				if profile == "original" {
					cfg.Limiter.MaxSeries = 6000
					cfg.Limiter.MaxSeriesLogs = 500
					cfg.Limiter.MaxLogTemplatesPerService = 10
				}
				runtime.GC()
				var before, after runtime.MemStats
				runtime.ReadMemStats(&before)
				now := time.Unix(1800000060, 0).UTC()
				e := identityEngine(b, cfg, &now)
				_ = runIdentityLogs(b, e, now, 10, 0, nil)
				runtime.GC()
				runtime.ReadMemStats(&after)
				b.ReportMetric(float64(int64(after.HeapAlloc)-int64(before.HeapAlloc)), "retained-heap-B")
				b.ReportMetric(float64(e.Limiter().Stats().ActiveBySignal[aggregate.SignalLog]), "named-series")
				b.ReportMetric(float64(reflect.TypeFor[aggregate.AggregateDelta]().Size()), "delta-struct-B")
				b.ReportMetric(float64(reflect.TypeFor[aggregate.SeriesKey]().Size()), "series-key-B")
				for _, w := range e.Snapshot().Windows {
					for _, d := range w.Series {
						if d.Sketch != nil {
							b.Fatal("log allocated a latency sketch")
						}
					}
				}
				runtime.KeepAlive(e)
			}
		})
	}
}

// Only 12 windows are written to a new temporary database. Measuring growth
// between windows 4 and 12 amortizes the dictionary and mutable-delta overhead;
// extrapolation still needs a constrained seven-day acceptance run.
func BenchmarkProductionLogIdentityStorage(b *testing.B) {
	for i := 0; i < b.N; i++ {
		cfg := productionIdentityConfig(b)
		path := filepath.Join(b.TempDir(), "aggregate.db")
		now := time.Unix(1800000060, 0).UTC()
		e, store, w := openIdentityStore(b, path, cfg, &now)
		defer func() { w.Stop(); _ = store.Close() }()
		var size4 int64
		for window := 1; window <= 12; window++ {
			_ = runIdentityLogs(b, e, now, 1, 0, nil)
			now = now.Add(aggregate.WindowSize + aggregate.AllowedLateness + time.Minute)
			if w.FinalizeDue(now) != 1 {
				b.Fatal("expected one finalized window")
			}
			if window != 4 && window != 12 {
				continue
			}
			w.Stop()
			if err := store.Close(); err != nil {
				b.Fatal(err)
			}
			info, err := os.Stat(path)
			if err != nil {
				b.Fatal(err)
			}
			if window == 4 {
				size4 = info.Size()
				b.ReportMetric(float64(size4), "four-windows-B")
				e, store, w = openIdentityStore(b, path, cfg, &now)
			} else {
				growth := info.Size() - size4
				b.ReportMetric(float64(info.Size()), "twelve-windows-B")
				b.ReportMetric(float64(growth)/8, "per-window-B")
				b.ReportMetric(float64(growth)/(8*9000), "per-bucket-B")
				b.ReportMetric(float64(growth)/8*2016, "projected-7d-log-B")
			}
		}
	}
}
