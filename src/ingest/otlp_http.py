"""HTTP OTLP ingestion endpoints: POST /v1/traces, /v1/logs, /v1/metrics.

Supports application/x-protobuf and application/json, gzip decompression, 4MB limit.
"""

from __future__ import annotations

import gzip
import json
import logging
from datetime import datetime, timezone
from typing import Any, Callable, Optional

from fastapi import APIRouter, Request, Response

logger = logging.getLogger(__name__)

MAX_BODY_SIZE = 4 * 1024 * 1024  # 4MB

router = APIRouter()

# These will be set by main.py during init
_repo: Any = None
_metrics: Any = None
_tsdb_agg: Any = None
_sampler: Any = None
_callbacks: dict[str, list[Callable]] = {"span": [], "log": [], "metric": []}


def configure(
    repo: Any,
    metrics: Any,
    tsdb_agg: Any = None,
    sampler: Any = None,
) -> None:
    global _repo, _metrics, _tsdb_agg, _sampler
    _repo = repo
    _metrics = metrics
    _tsdb_agg = tsdb_agg
    _sampler = sampler


def add_callback(event_type: str, fn: Callable) -> None:
    _callbacks.setdefault(event_type, []).append(fn)


async def _read_body(request: Request) -> bytes:
    """Read and optionally decompress the request body."""
    body = await request.body()
    if len(body) > MAX_BODY_SIZE:
        raise ValueError(f"Request body too large: {len(body)} > {MAX_BODY_SIZE}")
    encoding = request.headers.get("content-encoding", "")
    if encoding == "gzip":
        body = gzip.decompress(body)
    return body


def _parse_otlp_json_traces(data: dict) -> list[dict]:
    """Parse OTLP JSON trace data into flat span + trace dicts."""
    spans_out: list[dict] = []
    traces_out: list[dict] = []
    seen_traces: set[str] = set()

    for rs in data.get("resourceSpans", []):
        resource = rs.get("resource", {})
        service_name = ""
        for attr in resource.get("attributes", []):
            if attr.get("key") == "service.name":
                service_name = attr.get("value", {}).get("stringValue", "")

        for ss in rs.get("scopeSpans", []):
            for span in ss.get("spans", []):
                trace_id = span.get("traceId", "")
                span_id = span.get("spanId", "")
                parent_span_id = span.get("parentSpanId", "")
                name = span.get("name", "")
                start_ns = int(span.get("startTimeUnixNano", 0))
                end_ns = int(span.get("endTimeUnixNano", 0))
                duration_us = (end_ns - start_ns) // 1000 if end_ns > start_ns else 0
                start_time = datetime.fromtimestamp(start_ns / 1e9, tz=timezone.utc) if start_ns else datetime.now(timezone.utc)
                end_time = datetime.fromtimestamp(end_ns / 1e9, tz=timezone.utc) if end_ns else start_time

                attrs = {}
                for attr in span.get("attributes", []):
                    k = attr.get("key", "")
                    v = attr.get("value", {})
                    attrs[k] = v.get("stringValue") or v.get("intValue") or v.get("doubleValue") or ""

                status = span.get("status", {})
                status_code = status.get("code", 0)
                is_error = status_code == 2  # STATUS_CODE_ERROR

                span_dict = {
                    "trace_id": trace_id,
                    "span_id": span_id,
                    "parent_span_id": parent_span_id,
                    "operation_name": name,
                    "start_time": start_time,
                    "end_time": end_time,
                    "duration": duration_us,
                    "service_name": service_name,
                    "attributes_json": json.dumps(attrs) if attrs else "",
                }
                spans_out.append(span_dict)

                if trace_id and trace_id not in seen_traces:
                    seen_traces.add(trace_id)
                    traces_out.append({
                        "trace_id": trace_id,
                        "service_name": service_name,
                        "duration": duration_us,
                        "status": "STATUS_CODE_ERROR" if is_error else "OK",
                        "timestamp": start_time,
                    })

    return spans_out, traces_out


def _parse_otlp_json_logs(data: dict) -> list[dict]:
    """Parse OTLP JSON log data."""
    logs_out: list[dict] = []

    for rl in data.get("resourceLogs", []):
        resource = rl.get("resource", {})
        service_name = ""
        for attr in resource.get("attributes", []):
            if attr.get("key") == "service.name":
                service_name = attr.get("value", {}).get("stringValue", "")

        for sl in rl.get("scopeLogs", []):
            for log_record in sl.get("logRecords", []):
                ts_ns = int(log_record.get("timeUnixNano", 0))
                ts = datetime.fromtimestamp(ts_ns / 1e9, tz=timezone.utc) if ts_ns else datetime.now(timezone.utc)
                severity = log_record.get("severityText", "")
                body = ""
                body_val = log_record.get("body", {})
                if isinstance(body_val, dict):
                    body = body_val.get("stringValue", "")
                elif isinstance(body_val, str):
                    body = body_val

                trace_id = log_record.get("traceId", "")
                span_id = log_record.get("spanId", "")

                attrs = {}
                for attr in log_record.get("attributes", []):
                    k = attr.get("key", "")
                    v = attr.get("value", {})
                    attrs[k] = v.get("stringValue") or v.get("intValue") or v.get("doubleValue") or ""

                logs_out.append({
                    "trace_id": trace_id,
                    "span_id": span_id,
                    "severity": severity,
                    "body": body,
                    "service_name": service_name,
                    "attributes_json": json.dumps(attrs) if attrs else "",
                    "timestamp": ts,
                })

    return logs_out


