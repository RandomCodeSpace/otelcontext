"""Async CRUD repository for all OtelContext models."""

from __future__ import annotations

import logging
from datetime import datetime, timedelta, timezone
from typing import Any, Optional, Sequence

from sqlalchemy import delete, func, select, text, and_
from sqlalchemy.ext.asyncio import AsyncSession, async_sessionmaker

from src.db.models import (
    GraphSnapshot,
    Investigation,
    Log,
    MetricBucket,
    Span,
    Trace,
)

logger = logging.getLogger(__name__)


class Repository:
    """Thin async wrapper around SQLAlchemy for all data access."""

    def __init__(self, session_factory: async_sessionmaker[AsyncSession]):
        self._sf = session_factory

    # ------------------------------------------------------------------
    # Spans
    # ------------------------------------------------------------------

    async def batch_create_spans(self, spans: list[dict]) -> None:
        if not spans:
            return
        async with self._sf() as session:
            for batch_start in range(0, len(spans), 500):
                batch = spans[batch_start : batch_start + 500]
                objs = [Span(**s) for s in batch]
                session.add_all(objs)
            await session.commit()

    # ------------------------------------------------------------------
    # Traces
    # ------------------------------------------------------------------

    async def batch_create_traces(self, traces: list[dict]) -> None:
        if not traces:
            return
        async with self._sf() as session:
            for batch_start in range(0, len(traces), 500):
                batch = traces[batch_start : batch_start + 500]
                objs = [Trace(**t) for t in batch]
                session.add_all(objs)
            await session.commit()

    async def get_trace(self, trace_id: str) -> dict | None:
        async with self._sf() as session:
            stmt = select(Trace).where(Trace.trace_id == trace_id)
            result = await session.execute(stmt)
            trace = result.scalar_one_or_none()
            if trace is None:
                return None
            return trace.to_dict()

    async def get_traces_filtered(
        self,
        start: datetime | None,
        end: datetime | None,
        service_names: list[str] | None,
        status: str | None,
        search: str | None,
        limit: int = 20,
        offset: int = 0,
        sort_by: str | None = None,
        order_by: str | None = None,
    ) -> dict:
        async with self._sf() as session:
            base = select(Trace)
            count_base = select(func.count(Trace.id))
            conditions = []

            if start:
                conditions.append(Trace.timestamp >= start)
            if end:
                conditions.append(Trace.timestamp <= end)
            if service_names:
                conditions.append(Trace.service_name.in_(service_names))
            if status:
                conditions.append(Trace.status == status)
            if search:
                conditions.append(Trace.trace_id.like(f"%{search}%"))

            if conditions:
                base = base.where(and_(*conditions))
                count_base = count_base.where(and_(*conditions))

            count_result = await session.execute(count_base)
            total = count_result.scalar() or 0

            # Ordering
            order_col = Trace.timestamp
            if sort_by == "duration":
                order_col = Trace.duration
            base = base.order_by(order_col.desc() if order_by != "asc" else order_col.asc())
            base = base.limit(limit).offset(offset)

            result = await session.execute(base)
            traces = result.scalars().all()
            return {
                "data": [t.to_dict() for t in traces],
                "total": total,
            }

    async def purge_traces(self, cutoff: datetime) -> int:
        async with self._sf() as session:
            # Delete spans first
            await session.execute(delete(Span).where(Span.start_time < cutoff))
            result = await session.execute(delete(Trace).where(Trace.timestamp < cutoff))
            await session.commit()
            return result.rowcount or 0

    # ------------------------------------------------------------------
    # Logs
    # ------------------------------------------------------------------

    async def batch_create_logs(self, logs: list[dict]) -> None:
        if not logs:
            return
        async with self._sf() as session:
            for batch_start in range(0, len(logs), 500):
                batch = logs[batch_start : batch_start + 500]
                objs = [Log(**lg) for lg in batch]
                session.add_all(objs)
            await session.commit()

    async def get_log(self, log_id: int) -> dict | None:
        async with self._sf() as session:
            result = await session.execute(select(Log).where(Log.id == log_id))
            log = result.scalar_one_or_none()
            return log.to_dict() if log else None

    async def get_logs_v2(
        self,
        service_name: str = "",
        severity: str = "",
        search: str = "",
        trace_id: str = "",
        start_time: datetime | None = None,
        end_time: datetime | None = None,
        limit: int = 50,
        offset: int = 0,
    ) -> tuple[list[dict], int]:
        async with self._sf() as session:
            conditions = []
            if service_name:
                conditions.append(Log.service_name == service_name)
            if severity:
                conditions.append(Log.severity == severity)
            if trace_id:
                conditions.append(Log.trace_id == trace_id)
            if start_time:
                conditions.append(Log.timestamp >= start_time)
            if end_time:
                conditions.append(Log.timestamp <= end_time)
            # Note: search on compressed columns won't work via SQL LIKE.
            # For simplicity, search is skipped on compressed body.

            base = select(Log)
            count_q = select(func.count(Log.id))
            if conditions:
                base = base.where(and_(*conditions))
                count_q = count_q.where(and_(*conditions))

            count_result = await session.execute(count_q)
            total = count_result.scalar() or 0

            base = base.order_by(Log.timestamp.desc()).limit(limit).offset(offset)
            result = await session.execute(base)
            logs = result.scalars().all()
            return [lg.to_dict() for lg in logs], total

    async def get_log_context(self, target_time: datetime) -> list[dict]:
        start = target_time - timedelta(minutes=1)
        end = target_time + timedelta(minutes=1)
        async with self._sf() as session:
            stmt = (
                select(Log)
                .where(Log.timestamp.between(start, end))
                .order_by(Log.timestamp.asc())
            )
            result = await session.execute(stmt)
            return [lg.to_dict() for lg in result.scalars().all()]

    async def purge_logs(self, cutoff: datetime) -> int:
        async with self._sf() as session:
            result = await session.execute(delete(Log).where(Log.timestamp < cutoff))
            await session.commit()
            return result.rowcount or 0

    # ------------------------------------------------------------------
    # Metrics
    # ------------------------------------------------------------------

    async def batch_create_metrics(self, metrics: list[dict]) -> None:
        if not metrics:
            return
        async with self._sf() as session:
            for batch_start in range(0, len(metrics), 500):
                batch = metrics[batch_start : batch_start + 500]
                objs = [MetricBucket(**m) for m in batch]
                session.add_all(objs)
            await session.commit()

    async def get_metric_buckets(
        self,
        start: datetime | None,
        end: datetime | None,
        service_name: str = "",
        name: str = "",
    ) -> list[dict]:
        async with self._sf() as session:
            conditions = []
            if name:
                conditions.append(MetricBucket.name == name)
            if service_name:
                conditions.append(MetricBucket.service_name == service_name)
            if start:
                conditions.append(MetricBucket.time_bucket >= start)
            if end:
                conditions.append(MetricBucket.time_bucket <= end)

            stmt = select(MetricBucket)
            if conditions:
                stmt = stmt.where(and_(*conditions))
            stmt = stmt.order_by(MetricBucket.time_bucket.desc()).limit(1000)
            result = await session.execute(stmt)
            return [m.to_dict() for m in result.scalars().all()]

    async def get_metric_names(self, service_name: str = "") -> list[str]:
        async with self._sf() as session:
            stmt = select(MetricBucket.name).distinct()
            if service_name:
                stmt = stmt.where(MetricBucket.service_name == service_name)
            result = await session.execute(stmt)
            return [row[0] for row in result.all()]

    # ------------------------------------------------------------------
    # Services / Metadata
    # ------------------------------------------------------------------

    async def get_services(self) -> list[str]:
        async with self._sf() as session:
            stmt = select(Span.service_name).distinct().where(Span.service_name != "")
            result = await session.execute(stmt)
            return sorted([row[0] for row in result.all()])

    # ------------------------------------------------------------------
    # Stats
    # ------------------------------------------------------------------

    async def get_stats(self) -> dict:
        async with self._sf() as session:
            trace_count = (await session.execute(select(func.count(Trace.id)))).scalar() or 0
            span_count = (await session.execute(select(func.count(Span.id)))).scalar() or 0
            log_count = (await session.execute(select(func.count(Log.id)))).scalar() or 0
            metric_count = (await session.execute(select(func.count(MetricBucket.id)))).scalar() or 0
            return {
                "traces": trace_count,
                "spans": span_count,
                "logs": log_count,
                "metric_buckets": metric_count,
            }

    # ------------------------------------------------------------------
    # Traffic / Dashboard / Service-Map (simplified DB queries)
    # ------------------------------------------------------------------

    async def get_traffic_metrics(
        self, start: datetime, end: datetime, service_names: list[str] | None = None
    ) -> list[dict]:
        async with self._sf() as session:
            conditions = [Span.start_time.between(start, end)]
            if service_names:
                conditions.append(Span.service_name.in_(service_names))
            stmt = (
                select(
                    Span.service_name,
                    func.count(Span.id).label("count"),
                    func.avg(Span.duration).label("avg_duration"),
                )
                .where(and_(*conditions))
                .group_by(Span.service_name)
            )
            result = await session.execute(stmt)
            return [
                {"service_name": row.service_name, "count": row.count, "avg_duration": row.avg_duration or 0}
                for row in result.all()
            ]

    async def get_latency_heatmap(
        self, start: datetime, end: datetime, service_names: list[str] | None = None
    ) -> list[dict]:
        async with self._sf() as session:
            conditions = [Span.start_time.between(start, end)]
            if service_names:
                conditions.append(Span.service_name.in_(service_names))
            stmt = (
                select(
                    Span.service_name,
                    Span.operation_name,
                    func.count(Span.id).label("count"),
                    func.avg(Span.duration).label("avg_duration"),
                    func.max(Span.duration).label("max_duration"),
                )
                .where(and_(*conditions))
                .group_by(Span.service_name, Span.operation_name)
            )
            result = await session.execute(stmt)
            return [
                {
                    "service_name": row.service_name,
                    "operation_name": row.operation_name,
                    "count": row.count,
                    "avg_duration": row.avg_duration or 0,
                    "max_duration": row.max_duration or 0,
                }
                for row in result.all()
            ]

    async def get_dashboard_stats(
        self, start: datetime, end: datetime, service_names: list[str] | None = None
    ) -> dict:
        async with self._sf() as session:
            conditions = [Span.start_time.between(start, end)]
            if service_names:
                conditions.append(Span.service_name.in_(service_names))
            row = (
                await session.execute(
                    select(
                        func.count(Span.id).label("total_spans"),
                        func.avg(Span.duration).label("avg_duration"),
                    ).where(and_(*conditions))
                )
            ).one()
            trace_count = (
                await session.execute(
                    select(func.count(Trace.id)).where(Trace.timestamp.between(start, end))
                )
            ).scalar() or 0
            log_count = (
                await session.execute(
                    select(func.count(Log.id)).where(Log.timestamp.between(start, end))
                )
            ).scalar() or 0
            return {
                "total_spans": row.total_spans or 0,
                "total_traces": trace_count,
                "total_logs": log_count,
                "avg_duration_us": row.avg_duration or 0,
            }

    async def get_service_map_metrics(self, start: datetime, end: datetime) -> dict:
        """Build service map nodes and edges from span data."""
        async with self._sf() as session:
            # Nodes: service-level aggregates
            node_stmt = (
                select(
                    Span.service_name,
                    func.count(Span.id).label("total_traces"),
                    func.avg(Span.duration).label("avg_latency"),
                )
                .where(Span.start_time.between(start, end))
                .group_by(Span.service_name)
            )
            node_rows = (await session.execute(node_stmt)).all()
            nodes = []
            for r in node_rows:
                nodes.append({
                    "name": r.service_name,
                    "total_traces": r.total_traces,
                    "avg_latency_ms": (r.avg_latency or 0) / 1000.0,
                    "error_count": 0,
                })

            # Edges: parent-child span relationships across services
            # This is a simplified version
            return {"nodes": nodes, "edges": []}

    # ------------------------------------------------------------------
    # Spans for graph rebuild
    # ------------------------------------------------------------------

    async def get_spans_for_graph(self, since: datetime) -> list[dict]:
        async with self._sf() as session:
            stmt = (
                select(Span)
                .where(Span.start_time > since)
                .order_by(Span.start_time.asc())
                .limit(50000)
            )
            result = await session.execute(stmt)
            return [
                {
                    "span_id": s.span_id,
                    "parent_span_id": s.parent_span_id,
                    "service_name": s.service_name,
                    "operation_name": s.operation_name,
                    "duration": s.duration,
                    "trace_id": s.trace_id,
                    "start_time": s.start_time,
                }
                for s in result.scalars().all()
            ]

    # ------------------------------------------------------------------
    # Admin
    # ------------------------------------------------------------------

    async def vacuum_db(self) -> None:
        async with self._sf() as session:
            await session.execute(text("VACUUM"))
            await session.commit()

    # ------------------------------------------------------------------
    # Investigations
    # ------------------------------------------------------------------

    async def create_investigation(self, inv: dict) -> None:
        async with self._sf() as session:
            session.add(Investigation(**inv))
            await session.commit()

    async def get_investigations(
        self, service: str = "", severity: str = "", status: str = "", limit: int = 20
    ) -> list[dict]:
        limit = min(max(limit, 1), 100)
        async with self._sf() as session:
            stmt = select(Investigation).order_by(Investigation.created_at.desc()).limit(limit)
            if service:
                stmt = stmt.where(
                    (Investigation.trigger_service == service) | (Investigation.root_service == service)
                )
            if severity:
                stmt = stmt.where(Investigation.severity == severity)
            if status:
                stmt = stmt.where(Investigation.status == status)
            result = await session.execute(stmt)
            return [inv.to_dict() for inv in result.scalars().all()]

    async def get_investigation(self, inv_id: str) -> dict | None:
        async with self._sf() as session:
            result = await session.execute(select(Investigation).where(Investigation.id == inv_id))
            inv = result.scalar_one_or_none()
            return inv.to_dict() if inv else None

    # ------------------------------------------------------------------
    # Graph Snapshots
    # ------------------------------------------------------------------

    async def create_graph_snapshot(self, snap: dict) -> None:
        async with self._sf() as session:
            session.add(GraphSnapshot(**snap))
            await session.commit()

    async def get_graph_snapshot(self, at: datetime) -> dict | None:
        async with self._sf() as session:
            stmt = (
                select(GraphSnapshot)
                .where(GraphSnapshot.created_at <= at)
                .order_by(GraphSnapshot.created_at.desc())
                .limit(1)
            )
            result = await session.execute(stmt)
            snap = result.scalar_one_or_none()
            return snap.to_dict() if snap else None

    async def prune_old_snapshots(self, cutoff: datetime) -> int:
        async with self._sf() as session:
            result = await session.execute(
                delete(GraphSnapshot).where(GraphSnapshot.created_at < cutoff)
            )
            await session.commit()
            return result.rowcount or 0

    # ------------------------------------------------------------------
    # Logs for archival
    # ------------------------------------------------------------------

    async def get_old_logs(self, cutoff: datetime, limit: int = 10000) -> list[dict]:
        async with self._sf() as session:
            stmt = (
                select(Log).where(Log.timestamp < cutoff).order_by(Log.timestamp.asc()).limit(limit)
            )
            result = await session.execute(stmt)
            return [lg.to_dict() for lg in result.scalars().all()]

    async def get_old_traces(self, cutoff: datetime, limit: int = 10000) -> list[dict]:
        async with self._sf() as session:
            stmt = (
                select(Trace).where(Trace.timestamp < cutoff).order_by(Trace.timestamp.asc()).limit(limit)
            )
            result = await session.execute(stmt)
            return [t.to_dict() for t in result.scalars().all()]
