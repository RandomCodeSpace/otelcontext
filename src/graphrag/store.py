"""Four typed stores for the GraphRAG layered graph."""

from __future__ import annotations

import math
import threading
from datetime import datetime, timedelta, timezone
from typing import Optional

from src.graphrag.schema import (
    AnomalyNode,
    Edge,
    EdgeType,
    LogClusterNode,
    MetricNode,
    OperationNode,
    ServiceNode,
    SpanNode,
    TraceNode,
)


def _edge_key(et: EdgeType, from_id: str, to_id: str) -> str:
    return f"{et.value}|{from_id}|{to_id}"


def _compute_health(error_rate: float, avg_latency_ms: float) -> float:
    latency_dev = max(0, (avg_latency_ms - 100) / 100)
    score = 1.0 - (error_rate * 5) - (latency_dev * 0.1)
    return max(0.0, min(1.0, score))


class ServiceStore:
    """Permanent service topology data."""

    def __init__(self) -> None:
        self._lock = threading.RLock()
        self.services: dict[str, ServiceNode] = {}
        self.operations: dict[str, OperationNode] = {}
        self.edges: dict[str, Edge] = {}

    def upsert_service(self, name: str, duration_ms: float, is_error: bool, ts: datetime) -> None:
        with self._lock:
            svc = self.services.get(name)
            if svc is None:
                svc = ServiceNode(id=name, name=name, first_seen=ts, last_seen=ts)
                self.services[name] = svc
            svc.call_count += 1
            svc.total_ms += duration_ms
            if is_error:
                svc.error_count += 1
            if ts > svc.last_seen:
                svc.last_seen = ts
            if ts < svc.first_seen:
                svc.first_seen = ts
            svc.avg_latency = svc.total_ms / svc.call_count
            svc.error_rate = svc.error_count / svc.call_count
            svc.health_score = _compute_health(svc.error_rate, svc.avg_latency)

    def upsert_operation(self, service: str, operation: str, duration_ms: float, is_error: bool, ts: datetime) -> None:
        key = f"{service}|{operation}"
        with self._lock:
            op = self.operations.get(key)
            if op is None:
                op = OperationNode(id=key, service=service, operation=operation, first_seen=ts, last_seen=ts)
                self.operations[key] = op
            op.call_count += 1
            op.total_ms += duration_ms
            if is_error:
                op.error_count += 1
            if ts > op.last_seen:
                op.last_seen = ts
            op.avg_latency = op.total_ms / op.call_count
            op.error_rate = op.error_count / op.call_count
            op.health_score = _compute_health(op.error_rate, op.avg_latency)

            # EXPOSES edge
            ek = _edge_key(EdgeType.EXPOSES, service, key)
            if ek not in self.edges:
                self.edges[ek] = Edge(type=EdgeType.EXPOSES, from_id=service, to_id=key, updated_at=ts)

    def upsert_call_edge(self, source: str, target: str, duration_ms: float, is_error: bool, ts: datetime) -> None:
        ek = _edge_key(EdgeType.CALLS, source, target)
        with self._lock:
            e = self.edges.get(ek)
            if e is None:
                e = Edge(type=EdgeType.CALLS, from_id=source, to_id=target)
                self.edges[ek] = e
            e.call_count += 1
            e.total_ms += duration_ms
            if is_error:
                e.error_count += 1
            e.avg_ms = e.total_ms / e.call_count
            e.error_rate = e.error_count / e.call_count
            e.weight = float(e.call_count)
            e.updated_at = ts

    def get_service(self, name: str) -> ServiceNode | None:
        with self._lock:
            return self.services.get(name)

    def all_services(self) -> list[ServiceNode]:
        with self._lock:
            return list(self.services.values())

    def all_edges(self) -> list[Edge]:
        with self._lock:
            return list(self.edges.values())

    def call_edges_from(self, service: str) -> list[Edge]:
        with self._lock:
            return [e for e in self.edges.values() if e.type == EdgeType.CALLS and e.from_id == service]

    def call_edges_to(self, service: str) -> list[Edge]:
        with self._lock:
            return [e for e in self.edges.values() if e.type == EdgeType.CALLS and e.to_id == service]


