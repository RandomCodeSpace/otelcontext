"""Periodic DB rebuild and pruning for GraphRAG."""

from __future__ import annotations

import logging
from datetime import datetime, timedelta, timezone
from typing import TYPE_CHECKING

if TYPE_CHECKING:
    from src.graphrag.builder import GraphRAG

logger = logging.getLogger(__name__)


async def rebuild_from_db(g: "GraphRAG") -> None:
    """Load recent span data from the DB and merge into the graph.

    This catches data from before callbacks started (e.g., restart recovery).
    """
    since = datetime.now(timezone.utc) - timedelta(hours=1)

    try:
        rows = await g.repo.get_spans_for_graph(since)
    except Exception as e:
        logger.error("GraphRAG: failed to rebuild from DB: %s", e)
        return

    if not rows:
        return

    # Build spanID -> service map for edge resolution
    span_service: dict[str, str] = {}
    for r in rows:
        span_service[r["span_id"]] = r["service_name"]

    for r in rows:
        duration_ms = r["duration"] / 1000.0
        is_error = False  # simplified — status not stored in spans table directly

        g.service_store.upsert_service(r["service_name"], duration_ms, is_error, r["start_time"])
        if r["operation_name"]:
            g.service_store.upsert_operation(
                r["service_name"], r["operation_name"], duration_ms, is_error, r["start_time"]
            )

        # Cross-service edges
        if r["parent_span_id"]:
            parent_svc = span_service.get(r["parent_span_id"])
            if parent_svc and parent_svc != r["service_name"]:
                g.service_store.upsert_call_edge(
                    parent_svc, r["service_name"], duration_ms, is_error, r["start_time"]
                )

    logger.debug("GraphRAG rebuilt from DB: spans=%d services=%d", len(rows), len(g.service_store.services))
