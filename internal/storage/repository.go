package storage

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/telemetry"
	"gorm.io/gorm"
)

// partitionLookaheadFromEnv reads DB_PARTITION_LOOKAHEAD_DAYS, defaulting to
// 3 when unset, malformed, or out of the validation range. Validation also
// happens at the config layer; this fallback keeps the storage package
// usable when wired without a *config.Config (tests, embedded callers).
func partitionLookaheadFromEnv() int {
	v := strings.TrimSpace(os.Getenv("DB_PARTITION_LOOKAHEAD_DAYS"))
	if v == "" {
		return 3
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 || n > 365 {
		return 3
	}
	return n
}

// likeOpFor returns the case-insensitive LIKE operator for the given driver.
// Postgres LIKE is case-sensitive; SQLite/MySQL LIKE is case-insensitive by default.
// Callers should embed the returned token directly into SQL fragments.
func likeOpFor(driver string) string {
	switch strings.ToLower(driver) {
	case "postgres", "postgresql":
		return "ILIKE"
	default:
		return "LIKE"
	}
}

// likeOp returns the case-insensitive LIKE operator for this Repository's dialect.
func (r *Repository) likeOp() string { return likeOpFor(r.driver) }

// autoMigrateEnabled reports whether GORM AutoMigrate should run on startup.
// Defaults to true; set DB_AUTOMIGRATE=false to run versioned migrations externally.
func autoMigrateEnabled() bool {
	v, ok := os.LookupEnv("DB_AUTOMIGRATE")
	if !ok {
		return true
	}
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "0", "false", "no", "off":
		return false
	default:
		return true
	}
}

// Repository wraps the GORM database handle for all data access operations.
type Repository struct {
	db      *gorm.DB
	driver  string
	metrics *telemetry.Metrics

	// logsPartitioned is set to true when DB_POSTGRES_PARTITIONING=daily is
	// active and the `logs` parent has been provisioned as a partitioned
	// table. RetentionScheduler reads this to skip the logs DELETE — the
	// PartitionScheduler does retention via DROP PARTITION instead.
	//
	// Stored as atomic.Bool: written once during NewRepository (before any
	// goroutine reads it) and read by retention.go from a different
	// goroutine. atomic.Bool removes the memory-model fragility of a plain
	// bool that "works because the writer ran first" — no test catches a
	// torn read on amd64, but the contract is brittle.
	logsPartitioned atomic.Bool
}

// LogsPartitioned reports whether the `logs` table is provisioned as a
// declarative partitioned parent. Used by RetentionScheduler to bypass the
// row-level DELETE path when partition-level DROP is in charge of retention.
func (r *Repository) LogsPartitioned() bool { return r.logsPartitioned.Load() }

// MarkLogsPartitioned flips the partitioned flag. Called by the partitioning
// setup path (factory.go) once the partitioned schema is in place.
func (r *Repository) MarkLogsPartitioned() { r.logsPartitioned.Store(true) }

// NewRepository initializes the database connection using environment variables and migrates the schema.
func NewRepository(metrics *telemetry.Metrics) (*Repository, error) {
	driver := os.Getenv("DB_DRIVER")
	dsn := os.Getenv("DB_DSN")

	db, err := NewDatabase(driver, dsn)
	if err != nil {
		return nil, err
	}

	// Resolve effective driver name
	if driver == "" {
		driver = "sqlite"
	}

	// Auto-migration is enabled by default. Disable via DB_AUTOMIGRATE=false when
	// using versioned migrations in production (Postgres table locks, no rollback).
	if autoMigrateEnabled() {
		opts := MigrateOptions{
			PostgresPartitioning:   strings.ToLower(strings.TrimSpace(os.Getenv("DB_POSTGRES_PARTITIONING"))),
			PartitionLookaheadDays: partitionLookaheadFromEnv(),
		}
		if err := AutoMigrateModelsWithOptions(db, driver, opts); err != nil {
			return nil, err
		}
	} else {
		slog.Info("AutoMigrate skipped (DB_AUTOMIGRATE=false)")
	}

	// Register GORM Callback for DB Latency Metrics
	if metrics != nil {
		_ = db.Callback().Query().Before("gorm:query").Register("telemetry:before_query", func(d *gorm.DB) {
			d.Set("telemetry:start_time", time.Now())
		})
		_ = db.Callback().Query().After("gorm:query").Register("telemetry:after_query", func(d *gorm.DB) {
			if start, ok := d.Get("telemetry:start_time"); ok {
				duration := time.Since(start.(time.Time)).Seconds()
				metrics.ObserveDBLatency(duration)
			}
		})
		_ = db.Callback().Create().Before("gorm:create").Register("telemetry:before_create", func(d *gorm.DB) {
			d.Set("telemetry:start_time", time.Now())
		})
		_ = db.Callback().Create().After("gorm:create").Register("telemetry:after_create", func(d *gorm.DB) {
			if start, ok := d.Get("telemetry:start_time"); ok {
				duration := time.Since(start.(time.Time)).Seconds()
				metrics.ObserveDBLatency(duration)
			}
		})
	}

	repo := &Repository{db: db, driver: driver, metrics: metrics}
	// Detect partitioned-logs mode from the live schema so the
	// RetentionScheduler can skip the row-level DELETE path. We do this from
	// the DB rather than passing the config flag through several layers,
	// which keeps the storage package config-agnostic and resilient to a
	// half-applied migration.
	if driver == "postgres" || driver == "postgresql" {
		if rk, err := pgLogsRelkind(db); err == nil && rk == "p" {
			repo.logsPartitioned.Store(true)
			slog.Info("📦 Postgres: logs is partitioned — retention will use DROP PARTITION (via PartitionScheduler)")
		}
	}
	return repo, nil
}

