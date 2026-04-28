# Second-Opinion Correctness & Security Review
**Scope:** 7 squash-merge commits (acf904d..050525e) on `origin/main`  
**Date:** 2026-04-28  
**Reviewer:** independent pass (main shell)  
**Tool runs:** `go vet ./...` → clean; `go test ./internal/tsdb/... ./internal/mcp/... ./internal/ingest/... ./internal/storage/... -race -count=1 -timeout 120s` → **269 passed, 0 failures, 0 races detected**

---

## HIGH

### H-1 — Data race on `droppedBatches` in TSDB aggregator
**File:** `internal/tsdb/aggregator.go:35, 263, 287`

**Problem:** `droppedBatches int64` is a plain (non-atomic) `int64` that is written in `flush()` (the single aggregation goroutine, outside `a.mu`) and read in `DroppedBatches()` (any caller goroutine) without synchronization.

**Root cause:** The field is incremented after `a.mu.Unlock()` returns — inside the `select default` branch — so the lock is not held at the time of the write. `DroppedBatches()` reads it without acquiring any lock.

**Evidence:**
```go
// flush() — lock already released
select {
case a.flushChan <- batch:
default:
    a.droppedBatches++          // line 287 — no lock held
    ...
    slog.Warn(..., "total_dropped", a.droppedBatches)  // second unsynchronised read
}

// DroppedBatches() — separate goroutine, no lock
func (a *Aggregator) DroppedBatches() int64 {
    return a.droppedBatches     // line 263 — bare read
}
```

Note: the race detector did not fire in the test run because the existing tests do not call `DroppedBatches()` concurrently with `flush()`. The race is real and will surface under load or when a metrics scrape hits the Prometheus endpoint while the aggregation goroutine is flushing.

**Fix:** Change the field declaration to `droppedBatches atomic.Int64` and replace `a.droppedBatches++` with `a.droppedBatches.Add(1)`, and `a.droppedBatches` reads with `a.droppedBatches.Load()`. Alternatively, move the increment inside the `a.mu`-held section (but the lock scope currently ends before the channel select).

---

## MEDIUM

### M-1 — TOCTOU race in `Submit()` between fullness check and channel send
**File:** `internal/ingest/pipeline.go` (`Submit` function)

**Problem:** The backpressure logic reads `len(p.queue)` to compute a `fullness` ratio, decides whether to drop silently or admit, then separately attempts `p.queue <- b` inside a `select/default`. A batch at 89.9% fullness passes the soft-drop gate, but if the queue reaches 100% between the check and the send, the `default` branch fires and returns `ErrQueueFull` — the caller gets a 429 instead of the intended silent drop.

**Root cause:** Two separate operations (`len` then `send`) on a shared channel with no lock between them. The send attempt IS the only reliable fullness test.

**Fix:** Restructure `Submit` so the channel send is the single decision point. Use a tiered select with an explicit 90%-threshold check only for the intentional-drop path, but always attempt the real send without a prior `len` check. Example shape:
```go
select {
case p.queue <- b:
    return nil
default:
    if shouldDrop(b) {
        p.observeDrop(...)
        return nil   // silent drop
    }
    return ErrQueueFull
}
```
This eliminates the window between check and send.

---

### M-2 — `a.ring` and `a.onIngest` read outside `a.mu` in `Ingest()`
**File:** `internal/tsdb/aggregator.go` (`Ingest` and `SetRingBuffer`/`SetMetrics`)

**Problem:** `Ingest()` reads `a.ring` and `a.onIngest` directly without holding `a.mu`, while `SetRingBuffer()` and `SetMetrics()` write them under `a.mu`. If either setter is called after `Start()`, this is a data race on the pointer values.

**Root cause:** Pointers are treated as startup-only but no guard enforces that constraint — callers can technically call setters at any time.

**Fix (two options):**
1. Read `a.ring` and `a.onIngest` under a short `a.mu.RLock()` inside `Ingest()`.
2. Add a `started atomic.Bool` guard that panics in setters when the aggregator is already running, and document the startup-only contract in godoc.

Option 2 is cheaper and makes the invariant explicit.

---

### M-3 — `process()` skips log callbacks when `BatchCreateSpans` fails
**File:** `internal/ingest/pipeline.go:307-320`

**Problem:** When `BatchCreateSpans` returns an error, `process()` returns immediately, skipping the `BatchCreateLogs` write and all log/span callbacks (including `GraphRAG.OnLogIngested`). A partial batch that contains both spans and logs loses the log data silently; the error counter increments but the logs are not retried or DLQ'd.

**Root cause:** The `return` after `BatchCreateSpans` failure was intentional ("mirrors the synchronous path's tolerance") but the comment does not acknowledge that co-batched logs are lost, and the async pipeline has no per-signal retry or DLQ path.

**Fix:** Decouple spans and logs into separate `if` blocks that do not short-circuit each other, mirroring the treatment of `BatchCreateTraces` (which `continue`s rather than returns). The DLQ is already wired for span/log/trace typed envelopes — route the failed log slice there instead of dropping it.

