package storage

import (
	"fmt"
	"log"
	"os"
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
		dialector = postgres.Open(dsn)

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
	})
	if err != nil {
		return nil, fmt.Errorf("failed to connect to database (%s): %w", driver, err)
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
			log.Printf("📊 DB Pool Configured: MaxOpen=%d, MaxIdle=%d, Driver=%s", maxOpen, maxIdle, driver)
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
	// Disable FK checks during migration for MySQL
	if strings.ToLower(driver) == "mysql" {
		db.Exec("SET FOREIGN_KEY_CHECKS = 0")
		log.Println("🔓 Disabled foreign key checks for migration")
	}

	if err := db.AutoMigrate(&Trace{}, &Span{}, &Log{}, &MetricBucket{}); err != nil {
		return fmt.Errorf("failed to migrate database: %w", err)
	}

	// Drop foreign keys that AutoMigrate may have created (MySQL)
	if strings.ToLower(driver) == "mysql" {
		db.Exec("ALTER TABLE spans DROP FOREIGN KEY fk_traces_spans")
		db.Exec("ALTER TABLE logs DROP FOREIGN KEY fk_traces_logs")
		db.Exec("SET FOREIGN_KEY_CHECKS = 1")
		log.Println("🔓 Dropped FK constraints for async ingestion compatibility")
	}

	return nil
}

