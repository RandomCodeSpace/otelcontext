"""GraphRAG coordinator: event workers, ingestion callbacks, background loops."""

from __future__ import annotations

import asyncio
import json
import logging
import time
from dataclasses import dataclass
from datetime import datetime, timedelta, timezone
from typing import Any

from src.db.repository import Repository
from src.graphrag.anomaly import detect_anomalies
from src.graphrag.investigation import persist_investigation
from src.graphrag.refresh import rebuild_from_db
from src.graphrag.schema import (
    AnomalyNode,
    Edge,
    ErrorChainResult,
    SpanNode,
)
from src.graphrag.snapshot import prune_old_snapshots, take_snapshot
from src.graphrag.store import AnomalyStore, ServiceStore, SignalStore, TraceStore
from src.tsdb.aggregator import RawMetric
from src.vectordb.index import Index as VectorIndex

logger = logging.getLogger(__name__)

DEFAULT_WORKER_COUNT = 4
DEFAULT_CHANNEL_SIZE = 10000
DEFAULT_TRACE_TTL = 3600.0  # 1 hour
DEFAULT_REFRESH_EVERY = 60.0  # seconds
DEFAULT_SNAPSHOT_EVERY = 900.0  # 15 minutes
DEFAULT_ANOMALY_EVERY = 10.0  # seconds


@dataclass
class GraphRAGConfig:
    trace_ttl: float = DEFAULT_TRACE_TTL
    refresh_every: float = DEFAULT_REFRESH_EVERY
    snapshot_every: float = DEFAULT_SNAPSHOT_EVERY
    anomaly_every: float = DEFAULT_ANOMALY_EVERY
    worker_count: int = DEFAULT_WORKER_COUNT
    channel_size: int = DEFAULT_CHANNEL_SIZE


