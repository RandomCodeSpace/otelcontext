# Plan — Storage rebalance: drop FTS5, persist vectordb, cap log search

**Status:** Approved scope, ready for implementation
**Date:** 2026-04-30
**Reviewers:** codex (vectordb persistence design, 2026-04-30); user (scope sign-off, 2026-04-30)

## Problem

Three coupled inefficiencies surfaced while sizing for the 150-service × 7-day production target on SQLite:

1. **FTS5 inverted index consumes 30-40% of DB disk.** At the current 4GB sample, ~1.5GB; at full production scale, tens of GB. The UI never queries it (verified: `ui/src/hooks/useLogs.ts:21` calls `/api/logs` with no `q`); only the `search_logs` MCP tool depends on it, and that has a working LIKE fallback already wired at `internal/storage/log_repo.go:105`.
2. **vectordb is not persisted.** Daemon restart empties the in-memory TF-IDF index. `find_similar_logs` and GraphRAG `SimilarErrors` return degraded results until ERROR/WARN logs flow back in naturally — minutes to hours, depending on error rate.
3. **`search_logs` has no time-range cap.** Tolerable with FTS5; without it, an unscoped 7-day keyword query is a 30+ minute table scan. Worst case must be bounded.

## Goal

Cut SQLite footprint by 30-40%, eliminate vectordb cold-start blindness, and bound `search_logs` worst case to single-digit seconds — without changing `find_similar_logs`, GraphRAG, or any non-search MCP tool surface.

## Non-goals

- Inverted index / sublinear cosine search (requires raising 100k cap, separate change)
- Dense embeddings / ONNX
- Per-tenant snapshot sharding, multi-process flock
- Raising `VECTOR_INDEX_MAX_ENTRIES`
- Removing `search_logs` (kept, degraded via LIKE)
- Changes to Postgres `pg_trgm` path (SQLite-only)

---

## Phase 0a — `LOG_FTS_ENABLED` config flag

Default `false`. New SQLite deploys skip FTS5 entirely.

**Files:**
- `internal/config/config.go` — add `LogFTSEnabled bool`, env `LOG_FTS_ENABLED`, default false
- `internal/storage/fts5.go` — `fts5Available()` returns false when flag is off; `EnsureLogsFTS5()` becomes no-op
- `internal/storage/log_repo.go` — Repository gains `ftsEnabled bool`; `useFTS5` checks both driver + flag
- `internal/storage/factory.go` (or wherever `NewRepository` is) — thread `cfg.LogFTSEnabled` through
- `CLAUDE.md` — update FTS5 description, note opt-in

LIKE fallback already exists at `log_repo.go:105-108`; flipping the flag silently routes `filter.Search` queries through it. No new fallback code.

## Phase 0b — One-shot FTS5 reclaim

For existing DBs with `logs_fts`:

- New admin endpoint `POST /api/admin/drop_fts` (auth-gated, alongside existing `/api/admin/purge` + `/api/admin/vacuum`)
- Body: `DROP TRIGGER IF EXISTS logs_fts_ai; logs_fts_ad; logs_fts_au; DROP TABLE IF EXISTS logs_fts; VACUUM;`
- Returns `{"reclaimed_bytes": N, "elapsed_ms": M}`
- Refuses (405) if `LOG_FTS_ENABLED=true` — won't drop while in active use

**Files:**
- `internal/api/admin_handlers.go` (or wherever `handlePurge`/`handleVacuum` live) — add `handleDropFTS`
- `internal/api/server.go:108-109` — register route
- Test: integration test creates `logs_fts`, calls handler, asserts gone + bytes reclaimed

VACUUM blocks writes 10-60min on a 4GB DB — opt-in only, document for off-hours.

## Phase 0c — 24h cap on `search_logs`

**Description update at `internal/mcp/tools.go` ~line 37:**

> "Search log bodies by keyword. Limited to the last 24 hours. Strongly recommend setting `service_name` and/or `severity` to scope the search; unscoped keyword queries scan large row counts without FTS5. Returns up to `limit` results ordered by timestamp desc."

**Helper at `internal/mcp/tools.go` (or new `internal/mcp/clamp.go`):**

```go
func clampTo24h(start, end, now time.Time) (time.Time, time.Time, error) {
    if end.IsZero()    { end = now }
    if start.IsZero()  { start = end.Add(-24 * time.Hour) }
    if end.After(now)  { end = now }
    cutoff := now.Add(-24 * time.Hour)
    if end.Before(cutoff)   { return time.Time{}, time.Time{}, errors.New("search window must be within the last 24h") }
    if start.Before(cutoff) { start = cutoff }
    if !start.Before(end)   { return time.Time{}, time.Time{}, errors.New("start_time must be before end_time") }
    return start, end, nil
}
```

