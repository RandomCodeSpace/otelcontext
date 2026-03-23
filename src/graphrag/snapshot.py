"""Periodic graph snapshots persisted to DB."""

from __future__ import annotations

import json
import logging
import time
from datetime import datetime, timedelta, timezone
from typing import TYPE_CHECKING

if TYPE_CHECKING:
    from src.graphrag.builder import GraphRAG

logger = logging.getLogger(__name__)


async def take_snapshot(g: "GraphRAG") -> None:
    """Capture the current service topology and persist it."""
    services = g.service_store.all_services()
    edges = g.service_store.all_edges()

    if not services:
        return

    nodes_data = []
    total_calls = 0
    total_health = 0.0

    for svc in services:
        nodes_data.append({
            "id": svc.id,
            "type": "service",
            "name": svc.name,
            "health_score": svc.health_score,
            "error_rate": svc.error_rate,
            "avg_latency_ms": svc.avg_latency,
        })
        total_calls += svc.call_count
        total_health += svc.health_score

    # Include operations
    with g.service_store._lock:
        for op in g.service_store.operations.values():
            nodes_data.append({
                "id": op.id,
                "type": "operation",
                "name": op.operation,
                "health_score": op.health_score,
                "error_rate": op.error_rate,
                "avg_latency_ms": op.avg_latency,
            })

    edges_data = [
        {
            "from": e.from_id,
            "to": e.to_id,
            "type": e.type.value,
            "weight": e.weight,
            "call_count": e.call_count,
            "error_rate": e.error_rate,
        }
        for e in edges
    ]

    snap = {
        "id": f"snap_{int(time.time() * 1e9)}",
        "created_at": datetime.now(timezone.utc),
        "nodes": json.dumps(nodes_data),
        "edges": json.dumps(edges_data),
        "service_count": len(services),
        "total_calls": total_calls,
        "avg_health_score": total_health / len(services) if services else 0,
    }

    try:
        await g.repo.create_graph_snapshot(snap)
        logger.debug("Graph snapshot persisted: services=%d edges=%d", len(services), len(edges_data))
    except Exception as e:
        logger.error("Failed to persist graph snapshot: %s", e)


async def prune_old_snapshots(g: "GraphRAG") -> None:
    """Remove snapshots older than 7 days."""
    cutoff = datetime.now(timezone.utc) - timedelta(days=7)
    try:
        count = await g.repo.prune_old_snapshots(cutoff)
        if count > 0:
            logger.info("Pruned old graph snapshots: count=%d", count)
    except Exception as e:
        logger.error("Failed to prune old snapshots: %s", e)