// Stats aggregation and DB management

// GetStats returns high-level database stats scoped to the tenant carried on ctx.
// Unscoped aggregates (DB size, etc.) are not tenant-specific and are reported as-is.
func (r *Repository) GetStats(ctx context.Context) (map[string]any, error) {
	tenant := TenantFromContext(ctx)
	db := r.db.WithContext(ctx)

	var traceCount int64
	var logCount int64
	var errorCount int64

	if err := db.Model(&Trace{}).Where("tenant_id = ?", tenant).Count(&traceCount).Error; err != nil {
		return nil, fmt.Errorf("failed to count traces: %w", err)
	}

	if err := db.Model(&Log{}).Where("tenant_id = ?", tenant).Count(&logCount).Error; err != nil {
		return nil, fmt.Errorf("failed to count logs: %w", err)
	}

	if err := db.Model(&Log{}).Where("tenant_id = ? AND severity = ?", tenant, "ERROR").Count(&errorCount).Error; err != nil {
		return nil, fmt.Errorf("failed to count error logs: %w", err)
	}

	// Count distinct services across both logs and traces (tenant-scoped).
	var serviceNames []string
	db.Model(&Log{}).Where("tenant_id = ?", tenant).Distinct("service_name").Pluck("service_name", &serviceNames)
	traceServices := []string{}
	db.Model(&Trace{}).Where("tenant_id = ?", tenant).Distinct("service_name").Pluck("service_name", &traceServices)
	serviceSet := make(map[string]struct{}, len(serviceNames)+len(traceServices))
	for _, s := range serviceNames {
		if s != "" {
			serviceSet[s] = struct{}{}
		}
	}
	for _, s := range traceServices {
		if s != "" {
			serviceSet[s] = struct{}{}
		}
	}

	// Estimate DB size (SQLite only; 0 for other drivers).
	var dbSizeMB float64
	if r.driver == "sqlite" {
		var pageCount, pageSize int64
		r.db.Raw("PRAGMA page_count").Scan(&pageCount)
		r.db.Raw("PRAGMA page_size").Scan(&pageSize)
		dbSizeMB = float64(pageCount*pageSize) / (1024 * 1024)
	}

	return map[string]any{
		"LogCount":     logCount,
		"TraceCount":   traceCount,
		"ErrorCount":   errorCount,
		"ServiceCount": len(serviceSet),
		"DBSizeMB":     fmt.Sprintf("%.1f", dbSizeMB),
		// Legacy snake_case keys kept for any existing API consumers.
		"trace_count": traceCount,
		"error_count": errorCount,
	}, nil
}

// VacuumDB runs VACUUM on the database (SQLite only, no-op for others).
func (r *Repository) VacuumDB() error {
	if r.driver == "sqlite" {
		if err := r.db.Exec("VACUUM").Error; err != nil {
			return fmt.Errorf("failed to vacuum database: %w", err)
		}
		slog.Info("Database vacuumed successfully")
	} else {
		slog.Debug("Vacuum skipped", "driver", r.driver, "reason", "only applicable to SQLite")
	}
	return nil
}

// Close closes the underlying database connection.
func (r *Repository) Close() error {
	sqlDB, err := r.db.DB()
	if err != nil {
		return fmt.Errorf("failed to get underlying sql.DB: %w", err)
	}
	return sqlDB.Close()
}

// DB returns the underlying gorm.DB for advanced queries.
func (r *Repository) DB() *gorm.DB {
	return r.db
}

