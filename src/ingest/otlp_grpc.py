"""gRPC OTLP ingestion server on :4317.

Uses grpcio with opentelemetry-proto for Trace, Logs, and Metrics services.
"""

from __future__ import annotations

import asyncio
import json
import logging
from concurrent import futures
from datetime import datetime, timezone
from typing import Any, Callable

logger = logging.getLogger(__name__)

# These are set during configuration
_repo: Any = None
_metrics: Any = None
_tsdb_agg: Any = None
_callbacks: dict[str, list[Callable]] = {"span": [], "log": [], "metric": []}


def configure(repo: Any, metrics: Any, tsdb_agg: Any = None) -> None:
    global _repo, _metrics, _tsdb_agg
    _repo = repo
    _metrics = metrics
    _tsdb_agg = tsdb_agg


def add_callback(event_type: str, fn: Callable) -> None:
    _callbacks.setdefault(event_type, []).append(fn)


def _start_grpc_server(port: str) -> Any:
    """Start the gRPC server. Returns the server object for shutdown."""
    try:
        import grpc
        from opentelemetry.proto.collector.trace.v1 import trace_service_pb2, trace_service_pb2_grpc
        from opentelemetry.proto.collector.logs.v1 import logs_service_pb2, logs_service_pb2_grpc
        from opentelemetry.proto.collector.metrics.v1 import metrics_service_pb2, metrics_service_pb2_grpc
    except ImportError:
        logger.warning("grpcio or opentelemetry-proto not installed; gRPC server disabled")
        return None

    class TraceService(trace_service_pb2_grpc.TraceServiceServicer):
        def Export(self, request, context):
            from google.protobuf.json_format import MessageToDict
            data = MessageToDict(request)
            from src.ingest.otlp_http import _parse_otlp_json_traces
            spans, traces = _parse_otlp_json_traces(data)

            if _metrics:
                _metrics.otlp_spans_received.inc(len(spans))

            # Persist synchronously in thread pool
            if _repo and spans:
                loop = asyncio.new_event_loop()
                try:
                    loop.run_until_complete(_repo.batch_create_spans(spans))
                    loop.run_until_complete(_repo.batch_create_traces(traces))
                except Exception as e:
                    logger.error("gRPC: failed to persist traces: %s", e)
                finally:
                    loop.close()

            for span_dict in spans:
                for cb in _callbacks.get("span", []):
                    try:
                        cb(span_dict)
                    except Exception:
                        pass

            return trace_service_pb2.ExportTraceServiceResponse()

    class LogsService(logs_service_pb2_grpc.LogsServiceServicer):
        def Export(self, request, context):
            from google.protobuf.json_format import MessageToDict
            data = MessageToDict(request)
            from src.ingest.otlp_http import _parse_otlp_json_logs
            logs = _parse_otlp_json_logs(data)

            if _metrics:
                _metrics.otlp_logs_received.inc(len(logs))

            if _repo and logs:
                loop = asyncio.new_event_loop()
                try:
                    loop.run_until_complete(_repo.batch_create_logs(logs))
                except Exception as e:
                    logger.error("gRPC: failed to persist logs: %s", e)
                finally:
                    loop.close()

            for log_dict in logs:
                for cb in _callbacks.get("log", []):
                    try:
                        cb(log_dict)
                    except Exception:
                        pass

            return logs_service_pb2.ExportLogsServiceResponse()

    class MetricsService(metrics_service_pb2_grpc.MetricsServiceServicer):
        def Export(self, request, context):
            from google.protobuf.json_format import MessageToDict
            data = MessageToDict(request)
            from src.ingest.otlp_http import _parse_otlp_json_metrics
            raw_metrics = _parse_otlp_json_metrics(data)

            if _metrics:
                _metrics.otlp_metrics_received.inc(len(raw_metrics))

            if _tsdb_agg:
                loop = asyncio.new_event_loop()
                try:
                    for m in raw_metrics:
                        loop.run_until_complete(_tsdb_agg.ingest(m))
                except Exception as e:
                    logger.error("gRPC: failed to ingest metrics: %s", e)
                finally:
                    loop.close()

            for m in raw_metrics:
                for cb in _callbacks.get("metric", []):
                    try:
                        cb(m)
                    except Exception:
                        pass

            return metrics_service_pb2.ExportMetricsServiceResponse()

    server = grpc.server(futures.ThreadPoolExecutor(max_workers=10))
    trace_service_pb2_grpc.add_TraceServiceServicer_to_server(TraceService(), server)
    logs_service_pb2_grpc.add_LogsServiceServicer_to_server(LogsService(), server)
    metrics_service_pb2_grpc.add_MetricsServiceServicer_to_server(MetricsService(), server)

    server.add_insecure_port(f"[::]:{port}")
    server.start()
    logger.info("gRPC OTLP receiver started on port %s", port)
    return server
