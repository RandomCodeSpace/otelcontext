"""SQLAlchemy 2.x async models for OtelContext."""

from __future__ import annotations

import json
from datetime import datetime, timezone
from typing import Any, Optional

from sqlalchemy import (
    BigInteger,
    DateTime,
    Float,
    Index,
    Integer,
    LargeBinary,
    String,
    Text,
    func,
)
from sqlalchemy.orm import DeclarativeBase, Mapped, mapped_column, relationship
from sqlalchemy.types import TypeDecorator

from src.compress.zstd import compress, decompress

# ---------------------------------------------------------------------------
# Custom type: transparently gzip-compress text fields
# ---------------------------------------------------------------------------

GZIP_MAGIC = b"\x1f\x8b"


class CompressedText(TypeDecorator):
    """A string column that is transparently compressed on write and
    decompressed on read using gzip."""

    impl = LargeBinary
    cache_ok = True

    def process_bind_param(self, value: str | None, dialect: Any) -> bytes | None:
        if value is None or value == "":
            return None
        raw = value.encode("utf-8") if isinstance(value, str) else value
        return compress(raw)

    def process_result_value(self, value: bytes | None, dialect: Any) -> str | None:
        if value is None:
            return None
        raw = decompress(value)
        return raw.decode("utf-8")


# ---------------------------------------------------------------------------
# Base
# ---------------------------------------------------------------------------


class Base(DeclarativeBase):
    pass


# ---------------------------------------------------------------------------
# Trace
# ---------------------------------------------------------------------------


class Trace(Base):
    __tablename__ = "traces"

    id: Mapped[int] = mapped_column(Integer, primary_key=True, autoincrement=True)
    trace_id: Mapped[str] = mapped_column(String(32), unique=True, nullable=False, index=True)
    service_name: Mapped[str] = mapped_column(String(255), index=True, default="")
    duration: Mapped[int] = mapped_column(BigInteger, index=True, default=0)  # microseconds
    status: Mapped[str] = mapped_column(String(50), default="")
    timestamp: Mapped[datetime] = mapped_column(DateTime, index=True, default=lambda: datetime.now(timezone.utc))
    created_at: Mapped[datetime] = mapped_column(DateTime, default=lambda: datetime.now(timezone.utc))
    updated_at: Mapped[datetime] = mapped_column(
        DateTime, default=lambda: datetime.now(timezone.utc), onupdate=lambda: datetime.now(timezone.utc)
    )

    spans: Mapped[list["Span"]] = relationship(
        "Span", back_populates="trace", foreign_keys="Span.trace_id", primaryjoin="Trace.trace_id == Span.trace_id",
        lazy="selectin", viewonly=True,
    )

    @property
    def duration_ms(self) -> float:
        return self.duration / 1000.0

    @property
    def span_count(self) -> int:
        return len(self.spans) if self.spans else 0

    @property
    def operation(self) -> str:
        if self.spans:
            roots = [s for s in self.spans if not s.parent_span_id]
            if roots:
                return roots[0].operation_name
            return self.spans[0].operation_name
        return ""

    def to_dict(self) -> dict:
        return {
            "id": self.id,
            "trace_id": self.trace_id,
            "service_name": self.service_name,
            "duration": self.duration,
            "duration_ms": self.duration_ms,
            "span_count": self.span_count,
            "operation": self.operation,
            "status": self.status,
            "timestamp": self.timestamp.isoformat() if self.timestamp else None,
            "spans": [s.to_dict() for s in (self.spans or [])],
        }


# ---------------------------------------------------------------------------
# Span
# ---------------------------------------------------------------------------


