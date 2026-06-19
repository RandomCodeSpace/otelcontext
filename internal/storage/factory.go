package storage

import (
	"context"
	"fmt"
	"log"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/driver/sqlserver"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	_ "github.com/microsoft/go-mssqldb/azuread"

	"github.com/RandomCodeSpace/otelcontext/internal/membudget"
)

// NewDatabase creates a GORM database connection for any supported driver.
// Supported drivers: sqlite, postgres, mysql, sqlserver.
// Applies per-driver optimizations (WAL for SQLite, connection pooling for others).
func NewDatabase(driver, dsn string) (*gorm.DB, error) {
	var dialector gorm.Dialector

	switch strings.ToLower(driver) {
	case "postgres", "postgresql":
		if dsn == "" {
			return nil, fmt.Errorf("DB_DSN is required for postgres driver")
		}
		if isAzureEntraEnabled() {
			sqlDB, err := openPostgresWithEntra(dsn)
			if err != nil {
				return nil, fmt.Errorf("entra postgres: %w", err)
			}
			dialector = postgres.New(postgres.Config{Conn: sqlDB})
			log.Println("🔐 Postgres: Azure Entra ID authentication enabled")
		} else {
			dialector = postgres.Open(dsn)
		}

	case "sqlserver", "mssql":
		if dsn == "" {
			return nil, fmt.Errorf("DB_DSN is required for sqlserver driver")
		}
		dialector = sqlserver.Open(dsn)

	case "mysql":
		if dsn == "" {
			dsn = "root:admin@tcp(127.0.0.1:3306)/OtelContext?charset=utf8mb4&parseTime=True&loc=Local"
		}
		dialector = mysql.Open(dsn)

	case "sqlite", "":
		if dsn == "" {
			dsn = "OtelContext.db"
		}
		if driver == "" {
			driver = "sqlite"
			log.Println("DB_DRIVER not set, defaulting to sqlite (OtelContext.db)")
		}
		dialector = sqlite.Open(dsn)

	default:
		return nil, fmt.Errorf("unsupported database driver: %s", driver)
	}

	db, err := gorm.Open(dialector, &gorm.Config{
		Logger: logger.Default.LogMode(logger.Error),
		// RAN-49: never emit FK constraints during AutoMigrate.
		//
		// (1) Async ingestion: spans/logs can arrive before their parent trace,
		// so a real FK on (spans|logs).trace_id → traces.trace_id would reject
		// valid out-of-order writes — which is why the MySQL path explicitly
		// drops fk_traces_spans / fk_traces_logs after AutoMigrate.
		//
		// (2) RAN-21 made trace identity tenant-scoped: traces no longer carries
		// a single-column unique on trace_id (only the composite
		// (tenant_id, trace_id)). On Postgres, CREATE TABLE spans (... FK
		// trace_id REFERENCES traces(trace_id)) then fails at DDL time with
		// SQLSTATE 42830 ("there is no unique constraint matching given keys
		// for referenced table"), aborting boot on a fresh DB. SQLite hides
		// this because it does not validate FK targets at CREATE TABLE time.
		//
		// The model-level `constraint:false` GORM tag is not honored by
		// gorm v2's relationship parser, so the only reliable way to suppress
		// FK creation across all drivers is this config flag.
		DisableForeignKeyConstraintWhenMigrating: true,
	})
	if err != nil {
		// Never surface the DSN in error wraps — pgx occasionally embeds connection
		// string fragments (user:password@host) in its errors. Sanitize before returning.
		return nil, fmt.Errorf("failed to connect to database (%s): %s", driver, scrubDSN(err.Error()))
	}

	// SQLite pragmas — set via Exec because glebarez/sqlite doesn't honour
	// _pragma DSN params. Applied fail-closed: any PRAGMA failure aborts
	// startup with a wrapped error so an unexpected SQLite build that doesn't
	// support, e.g. mmap_size cannot silently regress the platform to
	// default-tuned behaviour. The set was hardened on 2026-05-24 to make
	// the platform survivable at 120 services on SQLite.
	//
	// cache_size and mmap_size are budget-scaled (the pure-Go driver's page
	// cache lives on the Go heap, so a fixed 256 MB/1 GB pair starved 4 GB
	// hosts) — see sqliteMemorySizes for the scaling and override rules.
	// wal_autocheckpoint=10000 = checkpoint after 10k pages so WAL stays bounded.
	// journal_size_limit=67108864 = hard-cap the WAL file at 64 MB.
	if strings.ToLower(driver) == "sqlite" || driver == "" {
		budget, budgetSource := membudget.Detect()
		cacheKB, mmapBytes := sqliteMemorySizes(budget)
		// Best-effort, deliberately NOT fail-closed, and ordered BEFORE the
		// stanza below: auto_vacuum only takes effect on databases this
		// process creates, and the journal_mode=WAL switch initializes the
		// file header — after which the stored auto_vacuum mode is frozen
		// (pre-existing files keep theirs either way). INCREMENTAL lets the
		// retention scheduler reclaim pages via PRAGMA incremental_vacuum
		// instead of a full daily VACUUM.
		if err := db.Exec("PRAGMA auto_vacuum=INCREMENTAL").Error; err != nil {
			log.Printf("⚠️  PRAGMA auto_vacuum=INCREMENTAL failed (best-effort, continuing): %v", err)
		}
		pragmas := []string{
			"PRAGMA journal_mode=WAL",
			"PRAGMA synchronous=NORMAL",
			fmt.Sprintf("PRAGMA cache_size=-%d", cacheKB),
			"PRAGMA temp_store=MEMORY",
			fmt.Sprintf("PRAGMA mmap_size=%d", mmapBytes),
			"PRAGMA wal_autocheckpoint=10000",
			"PRAGMA journal_size_limit=67108864",
			"PRAGMA busy_timeout=5000",
		}
		for _, p := range pragmas {
			if err := db.Exec(p).Error; err != nil {
				return nil, fmt.Errorf("sqlite pragma %q failed: %w", p, err)
			}
		}
		if budgetSource == "" {
			budgetSource = "none (fallback to hardcoded ceilings)"
		}
		log.Printf("📊 SQLite memory tuning: cache=%d KB, mmap=%d MB (budget=%d MB, source=%s)",
			cacheKB, mmapBytes/(1<<20), budget/(1<<20), budgetSource)
	}

	// Configure Connection Pool — configurable via env vars for non-SQLite drivers.
	sqlDB, err := db.DB()
	if err == nil {
		switch strings.ToLower(driver) {
		case "sqlite", "":
			sqlDB.SetMaxIdleConns(1)
			sqlDB.SetMaxOpenConns(1)
			sqlDB.SetConnMaxLifetime(time.Hour)
			log.Printf("📊 SQLite Optimization: MaxOpen=1, WAL Mode=Enabled")
		default:
			maxOpen := getEnvPoolInt("DB_MAX_OPEN_CONNS", 50)
			// Default idle pool raised 10→25: under 100–200 services the concurrent
			// readers (MCP semaphore up to 32) + ingest workers + retention +
			// partition scheduler churn far more than 10 idle conns, forcing constant
			// reconnects. 25 keeps a warm pool roughly half of MaxOpen.
			maxIdle := getEnvPoolInt("DB_MAX_IDLE_CONNS", 25)
			lifetime := getEnvPoolDuration("DB_CONN_MAX_LIFETIME", time.Hour)
			idleTime := getEnvPoolDuration("DB_CONN_MAX_IDLE_TIME", 10*time.Minute)
			sqlDB.SetMaxOpenConns(maxOpen)
			sqlDB.SetMaxIdleConns(maxIdle)
			sqlDB.SetConnMaxLifetime(lifetime)
			sqlDB.SetConnMaxIdleTime(idleTime)
			// driver is operator-set config (DB_DRIVER); strip CR/LF before logging so a
			// malformed value can't forge log lines (CWE-117 / go/log-injection). The
			// counts and duration are non-injectable.
			safeDriver := strings.NewReplacer("\r", "", "\n", "").Replace(driver)
			log.Printf("📊 DB Pool Configured: MaxOpen=%d, MaxIdle=%d, MaxIdleTime=%s, Driver=%s", maxOpen, maxIdle, idleTime, safeDriver)

			// Entra override: Azure AD tokens are ~60-90 min TTL. Cap pooled-conn
			// lifetime below that window so we never present a stale token on
			// reconnect-after-failure. Applied AFTER general config so it wins.
			if (strings.ToLower(driver) == "postgres" || strings.ToLower(driver) == "postgresql") && isAzureEntraEnabled() {
				entraMaxLifetime := 30 * time.Minute
				entraMaxIdle := 15 * time.Minute
				sqlDB.SetConnMaxLifetime(entraMaxLifetime)
				sqlDB.SetConnMaxIdleTime(entraMaxIdle)
				log.Printf("🔐 Entra pool override: ConnMaxLifetime=%s, ConnMaxIdleTime=%s (token TTL guard)", entraMaxLifetime, entraMaxIdle)
			}
		}
	}

	return db, nil
}

