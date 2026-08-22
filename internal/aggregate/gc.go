package aggregate

// Identity garbage collection (#200 Q1, Q2, Q5).
//
// The aggregate dictionary and the series table were append-only: every name a
// deployment ever emitted stayed on disk forever, long after the last bucket
// naming it was purged by retention. This is the collector that ends that.
//
// The algorithm is mark-and-sweep with transitive marking, run on the daily
// maintenance tick:
//
//	MARK   (no writer lock, consistent reader snapshot)
//	  1. Every series ID referenced by aggregate_buckets, aggregate_delta_log
//	     or aggregate_baseline is live.
//	  2. Every live series marks its tenant, service, name and dim-tuple IDs.
//	  3. Every live dim tuple marks the dim-key and dim-value IDs inside it.
//	  4. Every live dictionary row marks its own tenant.
//	  5. Miner templates, staged template mutations, overflow sentinels and
//	     persisted alias records mark their template IDs — BOTH ends of every
//	     alias, followed transitively.
//	  6. In-memory roots: the hot-path cache, the registrar's staged rows and
//	     hand-outs, the series registry's staged rows and hand-outs, and the
//	     engine's live shard keys.
//
//	SWEEP  (identity-maintenance barrier, on the writer)
//	  7. Revalidate every candidate against durable, active, pending and
//	     staged references one more time.
//	  8. Fence the survivors of that revalidation from new lookup.
//	  9. Delete series rows, then dictionary rows, then template rows, in ONE
//	     transaction.
//	 10. On success remove the forward and reverse map entries. On failure
//	     release the fence and change NOTHING.
//
// The split is the whole design. Step 1-6 is a full scan of three tables and
// must never hold the writer lock: a daily maintenance pass that stalls the
// group commit is an ACK-latency incident, not maintenance. Steps 7-10 are
// bounded by the candidate count and are the only part that serializes.
//
// Why the revalidation in step 7 is not paranoia: the scan ran without a lock,
// so anything could have referenced a candidate while it was running. The
// registrar and the series registry each keep a "handed out since the last
// completed sweep" set precisely so that window is closed — an ID a hot-path
// goroutine took before it ever reached the cache map is invisible to every
// other root.

import (
	"encoding/binary"
	"fmt"
	"log/slog"
	"sort"
	"time"
)

// GCStats is the outcome of one identity garbage-collection pass.
type GCStats struct {
	// SeriesScanned, DictScanned and TemplatesScanned are the row counts the
	// mark phase examined.
	SeriesScanned, DictScanned, TemplatesScanned int
	// SeriesSwept, DictSwept and TemplatesSwept are the rows actually deleted.
	SeriesSwept, DictSwept, TemplatesSwept int64
	// SeriesRetained and DictRetained are the rows the mark phase kept.
	SeriesRetained, DictRetained int
	// Revalidated counts candidates the barrier rescued after the lock-free
	// scan — a non-zero value is normal under load, a large one means the scan
	// is running too long.
	Revalidated int
	// MarkDuration is the lock-free scan; BarrierDuration is the part that
	// serializes with the writer. Only the second belongs in an ACK-latency
	// budget.
	MarkDuration, BarrierDuration time.Duration
	// Duration is the wall time of the whole pass.
	Duration time.Duration
}

// Barrier is the identity-maintenance barrier the collector runs its sweep
// through (#200 Q2). It is implemented by the group-commit writer: running the
// sweep on the writer goroutine is what makes "no commit interleaves with a
// delete" structural rather than a convention.
type Barrier interface {
	// RunBarrier executes fn with no group commit in flight and no commit
	// able to start until it returns. fn must be bounded — it is on the ACK
	// path of every Export parked behind the writer.
	RunBarrier(fn func()) error
}

// GCConfig configures one collection pass.
type GCConfig struct {
	// Store is the durable aggregate store. Must satisfy GCStore.
	Store Store
	// Registrar owns the dictionary identity maps.
	Registrar *DurableRegistrar
	// Cache is the hot-path intern cache in front of Registrar.
	Cache *Cache
	// Miner is the log-template miner whose templates are GC roots.
	Miner *TemplateMiner
	// Engine supplies the live shard keys.
	Engine *Engine
	// Series is the writer's series registry.
	Series *seriesRegistry
	// Barrier serializes the sweep with the commit path.
	Barrier Barrier
	// Metrics records the pass. nil disables recording.
	Metrics StoreMetrics
}

