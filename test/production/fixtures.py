#!/usr/bin/env python3
"""Bounded synthetic Prometheus history and OTLP host fixtures. Standard library only."""

import argparse
import contextlib
import datetime as dt
import fcntl
import json
import math
import pathlib
import re
import ssl
import subprocess
import time
import urllib.parse
import urllib.request

CADENCE = 60
BLOCK_SECONDS = 7200
HISTOGRAM = "fixture.request.duration"
HOST_METRICS = (
    "system.cpu.utilization", "system.memory.utilization",
    "system.filesystem.utilization", "system.memory.usage", "system.filesystem.usage",
)


def timestamp(raw):
    parsed = dt.datetime.fromisoformat(raw.replace("Z", "+00:00"))
    if parsed.tzinfo is None or parsed.microsecond:
        raise ValueError("end-time must include a timezone and whole seconds")
    return int(parsed.timestamp())


def utc(seconds):
    return dt.datetime.fromtimestamp(seconds, dt.timezone.utc).isoformat().replace("+00:00", "Z")


def host_attributes(index):
    return {
        "host.id": f"mock-host-id-{index:03d}", "host.name": f"mock-node-{index:03d}",
        "service.instance.id": f"mock-host-id-{index:03d}",
        "host.arch": "amd64", "os.type": "linux", "host.cpu.model": "Mock 8-core server",
        "host.cpu.logical.count": 8, "host.memory.total": 32 << 30,
        "k8s.node.name": f"mock-node-{index:03d}", "k8s.namespace.name": "loadtest",
    }


def host_values(index):
    cpu, memory, disk = (base + (index % 10) / 100 for base in (0.2, 0.4, 0.3))
    return cpu, memory, disk, memory * (32 << 30), disk * (100 << 30)


def selector(name, labels):
    # UTF-8 OpenMetrics/PromQL names must be quoted inside braces.
    return "{" + ",".join([json.dumps(name)] + [
        f"{json.dumps(k)}={json.dumps(str(v))}" for k, v in sorted(labels.items())
    ]) + "}"


def service_labels(index, prefix, hosts):
    return {
        "service.name": f"{prefix}{index:03d}", "service.instance.id": f"history-svc-{index:03d}",
        "host.id": f"mock-host-id-{index % hosts:03d}",
        "host.name": f"mock-node-{index % hosts:03d}", "fixture.kind": "history",
        "job": f"{prefix}{index:03d}", "instance": f"history-svc-{index:03d}",
    }


def series_specs(services, hosts, prefix):
    """Return label sets and value functions, never a matrix of all samples."""
    out = []
    for i in range(services):
        labels = service_labels(i, prefix, hosts)
        out.extend((
            (selector("queue.depth", labels), lambda n, i=i: 10 + i % 17),
            (selector("http.server.request.count", labels), lambda n, i=i: (n + 1) * CADENCE * (i + 1)),
            (selector("custom.reset.counter", labels), lambda n, i=i: (n % 60 + 1) * CADENCE * (i + 1)),
        ))
    for i in range(hosts):
        attrs = host_attributes(i)
        labels = {k: attrs[k] for k in ("host.id", "host.name", "service.instance.id")}
        labels.update({"fixture.kind": "history", "instance": attrs["host.id"]})
        out.extend((selector(name, labels), lambda n, value=value: value)
                   for name, value in zip(HOST_METRICS, host_values(i)))
    labels = service_labels(0, prefix, hosts)
    for le, count in (("0.1", 50), ("0.5", 90), ("1", 100), ("+Inf", 100)):
        out.append((selector(HISTOGRAM + "_bucket", dict(labels, le=le)),
                    lambda n, count=count: (n + 1) * count))
    out.append((selector(HISTOGRAM + "_count", labels), lambda n: (n + 1) * 100))
    out.append((selector(HISTOGRAM + "_sum", labels), lambda n: (n + 1) * 25))
    return out


def write_block(path, specs, start, end, history_start):
    count = 0
    with path.open("x") as output:
        for ts in range(start, end, CADENCE):
            n = (ts - history_start) // CADENCE
            for labels, value in specs:
                output.write(f"{labels} {value(n):.12g} {ts}\n")
                count += 1
        output.write("# EOF\n")
    return count


@contextlib.contextmanager
def empty_fixture(path):
    """Only a new/empty output is allowed. Never resume or merge live data."""
    if path.is_symlink():
        raise ValueError("fixture output cannot be a symlink")
    path.mkdir(parents=True, exist_ok=True)
    if any(path.iterdir()):
        raise ValueError(f"fixture output must be empty: {path}")
    # Exclusive creation also serializes simultaneous fixture generators.
    with (path / ".fixture.lock").open("x") as guard:
        fcntl.flock(guard, fcntl.LOCK_EX | fcntl.LOCK_NB)
        yield


