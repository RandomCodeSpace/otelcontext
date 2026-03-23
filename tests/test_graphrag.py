"""Tests for the GraphRAG layered graph."""

from datetime import datetime, timedelta, timezone

import pytest
from src.graphrag.store import ServiceStore, TraceStore, SignalStore, AnomalyStore
from src.graphrag.schema import SpanNode, AnomalyNode, AnomalyType, AnomalySeverity, EdgeType


def test_service_store_upsert():
    store = ServiceStore()
    now = datetime.now(timezone.utc)
    store.upsert_service("svc-a", 100.0, False, now)
    store.upsert_service("svc-a", 200.0, True, now)

    svc = store.get_service("svc-a")
    assert svc is not None
    assert svc.call_count == 2
    assert svc.error_count == 1
    assert svc.error_rate == 0.5
    assert svc.avg_latency == 150.0


def test_service_store_call_edges():
    store = ServiceStore()
    now = datetime.now(timezone.utc)
    store.upsert_service("svc-a", 100.0, False, now)
    store.upsert_service("svc-b", 100.0, False, now)
    store.upsert_call_edge("svc-a", "svc-b", 50.0, False, now)

    edges_from = store.call_edges_from("svc-a")
    assert len(edges_from) == 1
    assert edges_from[0].to_id == "svc-b"

    edges_to = store.call_edges_to("svc-b")
    assert len(edges_to) == 1
    assert edges_to[0].from_id == "svc-a"


def test_trace_store_prune():
    store = TraceStore(ttl_seconds=1)
    old = datetime.now(timezone.utc) - timedelta(seconds=10)
    recent = datetime.now(timezone.utc)

    store.upsert_span(SpanNode(id="s1", trace_id="t1", service="svc", timestamp=old))
    store.upsert_span(SpanNode(id="s2", trace_id="t2", service="svc", timestamp=recent))

    pruned = store.prune()
    assert pruned >= 1
    assert store.get_span("s1") is None
    assert store.get_span("s2") is not None


def test_signal_store_log_clusters():
    store = SignalStore()
    now = datetime.now(timezone.utc)
    store.upsert_log_cluster("lc1", "error template", "ERROR", "svc-a", now)
    store.upsert_log_cluster("lc1", "error template", "ERROR", "svc-a", now)

    clusters = store.log_clusters_for_service("svc-a")
    assert len(clusters) == 1
    assert clusters[0].count == 2


def test_anomaly_store():
    store = AnomalyStore()
    now = datetime.now(timezone.utc)
    store.add_anomaly(AnomalyNode(
        id="a1", type=AnomalyType.ERROR_SPIKE,
        severity=AnomalySeverity.WARNING, service="svc-a",
        evidence="test", timestamp=now,
    ))

    anomalies = store.anomalies_since(now - timedelta(minutes=1))
    assert len(anomalies) == 1

    svc_anomalies = store.anomalies_for_service("svc-a", now - timedelta(minutes=1))
    assert len(svc_anomalies) == 1

    store.prune_old(hours=0)
    assert len(store.anomalies_since(now - timedelta(minutes=1))) == 0


def test_error_spans_query():
    store = TraceStore(ttl_seconds=3600)
    now = datetime.now(timezone.utc)
    store.upsert_span(SpanNode(id="s1", trace_id="t1", service="svc-a", is_error=True, timestamp=now))
    store.upsert_span(SpanNode(id="s2", trace_id="t1", service="svc-a", is_error=False, timestamp=now))

    errors = store.error_spans("svc-a", now - timedelta(minutes=1))
    assert len(errors) == 1
    assert errors[0].id == "s1"
