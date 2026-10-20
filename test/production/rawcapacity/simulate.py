#!/usr/bin/env python3
"""Materialize sampled raw history, then advance production retention cutoffs.

This is synthetic time-shifted replication, not OTLP replay or a seven-day soak.
Only a new marked output directory is writable. The input must be a stopped,
checkpointed OWNED copy; it is opened immutable and its SHA256 is rechecked.
"""

import argparse
import collections
import datetime as dt
import hashlib
import json
from pathlib import Path
import sqlite3
import subprocess
import threading
import time
from typing import Any

UTC = dt.timezone.utc
TABLES = {"traces": "timestamp", "spans": "start_time", "logs": "timestamp"}
TIMES = {"timestamp", "start_time", "end_time", "created_at", "updated_at", "deleted_at"}


def stamp(value):
    return value.astimezone(UTC).strftime("%Y-%m-%d %H:%M:%S")


def parse(value):
    return dt.datetime.fromisoformat(value.replace("Z", "+00:00")).astimezone(UTC)


def shifted(value, seconds):
    # SQLite's strftime and Python's full datetime round-trip lose nanoseconds.
    # Shift only whole seconds; retain the entire fractional/timezone suffix.
    if value is None:
        return None
    return stamp(parse(value[:19] + "+00:00") + dt.timedelta(seconds=seconds)) + value[19:]


def identity(value, tile):
    if not value:
        return value
    return hashlib.sha256(f"rawcapacity-v1/{tile}/{value}".encode()).hexdigest()[:32]


def digest(path):
    with path.open("rb") as stream:
        return hashlib.file_digest(stream, "sha256").hexdigest()


def write_json(path, value):
    path.write_text(json.dumps(value, indent=2) + "\n")


def usage(path) -> dict[str, Any]:
    files = {}
    for suffix in ("", "-wal", "-shm"):
        p = Path(str(path) + suffix)
        try:
            s = p.stat()
            files[suffix or "db"] = {"bytes": s.st_size, "allocated": s.st_blocks * 512}
        except FileNotFoundError:
            # SQLite sidecars may be absent or removed during checkpointing.
            continue
    return {"files": files, "bytes": sum(x["bytes"] for x in files.values()),
            "allocated": sum(x["allocated"] for x in files.values())}


class Peak:
    def __init__(self, path):
        self.path, self.stop = path, threading.Event()
        self.values = collections.defaultdict(int)
        self.thread = threading.Thread(target=self.watch, daemon=True)

    def sample(self):
        u = usage(self.path)
        for key in ("bytes", "allocated"):
            self.values[key] = max(self.values[key], u[key])
        for name, values in u["files"].items():
            for key, value in values.items():
                k = f"{name}_{key}"
                self.values[k] = max(self.values[k], value)

    def watch(self):
        while not self.stop.wait(0.05):
            self.sample()


def inspect(conn):
    schema = conn.execute("SELECT type,name,tbl_name,sql FROM sqlite_master ORDER BY type,name").fetchall()
    objects = conn.execute("SELECT name,sum(pgsize),sum(payload),sum(unused) FROM dbstat GROUP BY name ORDER BY name").fetchall()
    return {"schema": [list(r) for r in schema], "objects": [list(r) for r in objects],
            "page_size": conn.execute("PRAGMA page_size").fetchone()[0],
            "pages": conn.execute("PRAGMA page_count").fetchone()[0],
            "freelist": conn.execute("PRAGMA freelist_count").fetchone()[0],
            "counts": {t: conn.execute(f"SELECT count(*) FROM {t}").fetchone()[0] for t in TABLES},
            "zero_span_headers": conn.execute("SELECT count(*) FROM traces WHERE retained_span_count=0").fetchone()[0]}


