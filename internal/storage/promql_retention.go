package storage

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// PurgePromQLBatched is system-wide retention, never a tenant API. Series are
// removed only after their newest sample expires. Export transactions update
// last_timestamp monotonically before inserting samples.
func (r *Repository) PurgePromQLBatched(ctx context.Context, cutoff time.Time, batchSize int, pause time.Duration) (int64, error) {
	switch strings.ToLower(r.driver) {
	case "", "sqlite", "postgres", "postgresql":
	default:
		return 0, nil // These drivers cannot enable the local backend.
	}
	if batchSize <= 0 {
		batchSize = 1000
	}
	var total int64
	for _, item := range []struct{ table, predicate string }{
		{"promql_samples", "timestamp < ?"},
		{"promql_series", "last_timestamp < ?"},
	} {
		for {
			if err := ctx.Err(); err != nil {
				return total, err
			}
			result := r.db.WithContext(ctx).Exec(batchedDeleteSQL(r.driver, item.table, item.predicate), cutoff.UnixMilli(), batchSize)
			if result.Error != nil {
				return total, fmt.Errorf("purge %s: %w", item.table, result.Error)
			}
			total += result.RowsAffected
			if result.RowsAffected < int64(batchSize) {
				break
			}
			select {
			case <-ctx.Done():
				return total, ctx.Err()
			case <-time.After(pause):
			}
		}
	}
	return total, nil
}
