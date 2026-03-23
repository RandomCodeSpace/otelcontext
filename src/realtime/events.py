"""Event hub for live-mode data snapshot pushes (separate from the log/metric hub)."""

from __future__ import annotations

import asyncio
import json
import logging
from typing import Any, Callable, Optional

from fastapi import WebSocket, WebSocketDisconnect

logger = logging.getLogger(__name__)


class EventHub:
    """Pushes periodic data snapshots and real-time notifications to connected clients."""

    def __init__(self, repo: Any, on_connect: Callable | None = None, on_disconnect: Callable | None = None):
        self._repo = repo
        self._clients: dict[WebSocket, asyncio.Queue] = {}
        self._lock = asyncio.Lock()
        self._stop_event = asyncio.Event()
        self._tasks: list[asyncio.Task] = []
        self._on_connect = on_connect
        self._on_disconnect = on_disconnect
        self._pending_refresh = False

    async def start(self, snapshot_interval: float = 5.0) -> None:
        self._tasks.append(asyncio.create_task(self._snapshot_loop(snapshot_interval)))

    async def stop(self) -> None:
        self._stop_event.set()
        for t in self._tasks:
            t.cancel()

    def notify_refresh(self) -> None:
        """Signal that new data has been ingested."""
        self._pending_refresh = True

    def broadcast_log(self, entry: dict) -> None:
        """Push a single log entry notification."""
        self._broadcast({"type": "log", "data": entry})

    def broadcast_metric(self, entry: dict) -> None:
        """Push a single metric entry notification."""
        self._broadcast({"type": "metric", "data": entry})

    def _broadcast(self, payload: dict) -> None:
        data = json.dumps(payload, default=str).encode()
        # Fire-and-forget style
        for ws, q in list(self._clients.items()):
            try:
                q.put_nowait(data)
            except asyncio.QueueFull:
                pass

    async def handle_websocket(self, ws: WebSocket) -> None:
        await ws.accept()
        q: asyncio.Queue[bytes] = asyncio.Queue(maxsize=64)
        async with self._lock:
            self._clients[ws] = q
        if self._on_connect:
            self._on_connect()

        writer_task = asyncio.create_task(self._writer(ws, q))
        try:
            while True:
                await ws.receive_text()
        except (WebSocketDisconnect, Exception):
            pass
        finally:
            writer_task.cancel()
            async with self._lock:
                self._clients.pop(ws, None)
            if self._on_disconnect:
                self._on_disconnect()

    async def _writer(self, ws: WebSocket, q: asyncio.Queue[bytes]) -> None:
        try:
            while True:
                data = await q.get()
                await ws.send_bytes(data)
        except Exception:
            pass

    async def _snapshot_loop(self, interval: float) -> None:
        while not self._stop_event.is_set():
            await asyncio.sleep(interval)
            if self._pending_refresh and self._clients:
                self._pending_refresh = False
                try:
                    stats = await self._repo.get_stats()
                    self._broadcast({"type": "snapshot", "data": stats})
                except Exception as e:
                    logger.error("EventHub snapshot error: %s", e)