class GraphRAG:
    """Main coordinator for the layered graph system."""

    def __init__(
        self,
        repo: Repository,
        vector_idx: VectorIndex | None = None,
        tsdb_agg: Any = None,
        ring_buf: Any = None,
        config: GraphRAGConfig | None = None,
    ):
        cfg = config or GraphRAGConfig()
        self.service_store = ServiceStore()
        self.trace_store = TraceStore(ttl_seconds=cfg.trace_ttl)
        self.signal_store = SignalStore()
        self.anomaly_store = AnomalyStore()

        self.repo = repo
        self.vector_idx = vector_idx
        self.tsdb_agg = tsdb_agg
        self.ring_buf = ring_buf

        self._event_queue: asyncio.Queue = asyncio.Queue(maxsize=cfg.channel_size)
        self._config = cfg
        self._tasks: list[asyncio.Task] = []
        self._stop_event = asyncio.Event()

    async def start(self) -> None:
        """Start background workers and loops."""
        # Event workers
        for i in range(self._config.worker_count):
            self._tasks.append(asyncio.create_task(self._event_worker()))

        # Background loops
        self._tasks.append(asyncio.create_task(self._refresh_loop()))
        self._tasks.append(asyncio.create_task(self._snapshot_loop()))
        self._tasks.append(asyncio.create_task(self._anomaly_loop()))

        logger.info(
            "GraphRAG started: workers=%d trace_ttl=%.0fs refresh=%.0fs",
            self._config.worker_count,
            self._config.trace_ttl,
            self._config.refresh_every,
        )

    async def stop(self) -> None:
        self._stop_event.set()
        for t in self._tasks:
            t.cancel()
        logger.info("GraphRAG stopped")

    # --- Ingestion callbacks ---

    def on_span_ingested(self, span_dict: dict) -> None:
        """Non-blocking enqueue from the ingestion pipeline."""
        try:
            self._event_queue.put_nowait(("span", span_dict))
        except asyncio.QueueFull:
            pass  # Best-effort; DB is source of truth

    def on_log_ingested(self, log_dict: dict) -> None:
        try:
            self._event_queue.put_nowait(("log", log_dict))
        except asyncio.QueueFull:
            pass

    def on_metric_ingested(self, metric: RawMetric) -> None:
        try:
            self._event_queue.put_nowait(("metric", metric))
        except asyncio.QueueFull:
            pass

    # --- Event worker ---

    async def _event_worker(self) -> None:
        while not self._stop_event.is_set():
            try:
                event_type, data = await asyncio.wait_for(self._event_queue.get(), timeout=2.0)
                if event_type == "span":
                    self._process_span(data)
                elif event_type == "log":
                    self._process_log(data)
                elif event_type == "metric":
                    self._process_metric(data)
            except asyncio.TimeoutError:
                continue
            except Exception as e:
                logger.error("GraphRAG event worker error: %s", e)

    def _process_span(self, span: dict) -> None:
        duration_ms = span.get("duration", 0) / 1000.0
        service = span.get("service_name", "")
        operation = span.get("operation_name", "")
        is_error = False
        start_time = span.get("start_time", datetime.now(timezone.utc))

        if not service:
            return

        self.service_store.upsert_service(service, duration_ms, is_error, start_time)
        if operation:
            self.service_store.upsert_operation(service, operation, duration_ms, is_error, start_time)

        # TraceStore
        trace_id = span.get("trace_id", "")
        self.trace_store.upsert_trace(trace_id, service, "OK", duration_ms, start_time)
        self.trace_store.upsert_span(SpanNode(
            id=span.get("span_id", ""),
            trace_id=trace_id,
            parent_span_id=span.get("parent_span_id", ""),
            service=service,
            operation=operation,
            duration=duration_ms,
            status_code="OK",
            is_error=is_error,
            timestamp=start_time,
        ))

        # CALLS edge for cross-service calls
        parent_span_id = span.get("parent_span_id", "")
        if parent_span_id:
            parent = self.trace_store.get_span(parent_span_id)
            if parent and parent.service != service:
                self.service_store.upsert_call_edge(parent.service, service, duration_ms, is_error, start_time)

    def _process_log(self, log: dict) -> None:
        service = log.get("service_name", "")
        if not service:
            return
        body = log.get("body", "")
        severity = log.get("severity", "")
        ts = log.get("timestamp", datetime.now(timezone.utc))

        cluster_id = f"lc_{service}_{_simple_hash(body):x}"
        self.signal_store.upsert_log_cluster(cluster_id, body, severity, service, ts)

        span_id = log.get("span_id", "")
        if span_id:
            self.signal_store.add_logged_during_edge(cluster_id, span_id, ts)

    def _process_metric(self, m: RawMetric) -> None:
        if not m.service_name:
            return
        self.signal_store.upsert_metric(m.name, m.service_name, m.value, m.timestamp)

    # --- Background loops ---

    async def _refresh_loop(self) -> None:
        # Initial rebuild
        await rebuild_from_db(self)

        while not self._stop_event.is_set():
            await asyncio.sleep(self._config.refresh_every)
            await rebuild_from_db(self)
            pruned = self.trace_store.prune()
            if pruned > 0:
                logger.debug("GraphRAG pruned expired traces/spans: count=%d", pruned)
            self.anomaly_store.prune_old()

    async def _snapshot_loop(self) -> None:
        while not self._stop_event.is_set():
            await asyncio.sleep(self._config.snapshot_every)
            await take_snapshot(self)
            await prune_old_snapshots(self)

    async def _anomaly_loop(self) -> None:
        while not self._stop_event.is_set():
            await asyncio.sleep(self._config.anomaly_every)
            detect_anomalies(self)

    # --- Investigation helper (called from anomaly detection) ---

    def persist_investigation(
        self,
        trigger_service: str,
        chains: list[ErrorChainResult],
        anomalies: list[AnomalyNode],
    ) -> None:
        """Schedule investigation persistence (fire-and-forget)."""
        try:
            loop = asyncio.get_running_loop()
            loop.create_task(persist_investigation(self, trigger_service, chains, anomalies))
        except RuntimeError:
            pass  # No running loop


def _simple_hash(s: str) -> int:
    h = 0
    for c in s:
        h = (h * 31 + ord(c)) & 0xFFFFFFFF
    return h