// Collect runs one mark-and-sweep pass over the aggregate identity tables.
//
// It is safe to call concurrently with ingest. It is NOT safe to call twice
// concurrently — the writer runs it, and the writer is one goroutine.
func Collect(cfg GCConfig) (GCStats, error) {
	var stats GCStats
	start := time.Now()
	metrics := cfg.Metrics
	if metrics == nil {
		metrics = noopStoreMetrics{}
	}
	gcs, ok := cfg.Store.(GCStore)
	if !ok || cfg.Registrar == nil || cfg.Series == nil || cfg.Barrier == nil {
		return stats, nil
	}

	mark, err := markPhase(gcs, cfg, &stats)
	if err != nil {
		stats.Duration = time.Since(start)
		metrics.RecordGC(stats, err)
		return stats, err
	}
	stats.MarkDuration = time.Since(start)

	barrierStart := time.Now()
	var sweepErr error
	if err := cfg.Barrier.RunBarrier(func() {
		sweepErr = sweepPhase(gcs, cfg, mark, &stats)
	}); err != nil {
		stats.Duration = time.Since(start)
		metrics.RecordGC(stats, err)
		return stats, err
	}
	stats.BarrierDuration = time.Since(barrierStart)
	stats.Duration = time.Since(start)
	metrics.RecordGC(stats, sweepErr)
	return stats, sweepErr
}

// markSet is the output of the lock-free mark phase.
type markSet struct {
	// seriesCandidates and dictCandidates are the rows nothing was found to
	// reference. They are candidates, not conclusions: the barrier revalidates
	// every one of them.
	seriesCandidates map[SeriesID]struct{}
	dictCandidates   map[uint32]struct{}
	// templateRows is the set of dictionary IDs that also back a durable
	// miner-state row, so the sweep deletes both together.
	templateRows map[uint32]struct{}
	// candidateKeys remembers each series candidate's identity. A candidate
	// the barrier rescues has to un-mark the dictionary IDs its key names, or
	// the sweep would delete the name of a series it just decided to keep.
	candidateKeys map[SeriesID]SeriesKey
	// dimTuples is the canonical encoding of every live dim tuple, so a
	// rescued series can re-mark the dim-key/dim-value IDs inside its tuple.
	dimTuples map[uint32][]byte
}

// markPhase performs the transitive mark WITHOUT the writer lock.
func markPhase(gcs GCStore, cfg GCConfig, stats *GCStats) (*markSet, error) {
	snap, err := gcs.GCSnapshot()
	if err != nil {
		return nil, err
	}
	referenced := snap.Referenced
	seriesRows, dictRows, templateRows := snap.Series, snap.Dict, snap.Templates
	stats.SeriesScanned = len(seriesRows)
	stats.DictScanned = len(dictRows)
	stats.TemplatesScanned = len(templateRows)

	// --- series liveness --------------------------------------------------
	liveSeries := make(map[SeriesID]struct{}, len(referenced))
	for id := range referenced {
		liveSeries[id] = struct{}{}
	}
	for id := range cfg.Series.Roots() {
		liveSeries[id] = struct{}{}
	}
	activeKeys := cfg.Engine.ActiveSeriesKeys()

	ms := &markSet{
		seriesCandidates: make(map[SeriesID]struct{}),
		dictCandidates:   make(map[uint32]struct{}),
		templateRows:     make(map[uint32]struct{}, len(templateRows)),
		candidateKeys:    make(map[SeriesID]SeriesKey),
	}
	for _, row := range templateRows {
		ms.templateRows[row.ID] = struct{}{}
	}

	liveDict := make(map[uint32]struct{}, len(dictRows))
	dimTuples := make(map[uint32][]byte, 64)
	dictByID := make(map[uint32]DictRow, len(dictRows))
	for _, row := range dictRows {
		dictByID[row.ID] = row
		if row.Kind == KindDimTuple {
			dimTuples[row.ID] = row.Value
		}
	}

	// A series survives when a durable row references it, when a live shard
	// still holds its key, or when the registry can still hand its ID out.
	for _, row := range seriesRows {
		_, referencedRow := liveSeries[row.ID]
		_, active := activeKeys[row.Key]
		if referencedRow || active {
			stats.SeriesRetained++
			markSeriesDict(row.Key, liveDict)
			continue
		}
		ms.seriesCandidates[row.ID] = struct{}{}
		ms.candidateKeys[row.ID] = row.Key
	}

	// --- transitive dictionary marking ------------------------------------
	// Dim tuples reached through a surviving series mark the dim-key and
	// dim-value IDs they contain. Without this the tuple lives and its
	// components are collected, which is the one failure mode that silently
	// unnames every dimension on a dashboard.
	for id := range liveDict {
		if enc, ok := dimTuples[id]; ok {
			markDimComponents(enc, liveDict)
		}
	}

	// Miner templates, staged mutations, sentinels and alias chains.
	if cfg.Miner != nil {
		for id := range cfg.Miner.Roots() {
			liveDict[id] = struct{}{}
		}
	}
	markAliasClosure(templateRows, liveDict)

	// In-memory roots that no durable row reflects yet.
	for id := range cfg.Registrar.Roots() {
		liveDict[id] = struct{}{}
	}
	if cfg.Cache != nil {
		for id := range cfg.Cache.Roots() {
			liveDict[id] = struct{}{}
		}
	}

	// A surviving dictionary row keeps its own tenant alive: a tenant ID that
	// scopes a live name is not garbage even when no series names it directly.
	for id := range liveDict {
		if row, ok := dictByID[id]; ok && row.TenantID != GlobalTenant {
			liveDict[row.TenantID] = struct{}{}
		}
	}

	for _, row := range dictRows {
		if _, live := liveDict[row.ID]; live {
			stats.DictRetained++
			continue
		}
		ms.dictCandidates[row.ID] = struct{}{}
	}
	ms.dimTuples = dimTuples
	return ms, nil
}