def expected_queries(start, end, services, hosts, prefix):
    labels = {"service.name": prefix + "000", "fixture.kind": "history"}
    gauge = selector("queue.depth", labels)
    counter = selector("http.server.request.count", labels)
    reset = selector("custom.reset.counter", labels)
    buckets = selector(HISTOGRAM + "_bucket", labels)
    reset_time = end - 3600 + 120  # Two minutes after the final hourly reset.
    checks = [
        {"name": "gauge", "query": gauge, "time": end - CADENCE, "value": 10,
         "labels": service_labels(0, prefix, hosts)},
        {"name": "service_count", "query": 'count(' + selector("queue.depth", {"fixture.kind": "history"}) + ')', "time": end - CADENCE, "value": services},
        {"name": "host_count", "query": 'count(' + selector(HOST_METRICS[0], {"fixture.kind": "history"}) + ')', "time": end - CADENCE, "value": hosts},
        {"name": "host_gauge", "query": selector(HOST_METRICS[0], {"host.id": "mock-host-id-000", "fixture.kind": "history"}),
         "time": end - CADENCE, "value": 0.2,
         "labels": {"host.name": "mock-node-000", "service.instance.id": "mock-host-id-000"}},
        {"name": "counter_rate", "query": f"rate({counter}[5m])", "time": end - CADENCE, "value": 1},
        {"name": "counter_increase", "query": f"increase({counter}[5m])", "time": end - CADENCE, "value": 300},
        {"name": "reset_rate", "query": f"rate({reset}[5m])", "time": reset_time, "value": 1},
        {"name": "reset_increase", "query": f"increase({reset}[5m])", "time": reset_time, "value": 300},
        {"name": "resets", "query": f"resets({reset}[5m])", "time": reset_time, "value": 1},
        {"name": "histogram_p95", "query": f"histogram_quantile(0.95, sum by (le) (rate({buckets}[5m])))", "time": end - CADENCE, "value": 0.75},
        {"name": "earliest_gauge", "query": gauge, "time": start, "value": 10, "before_retention": True},
        {"name": "raw_history_bounds", "query": f"{gauge}[{end - start}s]", "time": end - CADENCE,
         "first": start, "last": end - CADENCE, "samples": (end - start) // CADENCE, "before_retention": True},
    ]
    if end - start >= 7 * 86400:
        checks.append({"name": "six_days_ago", "query": gauge, "time": end - 6 * 86400, "value": 10})
    return checks


def make_history(args):
    now = int(time.time())
    end = timestamp(args.end_time) if args.end_time else now // BLOCK_SECONDS * BLOCK_SECONDS
    if end > now or end % BLOCK_SECONDS:
        raise ValueError("end-time must be in the past and aligned to two hours UTC")
    if not args.stopped_fixture:
        raise ValueError("--stopped-fixture is required: output is for an isolated stopped Prometheus")
    if not 1 <= args.services <= 200 or not 1 <= args.hosts <= 20:
        raise ValueError("services must be 1..200 and hosts 1..20")
    if not 2 <= args.hours <= 192 or args.hours % 2 or not args.service_prefix:
        raise ValueError("hours must be an even number in 2..192; service-prefix must be nonempty")
    version = subprocess.run([args.promtool, "--version"], check=True, capture_output=True, text=True, timeout=30)
    version_text = version.stdout + version.stderr
    if not re.search(r"version 3\.15\.0(?:[ ,\n]|$)", version_text):
        raise ValueError("this fixture requires promtool 3.15.0")
    start = end - args.hours * 3600
    specs = series_specs(args.services, args.hosts, args.service_prefix)
    manifest: dict = {
        "schema": 1, "simulation": True, "translation_strategy": "NoTranslation",
        "promtool_version": version_text.strip(), "services": args.services, "hosts": args.hosts,
        "service_prefix": args.service_prefix, "cadence_seconds": CADENCE,
        "first_timestamp": start, "last_timestamp": end - CADENCE, "end_exclusive": end,
        "first_utc": utc(start), "end_utc": utc(end), "series": len(specs),
        "samples_per_series": (end - start) // CADENCE,
        "samples": len(specs) * ((end - start) // CADENCE),
        "checks": expected_queries(start, end, args.services, args.hosts, args.service_prefix), "blocks": [],
    }
    output = pathlib.Path(args.out).absolute()
    with empty_fixture(output):
        tsdb = output / "tsdb"
        tsdb.mkdir()
        # Prometheus uses this lock path too. Hold it until block creation finishes.
        with (tsdb / "lock").open("x") as lock:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
            for block_start in range(start, end, BLOCK_SECONDS):
                path = output / f"block-{block_start}.openmetrics"
                samples = write_block(path, specs, block_start, block_start + BLOCK_SECONDS, start)
                with (output / "promtool.log").open("a") as log:
                    subprocess.run([args.promtool, "tsdb", "create-blocks-from", "openmetrics", str(path), str(tsdb)],
                                   check=True, stdout=log, stderr=subprocess.STDOUT, timeout=120)
                # Preserve the input on failure, discard it only after successful import.
                meta_paths = [p for p in tsdb.glob("*/meta.json") if json.loads(p.read_text())["minTime"] == block_start * 1000]
                if len(meta_paths) != 1:
                    raise ValueError(f"expected one block at {block_start}, found {len(meta_paths)}")
                meta = json.loads(meta_paths[0].read_text())
                if (meta["stats"]["numSamples"] != samples or meta["stats"]["numSeries"] != len(specs)
                        or meta["maxTime"] != (block_start + BLOCK_SECONDS - CADENCE) * 1000 + 1):
                    raise ValueError(f"block metadata disagrees with fixture: {meta}")
                manifest["blocks"].append({"ulid": meta["ulid"], "first_timestamp": block_start,
                                           "last_timestamp": block_start + BLOCK_SECONDS - CADENCE,
                                           "series": len(specs), "samples": samples, "openmetrics_bytes": path.stat().st_size})
                path.unlink()
                print(json.dumps({"block": len(manifest["blocks"]), "samples": samples, "first": utc(block_start)}), flush=True)
        (output / "manifest.json").write_text(json.dumps(manifest, indent=2) + "\n")
    print(json.dumps({"manifest": str(output / "manifest.json"), "series": len(specs), "samples": manifest["samples"]}))


def otlp_attributes(attrs):
    return [{"key": k, "value": {"intValue": str(v)} if isinstance(v, int) else {"stringValue": v}}
            for k, v in attrs.items()]


def host_payload(hosts, timestamp_ns):
    return {"resourceMetrics": [
        {"resource": {"attributes": otlp_attributes(host_attributes(i))},
         "scopeMetrics": [{"scope": {"name": "otelcontext.production.hosts"}, "metrics": [
             {"name": name, "unit": "By" if name.endswith(".usage") else "1", "gauge": {"dataPoints": [
                 {"timeUnixNano": str(timestamp_ns), "asDouble": value}
             ]}} for name, value in zip(HOST_METRICS, host_values(i))]}]}
        for i in range(hosts)
    ]}


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


def http_client(url, ca_file, loopback=False):
    parsed = urllib.parse.urlsplit(url)
    allowed_local = loopback and parsed.scheme == "http" and parsed.hostname in ("127.0.0.1", "::1", "localhost")
    if parsed.username or parsed.password or not parsed.hostname or (parsed.scheme != "https" and not allowed_local):
        raise ValueError("use HTTPS with verified certificates, or loopback HTTP for Prometheus verification")
    context = ssl.create_default_context(cafile=ca_file)
    return urllib.request.build_opener(NoRedirect(), urllib.request.HTTPSHandler(context=context))


def validate_otlp_response(status, body):
    if status != 200:
        raise ValueError(f"OTLP HTTP status {status}")
    response = json.loads(body) if body else {}
    if not isinstance(response, dict):
        raise ValueError("OTLP response must be a JSON object")
    partial = response.get("partialSuccess", response.get("partial_success", {})) or {}
    rejected = int(partial.get("rejectedDataPoints", partial.get("rejected_data_points", 0)))
    message = partial.get("errorMessage", partial.get("error_message", ""))
    if rejected or message:
        raise ValueError(f"OTLP partial response: rejected={rejected} message={message}")


def send_hosts(args):
    if not 1 <= args.hosts <= 20 or not 0 < args.interval <= 60 or not 0 < args.duration <= 10800:
        raise ValueError("hosts must be 1..20, interval 0..60s, duration 0..10800s")
    if args.interval < 1:
        raise ValueError("host interval must be at least one second")
    client = http_client(args.url, args.ca_file)
    url = args.url.rstrip("/") + "/v1/metrics"
    batches = math.ceil(args.duration / args.interval)
    stats = {"simulation": True, "hosts": args.hosts, "points_per_request": args.hosts * 5,
             "requests_attempted": 0, "requests_accepted": 0, "points_attempted": 0, "points_accepted": 0}
    latencies = []
    with pathlib.Path(args.report).open("x") as report:
        start = time.monotonic()
        try:
            for batch in range(batches):
                time.sleep(max(0, start + batch * args.interval - time.monotonic()))
                ts = time.time_ns()
                payload = json.dumps(host_payload(args.hosts, ts)).encode()
                req = urllib.request.Request(url, payload, {"Content-Type": "application/json"}, method="POST")
                stats["requests_attempted"] += 1
                stats["points_attempted"] += args.hosts * 5
                sent = time.monotonic()
                with client.open(req, timeout=args.timeout) as response:
                    validate_otlp_response(response.status, response.read(1024 * 1024))
                latency = (time.monotonic() - sent) * 1000
                latencies.append(latency)
                stats["requests_accepted"] += 1
                stats["points_accepted"] += args.hosts * 5
                report.write(json.dumps({"batch": batch, "timestamp_ns": ts, "points": args.hosts * 5, "latency_ms": latency}) + "\n")
                report.flush()
        except Exception as error:
            stats["error"] = str(error)
            raise
        finally:
            stats["elapsed_seconds"] = time.monotonic() - start
            ordered = sorted(latencies)
            stats["latency_ms"] = {"count": len(ordered), "max": max(ordered, default=0),
                                   "p95": ordered[math.ceil(len(ordered) * 0.95) - 1] if ordered else 0}
            report.write(json.dumps({"summary": stats}) + "\n")
            print(json.dumps(stats), flush=True)


def verify_history(args):
    manifest = json.loads(pathlib.Path(args.manifest).read_text())
    client = http_client(args.query_url, args.ca_file, loopback=True)
    for check in manifest["checks"]:
        if check.get("before_retention") and not args.before_retention:
            continue
        url = args.query_url + "?" + urllib.parse.urlencode({"query": check["query"], "time": check["time"]})
        with client.open(url, timeout=30) as response:
            data = json.load(response)
        if data.get("status") != "success" or data.get("warnings"):
            raise ValueError(f'{check["name"]}: Prometheus error/warning {data}')
        rows = data["data"]["result"]
        if len(rows) != 1:
            raise ValueError(f'{check["name"]}: expected one result, got {rows}')
        for label, expected in check.get("labels", {}).items():
            if rows[0]["metric"].get(label) != expected:
                raise ValueError(f'{check["name"]}: wrong {label} label: {rows[0]["metric"]}')
        if "value" in check:
            actual = float(rows[0]["value"][1])
            if not math.isclose(actual, check["value"], rel_tol=1e-9, abs_tol=1e-9):
                raise ValueError(f'{check["name"]}: {actual} != {check["value"]}')
        else:
            samples = rows[0]["values"]
            if len(samples) != check["samples"] or samples[0][0] != check["first"] or samples[-1][0] != check["last"]:
                raise ValueError(f'{check["name"]}: wrong count or timestamp bounds')
        print(json.dumps({"check": check["name"], "ok": True, "time": check["time"]}))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="command", required=True)
    history = commands.add_parser("prometheus", help="create native TSDB blocks in a new isolated fixture directory")
    history.add_argument("--out", required=True)
    history.add_argument("--promtool", required=True)
    history.add_argument("--stopped-fixture", action="store_true")
    history.add_argument("--hours", type=int, default=192, help="168 for seven days, 192 for eight, 2 for a smoke test")
    history.add_argument("--services", type=int, default=200)
    history.add_argument("--hosts", type=int, default=20)
    history.add_argument("--service-prefix", default="loadsim-svc-")
    history.add_argument("--end-time", help="exclusive RFC3339 boundary, aligned to two hours UTC")
    history.set_defaults(run=make_history)
    hosts = commands.add_parser("hosts", help="send mock host gauges via verified HTTPS OTLP JSON")
    hosts.add_argument("--url", required=True, help="app base URL, excluding /v1/metrics")
    hosts.add_argument("--ca-file")
    hosts.add_argument("--hosts", type=int, default=20)
    hosts.add_argument("--duration", type=float, default=60, help="seconds, at most 10800")
    hosts.add_argument("--interval", type=float, default=10, help="seconds")
    hosts.add_argument("--timeout", type=float, default=15)
    hosts.add_argument("--report", required=True, help="new NDJSON evidence file")
    hosts.set_defaults(run=send_hosts)
    verify = commands.add_parser("verify", help="evaluate exact fixture queries; only reads the server")
    verify.add_argument("--manifest", required=True)
    verify.add_argument("--query-url", required=True, help="full query endpoint, e.g. http://127.0.0.1:9090/api/v1/query")
    verify.add_argument("--ca-file")
    verify.add_argument("--before-retention", action="store_true", help="also require the original earliest sample and complete count")
    verify.set_defaults(run=verify_history)
    args = parser.parse_args()
    try:
        args.run(args)
    except (ValueError, OSError, subprocess.SubprocessError) as error:
        parser.exit(1, f"fixture failed: {error}\n")


if __name__ == "__main__":
    main()
