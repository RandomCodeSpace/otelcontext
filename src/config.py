"""Application configuration loaded from environment variables."""

from __future__ import annotations

import os
from dataclasses import dataclass, field


def _env(key: str, default: str = "") -> str:
    return os.environ.get(key, default)


def _env_int(key: str, default: int = 0) -> int:
    v = os.environ.get(key)
    if v is not None:
        try:
            return int(v)
        except ValueError:
            pass
    return default


def _env_float(key: str, default: float = 0.0) -> float:
    v = os.environ.get(key)
    if v is not None:
        try:
            return float(v)
        except ValueError:
            pass
    return default


def _env_bool(key: str, default: bool = False) -> bool:
    v = os.environ.get(key)
    if v is not None:
        return v.lower() in ("1", "true", "yes")
    return default


@dataclass
class Config:
    env: str = ""
    dev_mode: bool = False
    log_level: str = "INFO"
    http_port: str = "8080"
    grpc_port: str = "4317"
    db_driver: str = "sqlite"
    db_dsn: str = ""
    dlq_path: str = "./data/dlq"
    dlq_replay_interval: str = "5m"

    # Ingestion filtering
    ingest_min_severity: str = "INFO"
    ingest_allowed_services: str = ""
    ingest_excluded_services: str = ""

    # DB connection pool
    db_max_open_conns: int = 50
    db_max_idle_conns: int = 10
    db_conn_max_lifetime: str = "1h"

    # Hot/cold storage
    hot_retention_days: int = 7
    cold_storage_path: str = "./data/cold"
    cold_storage_max_gb: int = 50
    archive_schedule_hour: int = 2
    archive_batch_size: int = 10000

    # TSDB
    tsdb_ring_buffer_duration: str = "1h"

    # Adaptive sampling
    sampling_rate: float = 1.0
    sampling_always_on_errors: bool = True
    sampling_latency_threshold_ms: int = 500

    # Cardinality
    metric_attribute_keys: str = ""
    metric_max_cardinality: int = 10000

    # DLQ safety
    dlq_max_files: int = 1000
    dlq_max_disk_mb: int = 500
    dlq_max_retries: int = 10

    # API
    api_rate_limit_rps: int = 100

    # MCP
    mcp_enabled: bool = True
    mcp_path: str = "/mcp"

    # Compression
    compression_level: str = "default"

    # Vector index
    vector_index_max_entries: int = 100000


def load_config() -> Config:
    """Load configuration from environment variables with sensible defaults."""
    env = _env("APP_ENV", "development")
    return Config(
        env=env,
        dev_mode=(env == "development"),
        log_level=_env("LOG_LEVEL", "INFO"),
        http_port=_env("HTTP_PORT", "8080"),
        grpc_port=_env("GRPC_PORT", "4317"),
        db_driver=_env("DB_DRIVER", "sqlite"),
        db_dsn=_env("DB_DSN", ""),
        dlq_path=_env("DLQ_PATH", "./data/dlq"),
        dlq_replay_interval=_env("DLQ_REPLAY_INTERVAL", "5m"),
        ingest_min_severity=_env("INGEST_MIN_SEVERITY", "INFO"),
        ingest_allowed_services=_env("INGEST_ALLOWED_SERVICES", ""),
        ingest_excluded_services=_env("INGEST_EXCLUDED_SERVICES", ""),
        db_max_open_conns=_env_int("DB_MAX_OPEN_CONNS", 50),
        db_max_idle_conns=_env_int("DB_MAX_IDLE_CONNS", 10),
        db_conn_max_lifetime=_env("DB_CONN_MAX_LIFETIME", "1h"),
        hot_retention_days=_env_int("HOT_RETENTION_DAYS", 7),
        cold_storage_path=_env("COLD_STORAGE_PATH", "./data/cold"),
        cold_storage_max_gb=_env_int("COLD_STORAGE_MAX_GB", 50),
        archive_schedule_hour=_env_int("ARCHIVE_SCHEDULE_HOUR", 2),
        archive_batch_size=_env_int("ARCHIVE_BATCH_SIZE", 10000),
        tsdb_ring_buffer_duration=_env("TSDB_RING_BUFFER_DURATION", "1h"),
        sampling_rate=_env_float("SAMPLING_RATE", 1.0),
        sampling_always_on_errors=_env_bool("SAMPLING_ALWAYS_ON_ERRORS", True),
        sampling_latency_threshold_ms=_env_int("SAMPLING_LATENCY_THRESHOLD_MS", 500),
        metric_attribute_keys=_env("METRIC_ATTRIBUTE_KEYS", ""),
        metric_max_cardinality=_env_int("METRIC_MAX_CARDINALITY", 10000),
        dlq_max_files=_env_int("DLQ_MAX_FILES", 1000),
        dlq_max_disk_mb=_env_int("DLQ_MAX_DISK_MB", 500),
        dlq_max_retries=_env_int("DLQ_MAX_RETRIES", 10),
        api_rate_limit_rps=_env_int("API_RATE_LIMIT_RPS", 100),
        mcp_enabled=_env_bool("MCP_ENABLED", True),
        mcp_path=_env("MCP_PATH", "/mcp"),
        compression_level=_env("COMPRESSION_LEVEL", "default"),
        vector_index_max_entries=_env_int("VECTOR_INDEX_MAX_ENTRIES", 100000),
    )


def validate_config(cfg: Config) -> None:
    """Validate configuration values. Raises ValueError on bad config."""
    port = int(cfg.http_port)
    if port < 1 or port > 65535:
        raise ValueError(f"Invalid HTTP_PORT {cfg.http_port}: must be 1-65535")
    grpc = int(cfg.grpc_port)
    if grpc < 1 or grpc > 65535:
        raise ValueError(f"Invalid GRPC_PORT {cfg.grpc_port}: must be 1-65535")
    valid_drivers = {"sqlite", "postgres", "postgresql", "mysql", "mssql", "sqlserver"}
    if cfg.db_driver.lower() not in valid_drivers:
        raise ValueError(f"Invalid DB_DRIVER {cfg.db_driver}")
    if cfg.hot_retention_days < 1:
        raise ValueError("HOT_RETENTION_DAYS must be >= 1")
    if cfg.sampling_rate < 0 or cfg.sampling_rate > 1.0:
        raise ValueError("SAMPLING_RATE must be between 0 and 1")
    if cfg.compression_level not in ("default", "fast", "best"):
        raise ValueError(f"Invalid COMPRESSION_LEVEL {cfg.compression_level}")
