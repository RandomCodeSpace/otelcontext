"""Prometheus metrics and health endpoint."""

from __future__ import annotations

import time
from typing import Any

from prometheus_client import Counter, Gauge, Histogram, generate_latest, CONTENT_TYPE_LATEST


class Metrics:
    """Central metrics registry for OtelContext."""

    def __init__(self) -> None:
        self.start_time = time.time()

        # HTTP
        self.http_requests_total = Counter(
            "otelcontext_http_requests_total", "Total HTTP requests", ["method", "path", "status"]
        )
        self.http_request_duration = Histogram(
            "otelcontext_http_request_duration_seconds", "HTTP request duration", ["method", "path"]
        )

        # gRPC
        self.grpc_requests_total = Counter(
            "otelcontext_grpc_requests_total", "Total gRPC requests", ["method", "status"]
        )
        self.grpc_request_duration = Histogram(
            "otelcontext_grpc_request_duration_seconds", "gRPC request duration", ["method"]
        )

        # OTLP Ingestion
        self.otlp_spans_received = Counter("otelcontext_otlp_spans_received_total", "Spans received")
        self.otlp_logs_received = Counter("otelcontext_otlp_logs_received_total", "Logs received")
        self.otlp_metrics_received = Counter("otelcontext_otlp_metrics_received_total", "Metrics received")

        # WebSocket
        self.ws_active_connections = Gauge("otelcontext_ws_active_connections", "Active WS connections")
        self.ws_messages_sent = Counter("otelcontext_ws_messages_sent_total", "WS messages sent", ["type"])
        self.ws_slow_clients_removed = Counter("otelcontext_ws_slow_clients_removed_total", "Slow clients removed")

        # TSDB
        self.tsdb_ingest_total = Counter("otelcontext_tsdb_ingest_total", "TSDB data points ingested")
        self.tsdb_batches_dropped = Counter("otelcontext_tsdb_batches_dropped_total", "TSDB batches dropped")
        self.tsdb_cardinality_overflow = Counter("otelcontext_tsdb_cardinality_overflow_total", "Cardinality overflows")

        # DLQ
        self.dlq_enqueued_total = Counter("otelcontext_dlq_enqueued_total", "DLQ items enqueued")
        self.dlq_replay_success = Counter("otelcontext_dlq_replay_success_total", "DLQ replay successes")
        self.dlq_replay_failure = Counter("otelcontext_dlq_replay_failure_total", "DLQ replay failures")
        self.dlq_size = Gauge("otelcontext_dlq_size", "DLQ file count")
        self.dlq_disk_bytes = Gauge("otelcontext_dlq_disk_bytes", "DLQ disk usage bytes")

    def set_active_connections(self, count: int) -> None:
        self.ws_active_connections.set(count)

    def prometheus_response(self) -> tuple[bytes, str]:
        """Return (body, content_type) for /metrics."""
        return generate_latest(), CONTENT_TYPE_LATEST

    def health_data(self) -> dict:
        return {
            "status": "ok",
            "uptime_seconds": time.time() - self.start_time,
        }