**Apply at:**
- `internal/mcp/tools.go::toolSearchLogs` (~line 445) — after arg parse
- `internal/api/log_handlers.go::handleGetLogs` — when `q` param non-empty (defense in depth; UI doesn't hit this with `q` but direct HTTP callers shouldn't bypass)

**Tests:** 4 cases for `clampTo24h` (defaults, oversize window clamped, out-of-cap rejected, invalid range rejected) + 1 integration test each for MCP and HTTP.

**Behavior matrix:**

| Input | Effective window |
|---|---|
| nothing | now-24h → now |
| start = 5d ago | now-24h → now (start clamped) |
| start = 5d ago, end = 4d ago | rejected |
| end = future | start → now (end clamped) |
| `q=""` (no search) | unconstrained — cap only fires when search is set |

---

## Phase 1-5 — vectordb persistence

### Format

```
bytes[0:4]   magic              "VDB1" (ASCII)
bytes[4:8]   format version     uint32 BE
bytes[8:12]  payload CRC32-IEEE uint32 BE (over bytes[12:])
bytes[12:]   gob payload        Snapshot
```

```go
type Snapshot struct {
    LastIndexedID uint                  // max Log.ID seen by Add()
    MaxSize       int
    Docs          []LogVector
    IDF           map[string]float64
    WrittenAt     int64                 // unix seconds
}
```

### Tail-replay key

**Use `Log.ID` (auto-increment PK), not `Log.Timestamp`.** Codex caught: `Index.Add()` has no dedup; timestamp-based replay would double-index boundary entries on every restart. ID is monotonic, dedup-free, DB-agnostic.

Query: `WHERE id > ? AND severity IN ('ERROR','WARN','WARNING','FATAL','CRITICAL') ORDER BY id ASC LIMIT 10000`. Paged loop until empty.

### Atomic write

`writeAtomic(path, data)`: write to `path+".tmp"` → fsync → close → rename. On `EXDEV` (cross-device): log warn once, fall back to `os.WriteFile` directly. On fsync error: delete `.tmp`, return error.

### Lifecycle

**Startup** (in `cmd/main.go` after DB open, before ingest accept):
```go
idx := vectordb.New(cfg.VectorIndexMaxEntries)
if cfg.VectorIndexSnapshotPath != "" {
    _ = idx.LoadSnapshot(cfg.VectorIndexSnapshotPath)   // tolerates missing/corrupt
    _ = idx.ReplayFromDB(ctx, repo)                      // catches tail since LastIndexedID
    go idx.SnapshotLoop(ctx, path, cfg.VectorIndexSnapshotInterval)
}
```

**Shutdown:** final `idx.WriteSnapshot()` between gRPC/HTTP `Shutdown()` returning and `graphrag.Stop()` — captures every `Add()` that completed before ingest stopped.

### Phases

- **Phase 1**: `internal/vectordb/snapshot.go` + tests — encode/decode, magic/version/CRC, atomic write, EXDEV fallback. Pure isolation, no behavior change.
- **Phase 2**: wire into `Index` — add `lastIndexedID` field tracked in `Add()`; methods `LoadSnapshot`, `WriteSnapshot`, `LastIndexedID()`. Config fields added but no goroutine yet.
- **Phase 3**: DB tail replay — `Repository.LogsForVectorReplay(ctx, sinceID, limit) ([]Log, error)` (paged); `Index.ReplayFromDB(ctx, repo)` walks pages calling `Add()`.
- **Phase 4**: `Index.SnapshotLoop(ctx, path, interval)` background goroutine; wire startup/shutdown in `cmd/main.go`.
- **Phase 5**: 5 Prometheus metrics (`snapshot_writes_total{result}`, `snapshot_duration_seconds`, `snapshot_size_bytes`, `snapshot_load_total{result}`, `replay_logs_total`); CLAUDE.md vectordb section + config table.

---

## Files (combined)

### New
- `internal/vectordb/snapshot.go`
- `internal/vectordb/snapshot_test.go`
- `internal/mcp/clamp.go` + `clamp_test.go`