---

## LOW

### L-1 — MCP config negative values accepted without validation
**File:** `internal/config/config.go` (validation block, `MCP_MAX_CONCURRENT`, `MCP_CALL_TIMEOUT_MS`, `MCP_CACHE_TTL_MS`)

**Problem:** Negative values for the three MCP tunables are silently accepted by the config validation block and are treated as "disable" downstream. There is no documented contract for what negative means, and an operator who sets `MCP_MAX_CONCURRENT=-1` expecting a sensible default gets a fully open semaphore (no concurrency cap) without any log warning.

**Fix:** Add explicit range checks in the validation block (same pattern used for `HOT_RETENTION_DAYS`) and either reject negative values as invalid or clamp-and-warn:
```go
if cfg.MCPMaxConcurrent < 0 {
    slog.Warn("MCP_MAX_CONCURRENT < 0, treating as unlimited (no cap)")
}
```

---

### L-2 — `SetCallLimit()` replaces `callSlots` channel without draining
**File:** `internal/mcp/server.go` (`SetCallLimit`)

**Problem:** `SetCallLimit` creates a new channel and assigns it to `s.callSlots` while in-flight callers hold permits on the old channel. When those goroutines release (`<-s.callSlots`), they release to the now-GC-eligible old channel — the new channel is unaffected and starts empty. This means the new limit takes effect cleanly, but any in-flight calls that were counted against the old semaphore are not counted against the new one, so briefly up to `oldLimit + newLimit` concurrent calls can coexist.

**Root cause:** Live channel swap without a quiescing barrier.

**Impact:** Low — `SetCallLimit` is only called from `main.go` before the server begins serving. It becomes a problem only if it is ever called dynamically (e.g., a future live-reload path).

**Fix:** Document that `SetCallLimit` is startup-only and must not be called after `ServeHTTP` traffic starts. Alternatively, add a `sync.Mutex` guard and wait for in-flight slots to drain before swapping.

---

### L-3 — `LogsPartitioned()` flag is not synchronized
**File:** `internal/storage/repository.go:78, 82`

**Problem:** `logsPartitioned bool` is set once in `NewRepository` (or via `MarkLogsPartitioned` in `factory.go`) and read in `LogsPartitioned()` — all without any synchronization. Go's memory model requires explicit synchronization even for single-writer/single-reader boolean flags unless the write happens-before all reads via a channel or mutex.

**Root cause:** Plain bool field used as a concurrent flag.

**Fix:** Use `atomic.Bool` or ensure all callers of `LogsPartitioned()` are invoked after the startup sequence that writes the flag (a simple documented happens-before guarantee is sufficient if enforced).

---

## NOT BUGS (confirmed, closed)

| Item | Verdict |
|------|---------|
| `break` inside `select/default` in `server.go` tools/call handler | NOT a bug — explicit `if rpcErr != nil { break }` guard follows immediately |
| Goroutine leak in `runWithTimeout` | NOT a leak — bounded at `2 * maxConcurrent`; goroutine completes and slot is released |
| `retention.go` `totalRuns` channel math | Correct — `2 + logsExpected` properly accounts for the conditional logs goroutine |
| FTS5 UPDATE trigger two-step delete+insert | Correct per external-content FTS5 spec |
| `isQueueFull` gRPC error unwrapping | Correct — `grpcstatus.FromError` uses `errors.As` internally |
| `SetCallLimit` mid-flight channel swap | Only called pre-serving from `main.go`; low risk, documented above as L-2 |
| `pgLogsRelkind` partition detection | Correct — reads `pg_class.relkind = 'p'` from live schema rather than trusting config |
| LIKE query construction (`fmt.Sprintf` with `op`) | Not injectable — `op` is always `"LIKE"` or `"ILIKE"` (internal constant), user input only flows through GORM's parameterised `?` placeholders |
| Tenant isolation on all read paths | Correct — every `Repository` read method gates on `WHERE tenant_id = ?` with the context-derived tenant. Write paths stamp `TenantID` at parse time in `otlp.go`/`otlp_http.go` |
| `AutoMigrateModels` FTS5 gate | Correct — `fts5Available()` guards the virtual-table and trigger setup; FTS5 path only runs for SQLite |

---

## Summary

| Severity | Count | Items |
|----------|-------|-------|
| Critical | 0 | — |
| High | 1 | H-1 (droppedBatches data race) |
| Medium | 3 | M-1 (Submit TOCTOU), M-2 (ring/onIngest pointer read), M-3 (log loss on span failure) |
| Low | 3 | L-1 (MCP negative config), L-2 (SetCallLimit swap), L-3 (logsPartitioned bool sync) |

**Recommended immediate action:** Fix H-1 (`atomic.Int64`) and M-3 (decouple span/log error paths in `process()`) in the next patch. M-1 and M-2 are correctness issues that have not manifested in 269 tests but are real under concurrent load. L-1/L-2/L-3 are hardening items for the next sprint.
