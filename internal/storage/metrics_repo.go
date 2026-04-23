package storage

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"strings"
	"time"

	"gorm.io/gorm"
)

// sqliteP99RowCap is the maximum number of duration rows fetched for the
// in-memory p99 sort on SQLite. Queries returning more rows than this are
// capped with a warning; accuracy degrades gracefully at the tail.
const sqliteP99RowCap = 200_000

// TrafficPoint represents a data point for the traffic chart.
type TrafficPoint struct {
	Timestamp  time.Time `json:"timestamp"`
	Count      int64     `json:"count"`
	ErrorCount int64     `json:"error_count"`
}

// LatencyPoint represents a data point for the latency heatmap.
type LatencyPoint struct {
	Timestamp time.Time `json:"timestamp"`
	Duration  int64     `json:"duration"` // Microseconds
}

// ServiceError represents error counts per service.
type ServiceError struct {
	ServiceName string  `json:"service_name"`
	ErrorCount  int64   `json:"error_count"`
	TotalCount  int64   `json:"total_count"`
	ErrorRate   float64 `json:"error_rate"`
}

// DashboardStats represents aggregated metrics for the dashboard.
type DashboardStats struct {
	TotalTraces        int64          `json:"total_traces"`
	TotalLogs          int64          `json:"total_logs"`
	TotalErrors        int64          `json:"total_errors"`
	AvgLatencyMs       float64        `json:"avg_latency_ms"`
	ErrorRate          float64        `json:"error_rate"`
	ActiveServices     int64          `json:"active_services"`
	P99Latency         int64          `json:"p99_latency"`
	TopFailingServices []ServiceError `json:"top_failing_services"`
}

// BatchCreateMetrics inserts aggregated metrics in batches.
func (r *Repository) BatchCreateMetrics(buckets []MetricBucket) error {
	if len(buckets) == 0 {
		return nil
	}
	if err := r.db.CreateInBatches(buckets, 500).Error; err != nil {
		return fmt.Errorf("failed to batch create metrics: %w", err)
	}
	return nil
}

// GetMetricBuckets returns aggregated metrics for a specific time range and service,
// scoped to the tenant on ctx.
func (r *Repository) GetMetricBuckets(ctx context.Context, start, end time.Time, serviceName string, metricName string) ([]MetricBucket, error) {
	tenant := TenantFromContext(ctx)
	var buckets []MetricBucket
	query := r.db.WithContext(ctx).Where("tenant_id = ? AND time_bucket BETWEEN ? AND ?", tenant, start, end)
	if serviceName != "" {
		query = query.Where("service_name = ?", serviceName)
	}
	if metricName != "" {
		query = query.Where("name = ?", metricName)
	}
	if err := query.Order("time_bucket ASC").Find(&buckets).Error; err != nil {
		return nil, fmt.Errorf("failed to get metric buckets: %w", err)
	}
	return buckets, nil
}

// GetMetricNames returns a list of distinct metric names for the tenant on ctx,
// optionally filtered by service.
func (r *Repository) GetMetricNames(ctx context.Context, serviceName string) ([]string, error) {
	tenant := TenantFromContext(ctx)
	var names []string
	query := r.db.WithContext(ctx).Model(&MetricBucket{}).Where("tenant_id = ?", tenant)
	if serviceName != "" {
		query = query.Where("service_name = ?", serviceName)
	}
	if err := query.Distinct("name").Order("name ASC").Pluck("name", &names).Error; err != nil {
		return nil, fmt.Errorf("failed to get metric names: %w", err)
	}
	return names, nil
}

