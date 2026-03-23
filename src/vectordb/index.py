"""Embedded TF-IDF vector index for semantic log search.

Pure Python, in-process accelerator. The relational DB remains source of truth.
"""

from __future__ import annotations

import math
import re
import threading
from dataclasses import dataclass, field
from typing import Dict, List


@dataclass
class LogVector:
    log_id: int
    service_name: str
    severity: str
    body: str
    vec: Dict[str, float] = field(default_factory=dict)


@dataclass
class SearchResult:
    log_id: int
    service_name: str
    severity: str
    body: str
    score: float


STOP_WORDS = frozenset(
    {
        "the", "and", "for", "are", "was", "not", "with", "this", "that",
        "from", "has", "but", "have", "its", "been", "also", "than", "into",
    }
)

_TOKEN_RE = re.compile(r"[a-z0-9]+")


def _should_index(severity: str) -> bool:
    s = severity.upper()
    return s in ("ERROR", "WARN", "WARNING", "FATAL", "CRITICAL")


def _tokenize(text: str) -> list[str]:
    words = _TOKEN_RE.findall(text.lower())
    return [w for w in words if len(w) > 2 and w not in STOP_WORDS]


def _compute_tf(tokens: list[str]) -> dict[str, float]:
    counts: dict[str, int] = {}
    for t in tokens:
        counts[t] = counts.get(t, 0) + 1
    total = float(len(tokens))
    return {term: count / total for term, count in counts.items()}


def _vec_norm(v: dict[str, float]) -> float:
    return math.sqrt(sum(val * val for val in v.values()))


def _cosine(a: dict[str, float], norm_a: float, b: dict[str, float]) -> float:
    norm_b = _vec_norm(b)
    if norm_a == 0 or norm_b == 0:
        return 0.0
    dot = sum(a.get(term, 0) * vb for term, vb in b.items())
    return dot / (norm_a * norm_b)


class Index:
    """Thread-safe in-memory TF-IDF vector index for log bodies."""

    def __init__(self, max_size: int = 100_000):
        self._lock = threading.Lock()
        self._docs: list[LogVector] = []
        self._idf: dict[str, float] = {}
        self._max_size = max_size if max_size > 0 else 100_000
        self._dirty = False

    def add(self, log_id: int, service_name: str, severity: str, body: str) -> None:
        if not _should_index(severity):
            return
        tokens = _tokenize(body)
        if not tokens:
            return
        tf = _compute_tf(tokens)

        with self._lock:
            # FIFO eviction
            if len(self._docs) >= self._max_size:
                keep = self._docs[self._max_size // 10 :]
                self._docs = list(keep)
                self._dirty = True

            self._docs.append(LogVector(
                log_id=log_id,
                service_name=service_name,
                severity=severity,
                body=body,
                vec=tf,
            ))
            self._dirty = True

    def search(self, query: str, k: int = 10) -> list[SearchResult]:
        if k <= 0:
            k = 10
        tokens = _tokenize(query)
        if not tokens:
            return []
        query_tf = _compute_tf(tokens)

        with self._lock:
            if self._dirty:
                self._recompute_idf()
                self._dirty = False
            idf_snap = dict(self._idf)
            docs_snap = list(self._docs)

        # Build TF-IDF query vector
        query_vec = {term: tf * idf_snap.get(term, 0) for term, tf in query_tf.items()}
        query_norm = _vec_norm(query_vec)
        if query_norm == 0:
            return []

        scored: list[tuple[LogVector, float]] = []
        for doc in docs_snap:
            doc_vec = {term: tf * idf_snap.get(term, 0) for term, tf in doc.vec.items()}
            score = _cosine(query_vec, query_norm, doc_vec)
            if score > 0:
                scored.append((doc, score))

        scored.sort(key=lambda x: x[1], reverse=True)
        return [
            SearchResult(
                log_id=doc.log_id,
                service_name=doc.service_name,
                severity=doc.severity,
                body=doc.body,
                score=score,
            )
            for doc, score in scored[:k]
        ]

    def size(self) -> int:
        with self._lock:
            return len(self._docs)

    def _recompute_idf(self) -> None:
        """Rebuild IDF table. Must be called with lock held."""
        df: dict[str, int] = {}
        for doc in self._docs:
            for term in doc.vec:
                df[term] = df.get(term, 0) + 1
        n = float(len(self._docs))
        self._idf = {term: math.log(n / count) + 1 for term, count in df.items()}
