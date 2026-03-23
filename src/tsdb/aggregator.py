"""TSDB Aggregator: tumbling-window metric aggregation with async DB persistence."""

from __future__ import annotations

import asyncio
import json
import logging
from dataclasses import dataclass, field
from datetime import datetime, timezone
from typing import Any, Callable, Optional

from src.tsdb.ringbuffer import RingBuffer

logger = logging.getLogger(__name__)


@dataclass
class RawMetric:
    name: str
    service_name: str
    value: float
    timestamp: datetime
    attributes: dict[str, Any] = field(default_factory=dict)


@dataclass
class _Bucket:
    name: str
    service_name: str
    time_bucket: datetime
    min: float
    max: float
    sum: float
    count: int
    attributes_json: str = ""


class Aggregator:
    """In-memory tumbling window aggregator for metrics.

    Flushes to DB every window_seconds. Ring buffer receives every point for
    real-time dashboard queries.
    """

    def __init__(self, repo: Any, window_seconds: float = 30.0):
        self._repo = repo
        self._window_seconds = window_seconds
        self._buckets: dict[str, _Bucket] = {}
        self._lock = asyncio.Lock()
        self._ring: RingBuffer | None = None
        self._max_cardinality = 0
        self._overflow_key = "__cardinality_overflow__"
        self._tasks: list[asyncio.Task] = []
        self._stop_event = asyncio.Event()
        self._flush_queue: asyncio.Queue[list[dict]] = asyncio.Queue(maxsize=500)

        # Metric callbacks
        self._on_ingest: Callable[[], None] | None = None
        self._on_dropped: Callable[[], None] | None = None
        self._cardinality_overflow_cb: Callable[[], None] | None = None

    def set_ring_buffer(self, rb: RingBuffer) -> None:
        self._ring = rb

    def set_cardinality_limit(self, max_card: int, on_overflow: Callable[[], None] | None = None) -> None:
        self._max_cardinality = max_card
        self._cardinality_overflow_cb = on_overflow

    def set_metrics(self, on_ingest: Callable[[], None] | None, on_dropped: Callable[[], None] | None) -> None:
        self._on_ingest = on_ingest
        self._on_dropped = on_dropped

    async def ingest(self, m: RawMetric) -> None:
        """Add a raw metric point to the current window."""
        attr_json = json.dumps(m.attributes) if m.attributes else ""
        key = f"{m.service_name}|{m.name}|{attr_json}"

        # Feed ring buffer (thread-safe)
        if self._ring is not None:
            self._ring.record(m.name, m.service_name, m.value, m.timestamp)
        if self._on_ingest:
            self._on_ingest()

        async with self._lock:
            bucket = self._buckets.get(key)
            if bucket is None:
                # Cardinality guard
                if self._max_cardinality > 0 and len(self._buckets) >= self._max_cardinality:
                    if self._cardinality_overflow_cb:
                        self._cardinality_overflow_cb()
                    key = self._overflow_key
                    bucket = self._buckets.get(key)
                    if bucket is None:
                        ts = m.timestamp.replace(
                            second=m.timestamp.second - m.timestamp.second % int(self._window_seconds),
                            microsecond=0,
                        )
                        bucket = _Bucket(
                            name="__overflow__",
                            service_name=m.service_name,
                            time_bucket=ts,
                            min=m.value,
                            max=m.value,
                            sum=m.value,
                            count=1,
                        )
                        self._buckets[key] = bucket
                        return
                else:
                    ts = m.timestamp.replace(
                        second=m.timestamp.second - m.timestamp.second % int(self._window_seconds),
                        microsecond=0,
                    )
                    bucket = _Bucket(
                        name=m.name,
                        service_name=m.service_name,
                        time_bucket=ts,
                        min=m.value,
                        max=m.value,
                        sum=m.value,
                        count=1,
                        attributes_json=attr_json,
                    )
                    self._buckets[key] = bucket
                    return

            if m.value < bucket.min:
                bucket.min = m.value
            if m.value > bucket.max:
                bucket.max = m.value
            bucket.sum += m.value
            bucket.count += 1

    def bucket_count(self) -> int:
        return len(self._buckets)

    async def start(self) -> None:
        """Start flush loop and persistence workers."""
        for _ in range(3):
            self._tasks.append(asyncio.create_task(self._persistence_worker()))
        self._tasks.append(asyncio.create_task(self._flush_loop()))

    async def stop(self) -> None:
        self._stop_event.set()
        await self._flush()
        for t in self._tasks:
            t.cancel()

    async def _flush_loop(self) -> None:
        while not self._stop_event.is_set():
            await asyncio.sleep(self._window_seconds)
            await self._flush()

    async def _flush(self) -> None:
        async with self._lock:
            if not self._buckets:
                return
            batch = []
            for b in self._buckets.values():
                batch.append({
                    "name": b.name,
                    "service_name": b.service_name,
                    "time_bucket": b.time_bucket,
                    "min": b.min,
                    "max": b.max,
                    "sum": b.sum,
                    "count": b.count,
                    "attributes_json": b.attributes_json,
                })
            self._buckets.clear()

        try:
            self._flush_queue.put_nowait(batch)
        except asyncio.QueueFull:
            if self._on_dropped:
                self._on_dropped()
            logger.warning("TSDB flush queue full, dropping batch of %d", len(batch))

    async def _persistence_worker(self) -> None:
        while not self._stop_event.is_set():
            try:
                batch = await asyncio.wait_for(self._flush_queue.get(), timeout=5.0)
                if batch:
                    await self._repo.batch_create_metrics(batch)
            except asyncio.TimeoutError:
                continue
            except Exception as e:
                logger.error("TSDB persistence error: %s", e)