class Span(Base):
    __tablename__ = "spans"
    __table_args__ = (
        Index("ix_spans_trace_id", "trace_id"),
        Index("ix_spans_service_name", "service_name"),
        Index("ix_spans_operation_name", "operation_name"),
    )

    id: Mapped[int] = mapped_column(Integer, primary_key=True, autoincrement=True)
    trace_id: Mapped[str] = mapped_column(String(32), nullable=False)
    span_id: Mapped[str] = mapped_column(String(16), nullable=False)
    parent_span_id: Mapped[str] = mapped_column(String(16), default="")
    operation_name: Mapped[str] = mapped_column(String(255), default="")
    start_time: Mapped[datetime] = mapped_column(DateTime, default=lambda: datetime.now(timezone.utc))
    end_time: Mapped[datetime] = mapped_column(DateTime, default=lambda: datetime.now(timezone.utc))
    duration: Mapped[int] = mapped_column(BigInteger, default=0)  # microseconds
    service_name: Mapped[str] = mapped_column(String(255), default="")
    attributes_json: Mapped[Optional[str]] = mapped_column(CompressedText, nullable=True)

    trace: Mapped[Optional["Trace"]] = relationship(
        "Trace", back_populates="spans", foreign_keys=[trace_id],
        primaryjoin="Span.trace_id == Trace.trace_id", viewonly=True,
    )

    def to_dict(self) -> dict:
        return {
            "id": self.id,
            "trace_id": self.trace_id,
            "span_id": self.span_id,
            "parent_span_id": self.parent_span_id,
            "operation_name": self.operation_name,
            "start_time": self.start_time.isoformat() if self.start_time else None,
            "end_time": self.end_time.isoformat() if self.end_time else None,
            "duration": self.duration,
            "service_name": self.service_name,
            "attributes_json": self.attributes_json or "",
        }


# ---------------------------------------------------------------------------
# Log
# ---------------------------------------------------------------------------


class Log(Base):
    __tablename__ = "logs"
    __table_args__ = (
        Index("ix_logs_trace_id", "trace_id"),
        Index("ix_logs_service_name", "service_name"),
        Index("ix_logs_severity", "severity"),
        Index("ix_logs_timestamp", "timestamp"),
    )

    id: Mapped[int] = mapped_column(Integer, primary_key=True, autoincrement=True)
    trace_id: Mapped[str] = mapped_column(String(32), default="")
    span_id: Mapped[str] = mapped_column(String(16), default="")
    severity: Mapped[str] = mapped_column(String(50), default="")
    body: Mapped[Optional[str]] = mapped_column(CompressedText, nullable=True)
    service_name: Mapped[str] = mapped_column(String(255), default="")
    attributes_json: Mapped[Optional[str]] = mapped_column(CompressedText, nullable=True)
    ai_insight: Mapped[Optional[str]] = mapped_column(CompressedText, nullable=True)
    timestamp: Mapped[datetime] = mapped_column(DateTime, default=lambda: datetime.now(timezone.utc))

    def to_dict(self) -> dict:
        return {
            "id": self.id,
            "trace_id": self.trace_id,
            "span_id": self.span_id,
            "severity": self.severity,
            "body": self.body or "",
            "service_name": self.service_name,
            "attributes_json": self.attributes_json or "",
            "ai_insight": self.ai_insight or "",
            "timestamp": self.timestamp.isoformat() if self.timestamp else None,
        }


# ---------------------------------------------------------------------------
# MetricBucket
# ---------------------------------------------------------------------------


class MetricBucket(Base):
    __tablename__ = "metric_buckets"
    __table_args__ = (
        Index("ix_mb_name", "name"),
        Index("ix_mb_service_name", "service_name"),
        Index("ix_mb_time_bucket", "time_bucket"),
    )

    id: Mapped[int] = mapped_column(Integer, primary_key=True, autoincrement=True)
    name: Mapped[str] = mapped_column(String(255), nullable=False)
    service_name: Mapped[str] = mapped_column(String(255), nullable=False)
    time_bucket: Mapped[datetime] = mapped_column(DateTime, nullable=False)
    min: Mapped[float] = mapped_column(Float, default=0.0)
    max: Mapped[float] = mapped_column(Float, default=0.0)
    sum: Mapped[float] = mapped_column(Float, default=0.0)
    count: Mapped[int] = mapped_column(BigInteger, default=0)
    attributes_json: Mapped[Optional[str]] = mapped_column(CompressedText, nullable=True)

    def to_dict(self) -> dict:
        return {
            "id": self.id,
            "name": self.name,
            "service_name": self.service_name,
            "time_bucket": self.time_bucket.isoformat() if self.time_bucket else None,
            "min": self.min,
            "max": self.max,
            "sum": self.sum,
            "count": self.count,
            "attributes_json": self.attributes_json or "",
        }


