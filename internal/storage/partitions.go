package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"gorm.io/gorm"
)

// Postgres declarative partitioning for high-volume tables.
//
// Phase 3b restricts partitioning to the `logs` table — the only table whose
// retention purge dominates DB time at 7+ days × 100–200 services. Future
// phases can extend the same pattern to `traces` and `metric_buckets` if/when
// their retention costs justify the extra schema complexity.
//
// Design choices:
//
//   - Daily range partitions on `timestamp`. Daily granularity gives us
//     DROP PARTITION as a near-instant retention path (vs. row-by-row
//     DELETE) while keeping partition count bounded at ~retention + a few
//     days of lookahead — well below Postgres' soft ceiling for
//     query-planning overhead.
//
//   - Composite primary key (id, timestamp). Postgres requires the
//     partition key to be in every unique/PK constraint of a partitioned
//     table. `id` alone would error at DDL time.
//
//   - Indexes are created on the partitioned PARENT and propagate to all
//     current + future partitions automatically (Postgres ≥ 11). pg_trgm
//     GIN indexes also propagate this way.
//
//   - Greenfield only: if `logs` already exists as a non-partitioned table,
//     setupPostgresPartitionedLogs refuses to start. Migrating an existing
//     unpartitioned `logs` to partitioned requires moving data into a
//     swapped table — out of scope for this phase per the board ruling.

// PartitioningModeDaily is the canonical opt-in value for daily partitioning.
const PartitioningModeDaily = "daily"

// dailyPartitionPrefix is the table-name prefix used for partition children
// (e.g. logs_2026_04_27). Kept package-private to discourage callers from
// constructing names by hand — use partitionNameForDay.
const dailyPartitionPrefix = "logs_"

// partitionNameForDay returns the deterministic partition table name for the
// given UTC day. Format: `logs_YYYY_MM_DD`. Always normalized to UTC so two
// nodes with different local TZs converge on the same name.
func partitionNameForDay(day time.Time) string {
	d := day.UTC()
	return fmt.Sprintf("%s%04d_%02d_%02d", dailyPartitionPrefix, d.Year(), int(d.Month()), d.Day())
}

