"""Buffered WebSocket broadcast hub for real-time log and metric streaming."""

from __future__ import annotations

import asyncio
import json
import logging
from dataclasses import dataclass, field
from datetime import datetime
from typing import Any, Callable, Optional

from fastapi import WebSocket, WebSocketDisconnect

logger = logging.getLogger(__name__)


@dataclass
class LogEntry:
    id: int = 0
    trace_id: str = ""
    span_id: str = ""
    severity: str = ""
    body: str = ""
    service_name: str = ""
    attributes_json: str = ""
    ai_insight: str = ""
    timestamp: str = ""


@dataclass
class MetricEntry:
    name: str = ""
    service_name: str = ""
    value: float = 0.0
    timestamp: str = ""
    attributes: dict = field(default_factory=dict)


class Hub:
    """Buffered WebSocket broadcast hub.

    Buffers entries and flushes as JSON array when buffer >= 100 or every 500ms.
    Slow clients are evicted.
    """

    def __init__(self, on_connection_change: Callable[[int], None] | None = None):
        self._clients: dict[WebSocket, asyncio.Queue] = {}
        self._lock = asyncio.Lock()
        self._log_buffer: list[dict] = []
        self._metric_buffer: list[dict] = []
        self._max_buffer = 100
        self._flush_interval = 0.5
        self._on_connection_change = on_connection_change
        self._stop_event = asyncio.Event()
        self._tasks: list[asyncio.Task] = []
        self._dev_mode = False

    def set_dev_mode(self, dev: bool) -> None:
        self._dev_mode = dev

    async def start(self) -> None:
        self._tasks.append(asyncio.create_task(self._flush_loop()))

    async def stop(self) -> None:
        self._stop_event.set()
        await self._flush()
        for t in self._tasks:
            t.cancel()
        async with self._lock:
            for ws, q in self._clients.items():
                try:
                    await ws.close()
                except Exception:
                    pass
            self._clients.clear()

    async def handle_websocket(self, ws: WebSocket) -> None:
        """WebSocket endpoint handler."""
        await ws.accept()
        q: asyncio.Queue[bytes] = asyncio.Queue(maxsize=256)
        async with self._lock:
            self._clients[ws] = q
            count = len(self._clients)
        logger.info("WebSocket client connected: total=%d", count)
        if self._on_connection_change:
            self._on_connection_change(count)

        writer_task = asyncio.create_task(self._writer(ws, q))
        try:
            while True:
                await ws.receive_text()
        except WebSocketDisconnect:
            pass
        except Exception:
            pass
        finally:
            writer_task.cancel()
            async with self._lock:
                self._clients.pop(ws, None)
                count = len(self._clients)
            logger.info("WebSocket client disconnected: total=%d", count)
            if self._on_connection_change:
                self._on_connection_change(count)

    async def _writer(self, ws: WebSocket, q: asyncio.Queue[bytes]) -> None:
        try:
            while True:
                data = await q.get()
                await ws.send_bytes(data)
        except Exception:
            pass

    def broadcast_log(self, entry: dict) -> None:
        self._log_buffer.append(entry)
        if len(self._log_buffer) >= self._max_buffer:
            asyncio.get_event_loop().create_task(self._flush())

    def broadcast_metric(self, entry: dict) -> None:
        self._metric_buffer.append(entry)
        if len(self._metric_buffer) >= self._max_buffer:
            asyncio.get_event_loop().create_task(self._flush())

    async def _flush_loop(self) -> None:
        while not self._stop_event.is_set():
            await asyncio.sleep(self._flush_interval)
            await self._flush()

    async def _flush(self) -> None:
        logs = self._log_buffer
        self._log_buffer = []
        metrics = self._metric_buffer
        self._metric_buffer = []

        if logs:
            await self._broadcast_batch({"type": "logs", "data": logs})
        if metrics:
            await self._broadcast_batch({"type": "metrics", "data": metrics})

    async def _broadcast_batch(self, batch: dict) -> None:
        data = json.dumps(batch, default=str).encode()
        slow: list[WebSocket] = []

        async with self._lock:
            for ws, q in self._clients.items():
                try:
                    q.put_nowait(data)
                except asyncio.QueueFull:
                    slow.append(ws)

            for ws in slow:
                self._clients.pop(ws, None)
                logger.warning("Slow WebSocket client evicted")
                try:
                    await ws.close()
                except Exception:
                    pass
