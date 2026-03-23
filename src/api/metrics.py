"""Metrics and dashboard API endpoints."""

from __future__ import annotations

from datetime import datetime, timedelta, timezone
from typing import Optional

from fastapi import APIRouter, Query, Request
from fastapi.responses import JSONResponse

router = APIRouter(prefix="/api")


def _parse_time_range(start: str | None, end: str | None, default_minutes: int = 30):
    end_dt = datetime.fromisoformat(end) if end else datetime.now(timezone.utc)
    start_dt = datetime.fromisoformat(start) if start else end_dt - timedelta(minutes=default_minutes)
    return start_dt, end_dt


@router.get("/metrics")
async def get_metric_buckets(
    request: Request,
    name: str = "",
    service_name: str = "",
    start: Optional[str] = None,
    end: Optional[str] = None,
):
    if not name:
        return JSONResponse({"error": "metric name is required"}, status_code=400)
    repo = request.app.state.repo
    start_dt = datetime.fromisoformat(start) if start else None
    end_dt = datetime.fromisoformat(end) if end else None
    return await repo.get_metric_buckets(start_dt, end_dt, service_name, name)


@router.get("/metrics/traffic")
async def get_traffic_metrics(
    request: Request,
    start: Optional[str] = None,
    end: Optional[str] = None,
    service_name: Optional[list[str]] = Query(None),
):
    repo = request.app.state.repo
    start_dt, end_dt = _parse_time_range(start, end)
    return await repo.get_traffic_metrics(start_dt, end_dt, service_name)


@router.get("/metrics/latency_heatmap")
async def get_latency_heatmap(
    request: Request,
    start: Optional[str] = None,
    end: Optional[str] = None,
    service_name: Optional[list[str]] = Query(None),
):
    repo = request.app.state.repo
    start_dt, end_dt = _parse_time_range(start, end)
    return await repo.get_latency_heatmap(start_dt, end_dt, service_name)


@router.get("/metrics/dashboard")
async def get_dashboard_stats(
    request: Request,
    start: Optional[str] = None,
    end: Optional[str] = None,
    service_name: Optional[list[str]] = Query(None),
):
    repo = request.app.state.repo
    start_dt, end_dt = _parse_time_range(start, end)
    return await repo.get_dashboard_stats(start_dt, end_dt, service_name)


@router.get("/metrics/service-map")
async def get_service_map_metrics(
    request: Request,
    start: Optional[str] = None,
    end: Optional[str] = None,
):
    repo = request.app.state.repo
    start_dt, end_dt = _parse_time_range(start, end)
    return await repo.get_service_map_metrics(start_dt, end_dt)


@router.get("/metadata/services")
async def get_services(request: Request):
    repo = request.app.state.repo
    return await repo.get_services()


@router.get("/metadata/metrics")
async def get_metric_names(request: Request, service_name: str = ""):
    repo = request.app.state.repo
    return await repo.get_metric_names(service_name)