// setupPostgresPartitionedLogs provisions the partitioned `logs` parent table
// and an initial set of daily partitions covering [today - 1 day, today +
// lookaheadDays]. The trailing -1 day cushion absorbs clock skew on
// out-of-order ingest at the start-of-day rollover.
//
// Idempotent: if `logs` already exists and is partitioned, it is left
// untouched and we just top up partitions and indexes. If `logs` exists and
// is NOT partitioned, the function returns an error so the operator can
// migrate manually.
func setupPostgresPartitionedLogs(db *gorm.DB, lookaheadDays int) error {
	if lookaheadDays < 1 {
		lookaheadDays = 3
	}

	relkind, err := pgLogsRelkind(db)
	if err != nil {
		return fmt.Errorf("inspect logs relkind: %w", err)
	}
	switch relkind {
	case "":
		// Fresh DB — create the partitioned parent table with the same
		// columns GORM would have created, plus a composite PK that
		// includes the partition key.
		if err := db.Exec(`
			CREATE TABLE logs (
				id BIGSERIAL,
				tenant_id VARCHAR(64) NOT NULL DEFAULT 'default',
				trace_id VARCHAR(32),
				span_id VARCHAR(16),
				severity VARCHAR(50),
				body TEXT,
				service_name VARCHAR(255),
				attributes_json BYTEA,
				ai_insight BYTEA,
				timestamp TIMESTAMPTZ NOT NULL,
				PRIMARY KEY (id, timestamp)
			) PARTITION BY RANGE (timestamp)`).Error; err != nil {
			return fmt.Errorf("create partitioned logs: %w", err)
		}
		slog.Info("📦 Postgres: created partitioned logs table (RANGE on timestamp, daily)")
	case "p":
		// Already partitioned — accept and continue.
	case "r", "v", "m", "f", "t", "I":
		return fmt.Errorf("logs table already exists as a non-partitioned object (relkind=%q); DB_POSTGRES_PARTITIONING=daily is greenfield-only — drop the table or migrate before retrying", relkind)
	default:
		return fmt.Errorf("logs table has unexpected relkind=%q", relkind)
	}

	// Indexes on the parent — auto-cascade to children.
	parentIndexes := []string{
		`CREATE INDEX IF NOT EXISTS idx_logs_tenant_ts        ON logs (tenant_id, timestamp DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_logs_tenant_service   ON logs (tenant_id, service_name)`,
		`CREATE INDEX IF NOT EXISTS idx_logs_tenant_severity  ON logs (tenant_id, severity)`,
		`CREATE INDEX IF NOT EXISTS idx_logs_trace_id         ON logs (trace_id)`,
		`CREATE INDEX IF NOT EXISTS idx_logs_timestamp        ON logs (timestamp)`,
	}
	for _, ddl := range parentIndexes {
		if err := db.Exec(ddl).Error; err != nil {
			return fmt.Errorf("create parent index: %w", err)
		}
	}

	// pg_trgm indexes — propagate from parent to partitions only if the
	// extension is present, so we mirror factory.go's verify-then-create
	// pattern. Failing this is non-fatal (logs still ingest), so a missing
	// extension downgrades log search to seq scan rather than blocking boot.
	var trgmPresent int
	if catalogErr := db.Raw("SELECT 1 FROM pg_extension WHERE extname = 'pg_trgm'").Row().Scan(&trgmPresent); catalogErr == nil && trgmPresent == 1 {
		if err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_logs_body_trgm ON logs USING GIN (body gin_trgm_ops)`).Error; err != nil {
			slog.Warn("partitioned logs: pg_trgm GIN on body failed", "err", err)
		}
		if err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_logs_service_trgm ON logs USING GIN (service_name gin_trgm_ops)`).Error; err != nil {
			slog.Warn("partitioned logs: pg_trgm GIN on service_name failed", "err", err)
		}
	}

	// Pre-create initial partitions:
	//   - yesterday  (absorbs late-arriving events at the day rollover)
	//   - today
	//   - today+1 ... today+lookaheadDays
	// Total = lookaheadDays + 2 partitions.
	now := time.Now().UTC()
	for i := -1; i <= lookaheadDays; i++ {
		day := now.Add(time.Duration(i) * 24 * time.Hour)
		if err := EnsureLogsPartitionForDay(db, day); err != nil {
			return fmt.Errorf("ensure partition for %s: %w", day.Format("2006-01-02"), err)
		}
	}

	return nil
}

// EnsureLogsPartitionForDay creates the daily partition that covers `day`
// (UTC). Idempotent — uses CREATE TABLE IF NOT EXISTS PARTITION OF semantics
// so concurrent boots / scheduler ticks never collide.
func EnsureLogsPartitionForDay(db *gorm.DB, day time.Time) error {
	d := day.UTC().Truncate(24 * time.Hour)
	upper := d.Add(24 * time.Hour)
	name := partitionNameForDay(d)
	ddl := fmt.Sprintf(
		`CREATE TABLE IF NOT EXISTS %s PARTITION OF logs FOR VALUES FROM ('%s') TO ('%s')`,
		quoteIdent(name),
		d.Format("2006-01-02 15:04:05+00"),
		upper.Format("2006-01-02 15:04:05+00"),
	)
	if err := db.Exec(ddl).Error; err != nil {
		return fmt.Errorf("create partition %s: %w", name, err)
	}
	return nil
}

// EnsureLogsLookahead ensures partitions exist for the next `lookaheadDays`
// starting at "today" (UTC). Returns the count of partitions newly created
// for telemetry.
func EnsureLogsLookahead(db *gorm.DB, lookaheadDays int) (int, error) {
	if lookaheadDays < 1 {
		lookaheadDays = 1
	}
	now := time.Now().UTC()
	created := 0
	for i := 0; i <= lookaheadDays; i++ {
		day := now.Add(time.Duration(i) * 24 * time.Hour)
		// IF NOT EXISTS makes this idempotent; we don't try to detect
		// "did it actually create" because the DDL is cheap and the
		// observability value is low.
		if err := EnsureLogsPartitionForDay(db, day); err != nil {
			return created, err
		}
		created++
	}
	return created, nil
}

