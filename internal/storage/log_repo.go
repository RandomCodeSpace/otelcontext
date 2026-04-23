package storage

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"
	"gorm.io/gorm"
)

// ErrLogNotFoundOrWrongTenant is returned by UpdateLogInsight when the target
// row does not exist or belongs to a different tenant. Handlers should translate
// this to a 404 (to avoid confirming the existence of cross-tenant rows).
var ErrLogNotFoundOrWrongTenant = errors.New("log not found or not accessible by current tenant")

// LogFilter defines criteria for searching logs.
type LogFilter struct {
	ServiceName string
	Severity    string
	Search      string
	TraceID     string
	StartTime   time.Time
	EndTime     time.Time
	Limit       int
	Offset      int
}

// BatchCreateLogs inserts multiple logs in batches.
func (r *Repository) BatchCreateLogs(logs []Log) error {
	if len(logs) == 0 {
		return nil
	}
	if err := r.db.CreateInBatches(logs, 500).Error; err != nil {
		return fmt.Errorf("failed to batch create logs: %w", err)
	}
	return nil
}

// GetLog returns a single log by ID, scoped to the tenant on ctx.
func (r *Repository) GetLog(ctx context.Context, id uint) (*Log, error) {
	tenant := TenantFromContext(ctx)
	var l Log
	if err := r.db.WithContext(ctx).Where("tenant_id = ?", tenant).First(&l, id).Error; err != nil {
		return nil, fmt.Errorf("failed to get log: %w", err)
	}
	return &l, nil
}

// GetRecentLogs returns the most recent logs scoped to the tenant on ctx.
func (r *Repository) GetRecentLogs(ctx context.Context, limit int) ([]Log, error) {
	tenant := TenantFromContext(ctx)
	var logs []Log
	if err := r.db.WithContext(ctx).Where("tenant_id = ?", tenant).Order("timestamp desc").Limit(limit).Find(&logs).Error; err != nil {
		return nil, fmt.Errorf("failed to get recent logs: %w", err)
	}
	return logs, nil
}

// GetLogsV2 performs advanced filtering and search on logs scoped to the
// tenant on ctx. COUNT and SELECT run in parallel via errgroup for reduced latency.
func (r *Repository) GetLogsV2(ctx context.Context, filter LogFilter) ([]Log, int64, error) {
	tenant := TenantFromContext(ctx)
	var logs []Log
	var total int64

	base := r.db.WithContext(ctx).Model(&Log{}).Where("tenant_id = ?", tenant)

	if filter.ServiceName != "" {
		base = base.Where("service_name = ?", filter.ServiceName)
	}
	if filter.Severity != "" {
		base = base.Where("severity = ?", filter.Severity)
	}
	if filter.TraceID != "" {
		base = base.Where("trace_id = ?", filter.TraceID)
	}
	if !filter.StartTime.IsZero() {
		base = base.Where("timestamp >= ?", filter.StartTime)
	}
	if !filter.EndTime.IsZero() {
		base = base.Where("timestamp <= ?", filter.EndTime)
	}
	if filter.Search != "" {
		search := "%" + filter.Search + "%"
		op := r.likeOp()
		base = base.Where(fmt.Sprintf("body %s ? OR trace_id %s ?", op, op), search, search)
	}

	// Run COUNT and SELECT in parallel using independent sessions.
	var g errgroup.Group
	g.Go(func() error {
		return base.Session(&gorm.Session{}).Count(&total).Error
	})
	g.Go(func() error {
		return base.Session(&gorm.Session{}).
			Order("timestamp desc").
			Limit(filter.Limit).
			Offset(filter.Offset).
			Find(&logs).Error
	})
	if err := g.Wait(); err != nil {
		return nil, 0, fmt.Errorf("failed to fetch logs: %w", err)
	}

	return logs, total, nil
}

// GetLogContext returns logs surrounding a specific timestamp (+/- 1 minute),
// scoped to the tenant on ctx.
func (r *Repository) GetLogContext(ctx context.Context, targetTime time.Time) ([]Log, error) {
	tenant := TenantFromContext(ctx)
	start := targetTime.Add(-1 * time.Minute)
	end := targetTime.Add(1 * time.Minute)

	var logs []Log
	if err := r.db.WithContext(ctx).Where("tenant_id = ? AND timestamp BETWEEN ? AND ?", tenant, start, end).
		Order("timestamp asc").
		Find(&logs).Error; err != nil {
		return nil, fmt.Errorf("failed to fetch log context: %w", err)
	}
	return logs, nil
}

// UpdateLogInsight updates the AI insight for a specific log. The update is
// scoped to the tenant derived from ctx — a caller that attempts to update a
// log belonging to another tenant gets ErrLogNotFoundOrWrongTenant (IDOR fix).
func (r *Repository) UpdateLogInsight(ctx context.Context, logID uint, insight string) error {
	tenant := TenantFromContext(ctx)
	result := r.db.WithContext(ctx).
		Model(&Log{}).
		Where("id = ? AND tenant_id = ?", logID, tenant).
		Update("ai_insight", insight)
	if result.Error != nil {
		return fmt.Errorf("failed to update log insight: %w", result.Error)
	}
	if result.RowsAffected == 0 {
		return ErrLogNotFoundOrWrongTenant
	}
	return nil
}

// PurgeLogs deletes logs older than the given timestamp in a single statement.
// Suitable for SQLite; for Postgres at large retention volumes prefer PurgeLogsBatched.
func (r *Repository) PurgeLogs(olderThan time.Time) (int64, error) {
	result := r.db.Where("timestamp < ?", olderThan).Delete(&Log{})
	if result.Error != nil {
		return 0, fmt.Errorf("failed to purge logs: %w", result.Error)
	}
	slog.Info("Logs purged", "count", result.RowsAffected, "cutoff", olderThan)
	return result.RowsAffected, nil
}

// PurgeLogsBatched deletes logs in bounded chunks to avoid long locks and bloat on Postgres/MySQL.
// On SQLite it falls through to a single-statement delete.
//
// Tenant scope: this is a SYSTEM-WIDE retention operation and intentionally
// does NOT filter by tenant. All rows older than olderThan are purged across
// every tenant. Never expose this on a tenant-scoped API surface.
func (r *Repository) PurgeLogsBatched(ctx context.Context, olderThan time.Time, batchSize int, sleep time.Duration) (int64, error) {
	if batchSize <= 0 {
		batchSize = 10_000
	}
	driver := strings.ToLower(r.driver)
	if driver == "sqlite" || driver == "" {
		result := r.db.WithContext(ctx).Where("timestamp < ?", olderThan).Delete(&Log{})
		return result.RowsAffected, result.Error
	}

	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		result := r.db.WithContext(ctx).Exec(
			"DELETE FROM logs WHERE id IN (SELECT id FROM logs WHERE timestamp < ? ORDER BY id LIMIT ?)",
			olderThan, batchSize,
		)
		if result.Error != nil {
			return total, fmt.Errorf("batched purge logs: %w", result.Error)
		}
		total += result.RowsAffected
		if result.RowsAffected < int64(batchSize) {
			return total, nil
		}
		select {
		case <-ctx.Done():
			return total, ctx.Err()
		case <-time.After(sleep):
		}
	}
}
