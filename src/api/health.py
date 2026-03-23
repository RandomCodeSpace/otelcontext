"""Prometheus metrics endpoint."""

from __future__ import annotations

from fastapi import APIRouter, Request
from fastapi.responses import Response

router = APIRouter()


@router.get("/metrics/prometheus")
async def prometheus_metrics(request: Request):
    metrics = getattr(request.app.state, "metrics_collector", None)
    if metrics:
        body, content_type = metrics.prometheus_response()
        return Response(content=body, media_type=content_type)
    return Response(content="", media_type="text/plain")
