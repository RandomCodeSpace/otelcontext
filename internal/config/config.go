package config

import (
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"

	"github.com/joho/godotenv"
)

type Config struct {
	Env               string
	LogLevel          string
	HTTPPort          string
	GRPCPort          string
	DBDriver          string
	DBDSN             string
	DLQPath           string
	DLQReplayInterval string

	// Ingestion Filtering
	IngestMinSeverity      string
	IngestAllowedServices  string
	IngestExcludedServices string

	// DB Connection Pool
	DBMaxOpenConns    int
	DBMaxIdleConns    int
	DBConnMaxLifetime string // e.g. "1h", "30m"

	// Hot/Cold Storage
	HotRetentionDays    int
	ColdStoragePath     string
	ColdStorageMaxGB    int
	ArchiveScheduleHour int // 0-23, hour of day to run archival
	ArchiveBatchSize    int

	// TSDB
	TSDBRingBufferDuration string // e.g. "1h"

	// Smart Observability — Adaptive Sampling
	SamplingRate               float64
	SamplingAlwaysOnErrors     bool
	SamplingLatencyThresholdMs int

	// Smart Observability — Metric Cardinality
	MetricAttributeKeys  string // comma-separated allowlist
	MetricMaxCardinality int

	// DLQ Safety
	DLQMaxFiles   int
	DLQMaxDiskMB  int
	DLQMaxRetries int

	// API Protection
	APIRateLimitRPS int

	// MCP Server
	MCPEnabled bool
	MCPPath    string

	// Compression
	CompressionLevel string // "default", "fast", "best"

	// Vector Index
	VectorIndexMaxEntries int

	// DevMode disables origin checks for WebSocket and enables dev-friendly defaults.
	// Derived from APP_ENV == "development".
	DevMode bool
}

func Load(customPath string) (*Config, error) {
	envFile := ".env"
	if customPath != "" {
		envFile = customPath
	}

	if _, err := os.Stat(envFile); !os.IsNotExist(err) {
		if err := godotenv.Load(envFile); err != nil {
			log.Println("⚠️  Failed to load .env file, using system environment variables or defaults")
		} else {
			log.Println("✅ Loaded configuration from .env")
		}
	} else {
		log.Println("⚠️  No .env file found, using system environment variables or defaults")
	}

	env := getEnv("APP_ENV", "development")
	return &Config{
		Env:               env,
		DevMode:           env == "development",
		LogLevel:          getEnv("LOG_LEVEL", "INFO"),
		HTTPPort:          getEnv("HTTP_PORT", "8080"),
		GRPCPort:          getEnv("GRPC_PORT", "4317"),
		DBDriver:          getEnv("DB_DRIVER", "sqlite"),
		DBDSN:             getEnv("DB_DSN", ""),
		DLQPath:           getEnv("DLQ_PATH", "./data/dlq"),
		DLQReplayInterval: getEnv("DLQ_REPLAY_INTERVAL", "5m"),

		IngestMinSeverity:      getEnv("INGEST_MIN_SEVERITY", "INFO"),
		IngestAllowedServices:  getEnv("INGEST_ALLOWED_SERVICES", ""),
		IngestExcludedServices: getEnv("INGEST_EXCLUDED_SERVICES", ""),

		// DB Connection Pool
		DBMaxOpenConns:    getEnvInt("DB_MAX_OPEN_CONNS", 50),
		DBMaxIdleConns:    getEnvInt("DB_MAX_IDLE_CONNS", 10),
		DBConnMaxLifetime: getEnv("DB_CONN_MAX_LIFETIME", "1h"),

		// Hot/Cold Storage
		HotRetentionDays:    getEnvInt("HOT_RETENTION_DAYS", 7),
		ColdStoragePath:     getEnv("COLD_STORAGE_PATH", "./data/cold"),
		ColdStorageMaxGB:    getEnvInt("COLD_STORAGE_MAX_GB", 50),
		ArchiveScheduleHour: getEnvInt("ARCHIVE_SCHEDULE_HOUR", 2),
		ArchiveBatchSize:    getEnvInt("ARCHIVE_BATCH_SIZE", 10000),

		// TSDB
		TSDBRingBufferDuration: getEnv("TSDB_RING_BUFFER_DURATION", "1h"),

		// Adaptive Sampling
		SamplingRate:               getEnvFloat("SAMPLING_RATE", 1.0), // default: keep all
		SamplingAlwaysOnErrors:     getEnvBool("SAMPLING_ALWAYS_ON_ERRORS", true),
		SamplingLatencyThresholdMs: getEnvInt("SAMPLING_LATENCY_THRESHOLD_MS", 500),

		// Cardinality
		MetricAttributeKeys:  getEnv("METRIC_ATTRIBUTE_KEYS", ""),
		MetricMaxCardinality: getEnvInt("METRIC_MAX_CARDINALITY", 10000),

		// DLQ
		DLQMaxFiles:   getEnvInt("DLQ_MAX_FILES", 1000),
		DLQMaxDiskMB:  getEnvInt("DLQ_MAX_DISK_MB", 500),
		DLQMaxRetries: getEnvInt("DLQ_MAX_RETRIES", 10),

		// API
		APIRateLimitRPS: getEnvInt("API_RATE_LIMIT_RPS", 100),

		// MCP
		MCPEnabled: getEnvBool("MCP_ENABLED", true),
		MCPPath:    getEnv("MCP_PATH", "/mcp"),

		// Compression
		CompressionLevel: getEnv("COMPRESSION_LEVEL", "default"),

		// Vector
		VectorIndexMaxEntries: getEnvInt("VECTOR_INDEX_MAX_ENTRIES", 100000),
	}, nil
}

