"""Tests for the TSDB ring buffer."""

import time
from datetime import datetime, timezone

import pytest
from src.tsdb.ringbuffer import RingBuffer


def test_record_and_query():
    rb = RingBuffer(slots=10, window_dur=1.0)
    now = datetime.now(timezone.utc)
    rb.record("latency", "svc-a", 100.0, now)
    rb.record("latency", "svc-a", 200.0, now)
    rb.record("latency", "svc-a", 300.0, now)

    windows = rb.query_recent("latency", "svc-a", window_count=5)
    assert len(windows) >= 1
    w = windows[0]
    assert w.count == 3
    assert w.sum == 600.0
    assert w.min == 100.0
    assert w.max == 300.0


def test_percentiles():
    rb = RingBuffer(slots=10, window_dur=1.0)
    now = datetime.now(timezone.utc)
    for i in range(100):
        rb.record("latency", "svc-a", float(i), now)

    windows = rb.query_recent("latency", "svc-a", 1)
    assert len(windows) == 1
    w = windows[0]
    assert w.p50 >= 40  # roughly around 50
    assert w.p95 >= 90
    assert w.p99 >= 95


def test_metric_count():
    rb = RingBuffer(slots=10, window_dur=1.0)
    now = datetime.now(timezone.utc)
    rb.record("latency", "svc-a", 1.0, now)
    rb.record("errors", "svc-a", 1.0, now)
    rb.record("latency", "svc-b", 1.0, now)

    assert rb.metric_count() == 3


def test_all_keys():
    rb = RingBuffer(slots=10, window_dur=1.0)
    now = datetime.now(timezone.utc)
    rb.record("cpu", "svc-a", 0.5, now)
    rb.record("mem", "svc-b", 0.8, now)

    keys = rb.all_keys()
    assert len(keys) == 2
    assert "svc-a|cpu" in keys
    assert "svc-b|mem" in keys


def test_empty_query():
    rb = RingBuffer(slots=10, window_dur=1.0)
    windows = rb.query_recent("nonexistent", "svc", 5)
    assert windows == []
