"""Anomaly detection: error spikes, latency degradation, metric z-score."""

from __future__ import annotations

import time
from datetime import datetime, timedelta, timezone
from typing import TYPE_CHECKING

from src.graphrag.schema import AnomalyNode, AnomalySeverity, AnomalyType

if TYPE_CHECKING:
    from src.graphrag.builder import GraphRAG


def classify_error_severity(error_rate: float) -> AnomalySeverity:
    if error_rate > 0.2:
        return AnomalySeverity.CRITICAL
    if error_rate > 0.1:
        return AnomalySeverity.WARNING
    return AnomalySeverity.INFO


def classify_latency_severity(avg_ms: float) -> AnomalySeverity:
    if avg_ms > 2000:
        return AnomalySeverity.CRITICAL
    if avg_ms > 1000:
        return AnomalySeverity.WARNING
    return AnomalySeverity.INFO


def correlate_with_recent(g: "GraphRAG", anomaly: AnomalyNode) -> None:
    """Link an anomaly to other anomalies within +/- 30s."""
    window = timedelta(seconds=30)
    recent = g.anomaly_store.anomalies_since(anomaly.timestamp - window)
    for prev in recent:
        if prev.id == anomaly.id:
            continue
        if (anomaly.timestamp - window) < prev.timestamp < (anomaly.timestamp + window):
            g.anomaly_store.add_preceded_by_edge(anomaly.id, prev.id, anomaly.timestamp)


def detect_anomalies(g: "GraphRAG") -> None:
    """Run anomaly detection across all services."""
    services = g.service_store.all_services()
    now = datetime.now(timezone.utc)

    for svc in services:
        # Error rate spike
        baseline_error_rate = 0.02
        if svc.error_rate > baseline_error_rate * 2 and svc.error_rate > 0.05:
            anomaly = AnomalyNode(
                id=f"anom_{svc.name}_err_{int(now.timestamp() * 1e9)}",
                type=AnomalyType.ERROR_SPIKE,
                severity=classify_error_severity(svc.error_rate),
                service=svc.name,
                evidence=f"error rate {svc.error_rate*100:.1f}% (baseline ~{baseline_error_rate*100:.1f}%)",
                timestamp=now,
            )
            g.anomaly_store.add_anomaly(anomaly)
            correlate_with_recent(g, anomaly)

            # Trigger investigation
            from src.graphrag.queries import error_chain
            chains = error_chain(g, svc.name, now - timedelta(minutes=5), limit=5)
            if chains:
                anomalies = g.anomaly_store.anomalies_for_service(svc.name, now - timedelta(minutes=1))
                g.persist_investigation(svc.name, chains, anomalies)

        # Latency degradation
        if svc.avg_latency > 500 and svc.call_count > 10:
            anomaly = AnomalyNode(
                id=f"anom_{svc.name}_lat_{int(now.timestamp() * 1e9)}",
                type=AnomalyType.LATENCY_SPIKE,
                severity=classify_latency_severity(svc.avg_latency),
                service=svc.name,
                evidence=f"avg latency {svc.avg_latency:.0f}ms",
                timestamp=now,
            )
            g.anomaly_store.add_anomaly(anomaly)
            correlate_with_recent(g, anomaly)

    # Metric z-score anomalies
    with g.signal_store._lock:
        metrics_snap = list(g.signal_store.metrics.values())

    for m in metrics_snap:
        if m.sample_count < 10:
            continue
        range_size = m.rolling_max - m.rolling_min
        if range_size > 0:
            deviation = (m.rolling_avg - (m.rolling_min + range_size / 2)) / (range_size / 2)
            if abs(deviation) > 3.0:
                anomaly = AnomalyNode(
                    id=f"anom_{m.service}_metric_{int(now.timestamp() * 1e9)}",
                    type=AnomalyType.METRIC_ZSCORE,
                    severity=AnomalySeverity.WARNING,
                    service=m.service,
                    evidence=f"metric {m.metric_name} z-score {deviation:.1f} (avg={m.rolling_avg:.2f}, range=[{m.rolling_min:.2f}, {m.rolling_max:.2f}])",
                    timestamp=now,
                )
                g.anomaly_store.add_anomaly(anomaly)
                correlate_with_recent(g, anomaly)