func getEnv(key, fallback string) string {
	if value, exists := os.LookupEnv(key); exists {
		return value
	}
	return fallback
}

func getEnvInt(key string, fallback int) int {
	if v, exists := os.LookupEnv(key); exists {
		if i, err := strconv.Atoi(v); err == nil {
			return i
		}
	}
	return fallback
}

func getEnvFloat(key string, fallback float64) float64 {
	if v, exists := os.LookupEnv(key); exists {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return fallback
}

func getEnvBool(key string, fallback bool) bool {
	if v, exists := os.LookupEnv(key); exists {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return fallback
}

// Validate checks that all configuration values are within valid ranges.
// Call this once after Load() during startup to catch misconfiguration early.
func (c *Config) Validate() error {
	// Port validation
	httpPort, err := strconv.Atoi(c.HTTPPort)
	if err != nil || httpPort < 1 || httpPort > 65535 {
		return fmt.Errorf("invalid HTTP_PORT %q: must be 1-65535", c.HTTPPort)
	}
	grpcPort, err := strconv.Atoi(c.GRPCPort)
	if err != nil || grpcPort < 1 || grpcPort > 65535 {
		return fmt.Errorf("invalid GRPC_PORT %q: must be 1-65535", c.GRPCPort)
	}

	// DB driver
	validDrivers := map[string]bool{
		"sqlite": true, "postgres": true, "postgresql": true,
		"mysql": true, "mssql": true, "sqlserver": true,
	}
	if !validDrivers[strings.ToLower(c.DBDriver)] {
		return fmt.Errorf("invalid DB_DRIVER %q: must be one of sqlite, postgres, mysql, mssql", c.DBDriver)
	}

	// Numeric ranges
	if c.HotRetentionDays < 1 {
		return fmt.Errorf("HOT_RETENTION_DAYS must be >= 1, got %d", c.HotRetentionDays)
	}
	if c.ArchiveScheduleHour < 0 || c.ArchiveScheduleHour > 23 {
		return fmt.Errorf("ARCHIVE_SCHEDULE_HOUR must be 0-23, got %d", c.ArchiveScheduleHour)
	}
	if c.MetricMaxCardinality < 0 {
		return fmt.Errorf("METRIC_MAX_CARDINALITY must be >= 0, got %d", c.MetricMaxCardinality)
	}
	if c.SamplingRate < 0 || c.SamplingRate > 1.0 {
		return fmt.Errorf("SAMPLING_RATE must be between 0 and 1, got %f", c.SamplingRate)
	}
	if c.APIRateLimitRPS < 0 {
		return fmt.Errorf("API_RATE_LIMIT_RPS must be >= 0, got %d", c.APIRateLimitRPS)
	}
	if c.DBMaxOpenConns < 1 {
		return fmt.Errorf("DB_MAX_OPEN_CONNS must be >= 1, got %d", c.DBMaxOpenConns)
	}
	if c.DBMaxIdleConns < 0 {
		return fmt.Errorf("DB_MAX_IDLE_CONNS must be >= 0, got %d", c.DBMaxIdleConns)
	}

	// Compression level
	switch strings.ToLower(c.CompressionLevel) {
	case "default", "fast", "best":
	default:
		return fmt.Errorf("invalid COMPRESSION_LEVEL %q: must be one of default, fast, best", c.CompressionLevel)
	}

	return nil
}
