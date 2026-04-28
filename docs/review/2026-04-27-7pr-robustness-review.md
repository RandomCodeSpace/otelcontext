# Code Review — 7-PR Robustness Initiative (2026-04-27)

Scope: acf904d, 17f70dc, cf9c1f5, 96ec26e, 436201e, fdf2433, 050525e

## Critical

### C1. MCP `break` inside `select { default: }` does not escape outer switch
File: `/home/dev/projects/otelcontext/internal/mcp/server.go:283`
- `break` inside the `default:` case of the inner `select { case s.callSlots <- struct{}{}: default: ... break }` only breaks the `select`, not the outer `case "tools/call":`. This is a classic Go pitfall and the well-known reason the author added the follow-up guard at line 286-289 (`if rpcErr != nil { break }`). However, the comment at line 286 ("rpcErr was set inside the select-default") asserts intent the language does not provide; the construct only works because of the second guard. Even so, between line 285 and line 286 nothing actually depends on it - functionally correct - but: the comment "skip the call" misleadingly implies the inner `break` did the skipping. If anyone refactors out the second `if rpcErr != nil { break }` thinking it's redundant, semaphore acquisition will silently bypass the overload error and proceed to the call without ever filling a slot (because the `select` already returned). Recommend either: drop the inner `break` (it is dead) and reword the comment, or replace the construct with a labeled break.
- Severity escalation: low-likelihood-of-trip but high-impact-when-tripped, and the dead `break` actively misdirects readers about how the control flow works.
- Suggested fix: remove the inner `break` keyword (it is unreachable-effective code), and rewrite the comment as `// rpcErr was set in the inner select default; skip dispatch.`

## High

### H1. MCP semaphore is leaked when the call times out
File: `/home/dev/projects/otelcontext/internal/mcp/server.go:294-298`
- On timeout (`runWithTimeout` returns with `timedOut=true`), the handler does `<-s.callSlots` at line 296 to release the slot, but the goroutine spawned inside `runWithTimeout` (server.go:428) is still alive doing the actual tool work. The `s.inFlight.Add(-1)` at line 298 also under-counts: the goroutine continues to occupy DB / GraphRAG resources but the in-flight gauge says zero. Worse - the slot is freed for new admissions while the timed-out handler is still consuming connection-pool capacity, defeating the purpose of the semaphore as a backpressure control. Under sustained timeouts you can easily get N times the configured concurrency in flight. Author commented on this at line 421 ("its goroutine still runs to completion in the background") but mitigation is missing.
- Suggested fix: hold the slot until the goroutine *actually* finishes - e.g. let the goroutine itself release the slot via `defer` once `done` is sent, and use a non-blocking send on a closed `done` so the timeout path doesn't deadlock. Or, wrap the toolHandler so it must respect ctx before it touches the DB connection.

### H2. FTS5 `external content` triggers vs PurgeLogsBatched chunked DELETE - works only because triggers fire per-row
File: `/home/dev/projects/otelcontext/internal/storage/fts5.go:60-62`, `/home/dev/projects/otelcontext/internal/storage/log_repo.go:273`
- The DELETE trigger uses `INSERT INTO logs_fts(logs_fts, rowid, body, service_name) VALUES('delete', old.id, old.body, old.service_name)`. SQLite fires AFTER DELETE triggers per-row, so `DELETE FROM logs WHERE id IN (SELECT id ...)` chunks of 50,000 rows will execute 50,000 trigger inserts under one statement - within a busy DB this materially extends the DELETE window. Plan claimed "kept in sync via AFTER INSERT/DELETE/UPDATE triggers" - true, but the trigger DDL stores `body` and `service_name` in the operation, which is correct for an `external-content` table but means each DELETE statement effectively does 2 writes (logs + fts5) and the WAL grows accordingly. No test exercises a 25k-row purge with FTS triggers loaded, so the perf characteristic is unmeasured.
- Not a correctness bug. Recommend: add an integration-style test that purges >10k rows with FTS5 enabled and asserts both `logs` and `logs_fts` are empty afterward, then watch the duration metric. If it regresses retention SLO, switch to a transaction-bracketed bulk delete or use FTS5 contentless-delete idiom.