# ---------------------------------------------------------------------------
# Investigation (GraphRAG)
# ---------------------------------------------------------------------------


class Investigation(Base):
    __tablename__ = "investigations"

    id: Mapped[str] = mapped_column(String(64), primary_key=True)
    created_at: Mapped[datetime] = mapped_column(DateTime, default=lambda: datetime.now(timezone.utc))
    status: Mapped[str] = mapped_column(String(20), default="detected")
    severity: Mapped[str] = mapped_column(String(20), default="warning")
    trigger_service: Mapped[str] = mapped_column(String(255), index=True, default="")
    trigger_operation: Mapped[str] = mapped_column(String(255), default="")
    error_message: Mapped[str] = mapped_column(Text, default="")
    root_service: Mapped[str] = mapped_column(String(255), default="")
    root_operation: Mapped[str] = mapped_column(String(255), default="")
    causal_chain: Mapped[str] = mapped_column(Text, default="[]")
    trace_ids: Mapped[str] = mapped_column(Text, default="[]")
    error_logs: Mapped[str] = mapped_column(Text, default="[]")
    anomalous_metrics: Mapped[str] = mapped_column(Text, default="[]")
    affected_services: Mapped[str] = mapped_column(Text, default="[]")
    span_chain: Mapped[str] = mapped_column(Text, default="[]")

    def to_dict(self) -> dict:
        return {
            "id": self.id,
            "created_at": self.created_at.isoformat() if self.created_at else None,
            "status": self.status,
            "severity": self.severity,
            "trigger_service": self.trigger_service,
            "trigger_operation": self.trigger_operation,
            "error_message": self.error_message,
            "root_service": self.root_service,
            "root_operation": self.root_operation,
            "causal_chain": json.loads(self.causal_chain) if self.causal_chain else [],
            "trace_ids": json.loads(self.trace_ids) if self.trace_ids else [],
            "error_logs": json.loads(self.error_logs) if self.error_logs else [],
            "anomalous_metrics": json.loads(self.anomalous_metrics) if self.anomalous_metrics else [],
            "affected_services": json.loads(self.affected_services) if self.affected_services else [],
            "span_chain": json.loads(self.span_chain) if self.span_chain else [],
        }


# ---------------------------------------------------------------------------
# GraphSnapshot
# ---------------------------------------------------------------------------


class GraphSnapshot(Base):
    __tablename__ = "graph_snapshots"

    id: Mapped[str] = mapped_column(String(64), primary_key=True)
    created_at: Mapped[datetime] = mapped_column(DateTime, default=lambda: datetime.now(timezone.utc))
    nodes: Mapped[str] = mapped_column(Text, default="[]")
    edges: Mapped[str] = mapped_column(Text, default="[]")
    service_count: Mapped[int] = mapped_column(Integer, default=0)
    total_calls: Mapped[int] = mapped_column(BigInteger, default=0)
    avg_health_score: Mapped[float] = mapped_column(Float, default=0.0)

    def to_dict(self) -> dict:
        return {
            "id": self.id,
            "created_at": self.created_at.isoformat() if self.created_at else None,
            "nodes": json.loads(self.nodes) if self.nodes else [],
            "edges": json.loads(self.edges) if self.edges else [],
            "service_count": self.service_count,
            "total_calls": self.total_calls,
            "avg_health_score": self.avg_health_score,
        }