class TraceStore:
    """Trace/span detail with TTL-based pruning."""

    def __init__(self, ttl_seconds: float = 3600.0) -> None:
        self._lock = threading.RLock()
        self.traces: dict[str, TraceNode] = {}
        self.spans: dict[str, SpanNode] = {}
        self.edges: dict[str, Edge] = {}
        self.ttl = timedelta(seconds=ttl_seconds)

    def upsert_trace(self, trace_id: str, root_service: str, status: str, duration_ms: float, ts: datetime) -> None:
        with self._lock:
            t = self.traces.get(trace_id)
            if t is None:
                t = TraceNode(id=trace_id, root_service=root_service, status=status, duration=duration_ms, timestamp=ts)
                self.traces[trace_id] = t
            t.span_count += 1
            if duration_ms > t.duration:
                t.duration = duration_ms
            if status == "STATUS_CODE_ERROR":
                t.status = status

    def upsert_span(self, span: SpanNode) -> None:
        with self._lock:
            self.spans[span.id] = span
            ck = _edge_key(EdgeType.CONTAINS, span.trace_id, span.id)
            if ck not in self.edges:
                self.edges[ck] = Edge(type=EdgeType.CONTAINS, from_id=span.trace_id, to_id=span.id, updated_at=span.timestamp)
            if span.parent_span_id:
                pk = _edge_key(EdgeType.CHILD_OF, span.id, span.parent_span_id)
                if pk not in self.edges:
                    self.edges[pk] = Edge(type=EdgeType.CHILD_OF, from_id=span.id, to_id=span.parent_span_id, updated_at=span.timestamp)

    def get_span(self, span_id: str) -> SpanNode | None:
        with self._lock:
            return self.spans.get(span_id)

    def get_trace(self, trace_id: str) -> TraceNode | None:
        with self._lock:
            return self.traces.get(trace_id)

    def spans_for_trace(self, trace_id: str) -> list[SpanNode]:
        with self._lock:
            return [s for s in self.spans.values() if s.trace_id == trace_id]

    def error_spans(self, service: str, since: datetime) -> list[SpanNode]:
        with self._lock:
            return [s for s in self.spans.values() if s.is_error and s.service == service and s.timestamp > since]

    def prune(self) -> int:
        cutoff = datetime.now(timezone.utc) - self.ttl
        with self._lock:
            pruned = 0
            to_del = [sid for sid, s in self.spans.items() if s.timestamp < cutoff]
            for sid in to_del:
                del self.spans[sid]
                pruned += 1
            to_del_t = [tid for tid, t in self.traces.items() if t.timestamp < cutoff]
            for tid in to_del_t:
                del self.traces[tid]
            to_del_e = [ek for ek, e in self.edges.items() if e.updated_at < cutoff]
            for ek in to_del_e:
                del self.edges[ek]
            return pruned


