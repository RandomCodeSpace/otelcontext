"""Log API endpoints."""

from __future__ import annotations

from datetime import datetime
from typing import Optional

from fastapi import APIRouter, Query, Request
from fastapi.responses import JSONResponse

router = APIRouter(prefix="/api")


@router.get("/logs")
async def get_logs(
    request: Request,
    limit: int = Query(50, ge=1, le=1000),
    offset: int = Query(0, ge=0),
    service_name: Optional[str] = None,
    severity: Optional[str] = None,
    search: Optional[str] = None,
    start: Optional[str] = None,
    end: Optional[str] = None,
):
    repo = request.app.state.repo
    start_dt = datetime.fromisoformat(start) if start else None
    end_dt = datetime.fromisoformat(end) if end else None
    logs, total = await repo.get_logs_v2(
        service_name=service_name or "",
        severity=severity or "",
        search=search or "",
        start_time=start_dt,
        end_time=end_dt,
        limit=limit,
        offset=offset,
    )
    return {"data": logs, "total": total}


@router.get("/logs/context")
async def get_log_context(request: Request, timestamp: str):
    repo = request.app.state.repo
    try:
        ts = datetime.fromisoformat(timestamp)
    except ValueError:
        return JSONResponse({"error": "invalid timestamp"}, status_code=400)
    return await repo.get_log_context(ts)


@router.get("/logs/similar")
async def get_similar_logs(request: Request, query: str = "", k: int = 10):
    vector_idx = getattr(request.app.state, "vector_idx", None)
    if vector_idx is None or not query:
        return []
    results = vector_idx.search(query, k=k)
    return [
        {
            "log_id": r.log_id,
            "service_name": r.service_name,
            "severity": r.severity,
            "body": r.body,
            "score": r.score,
        }
        for r in results
    ]


@router.get("/logs/{log_id}/insight")
async def get_log_insight(request: Request, log_id: int):
    repo = request.app.state.repo
    log = await repo.get_log(log_id)
    if log is None:
        return JSONResponse({"error": "log not found"}, status_code=404)
    return {"insight": log.get("ai_insight", "")}
