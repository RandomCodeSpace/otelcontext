"""MCP tool definitions and execution (22 tools)."""

from __future__ import annotations

import json
from datetime import datetime, timedelta, timezone
from typing import Any


def list_tools() -> list[dict]:
    """Return the list of all 22 MCP tools."""
    return [
        # Legacy tools (12)
        _tool("get_system_graph", "Get the full system topology with health scores", {}),
        _tool("get_service_health", "Get health info for a specific service", {"service": {"type": "string"}}),
        _tool("search_logs", "Search logs with filters", {
            "service": {"type": "string"}, "severity": {"type": "string"},
            "query": {"type": "string"}, "limit": {"type": "integer"},
        }),
        _tool("tail_logs", "Get most recent logs", {"limit": {"type": "integer"}}),
        _tool("get_trace", "Get a specific trace by ID", {"trace_id": {"type": "string"}}),
        _tool("search_traces", "Search traces with filters", {
            "service": {"type": "string"}, "limit": {"type": "integer"},
        }),
        _tool("get_metrics", "Get metric buckets", {
            "name": {"type": "string"}, "service": {"type": "string"},
        }),
        _tool("get_dashboard_stats", "Get dashboard statistics", {}),
        _tool("get_storage_status", "Get storage statistics", {}),
        _tool("find_similar_logs", "Find logs similar to a query", {
            "query": {"type": "string"}, "k": {"type": "integer"},
        }),
        _tool("get_alerts", "Get current alerts/anomalies", {}),
        _tool("search_cold_archive", "Search cold archive", {
            "query": {"type": "string"}, "type": {"type": "string"},
        }),
        # GraphRAG tools (10)
        _tool("get_service_map", "Get service topology map", {
            "depth": {"type": "integer"}, "service": {"type": "string"},
        }),
        _tool("get_error_chains", "Get error chains for a service", {
            "service": {"type": "string"}, "time_range": {"type": "string"}, "limit": {"type": "integer"},
        }),
        _tool("trace_graph", "Get full span tree for a trace", {"trace_id": {"type": "string"}}),
        _tool("impact_analysis", "Analyze blast radius of a service failure", {
            "service": {"type": "string"}, "depth": {"type": "integer"},
        }),
        _tool("root_cause_analysis", "Find probable root causes for errors", {
            "service": {"type": "string"}, "time_range": {"type": "string"},
        }),
        _tool("correlated_signals", "Get all correlated signals for a service", {
            "service": {"type": "string"}, "time_range": {"type": "string"},
        }),
        _tool("get_investigations", "Query persisted investigations", {
            "service": {"type": "string"}, "severity": {"type": "string"},
            "status": {"type": "string"}, "limit": {"type": "integer"},
        }),
        _tool("get_investigation", "Get a single investigation by ID", {"investigation_id": {"type": "string"}}),
        _tool("get_graph_snapshot", "Get historical graph snapshot", {"time": {"type": "string"}}),
        _tool("get_anomaly_timeline", "Get recent anomalies", {
            "since": {"type": "string"}, "service": {"type": "string"},
        }),
    ]


def _tool(name: str, description: str, properties: dict) -> dict:
    return {
        "name": name,
        "description": description,
        "inputSchema": {
            "type": "object",
            "properties": properties,
        },
    }


