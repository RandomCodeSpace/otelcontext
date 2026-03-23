"""Transparent compression utilities.

Uses gzip (stdlib) for broad compatibility.
"""

from __future__ import annotations

import gzip

GZIP_MAGIC = b"\x1f\x8b"


def compress(data: bytes) -> bytes:
    """Compress data using gzip."""
    if not data:
        return data
    return gzip.compress(data, compresslevel=6)


def decompress(data: bytes) -> bytes:
    """Decompress data. Handles both gzip-compressed and plain data."""
    if not data:
        return data
    if data[:2] == GZIP_MAGIC:
        return gzip.decompress(data)
    return data