### Modified
- `internal/vectordb/index.go` — `lastIndexedID`, `LoadSnapshot`, `WriteSnapshot`, `SnapshotLoop`, `ReplayFromDB`
- `internal/storage/fts5.go` — gate on flag
- `internal/storage/log_repo.go` — Repository ftsEnabled field
- `internal/storage/repository.go` — `LogsForVectorReplay`
- `internal/storage/factory.go` — config wiring
- `internal/api/log_handlers.go` — 24h cap symmetry on `q=`
- `internal/api/admin_handlers.go` (or equivalent) — `handleDropFTS`
- `internal/api/server.go` — `/api/admin/drop_fts` route
- `internal/mcp/tools.go` — description update + `clampTo24h` call in `toolSearchLogs`
- `internal/config/config.go` — `LOG_FTS_ENABLED`, `VECTOR_INDEX_SNAPSHOT_PATH` (default `data/vectordb.snapshot`), `VECTOR_INDEX_SNAPSHOT_INTERVAL` (default `5m`)
- `cmd/main.go` (or `internal/ui/ui.go` — wherever vectordb is constructed) — startup/shutdown wiring
- `internal/telemetry/metrics.go` — 5 vectordb metrics
- `CLAUDE.md` — FTS5 description, vectordb persistence note, config table additions

## Acceptance criteria

### Phase 0 (FTS5 + cap)
1. `LOG_FTS_ENABLED=false` (default): new SQLite deploy has no `logs_fts` table; `search_logs` returns results via LIKE fallback.
2. `LOG_FTS_ENABLED=true`: existing FTS5 path works unchanged (regression test).
3. `POST /api/admin/drop_fts` reclaims expected bytes; returns 405 when flag is true.
4. `toolSearchLogs` with no times → defaults to last 24h.
5. start_time = 7d ago → clamped to now-24h.
6. Window entirely outside cap → rejected with explicit error.
7. HTTP `GET /api/logs?q=...` applies same cap.
8. Updated tool description visible in MCP `tools/list`.

### Phase 1-5 (vectordb persistence)
1. Daemon restart → `find_similar_logs` returns useful results in <1s (vs hours pre-change).
2. Tail replay correctness: seed N → snapshot → seed M → restart → exactly N+M entries, no duplicate `LogID`.
3. `kill -9` between snapshots: previous snapshot loads, no `.tmp` left behind.
4. CRC mismatch / wrong magic / version mismatch: warning logged, full rebuild kicks in.
5. 5 new metrics present at `/metrics`.
6. Manual: oteliq daemon shows `vectordb: loaded N entries from snapshot, replayed M from DB`.

## Risks

| Risk | Mitigation |
|---|---|
| Flag flipped mid-deployment leaves stale `logs_fts` | Stale table is harmless until 0b dropped. |
| VACUUM on 4GB blocks writes 10-60min | Document; admin endpoint is opt-in, not automatic. |
| 24h cap rejects legitimate historical queries | Explicit error; direct DB access remains for one-offs. |
| LIKE fallback slower than expected at scale | Accepted trade-off. Escalate to v2 vectordb design if painful. |
| Snapshot format breaks across Go versions | Magic + version + CRC → full rebuild on any decode error. Bump version on `LogVector` struct change. |
| Cross-device rename loses atomicity | EXDEV detected, fall back to direct write. Constraint documented. |

## Phasing for shipping

5 PRs, in order:

- **PR1**: Phase 0a + 0b + 0c (FTS5 disable + reclaim helper + 24h cap). Single PR — tightly related.
- **PR2**: Phase 1 + 2 (snapshot encode/decode + Index integration).
- **PR3**: Phase 3 (DB tail replay).
- **PR4**: Phase 4 (snapshot loop + shutdown hook).
- **PR5**: Phase 5 (metrics + docs).

PR1 ships standalone. PR2-5 are sequential. Phase 0 can land before any vectordb work.

## Verification

```bash
# Phase 0
go test ./internal/storage/... ./internal/mcp/... ./internal/api/...

# Phase 1-5
go test ./internal/vectordb/...

# Integration smoke
go vet ./...
go build -o /tmp/otelcontext .
LOG_FTS_ENABLED=false /tmp/otelcontext &
# Expect: "logs_fts: disabled (LOG_FTS_ENABLED=false)" on first start
# After restart: "vectordb: loaded N entries from snapshot, replayed M from DB"
```

## Resolved scope decisions

- Snapshot path: flat `data/vectordb.snapshot`
- zstd compression: not in v1 — ship uncompressed gob
- Phase split: 5 PRs (above)
- FTS5 default: off (`LOG_FTS_ENABLED=false`)
- 24h cap interpretation: search window must overlap with last 24h; window entirely older is rejected, not silently emptied