async def execute_tool(app: Any, tool_name: str, args: dict) -> Any:
    """Execute a tool and return the result."""
    repo = app.state.repo
    graphrag = getattr(app.state, "graphrag", None)
    vector_idx = getattr(app.state, "vector_idx", None)

    # --- Legacy tools ---

    if tool_name == "get_system_graph":
        from src.api.graph import _build_from_graphrag, _build_empty
        if graphrag:
            return _build_from_graphrag(graphrag) or _build_empty()
        return _build_empty()

    if tool_name == "get_service_health":
        svc_name = args.get("service", "")
        if graphrag:
            svc = graphrag.service_store.get_service(svc_name)
            if svc:
                return svc.to_dict()
        return {"error": "service not found"}

    if tool_name == "search_logs":
        logs, total = await repo.get_logs_v2(
            service_name=args.get("service", ""),
            severity=args.get("severity", ""),
            search=args.get("query", ""),
            limit=args.get("limit", 20),
        )
        return {"data": logs, "total": total}

    if tool_name == "tail_logs":
        logs, total = await repo.get_logs_v2(limit=args.get("limit", 20))
        return {"data": logs, "total": total}

    if tool_name == "get_trace":
        trace = await repo.get_trace(args.get("trace_id", ""))
        return trace or {"error": "trace not found"}

    if tool_name == "search_traces":
        return await repo.get_traces_filtered(
            start=None, end=None,
            service_names=[args["service"]] if args.get("service") else None,
            status=None, search=None,
            limit=args.get("limit", 20),
        )

    if tool_name == "get_metrics":
        return await repo.get_metric_buckets(None, None, args.get("service", ""), args.get("name", ""))

    if tool_name == "get_dashboard_stats":
        now = datetime.now(timezone.utc)
        return await repo.get_dashboard_stats(now - timedelta(minutes=30), now)

    if tool_name == "get_storage_status":
        return await repo.get_stats()

    if tool_name == "find_similar_logs":
        if vector_idx:
            results = vector_idx.search(args.get("query", ""), k=args.get("k", 10))
            return [{"log_id": r.log_id, "score": r.score, "body": r.body} for r in results]
        return []

    if tool_name == "get_alerts":
        if graphrag:
            from src.graphrag.queries import anomaly_timeline
            anomalies = anomaly_timeline(graphrag, datetime.now(timezone.utc) - timedelta(hours=1))
            return [a.to_dict() for a in anomalies]
        return []

    if tool_name == "search_cold_archive":
        from src.archive.archiver import search_cold_archive
        cold_path = getattr(app.state, "cold_storage_path", "./data/cold")
        return await search_cold_archive(cold_path, args.get("query", ""), args.get("type", "logs"))

    # --- GraphRAG tools ---

    if tool_name == "get_service_map":
        if graphrag:
            from src.graphrag.queries import service_map
            entries = service_map(graphrag, args.get("depth", 0))
            return [{"service": e.service.to_dict() if e.service else None} for e in entries]
        return []

    if tool_name == "get_error_chains":
        if graphrag:
            from src.graphrag.queries import error_chain
            since = datetime.now(timezone.utc) - timedelta(hours=1)
            if args.get("time_range"):
                try:
                    since = datetime.fromisoformat(args["time_range"])
                except ValueError:
                    pass
            chains = error_chain(graphrag, args.get("service", ""), since, args.get("limit", 10))
            return [
                {
                    "root_cause": c.root_cause.to_dict() if c.root_cause else None,
                    "trace_id": c.trace_id,
                    "span_chain": [s.to_dict() for s in c.span_chain],
                }
                for c in chains
            ]
        return []

    if tool_name == "trace_graph":
        if graphrag:
            from src.graphrag.queries import dependency_chain
            spans = dependency_chain(graphrag, args.get("trace_id", ""))
            return [s.to_dict() for s in spans]
        return []

    if tool_name == "impact_analysis":
        if graphrag:
            from src.graphrag.queries import impact_analysis
            result = impact_analysis(graphrag, args.get("service", ""), args.get("depth", 5))
            return {
                "service": result.service,
                "total_downstream": result.total_downstream,
                "affected_services": [
                    {"service": a.service, "depth": a.depth, "impact_score": a.impact_score}
                    for a in result.affected_services
                ],
            }
        return {"service": args.get("service", ""), "affected_services": [], "total_downstream": 0}

    if tool_name == "root_cause_analysis":
        if graphrag:
            from src.graphrag.queries import root_cause_analysis
            since = datetime.now(timezone.utc) - timedelta(hours=1)
            ranked = root_cause_analysis(graphrag, args.get("service", ""), since)
            return [
                {"service": r.service, "operation": r.operation, "score": r.score, "evidence": r.evidence}
                for r in ranked
            ]
        return []

    if tool_name == "correlated_signals":
        if graphrag:
            from src.graphrag.queries import correlated_signals
            since = datetime.now(timezone.utc) - timedelta(hours=1)
            result = correlated_signals(graphrag, args.get("service", ""), since)
            return {
                "service": result.service,
                "error_logs": [lc.to_dict() for lc in result.error_logs],
                "metrics": [m.to_dict() for m in result.metrics],
                "anomalies": [a.to_dict() for a in result.anomalies],
            }
        return {"service": args.get("service", ""), "error_logs": [], "metrics": [], "anomalies": []}

    if tool_name == "get_investigations":
        return await repo.get_investigations(
            service=args.get("service", ""),
            severity=args.get("severity", ""),
            status=args.get("status", ""),
            limit=args.get("limit", 20),
        )

    if tool_name == "get_investigation":
        inv = await repo.get_investigation(args.get("investigation_id", ""))
        return inv or {"error": "investigation not found"}

    if tool_name == "get_graph_snapshot":
        at_str = args.get("time", "")
        try:
            at = datetime.fromisoformat(at_str) if at_str else datetime.now(timezone.utc)
        except ValueError:
            at = datetime.now(timezone.utc)
        snap = await repo.get_graph_snapshot(at)
        return snap or {"error": "no snapshot found"}

    if tool_name == "get_anomaly_timeline":
        if graphrag:
            from src.graphrag.queries import anomaly_timeline
            since = datetime.now(timezone.utc) - timedelta(hours=1)
            if args.get("since"):
                try:
                    since = datetime.fromisoformat(args["since"])
                except ValueError:
                    pass
            anomalies = anomaly_timeline(graphrag, since)
            svc_filter = args.get("service", "")
            if svc_filter:
                anomalies = [a for a in anomalies if a.service == svc_filter]
            return [a.to_dict() for a in anomalies]
        return []

    raise ValueError(f"Unknown tool: {tool_name}")