class SignalStore:
    """Log cluster and metric correlation data."""

    def __init__(self) -> None:
        self._lock = threading.RLock()
        self.log_clusters: dict[str, LogClusterNode] = {}
        self.metrics: dict[str, MetricNode] = {}
        self.edges: dict[str, Edge] = {}

    def upsert_log_cluster(self, id: str, template: str, severity: str, service: str, ts: datetime) -> None:
        with self._lock:
            lc = self.log_clusters.get(id)
            if lc is None:
                lc = LogClusterNode(id=id, template=template, first_seen=ts, last_seen=ts)
                self.log_clusters[id] = lc
            lc.count += 1
            lc.severity_dist[severity] = lc.severity_dist.get(severity, 0) + 1
            if ts > lc.last_seen:
                lc.last_seen = ts
            # EMITTED_BY edge
            ek = _edge_key(EdgeType.EMITTED_BY, id, service)
            if ek not in self.edges:
                self.edges[ek] = Edge(type=EdgeType.EMITTED_BY, from_id=id, to_id=service, updated_at=ts)

    def add_logged_during_edge(self, cluster_id: str, span_id: str, ts: datetime) -> None:
        ek = _edge_key(EdgeType.LOGGED_DURING, cluster_id, span_id)
        with self._lock:
            if ek not in self.edges:
                self.edges[ek] = Edge(type=EdgeType.LOGGED_DURING, from_id=cluster_id, to_id=span_id, updated_at=ts)

    def upsert_metric(self, metric_name: str, service: str, value: float, ts: datetime) -> None:
        key = f"{metric_name}|{service}"
        with self._lock:
            m = self.metrics.get(key)
            if m is None:
                m = MetricNode(id=key, metric_name=metric_name, service=service,
                               rolling_min=value, rolling_max=value, rolling_avg=value, last_seen=ts)
                self.metrics[key] = m
                ek = _edge_key(EdgeType.MEASURED_BY, key, service)
                self.edges[ek] = Edge(type=EdgeType.MEASURED_BY, from_id=key, to_id=service, updated_at=ts)
            m.sample_count += 1
            if value < m.rolling_min:
                m.rolling_min = value
            if value > m.rolling_max:
                m.rolling_max = value
            m.rolling_avg = m.rolling_avg * 0.9 + value * 0.1
            m.last_seen = ts

    def log_clusters_for_service(self, service: str) -> list[LogClusterNode]:
        with self._lock:
            result = []
            for e in self.edges.values():
                if e.type == EdgeType.EMITTED_BY and e.to_id == service:
                    lc = self.log_clusters.get(e.from_id)
                    if lc:
                        result.append(lc)
            return result

    def metrics_for_service(self, service: str) -> list[MetricNode]:
        with self._lock:
            return [m for m in self.metrics.values() if m.service == service]


class AnomalyStore:
    """Detected anomalies and their temporal correlations."""

    def __init__(self) -> None:
        self._lock = threading.RLock()
        self.anomalies: dict[str, AnomalyNode] = {}
        self.edges: dict[str, Edge] = {}

    def add_anomaly(self, anomaly: AnomalyNode) -> None:
        with self._lock:
            self.anomalies[anomaly.id] = anomaly
            ek = _edge_key(EdgeType.TRIGGERED_BY, anomaly.id, anomaly.service)
            self.edges[ek] = Edge(type=EdgeType.TRIGGERED_BY, from_id=anomaly.id, to_id=anomaly.service, updated_at=anomaly.timestamp)

    def add_preceded_by_edge(self, anomaly_id: str, preceding_id: str, ts: datetime) -> None:
        ek = _edge_key(EdgeType.PRECEDED_BY, anomaly_id, preceding_id)
        with self._lock:
            self.edges[ek] = Edge(type=EdgeType.PRECEDED_BY, from_id=anomaly_id, to_id=preceding_id, updated_at=ts)

    def anomalies_since(self, since: datetime) -> list[AnomalyNode]:
        with self._lock:
            return [a for a in self.anomalies.values() if a.timestamp > since]

    def anomalies_for_service(self, service: str, since: datetime) -> list[AnomalyNode]:
        with self._lock:
            return [a for a in self.anomalies.values() if a.service == service and a.timestamp > since]

    def prune_old(self, hours: int = 24) -> None:
        cutoff = datetime.now(timezone.utc) - timedelta(hours=hours)
        with self._lock:
            to_del = [aid for aid, a in self.anomalies.items() if a.timestamp < cutoff]
            for aid in to_del:
                del self.anomalies[aid]
            to_del_e = [ek for ek, e in self.edges.items() if e.updated_at < cutoff]
            for ek in to_del_e:
                del self.edges[ek]