### H3. `getLogsV2LikeFallback` silently masks any DB error including non-FTS5 errors
File: `/home/dev/projects/otelcontext/internal/storage/log_repo.go:128-135`
- `g.Wait()` can fail for many reasons unrelated to FTS5: connection-pool exhaustion, query-cancelled, OOM, syntax in a future filter. The fallback runs `r.getLogsV2LikeFallback(...)` without inspecting `err`. So an outage that affects both paths returns whatever the fallback returns - which in the "DB really is down" case is also an error, but in the "table renamed during migration" case might return empty results without the operator ever seeing the FTS5 error in logs. CLAUDE.md root rule: "fix root causes, not paper over with silent fallbacks". 
- Suggested fix: log the FTS5 error before falling back (`slog.Warn("FTS5 query failed, falling back to LIKE", "err", err)`), and only fall back on a small allow-list of error classes (`SQLITE_ERROR` with "no such table", malformed-MATCH errors). Same applies to `searchLogsFTS5` at `repository.go:310-315`.

### H4. PartitionScheduler.Stop() races with itself; `done` channel set in the wrong place
File: `/home/dev/projects/otelcontext/internal/storage/partitions_scheduler.go:54,89-103,106`
- `done` is created once at construction (line 54) but the scheduler `loop` calls `defer close(s.done)` at line 106. If `Stop()` is called twice, both calls will see `started.Load() == true`, both will read the same `done` channel; the second `<-done` reads a closed channel (immediate return - safe). However, `s.cancel = cancel` at line 76 is mutated under `mu` only on Start; Stop reads it under `mu` (line 94), then calls `cancel()` outside the lock, which is fine. But Start does NOT reset `s.done` if called after a Stop, and `started.Store(true)` is never reset to false in Stop - so a Start/Stop/Start sequence will see `started` already true and skip work, while a fresh `done` channel never gets created and the loop never starts again. Lifecycle is one-shot in practice; not documented as such.
- Suggested fix: document one-shot behavior on the type, or reset `started`, `done`, and `cancel` at the end of `Stop()` after the goroutine has exited. Tests should cover Start-Stop-Start.

### H5. Pipeline `SoftThreshold` floor disallows valid 0.0 (always-soft)
File: `/home/dev/projects/otelcontext/internal/ingest/pipeline.go:141`
- `if cfg.SoftThreshold <= 0 || cfg.SoftThreshold >= 1.0 { cfg.SoftThreshold = d.SoftThreshold }`. The treatment of zero as "use default" silently replaces a deliberate operator choice ("always drop healthy batches when not strictly empty") with the 0.9 default. More problematic: `SoftThreshold = 0` is a sane configuration to express "in degraded mode, only ever ship priority batches"; the code masks this. CLAUDE.md: "Don't add error handling for scenarios that can't happen" - here a real value is being clobbered.
- Suggested fix: change to `if cfg.SoftThreshold < 0 || cfg.SoftThreshold > 1.0 { cfg.SoftThreshold = d.SoftThreshold }` and explicitly allow 0 and 1.

## Medium

### M1. TSDB `seriesPerTenant` decrement on flush-reset is unauthenticated
File: `/home/dev/projects/otelcontext/internal/tsdb/aggregator.go:281`
- `seriesPerTenant = make(map[string]int)` resets cardinality per flush window, which is the right knob to keep the budget per-window. However, plan said "seriesPerTenant counts unique (non-overflow) bucket keys per tenant and is reset by flush()". On a flush() that fails to persist, the counters reset but the buckets may be retained on the failure path - so the next window briefly under-counts. Also, the per-tenant cap check at line 178 reads `seriesPerTenant[m.TenantID]` - this is checked under `a.mu`, fine. But `cardinalityOverflow` callback is invoked inside the same lock window (line 184, 205) which makes Prometheus counter increments lock-contending under high overflow. `Inc()` on a CounterVec is internally locked; nesting under `a.mu` is benign but worth knowing.
- Suggested fix: capture the tenantID inside the lock, then call the callback after `mu.Unlock()`. Or use `sync.RWMutex` and run the cardinality check under RLock + a separate atomic for the count.

