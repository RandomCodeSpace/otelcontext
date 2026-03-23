"""OtelContext entrypoint: FastAPI app with all routes and background tasks."""

from __future__ import annotations

import asyncio
import logging
import os
import signal
import sys
import threading
from pathlib import Path

import uvicorn
from fastapi import FastAPI
from fastapi.staticfiles import StaticFiles
from fastapi.responses import FileResponse

# ---------------------------------------------------------------------------
# Logging
# ---------------------------------------------------------------------------
logging.basicConfig(
    level=logging.INFO,
    format="%(asctime)s %(levelname)s %(name)s: %(message)s",
    datefmt="%Y-%m-%dT%H:%M:%S",
)
logger = logging.getLogger("otelcontext")

VERSION = "6.0.0"


def print_banner():
    print(f"""
  ___ _____ _____ _
 / _ \\_   _| ____| |
| | | || | |  _| | |
| |_| || | | |___| |___
 \\___/ |_| |_____|_____|

  version: {VERSION} (Python/FastAPI)
""")


async def build_app() -> FastAPI:
    """Build and configure the FastAPI application."""
    from src.config import load_config, validate_config
    from src.db.session import init_db, get_session_factory, close_db
    from src.db.repository import Repository
    from src.telemetry.metrics import Metrics
    from src.vectordb.index import Index as VectorIndex
    from src.tsdb.aggregator import Aggregator
    from src.tsdb.ringbuffer import RingBuffer
    from src.graphrag.builder import GraphRAG, GraphRAGConfig
    from src.realtime.hub import Hub
    from src.realtime.events import EventHub
    from src.queue.dlq import DeadLetterQueue
    from src.archive.archiver import Archiver

    # 0. Load config
    cfg = load_config()
    validate_config(cfg)
    log_level = getattr(logging, cfg.log_level.upper(), logging.INFO)
    logging.getLogger().setLevel(log_level)
    logger.info("Starting OtelContext v%s env=%s", VERSION, cfg.env)

    # 1. Telemetry
    metrics = Metrics()
    logger.info("Internal telemetry initialized")

    # 2. Database
    await init_db(cfg)
    session_factory = get_session_factory()
    repo = Repository(session_factory)
    logger.info("Storage initialized: driver=%s", cfg.db_driver)

    # 3. DLQ
    dlq = DeadLetterQueue(
        directory=cfg.dlq_path,
        replay_interval=300.0,
        max_files=cfg.dlq_max_files,
        max_disk_mb=cfg.dlq_max_disk_mb,
        max_retries=cfg.dlq_max_retries,
    )
    logger.info("DLQ initialized: path=%s", cfg.dlq_path)

    # 4. WebSocket Hub
    hub = Hub(on_connection_change=lambda c: metrics.set_active_connections(c))
    hub.set_dev_mode(cfg.dev_mode)
    logger.info("WebSocket hub initialized")

    # 4b. Event Hub
    event_hub = EventHub(repo)
    logger.info("Event notification hub initialized")

    # 4c. TSDB
    ring_buf = RingBuffer(slots=120, window_dur=30.0)
    tsdb_agg = Aggregator(repo, window_seconds=30.0)
    tsdb_agg.set_ring_buffer(ring_buf)
    if cfg.metric_max_cardinality > 0:
        tsdb_agg.set_cardinality_limit(cfg.metric_max_cardinality)
    logger.info("TSDB ring buffer attached (120 slots x 30s = 1h)")

    # 4d. Archiver
    archiver = Archiver(
        repo,
        cold_path=cfg.cold_storage_path,
        hot_retention_days=cfg.hot_retention_days,
        schedule_hour=cfg.archive_schedule_hour,
        batch_size=cfg.archive_batch_size,
    )

    # 4e. Vector index
    vector_idx = VectorIndex(cfg.vector_index_max_entries)
    logger.info("Vector index initialized: max_entries=%d", cfg.vector_index_max_entries)

    # 4f. GraphRAG
    graphrag = GraphRAG(repo, vector_idx, tsdb_agg, ring_buf, GraphRAGConfig())
    logger.info("GraphRAG initialized")

    # 5. Build FastAPI app
    app = FastAPI(title="OtelContext", version=VERSION)

    # Store references on app.state
    app.state.repo = repo
    app.state.metrics_collector = metrics
    app.state.hub = hub
    app.state.event_hub = event_hub
    app.state.vector_idx = vector_idx
    app.state.graphrag = graphrag
    app.state.tsdb_agg = tsdb_agg
    app.state.ring_buf = ring_buf
    app.state.dlq = dlq
    app.state.archiver = archiver
    app.state.cold_storage_path = cfg.cold_storage_path
    app.state.cfg = cfg

    # 6. Configure OTLP HTTP ingestion
    from src.ingest import otlp_http, otlp_grpc

    otlp_http.configure(repo, metrics, tsdb_agg)
    otlp_grpc.configure(repo, metrics, tsdb_agg)

    # Wire callbacks
    def on_span(span_dict):
        graphrag.on_span_ingested(span_dict)

    def on_log(log_dict):
        event_hub.broadcast_log(log_dict)
        vector_idx.add(
            log_dict.get("id", 0),
            log_dict.get("service_name", ""),
            log_dict.get("severity", ""),
            log_dict.get("body", ""),
        )
        event_hub.notify_refresh()
        graphrag.on_log_ingested(log_dict)

    def on_metric(m):
        event_hub.broadcast_metric({
            "name": m.name, "service_name": m.service_name,
            "value": m.value, "timestamp": m.timestamp.isoformat(),
        })
        graphrag.on_metric_ingested(m)

    otlp_http.add_callback("span", on_span)
    otlp_http.add_callback("log", on_log)
    otlp_http.add_callback("metric", on_metric)
    otlp_grpc.add_callback("span", on_span)
    otlp_grpc.add_callback("log", on_log)
    otlp_grpc.add_callback("metric", on_metric)

    # 7. Register routes
    from src.ingest.otlp_http import router as otlp_router
    from src.api.traces import router as traces_router
    from src.api.logs import router as logs_router
    from src.api.metrics import router as metrics_router
    from src.api.graph import router as graph_router
    from src.api.admin import router as admin_router
    from src.api.health import router as health_router
    from src.mcp.server import router as mcp_router

    app.include_router(otlp_router)
    app.include_router(traces_router)
    app.include_router(logs_router)
    app.include_router(metrics_router)
    app.include_router(graph_router)
    app.include_router(admin_router)
    app.include_router(health_router)

    if cfg.mcp_enabled:
        mcp_path = cfg.mcp_path or "/mcp"
        app.include_router(mcp_router, prefix=mcp_path)
        logger.info("MCP endpoint registered: path=%s", mcp_path)

    # WebSocket routes
    from fastapi import WebSocket as WS

    @app.websocket("/ws")
    async def ws_endpoint(websocket: WS):
        await hub.handle_websocket(websocket)

    @app.websocket("/ws/events")
    async def ws_events_endpoint(websocket: WS):
        await event_hub.handle_websocket(websocket)

    @app.websocket("/ws/health")
    async def ws_health_endpoint(websocket: WS):
        await websocket.accept()
        try:
            while True:
                await asyncio.sleep(5)
                await websocket.send_json(metrics.health_data())
        except Exception:
            pass

    # 8. Static UI serving
    ui_dist = _find_ui_dist()
    if ui_dist:
        # SPA fallback: serve index.html for unmatched routes
        @app.get("/{full_path:path}")
        async def spa_fallback(full_path: str):
            file_path = ui_dist / full_path
            if file_path.is_file():
                return FileResponse(file_path)
            return FileResponse(ui_dist / "index.html")

        app.mount("/assets", StaticFiles(directory=str(ui_dist / "assets")), name="static-assets")
        logger.info("UI static files mounted from %s", ui_dist)
    else:
        logger.warning("UI dist directory not found; UI will not be served")

    # 9. Lifecycle: start/stop background tasks
    @app.on_event("startup")
    async def startup():
        await hub.start()
        await event_hub.start()
        await tsdb_agg.start()
        await graphrag.start()
        await dlq.start()
        await archiver.start()

        # Start gRPC in a thread
        grpc_server = otlp_grpc._start_grpc_server(cfg.grpc_port)
        app.state.grpc_server = grpc_server

        logger.info("All background tasks started")

    @app.on_event("shutdown")
    async def shutdown():
        logger.info("Shutting down OtelContext...")

        # 1. Stop ingestion
        grpc_server = getattr(app.state, "grpc_server", None)
        if grpc_server:
            grpc_server.stop(grace=5)

        # 2. Stop hubs
        await hub.stop()
        await event_hub.stop()

        # 3. Stop processing
        await tsdb_agg.stop()
        await archiver.stop()
        await graphrag.stop()

        # 4. Stop DLQ
        await dlq.stop()

        # 5. Close DB
        await close_db()
        logger.info("OtelContext shutdown complete")

    return app


def _find_ui_dist() -> Path | None:
    """Locate the built React UI dist directory."""
    candidates = [
        Path(__file__).parent.parent / "internal" / "ui" / "dist",
        Path(__file__).parent.parent / "ui" / "dist",
        Path("internal/ui/dist"),
        Path("ui/dist"),
    ]
    for p in candidates:
        if p.is_dir() and (p / "index.html").exists():
            return p
    return None


def main():
    print_banner()
    app = asyncio.run(build_app())

    port = int(os.environ.get("HTTP_PORT", "8080"))
    uvicorn.run(app, host="0.0.0.0", port=port, log_level="info")


if __name__ == "__main__":
    main()
