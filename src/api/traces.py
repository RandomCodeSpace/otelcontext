"""Trace API endpoints."""

from __future__ import annotations

from datetime import datetime
from typing import Optional

from fastapi import APIRouter, Query, Request

router = APIRouter(prefix="/api")


@router.get("/traces")
async def get_traces(
    request: Request,
    limit: int = Query(20, ge=1, le=1000),
    offset: int = Query(0, ge=0),
    start: Optional[str] = None,
    end: Optional[str] = None,
    service_name: Optional[list[str]] = Query(None),
    status: Optional[str] = None,
    search: Optional[str] = None,
    sort_by: Optional[str] = None,
    order_by: Optional[str] = None,
):
    repo = request.app.state.repo
    start_dt = datetime.fromisoformat(start) if start else None
    end_dt = datetime.fromisoformat(end) if end else None
    return await repo.get_traces_filtered(
        start=start_dt,
        end=end_dt,
        service_names=service_name,
        status=status,
        search=search,
        limit=limit,
        offset=offset,
        sort_by=sort_by,
        order_by=order_by,
    )


@router.get("/traces/{trace_id}")
async def get_trace(request: Request, trace_id: str):
    repo = request.app.state.repo
    trace = await repo.get_trace(trace_id)
    if trace is None:
        from fastapi.responses import JSONResponse
        return JSONResponse({"error": "trace not found"}, status_code=404)
    return trace