// markSeriesDict marks the four dictionary IDs a series key names.
func markSeriesDict(k SeriesKey, live map[uint32]struct{}) {
	for _, id := range [...]uint32{k.TenantID, k.ServiceID, k.NameID, k.DimsID} {
		if id != 0 {
			live[id] = struct{}{}
		}
	}
}

// markDimComponents decodes a canonical dim-tuple encoding and marks the
// dim-key and dim-value IDs inside it. A malformed tuple marks whatever it
// managed to decode and stops: refusing to guess is what keeps a corrupt row
// from taking live identities down with it.
func markDimComponents(enc []byte, live map[uint32]struct{}) {
	for len(enc) > 0 {
		keyID, n := binary.Uvarint(enc)
		if n <= 0 {
			return
		}
		enc = enc[n:]
		valueID, n := binary.Uvarint(enc)
		if n <= 0 {
			return
		}
		enc = enc[n:]
		if keyID > 0 && keyID <= uint64(^uint32(0)) {
			live[uint32(keyID)] = struct{}{}
		}
		if valueID > 0 && valueID <= uint64(^uint32(0)) {
			live[uint32(valueID)] = struct{}{}
		}
	}
}

// markAliasClosure follows persisted alias edges transitively from every
// already-live template. A retired alias is collectible only when nothing
// live reaches it, and marking one end of a chain has to mark the rest of it.
func markAliasClosure(rows []TemplateRow, live map[uint32]struct{}) {
	aliasOf := make(map[uint32]uint32, len(rows))
	aliasFrom := make(map[uint32][]uint32, len(rows))
	for _, row := range rows {
		if row.AliasOf != 0 {
			aliasOf[row.ID] = row.AliasOf
			aliasFrom[row.AliasOf] = append(aliasFrom[row.AliasOf], row.ID)
		}
	}
	queue := make([]uint32, 0, len(live))
	for id := range live {
		queue = append(queue, id)
	}
	for len(queue) > 0 {
		id := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		// Forward: the survivor a live retired ID forwards to.
		if to, ok := aliasOf[id]; ok {
			if _, seen := live[to]; !seen {
				live[to] = struct{}{}
				queue = append(queue, to)
			}
		}
		// Backward: every retired ID that forwards INTO a live template. A
		// historical series still names it, so it and its row stay.
		for _, from := range aliasFrom[id] {
			if _, seen := live[from]; !seen {
				live[from] = struct{}{}
				queue = append(queue, from)
			}
		}
	}
}

