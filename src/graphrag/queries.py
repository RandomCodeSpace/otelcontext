"""GraphRAG query functions: ErrorChain, ImpactAnalysis, RootCause, Dijkstra, etc."""

from __future__ import annotations

import heapq
import math
from collections import deque
from datetime import datetime, timedelta, timezone
from typing import TYPE_CHECKING

from src.graphrag.schema import (
    AffectedEntry,
    AnomalyNode,
    CorrelatedSignalsResult,
    Edge,
    EdgeType,
    ErrorChainResult,
    ImpactResult,
    RankedCause,
    RootCauseInfo,
    ServiceMapEntry,
    SpanNode,
)

if TYPE_CHECKING:
    from src.graphrag.builder import GraphRAG


def error_chain(g: "GraphRAG", service: str, since: datetime, limit: int = 10) -> list[ErrorChainResult]:
    if limit <= 0:
        limit = 10
    error_spans = g.trace_store.error_spans(service, since)
    if len(error_spans) > limit:
        error_spans = error_spans[:limit]

    results: list[ErrorChainResult] = []
    seen: set[str] = set()

    for span in error_spans:
        if span.trace_id in seen:
            continue
        seen.add(span.trace_id)

        chain = _trace_error_chain_upstream(g, span)
        if not chain:
            continue

        root_span = chain[-1]
        result = ErrorChainResult(
            root_cause=RootCauseInfo(
                service=root_span.service,
                operation=root_span.operation,
                span_id=root_span.id,
                trace_id=root_span.trace_id,
            ),
            span_chain=chain,
            trace_id=span.trace_id,
        )

        # Correlated logs
        for s in chain:
            if s.is_error:
                clusters = g.signal_store.log_clusters_for_service(s.service)
                for lc in clusters:
                    if lc.last_seen > since:
                        result.correlated_logs.append(lc)

        results.append(result)

    return results


def _trace_error_chain_upstream(g: "GraphRAG", span: SpanNode) -> list[SpanNode]:
    chain: list[SpanNode] = []
    visited: set[str] = set()
    current: SpanNode | None = span

    while current is not None and current.id not in visited:
        visited.add(current.id)
        chain.append(current)
        if not current.parent_span_id:
            break
        current = g.trace_store.get_span(current.parent_span_id)

    return chain


def impact_analysis(g: "GraphRAG", service: str, max_depth: int = 5) -> ImpactResult:
    if max_depth <= 0:
        max_depth = 5

    result = ImpactResult(service=service)
    visited = {service}
    queue: deque[tuple[str, int]] = deque([(service, 0)])

    while queue:
        svc, depth = queue.popleft()
        if depth >= max_depth:
            continue

        edges = g.service_store.call_edges_from(svc)
        for e in edges:
            if e.to_id in visited:
                continue
            visited.add(e.to_id)

            svc_node = g.service_store.get_service(e.to_id)
            impact = 1.0
            if svc_node is not None:
                impact = 1.0 - svc_node.health_score

            result.affected_services.append(AffectedEntry(
                service=e.to_id,
                depth=depth + 1,
                call_count=e.call_count,
                impact_score=impact,
            ))
            queue.append((e.to_id, depth + 1))

    result.total_downstream = len(result.affected_services)
    return result


def root_cause_analysis(g: "GraphRAG", service: str, since: datetime) -> list[RankedCause]:
    error_chains = error_chain(g, service, since, limit=20)
    anomalies = g.anomaly_store.anomalies_for_service(service, since)

    cause_scores: dict[str, RankedCause] = {}

    for ec in error_chains:
        if ec.root_cause is None:
            continue
        key = f"{ec.root_cause.service}|{ec.root_cause.operation}"
        rc = cause_scores.get(key)
        if rc is None:
            rc = RankedCause(service=ec.root_cause.service, operation=ec.root_cause.operation)
            cause_scores[key] = rc
        rc.score += 1.0
        rc.evidence.append(f"error chain from trace {ec.trace_id}")
        if ec.span_chain:
            rc.error_chain = ec.span_chain

    for a in anomalies:
        key_prefix = f"{a.service}|"
        matched = False
        for k, rc in cause_scores.items():
            if k.startswith(a.service):
                rc.score += 2.0
                rc.anomalies.append(a)
                rc.evidence.append(f"anomaly: {a.evidence}")
                matched = True
        if not matched:
            cause_scores[key_prefix] = RankedCause(
                service=a.service,
                score=2.0,
                anomalies=[a],
                evidence=[f"anomaly: {a.evidence}"],
            )

    ranked = sorted(cause_scores.values(), key=lambda x: x.score, reverse=True)
    return ranked


def dependency_chain(g: "GraphRAG", trace_id: str) -> list[SpanNode]:
    spans = g.trace_store.spans_for_trace(trace_id)
    spans.sort(key=lambda s: s.timestamp)
    return spans


def correlated_signals(g: "GraphRAG", service: str, since: datetime) -> CorrelatedSignalsResult:
    result = CorrelatedSignalsResult(service=service)

    clusters = g.signal_store.log_clusters_for_service(service)
    for lc in clusters:
        if lc.last_seen > since:
            result.error_logs.append(lc)

    metrics = g.signal_store.metrics_for_service(service)
    for m in metrics:
        result.metrics.append(m)

    anomalies = g.anomaly_store.anomalies_for_service(service, since)
    for a in anomalies:
        result.anomalies.append(a)

    result.error_chains = error_chain(g, service, since, limit=5)
    return result


def shortest_path(g: "GraphRAG", from_svc: str, to_svc: str) -> list[str]:
    """Dijkstra shortest path between services via CALLS edges."""
    with g.service_store._lock:
        adj: dict[str, dict[str, float]] = {}
        for e in g.service_store.edges.values():
            if e.type != EdgeType.CALLS:
                continue
            weight = 1.0 / e.call_count if e.call_count > 0 else 1.0
            adj.setdefault(e.from_id, {})[e.to_id] = weight
            adj.setdefault(e.to_id, {})[e.from_id] = weight

    dist: dict[str, float] = {from_svc: 0.0}
    prev: dict[str, str] = {}
    visited: set[str] = set()

    # Simple Dijkstra with linear scan (small graphs)
    while True:
        u = ""
        min_dist = float("inf")
        for node, d in dist.items():
            if node not in visited and d < min_dist:
                u = node
                min_dist = d
        if not u or u == to_svc:
            break
        visited.add(u)
        for neighbor, weight in adj.get(u, {}).items():
            alt = dist[u] + weight
            if neighbor not in dist or alt < dist[neighbor]:
                dist[neighbor] = alt
                prev[neighbor] = u

    if to_svc not in dist:
        return []
    path: list[str] = []
    at = to_svc
    while at:
        path.insert(0, at)
        if at == from_svc:
            break
        at = prev.get(at, "")
    if not path or path[0] != from_svc:
        return []
    return path


def anomaly_timeline(g: "GraphRAG", since: datetime) -> list[AnomalyNode]:
    anomalies = g.anomaly_store.anomalies_since(since)
    anomalies.sort(key=lambda a: a.timestamp, reverse=True)
    return anomalies


def service_map(g: "GraphRAG", depth: int = 0) -> list[ServiceMapEntry]:
    services = g.service_store.all_services()
    result: list[ServiceMapEntry] = []

    for svc in services:
        entry = ServiceMapEntry(
            service=svc,
            calls_to=g.service_store.call_edges_from(svc.name),
            called_by=g.service_store.call_edges_to(svc.name),
        )
        with g.service_store._lock:
            entry.operations = [op for op in g.service_store.operations.values() if op.service == svc.name]
        result.append(entry)

    return result
