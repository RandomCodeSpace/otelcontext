package storage

import (
	"context"
	"fmt"
	"time"

	"gorm.io/gorm"
)

// Exemplar-tier retention (#201 Q2).
//
// In AGGREGATE_MODE=aggregate the raw rows in traces/spans/logs are not the
// dataset — they are EXEMPLARS attached to a dataset that lives in
// aggregate.db. The aggregate tier keeps seven days because it is what the
// dashboards read; the exemplar tier keeps two, because 576 five-minute
// windows at the 3 MiB global window budget is 1.69 GiB of charged payload,
// and at the provisional 2x database/index/FTS amplification that is 3.38 GiB
// of the 4.5 GiB main relational tier. Seven days of the same rate is not a
// tighter fit, it is a full volume.
//
// This purge is separate from the HOT_RETENTION_DAYS purge and runs BEFORE it
// on the same hourly tick, so the cheap deletes happen while the volume still
// has room to write the journal for them.
//
// Transactionality: each pass deletes a bounded batch of traces AND the spans
// left dangling by that batch inside ONE transaction. A reader therefore never
// observes spans whose trace row has already gone — the partial state the
// old two-phase sweep exposed between its trace loop and its span loop.
// Batching keeps the SQLite writer lock held for a chunk, not for the whole
// multi-GB purge.

// ExemplarPurgeStats reports what one exemplar-tier purge removed.
type ExemplarPurgeStats struct {
	Traces      int64
	Spans       int64
	Logs        int64
	OrphanSpans int64
}

// Total is every row the pass deleted.
func (s ExemplarPurgeStats) Total() int64 {
	return s.Traces + s.Spans + s.Logs + s.OrphanSpans
}

// PurgeExemplarsBatched deletes exemplar traces, their spans, exemplar logs
// and expired weak references (spans whose trace row no longer exists) older
// than cutoff.
//
// FTS: `logs_fts` is content-linked to `logs` and kept in sync by the
// AFTER DELETE trigger created in fts5.go, so deleting a log row inside the
// transaction removes its index entry inside the same transaction. There is
// no separate FTS delete to get wrong.
//
// Tenant scope: SYSTEM-WIDE, like every other retention path. It scopes by
// age, never by tenant. Never expose it on a tenant-scoped API surface.
func (r *Repository) PurgeExemplarsBatched(ctx context.Context, cutoff time.Time, batchSize int, sleep time.Duration) (ExemplarPurgeStats, error) {
	var stats ExemplarPurgeStats
	if batchSize <= 0 {
		batchSize = 10_000
	}

	// Phase 1: traces + the spans that batch orphans, one transaction per batch.
	for {
		if err := ctx.Err(); err != nil {
			return stats, err
		}
		var traces, spans int64
		err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			res := tx.Exec(
				"DELETE FROM traces WHERE id IN (SELECT id FROM traces WHERE timestamp < ? ORDER BY id LIMIT ?)",
				cutoff, batchSize,
			)
			if res.Error != nil {
				return fmt.Errorf("purge exemplar traces: %w", res.Error)
			}
			traces = res.RowsAffected

			// Same transaction: the spans this batch just detached. Bounded to
			// start_time < cutoff so a span whose trace row has not committed
			// yet (Export writes traces and spans separately) is never a
			// candidate.
			res = tx.Exec(
				"DELETE FROM spans WHERE id IN (SELECT id FROM spans WHERE start_time < ? AND trace_id NOT IN (SELECT trace_id FROM traces) ORDER BY id LIMIT ?)",
				cutoff, batchSize,
			)
			if res.Error != nil {
				return fmt.Errorf("purge exemplar spans: %w", res.Error)
			}
			spans = res.RowsAffected
			return nil
		})
		if err != nil {
			return stats, err
		}
		stats.Traces += traces
		stats.Spans += spans
		if traces < int64(batchSize) && spans < int64(batchSize) {
			break
		}
		if err := yield(ctx, sleep); err != nil {
			return stats, err
		}
	}

	// Phase 2: exemplar logs. Their FTS rows go with them via the trigger.
	for {
		if err := ctx.Err(); err != nil {
			return stats, err
		}
		var logs int64
		err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			res := tx.Exec(
				"DELETE FROM logs WHERE id IN (SELECT id FROM logs WHERE timestamp < ? ORDER BY id LIMIT ?)",
				cutoff, batchSize,
			)
			if res.Error != nil {
				return fmt.Errorf("purge exemplar logs: %w", res.Error)
			}
			logs = res.RowsAffected
			return nil
		})
		if err != nil {
			return stats, err
		}
		stats.Logs += logs
		if logs < int64(batchSize) {
			break
		}
		if err := yield(ctx, sleep); err != nil {
			return stats, err
		}
	}

	// Phase 3: expired weak references left by ANY earlier purge — spans older
	// than the cutoff whose trace row is gone. Phase 1 catches the ones this
	// pass created; this catches the residue of a pass that was cancelled or
	// crashed halfway.
	for {
		if err := ctx.Err(); err != nil {
			return stats, err
		}
		res := r.db.WithContext(ctx).Exec(
			"DELETE FROM spans WHERE id IN (SELECT id FROM spans WHERE start_time < ? AND trace_id NOT IN (SELECT trace_id FROM traces) ORDER BY id LIMIT ?)",
			cutoff, batchSize,
		)
		if res.Error != nil {
			return stats, fmt.Errorf("sweep expired weak references: %w", res.Error)
		}
		stats.OrphanSpans += res.RowsAffected
		if res.RowsAffected < int64(batchSize) {
			break
		}
		if err := yield(ctx, sleep); err != nil {
			return stats, err
		}
	}

	return stats, nil
}

// yield pauses between purge batches so the single SQLite writer lock is
// actually released rather than merely re-acquired.
func yield(ctx context.Context, sleep time.Duration) error {
	if sleep <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(sleep)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// CheckpointWAL truncates the write-ahead log. Called when the disk watchdog
// enters raw-off: at 95% the WAL is a live claim on the volume, and the pages
// it holds are pages the purge just freed but cannot hand back until the log
// is checkpointed. No-op on non-SQLite drivers.
func (r *Repository) CheckpointWAL(ctx context.Context) error {
	if r.driver != "sqlite" && r.driver != "" {
		return nil
	}
	sqlDB, err := r.db.DB()
	if err != nil {
		return fmt.Errorf("checkpoint wal: %w", err)
	}
	return drainQuery(ctx, sqlDB, "PRAGMA wal_checkpoint(TRUNCATE)")
}
