"""GraphRAG schema: 7 node types, 9 edge types, query result types."""

from __future__ import annotations

from dataclasses import dataclass, field
from datetime import datetime, timezone
from enum import Enum
from typing import Any, Dict, List, Optional


# --- Node Types ---

class NodeType(str, Enum):
    SERVICE = "service"
    OPERATION = "operation"
    TRACE = "trace"
    SPAN = "span"
    LOG_CLUSTER = "log_cluster"
    METRIC = "metric"
    ANOMALY = "anomaly"


@dataclass
class ServiceNode:
    id: str = ""
    name: str = ""
    first_seen: datetime = field(default_factory=lambda: datetime.now(timezone.utc))
    last_seen: datetime = field(default_factory=lambda: datetime.now(timezone.utc))
    health_score: float = 1.0  # 0.0-1.0
    call_count: int = 0
    error_count: int = 0
    error_rate: float = 0.0
    avg_latency: float = 0.0  # ms
    total_ms: float = 0.0

    def to_dict(self) -> dict:
        return {
            "id": self.id, "name": self.name,
            "first_seen": self.first_seen.isoformat(), "last_seen": self.last_seen.isoformat(),
            "health_score": self.health_score, "call_count": self.call_count,
            "error_count": self.error_count, "error_rate": self.error_rate,
            "avg_latency_ms": self.avg_latency,
        }


@dataclass
class OperationNode:
    id: str = ""  # service|operation
    service: str = ""
    operation: str = ""
    first_seen: datetime = field(default_factory=lambda: datetime.now(timezone.utc))
    last_seen: datetime = field(default_factory=lambda: datetime.now(timezone.utc))
    health_score: float = 1.0
    call_count: int = 0
    error_count: int = 0
    error_rate: float = 0.0
    avg_latency: float = 0.0
    p50_latency: float = 0.0
    p95_latency: float = 0.0
    p99_latency: float = 0.0
    total_ms: float = 0.0

    def to_dict(self) -> dict:
        return {
            "id": self.id, "service": self.service, "operation": self.operation,
            "health_score": self.health_score, "call_count": self.call_count,
            "error_rate": self.error_rate, "avg_latency_ms": self.avg_latency,
        }


@dataclass
class TraceNode:
    id: str = ""  # trace_id
    root_service: str = ""
    duration: float = 0.0  # ms
    status: str = ""
    timestamp: datetime = field(default_factory=lambda: datetime.now(timezone.utc))
    span_count: int = 0


@dataclass
class SpanNode:
    id: str = ""  # span_id
    trace_id: str = ""
    parent_span_id: str = ""
    service: str = ""
    operation: str = ""
    duration: float = 0.0  # ms
    status_code: str = ""
    is_error: bool = False
    timestamp: datetime = field(default_factory=lambda: datetime.now(timezone.utc))

    def to_dict(self) -> dict:
        return {
            "id": self.id, "trace_id": self.trace_id, "parent_span_id": self.parent_span_id,
            "service": self.service, "operation": self.operation, "duration_ms": self.duration,
            "status_code": self.status_code, "is_error": self.is_error,
            "timestamp": self.timestamp.isoformat(),
        }


@dataclass
class LogClusterNode:
    id: str = ""
    template: str = ""
    count: int = 0
    first_seen: datetime = field(default_factory=lambda: datetime.now(timezone.utc))
    last_seen: datetime = field(default_factory=lambda: datetime.now(timezone.utc))
    severity_dist: Dict[str, int] = field(default_factory=dict)

    def to_dict(self) -> dict:
        return {
            "id": self.id, "template": self.template, "count": self.count,
            "first_seen": self.first_seen.isoformat(), "last_seen": self.last_seen.isoformat(),
            "severity_distribution": self.severity_dist,
        }


@dataclass
class MetricNode:
    id: str = ""  # metric_name|service
    metric_name: str = ""
    service: str = ""
    rolling_min: float = 0.0
    rolling_max: float = 0.0
    rolling_avg: float = 0.0
    sample_count: int = 0
    last_seen: datetime = field(default_factory=lambda: datetime.now(timezone.utc))

    def to_dict(self) -> dict:
        return {
            "id": self.id, "metric_name": self.metric_name, "service": self.service,
            "rolling_min": self.rolling_min, "rolling_max": self.rolling_max,
            "rolling_avg": self.rolling_avg, "sample_count": self.sample_count,
        }


