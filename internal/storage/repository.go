package storage

import (
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/telemetry"
	"gorm.io/gorm"
)

// Repository wraps the GORM database handle for all data access operations.
type Repository struct {
	db      *gorm.DB
	driver  string
	metrics *telemetry.Metrics
}

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

	if err := AutoMigrateModels(db, driver); err != nil {
		return nil, err
	}

	// Register GORM Callback for DB Latency Metrics
	if metrics != nil {
		db.Callback().Query().Before("gorm:query").Register("telemetry:before_query", func(d *gorm.DB) {
			d.Set("telemetry:start_time", time.Now())
		})
		db.Callback().Query().After("gorm:query").Register("telemetry:after_query", func(d *gorm.DB) {
			if start, ok := d.Get("telemetry:start_time"); ok {
				duration := time.Since(start.(time.Time)).Seconds()
				metrics.ObserveDBLatency(duration)
			}
		})
		db.Callback().Create().Before("gorm:create").Register("telemetry:before_create", func(d *gorm.DB) {
			d.Set("telemetry:start_time", time.Now())
		})
		db.Callback().Create().After("gorm:create").Register("telemetry:after_create", func(d *gorm.DB) {
			if start, ok := d.Get("telemetry:start_time"); ok {
				duration := time.Since(start.(time.Time)).Seconds()
				metrics.ObserveDBLatency(duration)
			}
		})
	}

	return &Repository{db: db, driver: driver, metrics: metrics}, nil
}

// Stats aggregation and DB management

// GetStats returns high-level database stats.
func (r *Repository) GetStats() (map[string]interface{}, error) {
	var traceCount int64
	var logCount int64
	var errorCount int64

	if err := r.db.Model(&Trace{}).Count(&traceCount).Error; err != nil {
		return nil, fmt.Errorf("failed to count traces: %w", err)
	}

	if err := r.db.Model(&Log{}).Count(&logCount).Error; err != nil {
		return nil, fmt.Errorf("failed to count logs: %w", err)
	}

	if err := r.db.Model(&Log{}).Where("severity = ?", "ERROR").Count(&errorCount).Error; err != nil {
		return nil, fmt.Errorf("failed to count error logs: %w", err)
	}

	// Count distinct services across both logs and traces.
	var serviceNames []string
	r.db.Model(&Log{}).Distinct("service_name").Pluck("service_name", &serviceNames)
	traceServices := []string{}
	r.db.Model(&Trace{}).Distinct("service_name").Pluck("service_name", &traceServices)
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

	return map[string]interface{}{
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

// RecentTraces returns the most recent traces.
func (r *Repository) RecentTraces(limit int) ([]Trace, error) {
	var traces []Trace
	if err := r.db.Order("timestamp desc").Limit(limit).Find(&traces).Error; err != nil {
		return nil, err
	}
	return traces, nil
}

// RecentLogs returns the most recent logs.
func (r *Repository) RecentLogs(limit int) ([]Log, error) {
	var logs []Log
	if err := r.db.Order("timestamp desc").Limit(limit).Find(&logs).Error; err != nil {
		return nil, err
	}
	return logs, nil
}

// SearchLogs searches for logs based on query.
func (r *Repository) SearchLogs(query string, limit int) ([]Log, error) {
	var logs []Log
	db := r.db.Order("timestamp desc").Limit(limit)
	if query != "" {
		db = db.Where("body LIKE ? OR service_name LIKE ?", "%"+query+"%", "%"+query+"%")
	}
	if err := db.Find(&logs).Error; err != nil {
		return nil, err
	}
	return logs, nil
}

