"""File-based Dead Letter Queue with exponential backoff replay."""

from __future__ import annotations

import asyncio
import json
import logging
import math
import os
import time
from pathlib import Path
from typing import Any, Callable, Optional

logger = logging.getLogger(__name__)


class DeadLetterQueue:
    """Disk-based resilience for failed database writes.

    When a batch insert fails, data is serialized to JSON on disk.
    A background worker periodically re-inserts with exponential backoff.
    """

    def __init__(
        self,
        directory: str,
        replay_interval: float = 300.0,
        replay_fn: Callable[[bytes], Any] | None = None,
        max_files: int = 1000,
        max_disk_mb: int = 500,
        max_retries: int = 10,
    ):
        self._dir = Path(directory)
        self._dir.mkdir(parents=True, exist_ok=True)
        self._interval = replay_interval
        self._replay_fn = replay_fn
        self._max_files = max_files
        self._max_disk_mb = max_disk_mb
        self._max_retries = max_retries
        self._retries: dict[str, int] = {}
        self._stop_event = asyncio.Event()
        self._task: asyncio.Task | None = None

        # Metric callbacks
        self._on_enqueue: Callable[[], None] | None = None
        self._on_success: Callable[[], None] | None = None
        self._on_failure: Callable[[], None] | None = None
        self._on_disk_bytes: Callable[[int], None] | None = None

    def set_metrics(
        self,
        on_enqueue: Callable | None = None,
        on_success: Callable | None = None,
        on_failure: Callable | None = None,
        on_disk_bytes: Callable | None = None,
    ) -> None:
        self._on_enqueue = on_enqueue
        self._on_success = on_success
        self._on_failure = on_failure
        self._on_disk_bytes = on_disk_bytes

    def set_replay_fn(self, fn: Callable[[bytes], Any]) -> None:
        self._replay_fn = fn

    async def start(self) -> None:
        self._task = asyncio.create_task(self._replay_worker())

    async def stop(self) -> None:
        self._stop_event.set()
        if self._task:
            self._task.cancel()

    def enqueue(self, batch: Any) -> None:
        """Serialize batch to JSON and write to disk."""
        data = json.dumps(batch, default=str).encode()
        self._enforce_limits(len(data))
        filename = f"batch_{int(time.time() * 1e9)}.json"
        path = self._dir / filename
        path.write_bytes(data)
        logger.warning("Batch written to DLQ: file=%s bytes=%d", filename, len(data))
        if self._on_enqueue:
            self._on_enqueue()

    def size(self) -> int:
        return sum(1 for f in self._dir.iterdir() if f.suffix == ".json" and f.is_file())

    def disk_bytes(self) -> int:
        return sum(f.stat().st_size for f in self._dir.iterdir() if f.suffix == ".json" and f.is_file())

    def _enforce_limits(self, incoming_bytes: int) -> None:
        if self._max_files == 0 and self._max_disk_mb == 0:
            return
        files = sorted(
            [f for f in self._dir.iterdir() if f.suffix == ".json" and f.is_file()],
            key=lambda f: f.name,
        )
        total_bytes = sum(f.stat().st_size for f in files)
        max_bytes = self._max_disk_mb * 1024 * 1024

        i = 0
        while i < len(files):
            over_files = self._max_files > 0 and len(files) - i >= self._max_files
            over_disk = max_bytes > 0 and total_bytes + incoming_bytes > max_bytes
            if not over_files and not over_disk:
                break
            total_bytes -= files[i].stat().st_size
            files[i].unlink(missing_ok=True)
            self._retries.pop(files[i].name, None)
            logger.warning("DLQ FIFO eviction: %s", files[i].name)
            i += 1

    async def _replay_worker(self) -> None:
        while not self._stop_event.is_set():
            await asyncio.sleep(self._interval)
            await self._process_files()

    async def _process_files(self) -> None:
        if self._replay_fn is None:
            return

        files = sorted(
            [f for f in self._dir.iterdir() if f.suffix == ".json" and f.is_file()],
            key=lambda f: f.name,
        )
        replayed = 0

        for fpath in files:
            name = fpath.name
            retries = self._retries.get(name, 0)

            if self._max_retries > 0 and retries >= self._max_retries:
                fpath.unlink(missing_ok=True)
                self._retries.pop(name, None)
                logger.error("DLQ max retries exceeded, dropping: %s", name)
                continue

            # Exponential backoff
            if retries > 0:
                backoff = min((2 ** (retries - 1)) * self._interval, 1800)
                age = time.time() - fpath.stat().st_mtime
                if age < backoff:
                    continue

            data = fpath.read_bytes()
            try:
                result = self._replay_fn(data)
                if asyncio.iscoroutine(result):
                    await result
            except Exception as e:
                self._retries[name] = retries + 1
                logger.warning("DLQ replay failed: file=%s retries=%d error=%s", name, retries + 1, e)
                if self._on_failure:
                    self._on_failure()
                # Touch file to reset backoff timer
                fpath.touch()
                continue

            fpath.unlink(missing_ok=True)
            self._retries.pop(name, None)
            replayed += 1
            logger.info("DLQ file replayed: %s", name)
            if self._on_success:
                self._on_success()

        if replayed > 0:
            logger.info("DLQ replay cycle: replayed=%d", replayed)
