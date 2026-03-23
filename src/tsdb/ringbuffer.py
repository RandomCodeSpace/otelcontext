"""In-memory ring buffer for per-metric sliding windows with pre-computed aggregates."""

from __future__ import annotations

import math
import threading
import time as _time
from dataclasses import dataclass, field
from datetime import datetime, timezone
from typing import Dict, List, Optional


@dataclass
class WindowAgg:
    metric_name: str = ""
    service_name: str = ""
    window_start: float = 0.0  # unix timestamp
    count: int = 0
    sum: float = 0.0
    min: float = float("inf")
    max: float = float("-inf")
    p50: float = 0.0
    p95: float = 0.0
    p99: float = 0.0
    samples: list[float] = field(default_factory=list)

    def to_dict(self) -> dict:
        return {
            "metric_name": self.metric_name,
            "service_name": self.service_name,
            "window_start": datetime.fromtimestamp(self.window_start, tz=timezone.utc).isoformat()
            if self.window_start
            else None,
            "count": self.count,
            "sum": self.sum,
            "min": self.min if self.min != float("inf") else 0,
            "max": self.max if self.max != float("-inf") else 0,
            "p50": self.p50,
            "p95": self.p95,
            "p99": self.p99,
        }


MAX_SAMPLES = 256


def _percentile(data: list[float], p: float) -> float:
    if not data:
        return 0.0
    s = sorted(data)
    idx = int(math.ceil(p / 100.0 * len(s))) - 1
    idx = max(0, min(idx, len(s) - 1))
    return s[idx]


class MetricRing:
    """Fixed-size circular buffer for a single metric+service key."""

    def __init__(self, metric_name: str, service_name: str, slots: int, window_dur: float):
        self._lock = threading.Lock()
        self._slots: list[WindowAgg] = [
            WindowAgg(metric_name=metric_name, service_name=service_name) for _ in range(slots)
        ]
        self._size = slots
        self._window_dur = window_dur
        self._metric_name = metric_name
        self._service_name = service_name
        self._current_idx = 0
        now = _time.time()
        self._current_start = now - (now % window_dur)
        self._slots[0].window_start = self._current_start

    def record(self, value: float, at: float) -> None:
        with self._lock:
            window_start = at - (at % self._window_dur)
            if window_start > self._current_start:
                steps = int((window_start - self._current_start) / self._window_dur)
                for _ in range(min(steps, self._size)):
                    self._current_idx = (self._current_idx + 1) % self._size
                    self._current_start += self._window_dur
                    slot = self._slots[self._current_idx]
                    slot.count = 0
                    slot.sum = 0.0
                    slot.min = float("inf")
                    slot.max = float("-inf")
                    slot.samples = []
                    slot.window_start = self._current_start
                    slot.metric_name = self._metric_name
                    slot.service_name = self._service_name

            slot = self._slots[self._current_idx]
            slot.count += 1
            slot.sum += value
            if value < slot.min:
                slot.min = value
            if value > slot.max:
                slot.max = value
            if len(slot.samples) < MAX_SAMPLES:
                slot.samples.append(value)

    def windows(self, n: int) -> list[WindowAgg]:
        with self._lock:
            n = min(n, self._size)
            snapshots = []
            for i in range(n):
                idx = (self._current_idx - i + self._size) % self._size
                slot = self._slots[idx]
                if slot.count > 0:
                    agg = WindowAgg(
                        metric_name=slot.metric_name,
                        service_name=slot.service_name,
                        window_start=slot.window_start,
                        count=slot.count,
                        sum=slot.sum,
                        min=slot.min,
                        max=slot.max,
                        samples=list(slot.samples),
                    )
                    snapshots.append(agg)

        # Compute percentiles outside the lock
        result = []
        for agg in snapshots:
            agg.p50 = _percentile(agg.samples, 50)
            agg.p95 = _percentile(agg.samples, 95)
            agg.p99 = _percentile(agg.samples, 99)
            if agg.min == float("inf"):
                agg.min = 0.0
            if agg.max == float("-inf"):
                agg.max = 0.0
            agg.samples = []
            result.append(agg)
        return result


class RingBuffer:
    """Manages per-metric ring buffers, keyed by 'service|metric'."""

    def __init__(self, slots: int = 120, window_dur: float = 30.0):
        self._lock = threading.RLock()
        self._rings: dict[str, MetricRing] = {}
        self._slots = slots
        self._window_dur = window_dur

    def record(self, metric_name: str, service_name: str, value: float, at: datetime | None = None) -> None:
        key = f"{service_name}|{metric_name}"
        ts = at.timestamp() if at else _time.time()

        with self._lock:
            ring = self._rings.get(key)
            if ring is None:
                ring = MetricRing(metric_name, service_name, self._slots, self._window_dur)
                self._rings[key] = ring

        ring.record(value, ts)

    def query_recent(self, metric_name: str, service_name: str, window_count: int = 10) -> list[WindowAgg]:
        key = f"{service_name}|{metric_name}"
        with self._lock:
            ring = self._rings.get(key)
        if ring is None:
            return []
        return ring.windows(window_count)

    def all_keys(self) -> list[str]:
        with self._lock:
            return list(self._rings.keys())

    def metric_count(self) -> int:
        with self._lock:
            return len(self._rings)