### M2. Pipeline `Submit` has TOCTOU between fullness sampling and channel send
File: `/home/dev/projects/otelcontext/internal/ingest/pipeline.go:198-213`
- `fullness := float64(len(p.queue)) / float64(p.cfg.Capacity)` at line 198 is racy with workers draining concurrently: the queue can drop from 95% to 50% between the read and the `select`, and a healthy batch will be dropped despite room available. Conversely workers can fall behind between read and `select` and a priority batch sees `default` and returns ErrQueueFull. The race is benign for the soft-drop direction (rare drop of a healthy batch), and for hard-drop the channel `default` is the actual gate so correctness holds. Worth documenting at the top of `Submit` so future maintainers don't try to "fix" the unsynchronized read.
- Suggested fix: docstring note. Don't add a lock - the design is "sample-and-decide" deliberately, and a lock would serialize all submitters.

### M3. MCP cache key normalization is incomplete: nested maps are not key-sorted
File: `/home/dev/projects/otelcontext/internal/mcp/cache.go:87-92`
- `cacheKey` only sorts top-level argument keys and serializes each value with `json.Marshal`. Go's `json.Marshal` does sort map keys alphabetically since 1.12, so this is fine in practice for `map[string]any`. But for `[]any` containing maps (e.g. `{"filters": [{"b":1,"a":2}]}`), Go json sorts those too. So the keying is actually stable for stdlib JSON inputs - good. However, slices preserve order and a client that sends `["a","b"]` vs `["b","a"]` should hit different cache entries (correct semantically: order matters). Not a bug.
- However: argument values come from `params.Arguments map[string]any` parsed via `json.Unmarshal`, which gives `map[string]any` for objects and `[]any` for arrays. So the stable property holds. No fix.

### M4. `isQueueFull` accepts ANY gRPC ResourceExhausted, including legitimate quota errors
File: `/home/dev/projects/otelcontext/internal/ingest/otlp_http.go:103-114`
- If a future change in `Export()` returns RESOURCE_EXHAUSTED for some reason other than the pipeline (e.g. tenant rate limit, gRPC max-recv-msg-size), HTTP returns 429 + Retry-After: 1 and the throttle metric increments. That is also the correct user-facing behavior (tell the client to back off), but the metric `otelcontext_http_otlp_throttled_total` will conflate two distinct backpressure causes. Not a bug, but operators reading the metric may be misled.
- Suggested fix: make the pipeline `ErrQueueFull` translate to a custom gRPC error with a metadata tag (e.g. `reason=pipeline-queue-full`) and let `isQueueFull` check for that tag, fall through to true for the bare RESOURCE_EXHAUSTED case but emit a different metric label.

### M5. `pgLogsRelkind` "no rows" detection by string-match
File: `/home/dev/projects/otelcontext/internal/storage/partitions.go:248-251`
- `strings.Contains(err.Error(), "no rows")` is fragile across drivers. The pgx driver returns `sql.ErrNoRows`; GORM's `Row().Scan()` may wrap it. Use `errors.Is(err, sql.ErrNoRows)` instead. Today this works because the only Postgres driver in use is pgx and the message contains "no rows", but a driver upgrade can break greenfield detection - and the failure mode is loud (refuse to start), so a bug here means an operator's first deploy fails inscrutably.
- Suggested fix: `if errors.Is(err, sql.ErrNoRows) { return "", nil }`.

## Low