class AnomalySeverity(str, Enum):
    CRITICAL = "critical"
    WARNING = "warning"
    INFO = "info"


class AnomalyType(str, Enum):
    ERROR_SPIKE = "error_spike"
    LATENCY_SPIKE = "latency_spike"
    METRIC_ZSCORE = "metric_zscore"


@dataclass
class AnomalyNode:
    id: str = ""
    type: AnomalyType = AnomalyType.ERROR_SPIKE
    severity: AnomalySeverity = AnomalySeverity.INFO
    service: str = ""
    evidence: str = ""
    timestamp: datetime = field(default_factory=lambda: datetime.now(timezone.utc))

    def to_dict(self) -> dict:
        return {
            "id": self.id, "type": self.type.value, "severity": self.severity.value,
            "service": self.service, "evidence": self.evidence,
            "timestamp": self.timestamp.isoformat(),
        }


# --- Edge Types ---

class EdgeType(str, Enum):
    CALLS = "CALLS"
    EXPOSES = "EXPOSES"
    CONTAINS = "CONTAINS"
    CHILD_OF = "CHILD_OF"
    EMITTED_BY = "EMITTED_BY"
    LOGGED_DURING = "LOGGED_DURING"
    MEASURED_BY = "MEASURED_BY"
    PRECEDED_BY = "PRECEDED_BY"
    TRIGGERED_BY = "TRIGGERED_BY"


@dataclass
class Edge:
    type: EdgeType = EdgeType.CALLS
    from_id: str = ""
    to_id: str = ""
    weight: float = 0.0
    call_count: int = 0
    error_rate: float = 0.0
    avg_ms: float = 0.0
    total_ms: float = 0.0
    error_count: int = 0
    updated_at: datetime = field(default_factory=lambda: datetime.now(timezone.utc))

    def to_dict(self) -> dict:
        return {
            "type": self.type.value, "from_id": self.from_id, "to_id": self.to_id,
            "weight": self.weight, "call_count": self.call_count,
            "error_rate": self.error_rate, "avg_latency_ms": self.avg_ms,
        }


# --- Query Result Types ---

@dataclass
class RootCauseInfo:
    service: str = ""
    operation: str = ""
    error_message: str = ""
    span_id: str = ""
    trace_id: str = ""

    def to_dict(self) -> dict:
        return {
            "service": self.service, "operation": self.operation,
            "error_message": self.error_message, "span_id": self.span_id,
            "trace_id": self.trace_id,
        }


@dataclass
class ErrorChainResult:
    root_cause: Optional[RootCauseInfo] = None
    span_chain: list[SpanNode] = field(default_factory=list)
    correlated_logs: list[LogClusterNode] = field(default_factory=list)
    anomalous_metrics: list[MetricNode] = field(default_factory=list)
    trace_id: str = ""


@dataclass
class AffectedEntry:
    service: str = ""
    depth: int = 0
    call_count: int = 0
    impact_score: float = 0.0


@dataclass
class ImpactResult:
    service: str = ""
    affected_services: list[AffectedEntry] = field(default_factory=list)
    total_downstream: int = 0


@dataclass
class RankedCause:
    service: str = ""
    operation: str = ""
    score: float = 0.0
    evidence: list[str] = field(default_factory=list)
    error_chain: list[SpanNode] = field(default_factory=list)
    anomalies: list[AnomalyNode] = field(default_factory=list)


@dataclass
class CorrelatedSignalsResult:
    service: str = ""
    error_logs: list[LogClusterNode] = field(default_factory=list)
    metrics: list[MetricNode] = field(default_factory=list)
    anomalies: list[AnomalyNode] = field(default_factory=list)
    error_chains: list[ErrorChainResult] = field(default_factory=list)


@dataclass
class ServiceMapEntry:
    service: Optional[ServiceNode] = None
    operations: list[OperationNode] = field(default_factory=list)
    calls_to: list[Edge] = field(default_factory=list)
    called_by: list[Edge] = field(default_factory=list)