// p99DurationForQuery computes the p99 latency (in microseconds) from the
// matching rows of session. It dispatches on r.driver:
//
//   - postgres / postgresql: native percentile_disc aggregate (single query).
//   - mysql: two-query COUNT + ORDER BY … OFFSET approach.
//   - default (sqlite + unknown): in-memory sort capped at sqliteP99RowCap rows.
//
// The caller must pass a fresh Session so nothing leaks across subsequent calls.
// ctx is threaded into every sub-session so client cancellation (disconnect/timeout)
// is honoured at the driver level.
func (r *Repository) p99DurationForQuery(ctx context.Context, session *gorm.DB) (int64, error) {
	switch strings.ToLower(r.driver) {
	case "postgres", "postgresql":
		// Use Rows() (not Row()) so we can explicitly Close the underlying
		// *sql.Rows — otherwise the connection leaks on sustained traffic.
		rows, err := session.Session(&gorm.Session{Context: ctx}).Select("COALESCE(percentile_disc(0.99) WITHIN GROUP (ORDER BY duration), 0)::bigint").Rows()
		if err != nil {
			return 0, err
		}
		defer rows.Close()
		var p int64
		if rows.Next() {
			if err := rows.Scan(&p); err != nil {
				return 0, err
			}
		}
		if err := rows.Err(); err != nil {
			return 0, err
		}
		return p, nil

	case "mysql":
		var n int64
		if err := session.Session(&gorm.Session{Context: ctx}).Model(&Trace{}).Count(&n).Error; err != nil {
			return 0, err
		}
		if n == 0 {
			return 0, nil
		}
		offset := int64(math.Ceil(float64(n)*0.99)) - 1
		if offset < 0 {
			offset = 0
		} else if offset >= n {
			offset = n - 1
		}
		var p int64
		if err := session.Session(&gorm.Session{Context: ctx}).Select("duration").Order("duration ASC").Offset(int(offset)).Limit(1).Scan(&p).Error; err != nil {
			return 0, err
		}
		return p, nil

	default: // sqlite and any unknown driver
		var durations []int64
		q := session.Session(&gorm.Session{Context: ctx}).Select("duration").Order("duration ASC").Limit(sqliteP99RowCap + 1)
		if err := q.Find(&durations).Error; err != nil {
			return 0, err
		}
		if len(durations) == 0 {
			return 0, nil
		}
		if len(durations) > sqliteP99RowCap {
			// Truncate to cap — accuracy degrades gracefully. Operators alert on
			// the counter (dataset is too large for in-memory p99 — migrate to
			// Postgres). Keep a low-volume debug log for dev observability.
			if r.metrics != nil {
				r.metrics.DashboardP99RowCapHitsTotal.Inc()
			}
			slog.Debug("p99 SQLite fallback capped rows", "cap", sqliteP99RowCap)
			durations = durations[:sqliteP99RowCap]
		}
		idx := int(math.Ceil(float64(len(durations))*0.99)) - 1
		if idx < 0 {
			idx = 0
		} else if idx >= len(durations) {
			idx = len(durations) - 1
		}
		return durations[idx], nil
	}
}

// GetDashboardStats calculates high-level metrics for the dashboard, scoped to
// the tenant on ctx.
func (r *Repository) GetDashboardStats(ctx context.Context, start, end time.Time, serviceNames []string) (*DashboardStats, error) {
	tenant := TenantFromContext(ctx)
	var stats DashboardStats

	baseQuery := r.db.WithContext(ctx).Model(&Trace{}).Where("tenant_id = ? AND timestamp BETWEEN ? AND ?", tenant, start, end)
	if len(serviceNames) > 0 {
		baseQuery = baseQuery.Where("service_name IN ?", serviceNames)
	}

	// 1. Total Traces
	if err := baseQuery.Session(&gorm.Session{}).Count(&stats.TotalTraces).Error; err != nil {
		return nil, fmt.Errorf("failed to count traces: %w", err)
	}

	// 2. Total Logs
	logQuery := r.db.WithContext(ctx).Model(&Log{}).Where("tenant_id = ? AND timestamp BETWEEN ? AND ?", tenant, start, end)
	if len(serviceNames) > 0 {
		logQuery = logQuery.Where("service_name IN ?", serviceNames)
	}
	if err := logQuery.Count(&stats.TotalLogs).Error; err != nil {
		return nil, fmt.Errorf("failed to count logs: %w", err)
	}

	// 3. Total Errors (traces with error status)
	op := r.likeOp()
	if err := baseQuery.Session(&gorm.Session{}).
		Where(fmt.Sprintf("status %s ?", op), "%ERROR%").
		Count(&stats.TotalErrors).Error; err != nil {
		return nil, fmt.Errorf("failed to count error traces: %w", err)
	}

	if stats.TotalTraces > 0 {
		stats.ErrorRate = (float64(stats.TotalErrors) / float64(stats.TotalTraces)) * 100
	}

	// 4. Average Latency (microseconds → milliseconds)
	type avgResult struct {
		Avg float64
	}
	var avg avgResult
	if err := baseQuery.Session(&gorm.Session{}).
		Select("COALESCE(AVG(duration), 0) as avg").
		Scan(&avg).Error; err != nil {
		slog.Warn("Failed to compute average latency", "error", err)
	} else {
		stats.AvgLatencyMs = avg.Avg / 1000.0 // microseconds → ms
	}

	// 5. Active Services
	if err := baseQuery.Session(&gorm.Session{}).
		Distinct("service_name").
		Count(&stats.ActiveServices).Error; err != nil {
		return nil, fmt.Errorf("failed to count active services: %w", err)
	}

	// 6. P99 Latency
	p99, err := r.p99DurationForQuery(ctx, baseQuery.Session(&gorm.Session{}))
	if err != nil {
		return nil, fmt.Errorf("failed to compute p99 latency: %w", err)
	}
	stats.P99Latency = p99

	// 7. Top Failing Services
	type svcCount struct {
		ServiceName string
		ErrorCount  int64
		TotalCount  int64
	}
	var svcCounts []svcCount
	if err := baseQuery.Session(&gorm.Session{}).
		Select(fmt.Sprintf("service_name, COUNT(*) as total_count, SUM(CASE WHEN status %s '%%ERROR%%' THEN 1 ELSE 0 END) as error_count", op)).
		Group("service_name").
		Having("error_count > 0").
		Order("error_count DESC").
		Limit(5).
		Scan(&svcCounts).Error; err != nil {
		slog.Warn("Failed to fetch top failing services", "error", err)
	} else {
		for _, sc := range svcCounts {
			rate := 0.0
			if sc.TotalCount > 0 {
				rate = float64(sc.ErrorCount) / float64(sc.TotalCount)
			}
			stats.TopFailingServices = append(stats.TopFailingServices, ServiceError{
				ServiceName: sc.ServiceName,
				ErrorCount:  sc.ErrorCount,
				TotalCount:  sc.TotalCount,
				ErrorRate:   rate,
			})
		}
	}

	return &stats, nil
}

