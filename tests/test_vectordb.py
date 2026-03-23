"""Tests for the TF-IDF vector index."""

import pytest
from src.vectordb.index import Index


def test_add_and_search():
    idx = Index(max_size=1000)
    idx.add(1, "svc-a", "ERROR", "connection refused to database server")
    idx.add(2, "svc-b", "ERROR", "timeout connecting to redis server")
    idx.add(3, "svc-a", "WARN", "high memory usage detected on server")

    results = idx.search("connection database", k=5)
    assert len(results) > 0
    assert results[0].log_id == 1


def test_only_indexes_error_warn():
    idx = Index(max_size=1000)
    idx.add(1, "svc-a", "INFO", "request completed successfully")
    idx.add(2, "svc-a", "DEBUG", "entering function foo")
    idx.add(3, "svc-a", "ERROR", "fatal error in processing")

    assert idx.size() == 1  # Only ERROR indexed


def test_eviction():
    idx = Index(max_size=100)
    for i in range(120):
        idx.add(i, "svc", "ERROR", f"error message number {i}")

    # After eviction, size should be <= max_size
    assert idx.size() <= 100


def test_search_empty():
    idx = Index(max_size=1000)
    results = idx.search("hello world")
    assert results == []


def test_search_no_tokens():
    idx = Index(max_size=1000)
    idx.add(1, "svc", "ERROR", "error")
    results = idx.search("")
    assert results == []
