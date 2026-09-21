#!/usr/bin/env python3
"""Send reproducible application-map fixtures through the real OTLP HTTP receiver.

Example: python3 scripts/simulate_application_map.py --url http://127.0.0.1:18080 \
    --shape connected --services 150 --batches 10 --interval 1 --report /tmp/map.json

Each dependency uses a valid two-service parent/child trace. Service-level cycles
use separate traces. No graph API interception, database edits, or third-party
Python packages are used. Each batch acknowledges its parent export before
exporting children so the live topology join sees every parent first. For
deterministic immediate topology, run the receiver with INGEST_ASYNC_ENABLED=false.
Use a fresh database when comparing different shapes.
"""

import argparse
import hashlib
import json
import sys
import time
import urllib.error
import urllib.request
from pathlib import Path
from typing import Any


DOMAINS = (
    "checkout", "identity", "catalog", "fulfillment", "search", "billing",
    "messaging", "analytics", "media", "platform",
)
ROLES = (
    "gateway", "api", "validator", "worker", "cache", "store", "queue",
    "indexer", "notifier", "scheduler", "audit", "adapter", "reader", "writer",
    "pipeline",
)


def topology(shape, services):
    nodes = []
    for index in range(services):
        group, member = divmod(index, len(ROLES))
        domain = DOMAINS[group % len(DOMAINS)]
        suffix = "" if group < len(DOMAINS) else "-" + str(group // len(DOMAINS) + 1)
        nodes.append({
            "name": domain + suffix + "-" + ROLES[member],
            "host": "node-{:02d}-{}".format(group + 1, member % 3 + 1),
        })
    connected = 0 if shape == "disconnected" else services
    if shape == "mixed":
        connected = services - max(1, services // 5)
    edges = []
    roots = []
    for start in range(0, connected, len(ROLES)):
        size = min(len(ROLES), connected - start)
        roots.append(start)
        for member in range(1, size):
            edges.append((start + (member - 1) // 3, start + member))
        # A fan-in and a service-level cycle, expressed as independent traces.
        if size > 7:
            edges.extend(((start + 4, start + 5), (start + 5, start + 4), (start + 6, start + 7)))
    if shape == "large":
        edges.extend((roots[0], root) for root in roots[1:])
    return nodes, edges


def string_attribute(key, value):
    return {"key": key, "value": {"stringValue": value}}


def span(nodes, service, trace_id, span_id, parent_id, batch, now):
    duration_ms = 8 + (service * 11 + batch * 3) % 85
    failed = service % 29 == 9
    value: dict[str, Any] = {
        "traceId": trace_id,
        "spanId": span_id,
        "name": nodes[service]["name"] + " request",
        "kind": 2,
        "startTimeUnixNano": str(now - duration_ms * 1_000_000),
        "endTimeUnixNano": str(now),
        "status": {"code": 2 if failed else 1},
        "attributes": [
            string_attribute("http.request.method", "GET"),
            string_attribute("http.route", "/application-map/fixture"),
            {"key": "http.response.status_code", "value": {"intValue": "500" if failed else "200"}},
        ],
    }
    if parent_id:
        value["parentSpanId"] = parent_id
    return value


def payload(nodes, edges, batch, run_id):
    now = time.time_ns()
    by_service = [[] for _ in nodes]
    for index, (source, target) in enumerate(edges):
        identity = "{}:{}:{}".format(run_id, batch, index).encode()
        trace_id = hashlib.blake2b(identity, digest_size=16).hexdigest()
        parent_id = hashlib.blake2b(identity + b":parent", digest_size=8).hexdigest()
        child_id = hashlib.blake2b(identity + b":child", digest_size=8).hexdigest()
        parent = span(nodes, source, trace_id, parent_id, "", batch, now)
        child = span(nodes, target, trace_id, child_id, parent_id, batch, now - 1_000_000)
        # Enclose the downstream span without falsifying either duration.
        parent["startTimeUnixNano"] = str(min(int(parent["startTimeUnixNano"]), int(child["startTimeUnixNano"]) - 1_000_000))
        by_service[source].append(parent)
        by_service[target].append(child)
    for index, spans in enumerate(by_service):
        if not spans:
            identity = "{}:{}:isolated:{}".format(run_id, batch, index).encode()
            spans.append(span(nodes, index,
                              hashlib.blake2b(identity, digest_size=16).hexdigest(),
                              hashlib.blake2b(identity, digest_size=8).hexdigest(),
                              "", batch, now))
    resources = []
    for node, spans in zip(nodes, by_service):
        resources.append({
            "resource": {"attributes": [
                string_attribute("service.name", node["name"]),
                string_attribute("host.name", node["host"]),
            ]},
            "scopeSpans": [{"scope": {"name": "otelcontext.application-map-fixture"}, "spans": spans}],
        })
    return {"resourceSpans": resources}, sum(map(len, by_service))


def ordered_exports(body):
    """Separate parents from children; resource batches are processed concurrently."""
    for children in (False, True):
        resources = []
        count = 0
        for resource in body["resourceSpans"]:
            scopes = []
            for scope in resource["scopeSpans"]:
                spans = [item for item in scope["spans"] if bool(item.get("parentSpanId")) == children]
                if spans:
                    scopes.append({**scope, "spans": spans})
                    count += len(spans)
            if scopes:
                resources.append({**resource, "scopeSpans": scopes})
        if resources:
            yield {"resourceSpans": resources}, count


def main():
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--url", default="http://127.0.0.1:8080", help="OtelContext HTTP base URL")
    parser.add_argument("--shape", choices=("connected", "disconnected", "mixed", "large"), default="connected")
    parser.add_argument("--services", type=int, default=150)
    parser.add_argument("--batches", type=int, default=1)
    parser.add_argument("--interval", type=float, default=1, help="Seconds between acknowledged batches")
    parser.add_argument("--report", type=Path, help="Write the fixture and export receipt as JSON")
    args = parser.parse_args()
    if args.services < 1 or args.batches < 1 or args.interval < 0:
        parser.error("services and batches must be positive; interval cannot be negative")
    nodes, edges = topology(args.shape, args.services)
    connected = {index for edge in edges for index in edge}
    report: dict[str, Any] = {
        "shape": args.shape,
        "url": args.url.rstrip("/"),
        "services": len(nodes),
        "connected_services": len(connected),
        "disconnected_services": len(nodes) - len(connected),
        "dependencies": len(edges),
        "batches": args.batches,
        "acknowledged_batches": 0,
        "attempted_exports": 0,
        "acknowledged_exports": 0,
        "attempted_spans": 0,
        "acknowledged_spans": 0,
        "export_errors": [],
        "service_names": [node["name"] for node in nodes],
        "edges": [{"source": nodes[source]["name"], "target": nodes[target]["name"]} for source, target in edges],
    }
    run_id = str(time.time_ns())
    started = time.monotonic()
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    for batch in range(args.batches):
        body, _ = payload(nodes, edges, batch, run_id)
        for export, count in ordered_exports(body):
            report["attempted_spans"] += count
            report["attempted_exports"] += 1
            request = urllib.request.Request(report["url"] + "/v1/traces", data=json.dumps(export).encode(),
                                             headers={"Content-Type": "application/json"}, method="POST")
            try:
                with opener.open(request, timeout=20) as response:
                    result = json.loads(response.read() or b"{}")
                rejected = int(result.get("partialSuccess", {}).get("rejectedSpans", 0))
                report["acknowledged_spans"] += count - rejected
                report["acknowledged_exports"] += 1
                if rejected:
                    report["export_errors"].append("batch {}: {} rejected spans".format(batch + 1, rejected))
            except (urllib.error.URLError, ValueError, TimeoutError) as error:
                report["export_errors"].append("batch {}: {}".format(batch + 1, error))
            if report["export_errors"]:
                break
        if report["export_errors"]:
            break
        report["acknowledged_batches"] += 1
        if batch + 1 < args.batches:
            time.sleep(args.interval)
    report["elapsed_seconds"] = round(time.monotonic() - started, 3)
    if args.report:
        args.report.parent.mkdir(parents=True, exist_ok=True)
        args.report.write_text(json.dumps(report, indent=2) + "\n")
    print(json.dumps({key: value for key, value in report.items() if key not in ("service_names", "edges")}, indent=2))
    return 1 if report["export_errors"] else 0


if __name__ == "__main__":
    sys.exit(main())