// sweepPhase runs inside the identity-maintenance barrier.
func sweepPhase(gcs GCStore, cfg GCConfig, ms *markSet, stats *GCStats) error {
	// The series registry is the writer's own structure and the barrier runs
	// on the writer, so it is held across the sweep rather than fenced. The
	// dictionary registrar is on the ingest hot path and gets the fence
	// instead: no ingest goroutine may block on a DELETE.
	cfg.Series.Lock()
	defer cfg.Series.Unlock()

	before := len(ms.seriesCandidates) + len(ms.dictCandidates)
	rescued := make([]SeriesID, 0, 8)
	for id := range ms.candidateKeys {
		if _, still := ms.seriesCandidates[id]; !still {
			continue
		}
		rescued = append(rescued, id)
	}
	cfg.Series.revalidateLocked(ms.seriesCandidates)
	cfg.Registrar.Revalidate(ms.dictCandidates)

	// A series the barrier rescued keeps its NAME alive too. Re-mark the four
	// dictionary IDs its key holds — plus the components of its dim tuple —
	// and drop them from the dictionary candidate set. Sweeping the name of a
	// series we just decided to keep is the exact shape of the bug this whole
	// phase exists to prevent.
	remark := make(map[uint32]struct{}, 8)
	for _, id := range rescued {
		if _, gone := ms.seriesCandidates[id]; gone {
			continue
		}
		markSeriesDict(ms.candidateKeys[id], remark)
	}
	for id := range remark {
		if enc, ok := ms.dimTuples[id]; ok {
			markDimComponents(enc, remark)
		}
	}
	for id := range remark {
		delete(ms.dictCandidates, id)
	}
	stats.Revalidated = before - (len(ms.seriesCandidates) + len(ms.dictCandidates))
	stats.SeriesRetained += len(rescued) - len(ms.seriesCandidates)

	if len(ms.seriesCandidates) == 0 && len(ms.dictCandidates) == 0 {
		cfg.Registrar.ClearTouched()
		cfg.Series.clearTouchedLocked()
		return nil
	}

	cfg.Registrar.Fence(ms.dictCandidates)
	if cfg.Cache != nil {
		cfg.Cache.Fence(ms.dictCandidates)
	}

	seriesIDs := sortedSeriesIDs(ms.seriesCandidates)
	dictIDs := sortedDictIDs(ms.dictCandidates)
	templateIDs := make([]uint32, 0, len(dictIDs))
	for _, id := range dictIDs {
		if _, ok := ms.templateRows[id]; ok {
			templateIDs = append(templateIDs, id)
		}
	}

	sweep, err := gcs.SweepIdentities(seriesIDs, dictIDs, templateIDs)
	if err != nil {
		// Nothing was deleted, so nothing in memory may change. Release the
		// fence and leave every map exactly as it was.
		cfg.Registrar.Unfence()
		if cfg.Cache != nil {
			cfg.Cache.Unfence()
		}
		return fmt.Errorf("aggregate gc: sweep failed: %w", err)
	}
	stats.SeriesSwept = sweep.Series
	stats.DictSwept = sweep.Dict
	stats.TemplatesSwept = sweep.Templates

	// Committed. Only now do the forward and reverse maps lose their entries;
	// Forget also drops the fence, so a swept value's next appearance mints a
	// fresh ID instead of resurrecting a deleted one.
	cfg.Series.forgetLocked(ms.seriesCandidates)
	if cfg.Cache != nil {
		cfg.Cache.Forget(ms.dictCandidates)
		cfg.Cache.Unfence()
	}
	cfg.Registrar.Forget(ms.dictCandidates)
	return nil
}

// sortedSeriesIDs renders a candidate set as an ordered slice; deletion order
// is deterministic so a failure is reproducible.
func sortedSeriesIDs(set map[SeriesID]struct{}) []SeriesID {
	out := make([]SeriesID, 0, len(set))
	for id := range set {
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// sortedDictIDs is sortedSeriesIDs for dictionary IDs.
func sortedDictIDs(set map[uint32]struct{}) []uint32 {
	out := make([]uint32, 0, len(set))
	for id := range set {
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// LogGC prints one collection pass at info level.
func LogGC(stats GCStats, err error) {
	if err != nil {
		slog.Error("aggregate: identity gc failed", "error", err,
			"mark_duration", stats.MarkDuration, "barrier_duration", stats.BarrierDuration)
		return
	}
	if stats.SeriesSwept == 0 && stats.DictSwept == 0 && stats.TemplatesSwept == 0 {
		slog.Debug("aggregate: identity gc found nothing to collect",
			"series_scanned", stats.SeriesScanned, "dict_scanned", stats.DictScanned,
			"duration", stats.Duration)
		return
	}
	slog.Info("🧹 Aggregate identity gc complete",
		"series_swept", stats.SeriesSwept,
		"dict_swept", stats.DictSwept,
		"templates_swept", stats.TemplatesSwept,
		"series_retained", stats.SeriesRetained,
		"dict_retained", stats.DictRetained,
		"revalidated", stats.Revalidated,
		"mark_duration", stats.MarkDuration,
		"barrier_duration", stats.BarrierDuration,
		"duration", stats.Duration,
	)
}