func getEnvPoolInt(key string, fallback int) int {
	if v, ok := os.LookupEnv(key); ok {
		if i, err := strconv.Atoi(v); err == nil {
			return i
		}
	}
	return fallback
}

// SQLite memory sizing bounds. The max values are the pre-2026-06 hardcoded
// stanza (256 MB page cache, 1 GB mmap window) and double as the fallback
// when budget detection fails — never worse than the previous behaviour.
const (
	sqliteCacheKBMin   = 65536      // 64 MB
	sqliteCacheKBMax   = 262144     // 256 MB (legacy hardcoded value)
	sqliteMmapBytesMin = 268435456  // 256 MB
	sqliteMmapBytesMax = 1073741824 // 1 GB (legacy hardcoded value)
)

// sqliteMemorySizes resolves the SQLite page-cache size (in KB, applied as a
// negative cache_size) and mmap window (in bytes) for the startup PRAGMA
// stanza. With the pure-Go driver both budgets are Go-heap/address-space
// costs, so they scale with the detected memory budget instead of being
// hardcoded: cache = budget/32 clamped to [64 MB, 256 MB], mmap = budget/8
// clamped to [256 MB, 1 GB] — a 4 GB host yields 128 MB cache + 512 MB mmap.
// budget <= 0 (detection failed) falls back to the legacy hardcoded maxima.
// Operator overrides win unconditionally: SQLITE_CACHE_SIZE_KB (> 0) and
// SQLITE_MMAP_SIZE_BYTES (>= 0; 0 disables mmap). Invalid values are ignored.
func sqliteMemorySizes(budget int64) (cacheKB, mmapBytes int64) {
	cacheKB = sqliteCacheKBMax
	mmapBytes = sqliteMmapBytesMax
	if budget > 0 {
		cacheKB = clampInt64(budget/32/1024, sqliteCacheKBMin, sqliteCacheKBMax)
		mmapBytes = clampInt64(budget/8, sqliteMmapBytesMin, sqliteMmapBytesMax)
	}
	if v, ok := getEnvInt64("SQLITE_CACHE_SIZE_KB"); ok && v > 0 {
		cacheKB = v
	}
	if v, ok := getEnvInt64("SQLITE_MMAP_SIZE_BYTES"); ok && v >= 0 {
		mmapBytes = v
	}
	return cacheKB, mmapBytes
}

