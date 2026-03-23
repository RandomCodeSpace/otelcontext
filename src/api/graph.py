"""System graph API endpoint (AI-consumable topology + health)."""

from __future__ import annotations

import math
import time
from datetime import datetime, timezone
from typing import Any, Optional

from fastapi import APIRouter, Request
from fastapi.responses import JSONResponse

router = APIRouter(prefix="/api")

_START_TIME = time.time()

# Simple TTL cache
_cache: dict[str, tuple[float, Any]] = {}
_CACHE_TTL = 10.0


def _compute_health_score(error_rate: float, avg_latency_ms: float) -> float:
    score = 1.0 - (error_rate * 5.0)
    if avg_latency_ms > 200:
        score -= (avg_latency_ms - 200) / 2000
    return max(0.0, min(1.0, round(score * 100) / 100))


def _health_status(score: float) -> str:
    if score >= 0.9:
        return "healthy"
    if score >= 0.7:
        return "degraded"
    return "critical"


def _build_alerts(error_rate: float, avg_latency_ms: float) -> list[str]:
    alerts = []
    if error_rate > 0.05:
        alerts.append("error rate above 5%")
    if error_rate > 0.10:
        alerts.append("error rate above 10% - investigate immediately")
    if avg_latency_ms > 500:
        alerts.append("avg latency above 500ms")
    if avg_latency_ms > 1000:
        alerts.append("avg latency above 1s - SLA breach risk")
    return alerts


@router.get("/system/graph")
async def get_system_graph(request: Request):
    now = time.time()

    # Check cache
    cached = _cache.get("system_graph")
    if cached and now - cached[0] < _CACHE_TTL:
        return JSONResponse(content=cached[1], headers={"X-Cache": "HIT"})

    graphrag = getattr(request.app.state, "graphrag", None)
    resp = None

    if graphrag is not None:
        resp = _build_from_graphrag(graphrag)

    if resp is None:
        resp = _build_empty()

    _cache["system_graph"] = (now, resp)
    return JSONResponse(content=resp, headers={"X-Cache": "MISS"})


def _build_from_graphrag(graphrag: Any) -> dict | None:
    from src.graphrag.queries import service_map

    entries = service_map(graphrag, 0)
    if not entries:
        return None

    nodes = []
    total_error_rate = 0.0
    total_latency = 0.0

    for entry in entries:
        svc = entry.service
        if svc is None:
            continue
        alerts = _build_alerts(svc.error_rate, svc.avg_latency)
        nodes.append({
            "id": svc.name,
            "type": "service",
            "health_score": round(svc.health_score * 100) / 100,
            "status": _health_status(svc.health_score),
            "metrics": {
                "request_rate_rps": round(svc.call_count / 300 * 100) / 100,
                "error_rate": round(svc.error_rate * 1000000) / 1000000,
                "avg_latency_ms": round(svc.avg_latency * 100) / 100,
                "p99_latency_ms": round(svc.avg_latency * 2.5 * 100) / 100,
                "span_count_1h": svc.call_count,
            },
            "alerts": alerts,
        })
        total_error_rate += svc.error_rate
        total_latency += svc.avg_latency

    edges = []
    all_edges = graphrag.service_store.all_edges()
    for e in all_edges:
        if e.type.value == "CALLS":
            edges.append({
                "source": e.from_id,
                "target": e.to_id,
                "call_count": e.call_count,
                "avg_latency_ms": round(e.avg_ms * 100) / 100,
                "error_rate": round(e.error_rate * 1000000) / 1000000,
                "status": _health_status(_compute_health_score(e.error_rate, e.avg_ms)),
            })

    return _build_summary(nodes, edges, total_error_rate, total_latency)


def _build_summary(nodes: list, edges: list, total_error_rate: float, total_latency: float) -> dict:
    healthy = sum(1 for n in nodes if n["status"] == "healthy")
    degraded = sum(1 for n in nodes if n["status"] == "degraded")
    critical = sum(1 for n in nodes if n["status"] == "critical")

    n_count = max(len(nodes), 1)
    overall = max(0.0, round((1.0 - total_error_rate / n_count) * 100) / 100)
    avg_lat = round(total_latency / n_count * 100) / 100 if nodes else 0

    return {
        "timestamp": datetime.now(timezone.utc).isoformat(),
        "system": {
            "total_services": len(nodes),
            "healthy": healthy,
            "degraded": degraded,
            "critical": critical,
            "overall_health_score": overall,
            "total_error_rate": round(total_error_rate / n_count * 10000) / 10000,
            "avg_latency_ms": avg_lat,
            "uptime_seconds": time.time() - _START_TIME,
        },
        "nodes": nodes,
        "edges": edges,
    }


def _build_empty() -> dict:
    return _build_summary([], [], 0.0, 0.0)