def _parse_otlp_json_metrics(data: dict) -> list[dict]:
    """Parse OTLP JSON metric data into RawMetric-compatible dicts."""
    from src.tsdb.aggregator import RawMetric

    metrics_out: list[RawMetric] = []

    for rm in data.get("resourceMetrics", []):
        resource = rm.get("resource", {})
        service_name = ""
        for attr in resource.get("attributes", []):
            if attr.get("key") == "service.name":
                service_name = attr.get("value", {}).get("stringValue", "")

        for sm in rm.get("scopeMetrics", []):
            for metric in sm.get("metrics", []):
                name = metric.get("name", "")
                # Handle gauge, sum, histogram data points
                for dp_key in ("gauge", "sum", "histogram"):
                    dp_obj = metric.get(dp_key)
                    if dp_obj is None:
                        continue
                    data_points = dp_obj.get("dataPoints", [])
                    for dp in data_points:
                        ts_ns = int(dp.get("timeUnixNano", 0))
                        ts = datetime.fromtimestamp(ts_ns / 1e9, tz=timezone.utc) if ts_ns else datetime.now(timezone.utc)
                        value = dp.get("asDouble") or dp.get("asInt") or dp.get("sum", 0)
                        if isinstance(value, str):
                            try:
                                value = float(value)
                            except ValueError:
                                value = 0.0
                        metrics_out.append(RawMetric(
                            name=name,
                            service_name=service_name,
                            value=float(value),
                            timestamp=ts,
                        ))

    return metrics_out


@router.post("/v1/traces")
async def ingest_traces(request: Request) -> Response:
    try:
        body = await _read_body(request)
    except ValueError as e:
        return Response(content=str(e), status_code=413)

    content_type = request.headers.get("content-type", "")

    if "json" in content_type:
        data = json.loads(body)
        spans, traces = _parse_otlp_json_traces(data)
    elif "protobuf" in content_type:
        # Protobuf parsing via opentelemetry-proto
        try:
            from opentelemetry.proto.collector.trace.v1.trace_service_pb2 import ExportTraceServiceRequest
            req = ExportTraceServiceRequest()
            req.ParseFromString(body)
            # Convert protobuf to JSON then parse
            from google.protobuf.json_format import MessageToDict
            data = MessageToDict(req)
            spans, traces = _parse_otlp_json_traces(data)
        except ImportError:
            return Response(content="protobuf support not available", status_code=501)
    else:
        # Try JSON as default
        try:
            data = json.loads(body)
            spans, traces = _parse_otlp_json_traces(data)
        except json.JSONDecodeError:
            return Response(content="unsupported content type", status_code=415)

    if _metrics:
        _metrics.otlp_spans_received.inc(len(spans))

    # Persist
    if _repo and spans:
        try:
            await _repo.batch_create_spans(spans)
            await _repo.batch_create_traces(traces)
        except Exception as e:
            logger.error("Failed to persist traces: %s", e)

    # Callbacks
    for span_dict in spans:
        for cb in _callbacks.get("span", []):
            try:
                cb(span_dict)
            except Exception:
                pass

    return Response(content="{}", media_type="application/json")


@router.post("/v1/logs")
async def ingest_logs(request: Request) -> Response:
    try:
        body = await _read_body(request)
    except ValueError as e:
        return Response(content=str(e), status_code=413)

    content_type = request.headers.get("content-type", "")

    if "json" in content_type:
        data = json.loads(body)
        logs = _parse_otlp_json_logs(data)
    elif "protobuf" in content_type:
        try:
            from opentelemetry.proto.collector.logs.v1.logs_service_pb2 import ExportLogsServiceRequest
            req = ExportLogsServiceRequest()
            req.ParseFromString(body)
            from google.protobuf.json_format import MessageToDict
            data = MessageToDict(req)
            logs = _parse_otlp_json_logs(data)
        except ImportError:
            return Response(content="protobuf support not available", status_code=501)
    else:
        try:
            data = json.loads(body)
            logs = _parse_otlp_json_logs(data)
        except json.JSONDecodeError:
            return Response(content="unsupported content type", status_code=415)

    if _metrics:
        _metrics.otlp_logs_received.inc(len(logs))

    if _repo and logs:
        try:
            await _repo.batch_create_logs(logs)
        except Exception as e:
            logger.error("Failed to persist logs: %s", e)

    for log_dict in logs:
        for cb in _callbacks.get("log", []):
            try:
                cb(log_dict)
            except Exception:
                pass

    return Response(content="{}", media_type="application/json")


@router.post("/v1/metrics")
async def ingest_metrics(request: Request) -> Response:
    try:
        body = await _read_body(request)
    except ValueError as e:
        return Response(content=str(e), status_code=413)

    content_type = request.headers.get("content-type", "")

    if "json" in content_type:
        data = json.loads(body)
        raw_metrics = _parse_otlp_json_metrics(data)
    elif "protobuf" in content_type:
        try:
            from opentelemetry.proto.collector.metrics.v1.metrics_service_pb2 import ExportMetricsServiceRequest
            req = ExportMetricsServiceRequest()
            req.ParseFromString(body)
            from google.protobuf.json_format import MessageToDict
            data = MessageToDict(req)
            raw_metrics = _parse_otlp_json_metrics(data)
        except ImportError:
            return Response(content="protobuf support not available", status_code=501)
    else:
        try:
            data = json.loads(body)
            raw_metrics = _parse_otlp_json_metrics(data)
        except json.JSONDecodeError:
            return Response(content="unsupported content type", status_code=415)

    if _metrics:
        _metrics.otlp_metrics_received.inc(len(raw_metrics))

    if _tsdb_agg:
        for m in raw_metrics:
            await _tsdb_agg.ingest(m)

    for m in raw_metrics:
        for cb in _callbacks.get("metric", []):
            try:
                cb(m)
            except Exception:
                pass

    return Response(content="{}", media_type="application/json")