### L1. SSE writer is not protected against concurrent writes
File: `/home/dev/projects/otelcontext/internal/mcp/server.go:340-359`
- The SSE handler writes both heartbeat (`: keep-alive\n\n`) and notification frames into the same `http.ResponseWriter` from one goroutine via select - so single-writer is OK. Just confirming for the record.

### L2. Pipeline `worker` shutdown is leaky on parent ctx cancel between drain ticks
File: `/home/dev/projects/otelcontext/internal/ingest/pipeline.go:254-273`
- When ctx is canceled (line 257) workers exit immediately without draining the buffered queue, dropping any in-flight batches. The Stop() path (line 261) drains; the ctx-cancel path does not. Production shutdown uses `Stop()` so this is not exercised, but tests using ctx cancel may mis-attribute drops.
- Suggested fix: either always drain on exit, or document that ctx cancel is the "abort" path.

### L3. Partition cutoff comparison uses `!upper.After(cutoffUTC)` which is correct but easily misread
File: `/home/dev/projects/otelcontext/internal/storage/partitions.go:227`
- `!upper.After(cutoff)` is `upper <= cutoff`. The DROP triggers when the upper bound is less-than-or-equal to the cutoff. With daily partitions where upper is `day+24h`, this drops a partition exactly at the moment the cutoff sweeps past its upper. Correct, but the comment "Entire partition range ends at or before the cutoff" reads as `<` not `<=`. Make it `if upper.Before(cutoff) || upper.Equal(cutoff)` for legibility.

### L4. `defaultCacheTTL` is referenced but I did not see it defined in the snippet
File: `/home/dev/projects/otelcontext/internal/mcp/server.go` (line ~40)
- Visible in the const block in earlier slice; not a finding, just confirming the const exists.

## Plan Alignment Summary

| Phase | Plan claim | Verdict |
|---|---|---|
| 0 | Workers 4->16, queue 10k->100k | Implemented; verified in CLAUDE.md and config.go |
| 1 | Hybrid backpressure: <90% accept, 90-100% drop healthy, 100% RESOURCE_EXHAUSTED | Implemented; race noted (M2) |
| 2 | Per-tenant cap checked first | Implemented at aggregator.go:178 (overTenantCap before overGlobalCap) |
| 3a | FTS5 + BM25 + triggers | Implemented; perf char unmeasured (H2) |
| 3b/5 | Greenfield-only partitioning, refuse pre-existing unpartitioned logs | Implemented at partitions.go:99-100 |
| 4 | HTTP 429 + Retry-After parity | Implemented; metric semantics noted (M4) |
| 6 | Concurrency cap, timeout, cache, SSE keep-alive | Implemented; semaphore leak on timeout (H1) |

## Backwards-Compatibility Audit

- `INGEST_ASYNC_ENABLED=true` is the new default (ingestion path changes). Plan flagged this. Acceptable.
- `DB_POSTGRES_PARTITIONING=""` correctly stays legacy.
- `METRIC_MAX_CARDINALITY_PER_TENANT=0` correctly stays unlimited.
- `MCP_CACHE_TTL_MS=0` correctly disables cache.
- No subtle default flips found.

## Test Strength

- Pipeline tests cover nil/empty/soft/hard. Strong assertions.
- TSDB cardinality tests should explicitly verify the per-tenant cap fires BEFORE global; needs a test where global has headroom but per-tenant is exceeded. Recommend adding.
- FTS5 tests cover BM25, prefix, stemming, tenant isolation, delete trigger - good. Missing: large purge perf regression.
- Partition tests cover greenfield refuse and DROP. Missing: PartitionScheduler Start-Stop-Start lifecycle (H4).
- MCP robustness tests cover concurrency cap and timeout. Missing: cache-key isolation with conflicting tenant args, semaphore leak on timeout (H1).

## Adherence to Project Rules

- Native net/http only: confirmed.
- Embedded DBs only: confirmed.
- No new frameworks introduced.
- Minimal-diff discipline: largely respected; no scope creep observed.
