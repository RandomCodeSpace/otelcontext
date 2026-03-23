"""Admin and system endpoints: stats, health, purge, vacuum, archive search."""

from __future__ import annotations

from datetime import datetime, timedelta, timezone
from typing import Optional

from fastapi import APIRouter, Query, Request
from fastapi.responses import JSONResponse, Response

router = APIRouter()


@router.get("/api/stats")
async def get_stats(request: Request):
    repo = request.app.state.repo
    return await repo.get_stats()


@router.get("/api/health")
async def health(request: Request):
    metrics = getattr(request.app.state, "metrics_collector", None)
    if metrics:
        return metrics.health_data()
    return {"status": "ok"}


@router.delete("/api/admin/purge")
async def purge(request: Request, days: int = Query(7, ge=1)):
    repo = request.app.state.repo
    cutoff = datetime.now(timezone.utc) - timedelta(days=days)
    logs_deleted = await repo.purge_logs(cutoff)
    traces_deleted = await repo.purge_traces(cutoff)
    return {
        "logs_purged": logs_deleted,
        "traces_purged": traces_deleted,
        "cutoff": cutoff.isoformat(),
    }


@router.post("/api/admin/vacuum")
async def vacuum(request: Request):
    repo = request.app.state.repo
    try:
        await repo.vacuum_db()
    except Exception as e:
        return JSONResponse({"error": str(e)}, status_code=500)
    return {"status": "vacuumed"}


@router.get("/api/archive/search")
async def search_archive(
    request: Request,
    query: str = "",
    type: str = "logs",
    limit: int = Query(100, ge=1, le=1000),
):
    cold_path = getattr(request.app.state, "cold_storage_path", "./data/cold")
    from src.archive.archiver import search_cold_archive
    results = await search_cold_archive(cold_path, query, type, limit)
    return results