func clampInt64(v, lo, hi int64) int64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func getEnvInt64(key string) (int64, bool) {
	if v, ok := os.LookupEnv(key); ok {
		if i, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil {
			return i, true
		}
	}
	return 0, false
}

// scrubDSN returns a DSN-like string with passwords and sensitive fields redacted.
// Handles two common forms:
//   - URL:   postgres://user:PASS@host/db        → postgres://user:REDACTED@host/db
//   - KV:    host=x password=PASS sslmode=...    → host=x password=REDACTED sslmode=...
//
// Intended for error-string sanitization — we scan for password markers anywhere
// in the input, since pgx embeds DSN fragments mid-error. Non-password content is
// left intact so operators can still identify the failing host.
func scrubDSN(s string) string {
	if s == "" {
		return s
	}

	// KV form: password=... up to the next whitespace.
	s = kvPasswordRE.ReplaceAllString(s, "password=REDACTED")

	// URL form: scheme://user:password@host — redact the password segment only.
	s = urlPasswordRE.ReplaceAllString(s, "$1:REDACTED@")

	return s
}

var (
	// password=<val> where <val> runs to whitespace, end-of-string, or a quote boundary.
	kvPasswordRE = regexp.MustCompile(`(?i)password\s*=\s*('[^']*'|"[^"]*"|\S+)`)
	// scheme://user:password@ — we only capture scheme://user to rebuild with REDACTED.
	urlPasswordRE = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+\-.]*://[^:/@\s]+):[^@\s]+@`)
)

