"""Daily archival of old data to compressed JSONL files with SHA256 manifest."""

from __future__ import annotations

import asyncio
import gzip
import hashlib
import json
import logging
from datetime import datetime, timedelta, timezone
from pathlib import Path
from typing import Any

logger = logging.getLogger(__name__)


class Archiver:
    """Hot/cold storage tiering: archives old logs and traces to compressed JSONL."""

    def __init__(
        self,
        repo: Any,
        cold_path: str = "./data/cold",
        hot_retention_days: int = 7,
        schedule_hour: int = 2,
        batch_size: int = 10000,
    ):
        self._repo = repo
        self._cold_path = Path(cold_path)
        self._hot_retention_days = hot_retention_days
        self._schedule_hour = schedule_hour
        self._batch_size = batch_size
        self._stop_event = asyncio.Event()
        self._task: asyncio.Task | None = None

    async def start(self) -> None:
        self._task = asyncio.create_task(self._schedule_loop())

    async def stop(self) -> None:
        self._stop_event.set()
        if self._task:
            self._task.cancel()

    async def _schedule_loop(self) -> None:
        while not self._stop_event.is_set():
            now = datetime.now(timezone.utc)
            # Wait until the next schedule_hour
            target = now.replace(hour=self._schedule_hour, minute=0, second=0, microsecond=0)
            if target <= now:
                target += timedelta(days=1)
            wait = (target - now).total_seconds()
            try:
                await asyncio.sleep(min(wait, 3600))  # wake at most hourly to check
                now2 = datetime.now(timezone.utc)
                if now2.hour == self._schedule_hour:
                    await self.run_archive()
            except asyncio.CancelledError:
                return

    async def run_archive(self) -> None:
        """Archive old data beyond hot retention."""
        cutoff = datetime.now(timezone.utc) - timedelta(days=self._hot_retention_days)
        logger.info("Starting archival: cutoff=%s", cutoff.isoformat())

        # Archive logs
        try:
            logs = await self._repo.get_old_logs(cutoff, self._batch_size)
            if logs:
                await self._write_archive("logs", cutoff, logs)
                await self._repo.purge_logs(cutoff)
                logger.info("Archived %d logs", len(logs))
        except Exception as e:
            logger.error("Failed to archive logs: %s", e)

        # Archive traces
        try:
            traces = await self._repo.get_old_traces(cutoff, self._batch_size)
            if traces:
                await self._write_archive("traces", cutoff, traces)
                await self._repo.purge_traces(cutoff)
                logger.info("Archived %d traces", len(traces))
        except Exception as e:
            logger.error("Failed to archive traces: %s", e)

    async def _write_archive(self, data_type: str, cutoff: datetime, records: list[dict]) -> None:
        """Write records as gzipped JSONL with SHA256 manifest."""
        now = datetime.now(timezone.utc)
        dir_path = self._cold_path / str(now.year) / f"{now.month:02d}" / f"{now.day:02d}"
        dir_path.mkdir(parents=True, exist_ok=True)

        file_path = dir_path / f"{data_type}.jsonl.gz"
        sha = hashlib.sha256()

        with gzip.open(file_path, "wt", encoding="utf-8") as f:
            for record in records:
                line = json.dumps(record, default=str) + "\n"
                f.write(line)
                sha.update(line.encode())

        # Write manifest
        manifest_path = dir_path / f"{data_type}.manifest.json"
        manifest = {
            "type": data_type,
            "count": len(records),
            "cutoff": cutoff.isoformat(),
            "file": str(file_path),
            "sha256": sha.hexdigest(),
            "created_at": now.isoformat(),
        }
        manifest_path.write_text(json.dumps(manifest, indent=2))


async def search_cold_archive(cold_path: str, query: str, data_type: str = "logs", limit: int = 100) -> list[dict]:
    """Search through cold archive files for matching records."""
    results: list[dict] = []
    base = Path(cold_path)
    if not base.exists():
        return results

    for gz_file in sorted(base.rglob(f"{data_type}.jsonl.gz"), reverse=True):
        try:
            with gzip.open(gz_file, "rt", encoding="utf-8") as f:
                for line in f:
                    if query.lower() in line.lower():
                        try:
                            results.append(json.loads(line))
                        except json.JSONDecodeError:
                            pass
                        if len(results) >= limit:
                            return results
        except Exception:
            continue

    return results
