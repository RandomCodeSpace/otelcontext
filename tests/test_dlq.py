"""Tests for the Dead Letter Queue."""

import asyncio
import json
import tempfile
from pathlib import Path

import pytest
from src.queue.dlq import DeadLetterQueue


@pytest.fixture
def tmp_dir(tmp_path):
    return str(tmp_path / "dlq")


def test_enqueue(tmp_dir):
    dlq = DeadLetterQueue(directory=tmp_dir, max_files=10)
    dlq.enqueue({"type": "logs", "data": [{"body": "test"}]})
    assert dlq.size() == 1
    assert dlq.disk_bytes() > 0


def test_enqueue_fifo_eviction(tmp_dir):
    dlq = DeadLetterQueue(directory=tmp_dir, max_files=3)
    for i in range(5):
        dlq.enqueue({"i": i})
    assert dlq.size() <= 3


@pytest.mark.asyncio
async def test_replay(tmp_dir):
    replayed = []

    async def replay_fn(data: bytes):
        replayed.append(json.loads(data))

    dlq = DeadLetterQueue(directory=tmp_dir, replay_interval=0.1)
    dlq.set_replay_fn(replay_fn)
    dlq.enqueue({"type": "logs", "data": [1, 2, 3]})
    assert dlq.size() == 1

    await dlq._process_files()
    assert dlq.size() == 0
    assert len(replayed) == 1


@pytest.mark.asyncio
async def test_replay_failure_and_retry(tmp_dir):
    attempt = [0]

    async def failing_fn(data: bytes):
        attempt[0] += 1
        if attempt[0] < 2:
            raise Exception("transient error")

    dlq = DeadLetterQueue(directory=tmp_dir, replay_interval=0.01, max_retries=5)
    dlq.set_replay_fn(failing_fn)
    dlq.enqueue({"test": True})

    # First attempt fails
    await dlq._process_files()
    assert dlq.size() == 1

    # Backoff requires: age >= 2^(retries-1) * interval
    # retries=1, interval=0.01 => need age >= 0.01s
    import asyncio, os, time
    # Set mtime to past to bypass backoff
    for f in Path(tmp_dir).iterdir():
        if f.suffix == ".json":
            old_time = time.time() - 10
            os.utime(f, (old_time, old_time))

    # Second attempt succeeds
    await dlq._process_files()
    assert dlq.size() == 0