func getEnvPoolDuration(key string, fallback time.Duration) time.Duration {
	if v, ok := os.LookupEnv(key); ok {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return fallback
}

// AutoMigrateModels runs GORM auto-migration for all OtelContext models.
//
// When DB_POSTGRES_PARTITIONING=daily, the `logs` table is provisioned as a
// declarative range-partitioned parent BEFORE GORM AutoMigrate runs, so
// AutoMigrate sees an existing table and skips it (GORM's IF NOT EXISTS
// behaviour). This is greenfield-only — see setupPostgresPartitionedLogs for
// the safety check.
func AutoMigrateModels(db *gorm.DB, driver string) error {
	return AutoMigrateModelsWithOptions(db, driver, MigrateOptions{})
}

// MigrateOptions tunes AutoMigrateModelsWithOptions. Empty zero value
// preserves the legacy AutoMigrateModels behaviour.
type MigrateOptions struct {
	// PostgresPartitioning, when "daily", provisions `logs` as a partitioned
	// table. Any other value (including the empty string) keeps the legacy
	// unpartitioned schema.
	PostgresPartitioning string
	// PartitionLookaheadDays is the number of future daily partitions to
	// pre-create at boot. Defaults to 3 when zero.
	PartitionLookaheadDays int
	// Timeout, when > 0, bounds the AutoMigrate call. Without it,
	// db.AutoMigrate inherits no deadline and an ALTER TABLE waiting on a
	// Postgres relation lock can hang startup indefinitely. The timeout is
	// applied via db.WithContext to the AutoMigrate call only — pre/post
	// hooks (FTS5 triggers, legacy index drops) are not bounded since they
	// don't take long-held locks. Zero preserves legacy unbounded behaviour.
	Timeout time.Duration
}

// AutoMigrateModelsWithOptions is the option-driven variant of
// AutoMigrateModels. Existing callers should continue to use AutoMigrateModels
// — the options entry point is for new wiring (currently main.go) that needs
// to plumb the partitioning flag.
func AutoMigrateModelsWithOptions(db *gorm.DB, driver string, opts MigrateOptions) error {
	driver = strings.ToLower(driver)

	// Disable FK checks during migration for MySQL.
	// New databases will not get FKs created (DisableForeignKeyConstraintWhenMigrating
	// in NewDatabase), but legacy MySQL DBs may still carry fk_traces_spans /
	// fk_traces_logs from before RAN-49 — toggling FK_CHECKS=0 keeps the
	// post-migrate DROP statements below safe regardless of legacy state.
	if driver == "mysql" {
		db.Exec("SET FOREIGN_KEY_CHECKS = 0")
		log.Println("🔓 Disabled foreign key checks for migration")
	}

	// Postgres partitioning: provision the partitioned `logs` parent + initial
	// daily partitions BEFORE GORM AutoMigrate runs, and skip Log from
	// AutoMigrate's slice. AutoMigrate would otherwise try to ALTER the
	// timestamp column (because the model tag doesn't carry an explicit
	// `not null` and the partitioned PK forces NOT NULL on the column),
	// which Postgres rejects because the column is part of the partition key.
	logsPartitioned := false
	if (driver == "postgres" || driver == "postgresql") && opts.PostgresPartitioning == PartitioningModeDaily {
		if err := setupPostgresPartitionedLogs(db, opts.PartitionLookaheadDays); err != nil {
			return fmt.Errorf("setup partitioned logs: %w", err)
		}
		log.Printf("📦 Postgres: declarative partitioning enabled (daily, lookahead=%d days)", opts.PartitionLookaheadDays)
		logsPartitioned = true
	}

	// Dedupe spans BEFORE AutoMigrate adds the composite uniqueIndex
	// idx_spans_tenant_trace_span on (tenant_id, trace_id, span_id).
	// Pre-RAN-65 deployments may have duplicates from DLQ replays; the
	// unique index would fail to create against violating rows. No-op on
	// fresh databases or when the unique index already exists.
	if err := dedupeSpansForUniqueIndex(db, driver); err != nil {
		log.Printf("⚠️  span dedupe before unique index failed: %v", err)
	}

	migrateModels := []any{&Trace{}, &Span{}, &MetricBucket{}}
	if !logsPartitioned {
		migrateModels = append(migrateModels, &Log{})
	}
	// Apply a deadline to the AutoMigrate call when configured so a Postgres
	// relation-lock wait cannot hang startup indefinitely. WithContext returns
	// a session-scoped *gorm.DB; the parent db is unaffected for the post-
	// migration helpers below.
	migrator := db
	if opts.Timeout > 0 {
		ctx, cancel := context.WithTimeout(context.Background(), opts.Timeout)
		defer cancel()
		migrator = db.WithContext(ctx)
	}
	if err := migrator.AutoMigrate(migrateModels...); err != nil {
		return fmt.Errorf("failed to migrate database: %w", err)
	}

	// RAN-21: retire the pre-composite standalone unique index on traces.trace_id.
	// AutoMigrate never drops indexes that no longer appear on struct tags, so on
	// pre-existing databases the old uniqueIndex would persist and still block
	// cross-tenant trace_id reuse. This is idempotent across drivers and a no-op
	// on fresh databases.
	if err := dropLegacyTraceIDUniqueIndex(db, driver); err != nil {
		log.Printf("⚠️  legacy trace_id unique index drop failed: %v", err)
	}

	// Legacy MySQL cleanup: drop FKs that pre-RAN-49 migrations created. Fresh
	// MySQL DBs after RAN-49 won't have these (FK creation is now disabled at
	// the gorm.Config layer), but pre-existing deployments still need this
	// drop to keep async ingestion non-blocking.
	if driver == "mysql" {
		db.Exec("ALTER TABLE spans DROP FOREIGN KEY fk_traces_spans")
		db.Exec("ALTER TABLE logs DROP FOREIGN KEY fk_traces_logs")
		db.Exec("SET FOREIGN_KEY_CHECKS = 1")
		log.Println("🔓 Dropped legacy FK constraints (no-op on fresh DBs)")
	}

	// SQLite: provision FTS5 virtual table + triggers on logs.body / logs.service_name.
	// Search routes through bm25() ranking on this driver; LIKE remains the fallback
	// if FTS5 is unavailable (older SQLite builds without FTS5 compiled in).
	//
	// Gated on LOG_FTS_ENABLED (default false) — FTS5's inverted index typically
	// consumes 30-40% of SQLite DB disk for log-heavy workloads. When disabled,
	// search_logs falls back transparently to LIKE via the existing branch in
	// log_repo.go:105.
	if driver == "sqlite" || driver == "" {
		if logFTSEnabledFromEnv() {
			if err := setupSQLiteFTS5(db); err != nil {
				log.Printf("⚠️  SQLite FTS5 setup failed (%v) — log search will fall back to LIKE", err)
			}
		} else {
			log.Println("ℹ️  SQLite FTS5 disabled (LOG_FTS_ENABLED=false) — log search uses LIKE")
		}
	}

	// Postgres: enable pg_trgm and create a GIN index on logs.body for fuzzy ILIKE search.
	// Azure Database for PostgreSQL allows pg_trgm by default. If the role lacks
	// CREATE EXTENSION privilege, an operator can pre-create the extension and this
	// call becomes a no-op thanks to IF NOT EXISTS.
	//
	// We *verify* the extension is present in pg_extension before creating GIN
	// indexes — otherwise index creation would fail with a confusing "operator
	// class gin_trgm_ops does not exist" error. On missing extension we log a
	// prominent warning and skip the GIN indexes: substring log search then
	// falls back to sequential scan, but the app stays functional.
	if driver == "postgres" || driver == "postgresql" {
		createErr := db.Exec("CREATE EXTENSION IF NOT EXISTS pg_trgm").Error
		if createErr != nil {
			log.Printf("⚠️  WARNING: CREATE EXTENSION pg_trgm failed (%v)", createErr)
		}

		var present int
		catalogErr := db.Raw("SELECT 1 FROM pg_extension WHERE extname = 'pg_trgm'").Row().Scan(&present)

		if catalogErr != nil || present != 1 {
			log.Printf("⚠️  WARNING: pg_trgm extension NOT installed — skipping GIN trigram indexes on logs.body and logs.service_name. " +
				"Substring log search will fall back to sequential scan (slow on large tables). " +
				"To fix: have a superuser run `CREATE EXTENSION pg_trgm` against this database, then restart OtelContext.")
		} else {
			bodyErr := db.Exec("CREATE INDEX IF NOT EXISTS idx_logs_body_trgm ON logs USING GIN (body gin_trgm_ops)").Error
			if bodyErr != nil {
				log.Printf("⚠️  index creation failed: idx_logs_body_trgm: %v", bodyErr)
			}
			// Composite trigram index for service_name + body filtering (common pattern).
			svcErr := db.Exec("CREATE INDEX IF NOT EXISTS idx_logs_service_trgm ON logs USING GIN (service_name gin_trgm_ops)").Error
			if svcErr != nil {
				log.Printf("⚠️  index creation failed: idx_logs_service_trgm: %v", svcErr)
			}
			if bodyErr == nil && svcErr == nil {
				log.Println("🔎 Postgres: pg_trgm extension verified; GIN indexes ready on logs.body and logs.service_name")
			}
		}

		// BRIN indexes on the time columns that drive the by-age retention DELETE.
		// spans/traces are append-mostly and physically time-ordered, so BRIN is a
		// tiny (kilobytes) index that lets the planner range-prune the 7-day purge
		// without the write/space cost of a B-tree. Best-effort: a failure here
		// only means retention falls back to a fuller scan, so we log and continue.
		brinIndexes := []struct{ name, ddl string }{
			{"idx_spans_start_time_brin", "CREATE INDEX IF NOT EXISTS idx_spans_start_time_brin ON spans USING BRIN (start_time)"},
			{"idx_traces_timestamp_brin", "CREATE INDEX IF NOT EXISTS idx_traces_timestamp_brin ON traces USING BRIN (timestamp)"},
		}
		for _, idx := range brinIndexes {
			if err := db.Exec(idx.ddl).Error; err != nil {
				log.Printf("⚠️  index creation failed: %s: %v", idx.name, err)
			}
		}
	}

	return nil
}