// DropExpiredLogsPartitions drops every daily logs partition whose entire
// upper bound is older than `cutoff`. Returns the number of partitions
// dropped. Safe to call repeatedly (no-op when nothing matches).
//
// We use the partition catalog (pg_partitioned_table + pg_inherits) instead
// of guessing names, so partitions created by earlier code paths or operator
// scripts are also covered.
func DropExpiredLogsPartitions(ctx context.Context, db *gorm.DB, cutoff time.Time) (int, error) {
	cutoffUTC := cutoff.UTC()

	type row struct {
		Name  string
		Bound string // "FOR VALUES FROM ('...') TO ('...')"
	}

	// pg_get_expr unfolds the partition bound expression to text. We parse
	// the upper bound out of it and compare to cutoff.
	var rows []row
	if err := db.WithContext(ctx).Raw(`
		SELECT c.relname AS name,
		       pg_get_expr(c.relpartbound, c.oid) AS bound
		FROM pg_class p
		JOIN pg_inherits i ON i.inhparent = p.oid
		JOIN pg_class c    ON c.oid       = i.inhrelid
		WHERE p.relname = 'logs'
		  AND p.relkind = 'p'
	`).Scan(&rows).Error; err != nil {
		return 0, fmt.Errorf("list partitions: %w", err)
	}

	dropped := 0
	for _, r := range rows {
		upper, ok := parsePartitionUpper(r.Bound)
		if !ok {
			slog.Debug("partition: unable to parse bound", "name", r.Name, "bound", r.Bound)
			continue
		}
		if !upper.After(cutoffUTC) {
			// Entire partition range ends at or before the cutoff — safe
			// to drop. Use IF EXISTS so a concurrent drop from another
			// scheduler instance doesn't error.
			if err := db.WithContext(ctx).Exec(fmt.Sprintf(`DROP TABLE IF EXISTS %s`, quoteIdent(r.Name))).Error; err != nil {
				return dropped, fmt.Errorf("drop partition %s: %w", r.Name, err)
			}
			slog.Info("🗑️  dropped expired logs partition", "name", r.Name, "upper", upper.Format(time.RFC3339))
			dropped++
		}
	}
	return dropped, nil
}

// pgLogsRelkind returns the relkind of the `logs` relation, or "" if it does
// not exist. Used to gate the greenfield enforcement and to recognize an
// already-partitioned parent on subsequent boots.
func pgLogsRelkind(db *gorm.DB) (string, error) {
	var relkind string
	row := db.Raw(`SELECT relkind::text FROM pg_class WHERE relname = 'logs' AND relnamespace = (SELECT oid FROM pg_namespace WHERE nspname = current_schema())`).Row()
	if err := row.Scan(&relkind); err != nil {
		// "table doesn't exist yet" path — sql.ErrNoRows is the standard
		// signal here, not a string match against the message.
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		return "", err
	}
	return relkind, nil
}

// parsePartitionUpper extracts the upper bound timestamp from a partition
// bound expression of the form
// `FOR VALUES FROM ('YYYY-MM-DD HH:MM:SS+TZ') TO ('YYYY-MM-DD HH:MM:SS+TZ')`.
// Returns (zero, false) if parsing fails — caller logs and skips.
func parsePartitionUpper(boundExpr string) (time.Time, bool) {
	// The expression is generated by Postgres; format is stable. We only
	// need the second quoted timestamp.
	_, rest, ok := strings.Cut(boundExpr, " TO (")
	if !ok {
		return time.Time{}, false
	}
	// rest now begins with "'YYYY-...'"). Pull the bytes between the first
	// pair of single quotes.
	start := strings.IndexByte(rest, '\'')
	if start < 0 {
		return time.Time{}, false
	}
	end := strings.IndexByte(rest[start+1:], '\'')
	if end < 0 {
		return time.Time{}, false
	}
	tsStr := rest[start+1 : start+1+end]
	// Postgres prints the bound in the session TZ; we ask above queries to
	// parse it as RFC3339-ish. Try a few layouts.
	layouts := []string{
		"2006-01-02 15:04:05-07",
		"2006-01-02 15:04:05+00",
		"2006-01-02 15:04:05Z07:00",
		"2006-01-02 15:04:05",
	}
	for _, l := range layouts {
		if t, err := time.Parse(l, tsStr); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}

// quoteIdent returns a Postgres-safe quoted identifier. We deliberately keep
// this minimal — partition names are derived from a fixed prefix + UTC date,
// so the only quoting required is wrapping in double quotes and doubling any
// embedded `"`. Using fmt.Sprintf with a raw identifier would expose us to
// SQL injection if a future caller passes user-controlled input.
func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}
