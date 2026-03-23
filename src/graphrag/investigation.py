"""Investigation persistence for GraphRAG."""

from __future__ import annotations

import json
import logging
import time
from datetime import datetime, timezone
from typing import TYPE_CHECKING, Any

from src.graphrag.schema import AnomalyNode, AnomalySeverity, ErrorChainResult

if TYPE_CHECKING:
    from src.graphrag.builder import GraphRAG

logger = logging.getLogger(__name__)


async def persist_investigation(
    g: "GraphRAG",
    trigger_service: str,
    chains: list[ErrorChainResult],
    anomalies: list[AnomalyNode],
) -> None:
    """Save an investigation record from an error chain analysis."""
    if not chains:
        return

    first_chain = chains[0]
    if first_chain.root_cause is None:
        return

    inv_id = f"inv_{int(time.time() * 1e9)}"

    severity = "warning"
    if anomalies:
        for a in anomalies:
            if a.severity == AnomalySeverity.CRITICAL:
                severity = "critical"
                break

    trace_ids = [c.trace_id for c in chains]

    causal = [
        {
            "service": s.service,
            "operation": s.operation,
            "span_id": s.id,
            "is_error": s.is_error,
        }
        for s in first_chain.span_chain
    ]

    from src.graphrag.queries import impact_analysis

    impact = impact_analysis(g, trigger_service, max_depth=3)
    affected = [a.service for a in impact.affected_services]

    inv_data = {
        "id": inv_id,
        "created_at": datetime.now(timezone.utc),
        "status": "detected",
        "severity": severity,
        "trigger_service": trigger_service,
        "trigger_operation": first_chain.root_cause.operation,
        "error_message": first_chain.root_cause.error_message,
        "root_service": first_chain.root_cause.service,
        "root_operation": first_chain.root_cause.operation,
        "causal_chain": json.dumps(causal),
        "trace_ids": json.dumps(trace_ids),
        "error_logs": json.dumps([lc.to_dict() for lc in first_chain.correlated_logs]),
        "anomalous_metrics": json.dumps([m.to_dict() for m in first_chain.anomalous_metrics]),
        "affected_services": json.dumps(affected),
        "span_chain": json.dumps([s.to_dict() for s in first_chain.span_chain]),
    }

    try:
        await g.repo.create_investigation(inv_data)
        logger.info("Investigation persisted: id=%s service=%s severity=%s", inv_id, trigger_service, severity)
    except Exception as e:
        logger.error("Failed to persist investigation: %s", e)