def load_template(conn, start, hours):
    end = start + dt.timedelta(hours=hours)
    traces = [dict(r) for r in conn.execute("SELECT * FROM traces WHERE timestamp>=? AND timestamp<? ORDER BY id", (stamp(start), stamp(end)))]
    if not traces or len(traces) > 1_000_000:
        raise ValueError("source window must contain 1..1,000,000 traces")
    anchors = {(r["tenant_id"], r["trace_id"]): r["timestamp"] for r in traces}
    all_parents = {(r[0], r[1]) for r in conn.execute("SELECT tenant_id,trace_id FROM traces")}
    rows = {"traces": traces, "spans": [], "logs": []}
    # Keep trace children with their parent, including a child just across a
    # time-window edge. Preserve standalone or already-dangling observations.
    for table in ("spans", "logs"):
        for row in conn.execute(f"SELECT * FROM {table} ORDER BY id"):
            row = dict(row)
            key = (row["tenant_id"], row["trace_id"])
            anchor = anchors.get(key)
            if anchor is not None or (key not in all_parents and stamp(start) <= row[TABLES[table]] < stamp(end)):
                rows[table].append(row)
    by_hour = [{t: [] for t in TABLES} for _ in range(hours)]
    for table, records in rows.items():
        for row in records:
            anchor = anchors.get((row["tenant_id"], row["trace_id"]), row[TABLES[table]])
            hour = int((parse(anchor) - start).total_seconds() // 3600)
            if 0 <= hour < hours:
                by_hour[hour][table].append(row)
    # A missing parent already present in the observed source is not repaired.
    # Fail if selecting this window would introduce a new broken reference.
    selected = set(anchors)
    for table in ("spans", "logs"):
        for row in rows[table]:
            key = (row["tenant_id"], row["trace_id"])
            if key in all_parents and key not in selected:
                raise ValueError("source window cuts a trace association; choose a different window")
    return rows, by_hour


def append_rows(conn, rows, seconds, tile, max_bytes, path):
    counts = {}
    for table, records in rows.items():
        counts[table] = len(records)
        if not records:
            continue
        columns = [c for c in records[0] if c != "id"]
        sql = f'INSERT INTO {table} ({",".join(columns)}) VALUES ({",".join("?" for _ in columns)})'
        for offset in range(0, len(records), 500):
            if usage(path)["allocated"] > max_bytes:
                raise RuntimeError("fixture exceeded explicit physical-byte bound")
            values = []
            for row in records[offset:offset + 500]:
                values.append([identity(row[c], tile) if c == "trace_id" else
                               shifted(row[c], seconds) if c in TIMES else row[c] for c in columns])
            with conn:
                conn.executemany(sql, values)
    return counts


def helper_command(helper, action, out, *args):
    return [str(helper), "-action", action, "-dir", str(out), *args]


def helper_result(helper, action, out, *args):
    p = subprocess.run(helper_command(helper, action, out, *args), capture_output=True, text=True, timeout=1800)
    if p.returncode:
        raise RuntimeError(f"helper {action} failed: {p.stderr[-2000:]} {p.stdout[-2000:]}")
    # GORM may print an expected not-found diagnostic before the JSON result.
    return json.loads(p.stdout.strip().splitlines()[-1])


def snapshot(conn, path):
    checkpoint = list(conn.execute("PRAGMA wal_checkpoint(TRUNCATE)").fetchone())
    if checkpoint[0]:
        raise RuntimeError(f"busy checkpoint: {checkpoint}")
    return {"usage": usage(path), "pages": conn.execute("PRAGMA page_count").fetchone()[0],
            "freelist": conn.execute("PRAGMA freelist_count").fetchone()[0],
            "counts": {t: conn.execute(f"SELECT count(*) FROM {t}").fetchone()[0] for t in TABLES}}


def rolling_step(conn, path, helper, out, rows, shift, tile, cutoff, step, max_bytes):
    # This small input container lives outside the measured app directory.
    # Only raw.db has production indexes. Go copies the staged values through
    # the SAME production pool used by the purger, preserving blob bytes.
    stage = out.parent / f"{out.name}-append-{step}.db"
    stage.touch(mode=0o600, exist_ok=False)
    staged = sqlite3.connect(stage)
    try:
        for table in TABLES:
            ddl = conn.execute("SELECT sql FROM sqlite_master WHERE type='table' AND name=?", (table,)).fetchone()[0]
            staged.execute(ddl)
        expected = append_rows(staged, rows, shift, tile, 64 << 20, stage)
    finally:
        staged.close()
    if usage(path)["allocated"] > max_bytes:
        raise RuntimeError("fixture exceeded explicit physical-byte bound")
    result = helper_result(helper, "roll", out, "-cutoff", cutoff.isoformat(), "-append-db", str(stage))
    if usage(path)["allocated"] > max_bytes:
        raise RuntimeError("fixture exceeded explicit physical-byte bound")
    assert result["appended"] == expected
    point = {"hour": step, "cutoff": cutoff.isoformat(), **result,
             "stage_input_bytes": stage.stat().st_size, "snapshot": snapshot(conn, path)}
    for table in ("traces", "logs"):
        assert conn.execute(f"SELECT count(*) FROM {table} WHERE timestamp < ?", (stamp(cutoff),)).fetchone()[0] == 0
    point["oldest_trace"] = conn.execute("SELECT min(timestamp) FROM traces").fetchone()[0]
    return point


def probe(helper, out, trace_id):
    result = helper_result(helper, "read", out, "-trace", trace_id)
    if not result["found"]:
        raise AssertionError("retained trace missing")
    trace = result["trace"]
    spans, logs = trace.get("spans", []), trace.get("logs", [])
    assert spans and logs and result["log_total"] == len(logs) and result["fts_log_total"] > 0
    span_ids = {s["span_id"] for s in spans}
    for row in spans + logs:
        assert row["trace_id"] == trace_id
        json.loads(row["attributes_json"])
    for log in logs:
        assert log["span_id"] in span_ids
    return {"trace_id": trace_id, "timestamp": trace["timestamp"], "spans": len(spans),
            "logs": len(logs), "attributes_decoded": True, "log_repository_read": True,
            "fts_repository_search_count": result["fts_log_total"]}


def simulate(args):
    source, helper, out = args.source.resolve(), args.helper.resolve(), args.out.resolve()
    if out.exists() or not source.is_file() or not helper.is_file():
        raise ValueError("require NEW output, existing owned source copy, and compiled helper")
    if args.source_hours < 1 or args.source_hours > 24 or args.hours < 2 or args.hours > 168 or not 1 <= args.roll_hours <= 24:
        raise ValueError("source-hours 1..24, hours 2..168 and roll-hours 1..24 required")
    if args.hours % args.source_hours:
        raise ValueError("history hours must be a multiple of source-hours")
    if Path(str(source) + "-wal").exists() and Path(str(source) + "-wal").stat().st_size:
        raise ValueError("source copy must be checkpointed; nonempty WAL refused")
    start, end = parse(args.source_start), parse(args.end)
    if start.minute or start.second or end.minute or end.second:
        raise ValueError("whole-hour source-start and end required")
    original_hash = digest(source)
    src = sqlite3.connect(source.as_uri() + "?mode=ro&immutable=1", uri=True)
    src.row_factory = sqlite3.Row
    source_info = inspect(src)
    rows, by_hour = load_template(src, start, args.source_hours)
    candidates = {r["trace_id"]: r for r in rows["traces"]}
    span_ids = {r["trace_id"] for r in rows["spans"]}
    log_ids = {r["trace_id"] for r in rows["logs"]}
    example = next(candidates[i] for i in sorted(span_ids & log_ids) if i in candidates)
    example_id = example["trace_id"]
    src.close()
    helper_result(helper, "init", out)
    write_json(out / "source-inspection.json", source_info)
    path = out / "raw.db"
    conn = sqlite3.connect(path, timeout=60)
    # Same durable SQLite settings as storage.NewDatabase; no dropped indexes,
    # disabled triggers, bulk-load VACUUM, mmap, or large page cache shortcut.
    for statement in ("PRAGMA journal_mode=WAL", "PRAGMA synchronous=NORMAL", "PRAGMA cache_size=-65536",
                      "PRAGMA temp_store=MEMORY", "PRAGMA mmap_size=0", "PRAGMA wal_autocheckpoint=10000",
                      "PRAGMA journal_size_limit=67108864"):
        conn.execute(statement)
    peak = Peak(path)
    peak.thread.start()
    begun = time.time()
    report: dict[str, Any] = {"simulation": True, "source": str(source), "source_sha256": original_hash,
              "helper_sha256": digest(helper), "simulator_sha256": digest(Path(__file__)),
              "python_sqlite_version": sqlite3.sqlite_version,
              "template_start": start.isoformat(), "template_hours": args.source_hours,
              "history_hours": args.hours, "roll_hours": args.roll_hours, "end": end.isoformat(),
              "fts_added_to_versioned_schema": True, "synthetic_not_soak": True,
              "generation_started_utc": dt.datetime.now(UTC).isoformat(),
              "template_counts": {t: len(rs) for t, rs in rows.items()},
              "template_zero_span_headers": sum(r["retained_span_count"] == 0 for r in rows["traces"]),
              "source_attributes_stored_bytes": {t: sum(len(r["attributes_json"] or b"") for r in rows[t]) for t in ("spans", "logs")},
              "steps": []}
    write_json(out / "run.json", report)
    try:
        # A small correlated sentinel is deliberately outside the retention
        # range, with a unique FTS token to prove DELETE trigger maintenance.
        sentinel = {t: [dict(r) for r in rs if r["trace_id"] == example_id] for t, rs in rows.items()}
        for row in sentinel["logs"]:
            row["body"] = "rawcapacityexpiredsentinel"
        history_start = end - dt.timedelta(hours=args.hours)
        sentinel_shift = int((history_start - start - dt.timedelta(days=1)).total_seconds())
        append_rows(conn, sentinel, sentinel_shift, -1, args.max_bytes, path)
        report["sentinel_counts"] = {t: len(rs) for t, rs in sentinel.items()}
        total = collections.Counter(report["sentinel_counts"])
        for hour in range(args.hours):
            tile, index = divmod(hour, args.source_hours)
            shift = int((history_start - start).total_seconds()) + tile * args.source_hours * 3600
            total.update(append_rows(conn, by_hour[index], shift, tile, args.max_bytes, path))
            if (hour + 1) % 24 == 0 or hour + 1 == args.hours:
                point = {"seeded_hours": hour + 1, "elapsed_seconds": time.time() - begun, "usage": usage(path)}
                print(json.dumps(point), flush=True)
        report["seed"] = snapshot(conn, path)
        assert report["seed"]["counts"] == dict(total)
        report["generation_finished_utc"] = dt.datetime.now(UTC).isoformat()
        report["generation_seconds"] = time.time() - begun
        report["seed_peak"] = dict(peak.values)
        write_json(out / "seed-inspection.json", inspect(conn))
        fts = conn.execute("SELECT count(*) FROM logs_fts WHERE logs_fts MATCH 'rawcapacityexpiredsentinel'").fetchone()[0]
        assert fts == len(sentinel["logs"])
        # At seven days this tile starts exactly six days before virtual now.
        keep_hour = min(24, args.hours - args.source_hours)
        keep_tile = keep_hour // args.source_hours
        keep_id = identity(example_id, keep_tile)
        report["retained_before"] = probe(helper, out, keep_id)
        report["cancel"] = helper_result(helper, "cancel", out, "-cutoff", history_start.isoformat())
        report["initial_purge"] = helper_result(helper, "purge", out, "-cutoff", history_start.isoformat())
        report["expired_trace_read"] = helper_result(helper, "read", out, "-trace", identity(example_id, -1))
        assert not report["expired_trace_read"]["found"]
        assert conn.execute("SELECT count(*) FROM logs_fts WHERE logs_fts MATCH 'rawcapacityexpiredsentinel'").fetchone()[0] == 0
        report["fts_sentinel_insert_delete"] = True
        report["after_initial_purge"] = snapshot(conn, path)
        write_json(out / "run.json", report)
        print(json.dumps({"generation_finished": report["generation_finished_utc"], "seed": report["seed"]}), flush=True)
        for step in range(1, args.roll_hours + 1):
            cutoff = history_start + dt.timedelta(hours=step)
            hour = args.hours + step - 1
            tile, index = divmod(hour, args.source_hours)
            shift = int((history_start - start).total_seconds()) + tile * args.source_hours * 3600
            point = rolling_step(conn, path, helper, out, by_hour[index], shift, tile, cutoff, step, args.max_bytes)
            report["steps"].append(point)
            write_json(out / "run.json", report)
            print(json.dumps(point), flush=True)
        report["retained_after"] = probe(helper, out, keep_id)
        # FTS integrity-check with rank=1 checks the index against external
        # content, not just the FTS internal b-trees.
        with conn:
            conn.execute("INSERT INTO logs_fts(logs_fts,rank) VALUES('integrity-check',1)")
        report["fts_integrity"] = True
        report["quick_check"] = conn.execute("PRAGMA quick_check").fetchone()[0]
        assert report["quick_check"] == "ok"
        report["final"] = snapshot(conn, path)
        write_json(out / "final-inspection.json", inspect(conn))
        report["source_unchanged"] = digest(source) == original_hash
        assert report["source_unchanged"]
        report["passed"] = True
    finally:
        peak.stop.set()
        peak.thread.join()
        peak.sample()
        report["peak_50ms_samples"] = dict(peak.values)
        report["finished_utc"] = dt.datetime.now(UTC).isoformat()
        report["elapsed_seconds"] = time.time() - begun
        conn.close()
        report["closed_usage"] = usage(path)
        write_json(out / "run.json", report)
    return report


if __name__ == "__main__":
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("--source", type=Path, required=True)
    p.add_argument("--helper", type=Path, required=True)
    p.add_argument("--out", type=Path, required=True)
    p.add_argument("--source-start", required=True)
    p.add_argument("--source-hours", type=int, default=2)
    p.add_argument("--end", default="2026-10-18T21:00:00Z")
    p.add_argument("--hours", type=int, default=168)
    p.add_argument("--roll-hours", type=int, default=6)
    p.add_argument("--max-bytes", type=int, default=12 << 30)
    simulate(p.parse_args())
