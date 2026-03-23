"""Per-service adaptive token bucket sampler."""

from __future__ import annotations

import random
import threading
import time
from dataclasses import dataclass


@dataclass
class _TokenBucket:
    tokens: float
    last_refill: float
    rate: float  # tokens per second (= sampling_rate)

    def allow(self) -> bool:
        now = time.monotonic()
        elapsed = now - self.last_refill
        self.tokens = min(self.rate, self.tokens + elapsed * self.rate)
        self.last_refill = now
        if self.tokens >= 1.0:
            self.tokens -= 1.0
            return True
        return False


class Sampler:
    """Adaptive per-service sampler with always-on errors and latency threshold."""

    def __init__(
        self,
        rate: float = 1.0,
        always_on_errors: bool = True,
        latency_threshold_ms: float = 500.0,
    ):
        self._rate = rate
        self._always_on_errors = always_on_errors
        self._latency_threshold_ms = latency_threshold_ms
        self._buckets: dict[str, _TokenBucket] = {}
        self._lock = threading.Lock()

    def should_sample(
        self,
        service_name: str,
        is_error: bool = False,
        duration_ms: float = 0.0,
    ) -> bool:
        """Return True if this span/log should be kept."""
        if self._rate >= 1.0:
            return True
        if self._always_on_errors and is_error:
            return True
        if self._latency_threshold_ms > 0 and duration_ms >= self._latency_threshold_ms:
            return True

        with self._lock:
            bucket = self._buckets.get(service_name)
            if bucket is None:
                bucket = _TokenBucket(tokens=self._rate, last_refill=time.monotonic(), rate=self._rate)
                self._buckets[service_name] = bucket
            return bucket.allow()
