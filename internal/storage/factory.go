package storage

import (
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

	// SQLite pragmas must be set via Exec (glebarez/sqlite doesn't support _pragma DSN params)
	if strings.ToLower(driver) == "sqlite" || driver == "" {
		db.Exec("PRAGMA journal_mode=WAL")
		db.Exec("PRAGMA busy_timeout=5000")
		db.Exec("PRAGMA synchronous=NORMAL")
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
			maxIdle := getEnvPoolInt("DB_MAX_IDLE_CONNS", 10)
			lifetime := getEnvPoolDuration("DB_CONN_MAX_LIFETIME", time.Hour)
			sqlDB.SetMaxOpenConns(maxOpen)
			sqlDB.SetMaxIdleConns(maxIdle)
			sqlDB.SetConnMaxLifetime(lifetime)
			log.Printf("📊 DB Pool Configured: MaxOpen=%d, MaxIdle=%d, Driver=%s", maxOpen, maxIdle, driver) // #nosec G706 -- log.Printf with controlled config ints and enum driver

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
func AutoMigrateModels(db *gorm.DB, driver string) error {
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

	if err := db.AutoMigrate(&Trace{}, &Span{}, &Log{}, &MetricBucket{}); err != nil {
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
	}

	return nil
}
