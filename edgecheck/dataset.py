"""Deterministic sample dataset generated from a fixed seed.

60 URLs, ~6MB total:
  /a/vary/00..19   backend A, Vary: Accept-Encoding (10..19 also chunked)
  /b/head/00..09   backend B, HEAD Content-Length != GET length
  /b/etag/00..04   backend B, ETag / 304 revalidation
  /b/plain/00..04  backend B, plain
  /c/plain/00..19  backend C (h2c upgrade only), plain
"""
from __future__ import annotations

import gzip
import hashlib
from dataclasses import dataclass

VARY_COUNT = 20
HEAD_COUNT = 10
ETAG_COUNT = 5
B_PLAIN_COUNT = 5
C_PLAIN_COUNT = 20
CHUNKED_OFFSET = 10  # /a/vary/10..19 are chunked
HEAD_DELTA = 4096    # HEAD declares GET size + 4096 on backend B
DIAG_PATH = "/c/diag/force-reset"


@dataclass(frozen=True)
class Sample:
    sample_id: str
    path: str
    backend: str
    size: int
    vary: bool = False
    chunked: bool = False
    etag: bool = False
    head_mismatch: bool = False


def build_samples() -> list[Sample]:
    samples: list[Sample] = []
    for i in range(VARY_COUNT):
        samples.append(Sample(
            sample_id=f"A{i:02d}", path=f"/a/vary/{i:02d}", backend="A",
            size=102400 + (i % 5) * 1024, vary=True, chunked=i >= CHUNKED_OFFSET,
        ))
    for i in range(HEAD_COUNT):
        samples.append(Sample(
            sample_id=f"B{i:02d}", path=f"/b/head/{i:02d}", backend="B",
            size=101376 + (i % 4) * 1024, head_mismatch=True,
        ))
    for i in range(ETAG_COUNT):
        samples.append(Sample(
            sample_id=f"B{10 + i:02d}", path=f"/b/etag/{i:02d}", backend="B",
            size=99328 + i * 1024, etag=True,
        ))
    for i in range(B_PLAIN_COUNT):
        samples.append(Sample(
            sample_id=f"B{15 + i:02d}", path=f"/b/plain/{i:02d}", backend="B",
            size=97280 + i * 1024,
        ))
    for i in range(C_PLAIN_COUNT):
        samples.append(Sample(
            sample_id=f"C{i:02d}", path=f"/c/plain/{i:02d}", backend="C",
            size=103424 + (i % 6) * 1024,
        ))
    return samples


def _stream(key: str, size: int) -> bytes:
    out = bytearray()
    counter = 0
    while len(out) < size:
        out += hashlib.sha256(f"{key}:{counter}".encode()).digest()
        counter += 1
    return bytes(out[:size])


class Dataset:
    """Lazily materialized deterministic bodies for one seed."""

    def __init__(self, seed: int):
        self.seed = seed
        self.samples = build_samples()
        self.by_path = {s.path: s for s in self.samples}
        self._cache: dict[str, bytes] = {}

    def identity_body(self, sample: Sample) -> bytes:
        key = f"id:{sample.sample_id}"
        if key not in self._cache:
            self._cache[key] = _stream(
                f"{self.seed}:{sample.sample_id}:identity", sample.size)
        return self._cache[key]

    def gzip_variant(self, sample: Sample) -> bytes:
        key = f"gv:{sample.sample_id}"
        if key not in self._cache:
            self._cache[key] = _stream(
                f"{self.seed}:{sample.sample_id}:gzip", sample.size)
        return self._cache[key]

    def gzip_wire(self, sample: Sample) -> bytes:
        key = f"gw:{sample.sample_id}"
        if key not in self._cache:
            self._cache[key] = gzip.compress(
                self.gzip_variant(sample), compresslevel=6, mtime=0)
        return self._cache[key]

    def etag_value(self, sample: Sample) -> str:
        digest = hashlib.sha256(self.identity_body(sample)).hexdigest()
        return f'"{digest[:16]}"'

    def head_declared_length(self, sample: Sample) -> int:
        return sample.size + HEAD_DELTA

    def total_bytes(self) -> int:
        return sum(s.size for s in self.samples)

    def select(self, **flags) -> list[Sample]:
        out = []
        for s in self.samples:
            if all(getattr(s, k) == v for k, v in flags.items()):
                out.append(s)
        return out