// GetTrafficMetrics returns request counts bucketed by minute (including error
// counts), scoped to the tenant on ctx.
func (r *Repository) GetTrafficMetrics(ctx context.Context, start, end time.Time, serviceNames []string) ([]TrafficPoint, error) {
	tenant := TenantFromContext(ctx)
	var points []TrafficPoint

	type traceRow struct {
		Timestamp time.Time
		Status    string
	}
	var rows []traceRow

	query := r.db.WithContext(ctx).Model(&Trace{}).
		Select("timestamp, status").
		Where("tenant_id = ? AND timestamp BETWEEN ? AND ?", tenant, start, end)

	if len(serviceNames) > 0 {
		query = query.Where("service_name IN ?", serviceNames)
	}

	if err := query.Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("failed to fetch traffic rows: %w", err)
	}

	type bucket struct {
		count      int64
		errorCount int64
	}
	buckets := make(map[int64]*bucket)
	for _, r := range rows {
		ts := r.Timestamp.Truncate(time.Minute).Unix()
		b, ok := buckets[ts]
		if !ok {
			b = &bucket{}
			buckets[ts] = b
		}
		b.count++
		if strings.Contains(strings.ToUpper(r.Status), "ERROR") {
			b.errorCount++
		}
	}

	for ts, b := range buckets {
		points = append(points, TrafficPoint{
			Timestamp:  time.Unix(ts, 0),
			Count:      b.count,
			ErrorCount: b.errorCount,
		})
	}

	sort.Slice(points, func(i, j int) bool {
		return points[i].Timestamp.Before(points[j].Timestamp)
	})

	return points, nil
}

// GetLatencyHeatmap returns trace duration and timestamps for heatmap rendering,
// scoped to the tenant on ctx.
func (r *Repository) GetLatencyHeatmap(ctx context.Context, start, end time.Time, serviceNames []string) ([]LatencyPoint, error) {
	tenant := TenantFromContext(ctx)
	var points []LatencyPoint
	query := r.db.WithContext(ctx).Model(&Trace{}).
		Select("timestamp, duration").
		Where("tenant_id = ? AND timestamp BETWEEN ? AND ?", tenant, start, end)

	if len(serviceNames) > 0 {
		query = query.Where("service_name IN ?", serviceNames)
	}

	if err := query.Order("timestamp DESC").Limit(2000).Find(&points).Error; err != nil {
		return nil, fmt.Errorf("failed to get latency heatmap: %w", err)
	}
	return points, nil
}

// PurgeMetricBucketsBatched deletes metric buckets older than the given timestamp in bounded chunks.
//
// Tenant scope: this is a SYSTEM-WIDE retention operation and intentionally
// does NOT filter by tenant. Rows are deleted across every tenant. Never expose
// this on a tenant-scoped API surface.
func (r *Repository) PurgeMetricBucketsBatched(ctx context.Context, olderThan time.Time, batchSize int, sleep time.Duration) (int64, error) {
	if batchSize <= 0 {
		batchSize = 10_000
	}
	driver := strings.ToLower(r.driver)
	if driver == "sqlite" || driver == "" {
		result := r.db.WithContext(ctx).Where("time_bucket < ?", olderThan).Delete(&MetricBucket{})
		return result.RowsAffected, result.Error
	}

	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		result := r.db.WithContext(ctx).Exec(
			"DELETE FROM metric_buckets WHERE id IN (SELECT id FROM metric_buckets WHERE time_bucket < ? ORDER BY id LIMIT ?)",
			olderThan, batchSize,
		)
		if result.Error != nil {
			return total, fmt.Errorf("batched purge metric_buckets: %w", result.Error)
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

// GetServices returns a list of all distinct service names seen in traces for
// the tenant on ctx.
func (r *Repository) GetServices(ctx context.Context) ([]string, error) {
	tenant := TenantFromContext(ctx)
	var services []string
	if err := r.db.WithContext(ctx).Model(&Trace{}).
		Where("tenant_id = ?", tenant).
		Distinct("service_name").
		Order("service_name ASC").
		Pluck("service_name", &services).Error; err != nil {
		return nil, fmt.Errorf("failed to get services: %w", err)
	}
	return services, nil
}