// NewRepositoryFromDB constructs a Repository from an existing *gorm.DB.
// Intended for tests and advanced wiring — production code should use NewRepository.
func NewRepositoryFromDB(db *gorm.DB, driver string) *Repository {
	if driver == "" {
		driver = "sqlite"
	}
	return &Repository{db: db, driver: driver}
}

// RecentTraces returns the most recent traces scoped to the tenant carried on ctx.
func (r *Repository) RecentTraces(ctx context.Context, limit int) ([]Trace, error) {
	tenant := TenantFromContext(ctx)
	var traces []Trace
	if err := r.db.WithContext(ctx).Where("tenant_id = ?", tenant).Order("timestamp desc").Limit(limit).Find(&traces).Error; err != nil {
		return nil, err
	}
	return traces, nil
}

// RecentLogs returns the most recent logs scoped to the tenant carried on ctx.
func (r *Repository) RecentLogs(ctx context.Context, limit int) ([]Log, error) {
	tenant := TenantFromContext(ctx)
	var logs []Log
	if err := r.db.WithContext(ctx).Where("tenant_id = ?", tenant).Order("timestamp desc").Limit(limit).Find(&logs).Error; err != nil {
		return nil, err
	}
	return logs, nil
}

// SearchLogs searches for logs based on query, scoped to the tenant carried on ctx.
//
// On SQLite the search routes through the FTS5 virtual table (`logs_fts`) and
// orders by BM25 score for relevance. On Postgres / MySQL / others it falls
// back to LIKE/ILIKE against logs.body and logs.service_name (Postgres uses
// the pg_trgm GIN indexes built in AutoMigrateModels).
func (r *Repository) SearchLogs(ctx context.Context, query string, limit int) ([]Log, error) {
	tenant := TenantFromContext(ctx)
	if query != "" && fts5Available(r.driver) {
		return r.searchLogsFTS5(ctx, tenant, query, limit)
	}
	var logs []Log
	db := r.db.WithContext(ctx).Where("tenant_id = ?", tenant).Order("timestamp desc").Limit(limit)
	if query != "" {
		op := r.likeOp()
		db = db.Where(fmt.Sprintf("body %s ? OR service_name %s ?", op, op), "%"+query+"%", "%"+query+"%")
	}
	if err := db.Find(&logs).Error; err != nil {
		return nil, err
	}
	return logs, nil
}

// searchLogsFTS5 runs the FTS5 + BM25 path. The MATCH expression is built via
// fts5MatchExpr to keep user input safe from FTS5 query syntax. Results are
// returned ordered by BM25 score (lower = more relevant in SQLite's
// implementation, which returns negative scores).
func (r *Repository) searchLogsFTS5(ctx context.Context, tenant, query string, limit int) ([]Log, error) {
	matchExpr := fts5MatchExpr(query)
	if matchExpr == "" {
		var logs []Log
		err := r.db.WithContext(ctx).Where("tenant_id = ?", tenant).Order("timestamp desc").Limit(limit).Find(&logs).Error
		return logs, err
	}
	var logs []Log
	err := r.db.WithContext(ctx).
		Table("logs").
		Joins("JOIN "+fts5LogsTable+" ON logs.id = "+fts5LogsTable+".rowid").
		Where("logs.tenant_id = ? AND "+fts5LogsTable+" MATCH ?", tenant, matchExpr).
		Order("bm25(" + fts5LogsTable + ") ASC").
		Limit(limit).
		Find(&logs).Error
	if err != nil {
		// FTS5 setup is provisioned at migrate-time and the trigger keeps
		// it in sync — a query error here means something genuinely went
		// wrong (corrupt index, missing table on a half-migrated DB). We
		// fall back to LIKE so the API stays available, but log loudly
		// so the operator notices and can rebuild the index. CLAUDE.md
		// "fix root cause" rule: the fallback is the seatbelt, the log is
		// the dashboard light.
		slog.Warn("FTS5 search failed, falling back to LIKE", "tenant", tenant, "query", query, "error", err)
		return r.searchLogsLikeFallback(ctx, tenant, query, limit)
	}
	return logs, nil
}

func (r *Repository) searchLogsLikeFallback(ctx context.Context, tenant, query string, limit int) ([]Log, error) {
	var logs []Log
	op := r.likeOp()
	err := r.db.WithContext(ctx).
		Where("tenant_id = ?", tenant).
		Where(fmt.Sprintf("body %s ? OR service_name %s ?", op, op), "%"+query+"%", "%"+query+"%").
		Order("timestamp desc").
		Limit(limit).
		Find(&logs).Error
	return logs, err
}
