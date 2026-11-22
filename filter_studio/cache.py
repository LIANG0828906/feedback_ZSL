"""Content-addressed stage cache and explicit dependency invalidation."""

from __future__ import annotations

import hashlib
import threading
from dataclasses import dataclass
from datetime import datetime, timezone
from typing import Any, Iterable, Mapping

import numpy as np

from .stages import GRAPH, STAGE_IDS, canonical_json


def hash_bytes(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def hash_array(array: np.ndarray) -> str:
    canonical = np.ascontiguousarray(array)
    digest = hashlib.sha256()
    digest.update(str(canonical.dtype).encode("ascii"))
    digest.update(b"\0")
    digest.update(repr(tuple(canonical.shape)).encode("ascii"))
    digest.update(b"\0")
    digest.update(canonical.tobytes(order="C"))
    return digest.hexdigest()


@dataclass(frozen=True)
class CacheEntry:
    stage: str
    cache_key: str
    upstream_key: str
    input_hash: str
    stage_version: int
    params: dict[str, int | float]
    output: np.ndarray
    elapsed_sec: float
    output_hash: str
    committed_at: str

    def metadata(self) -> dict[str, Any]:
        return {
            "stage": self.stage,
            "cache_key": self.cache_key,
            "upstream_key": self.upstream_key,
            "input_hash": self.input_hash,
            "stage_version": self.stage_version,
            "params": dict(self.params),
            "elapsed_sec": self.elapsed_sec,
            "output_hash": self.output_hash,
            "committed_at": self.committed_at,
            "dtype": str(self.output.dtype),
            "shape": list(self.output.shape),
        }


def make_cache_key(
    *,
    input_hash: str,
    stage_id: str,
    params: Mapping[str, int | float],
    upstream_key: str,
    stage_version: int,
) -> str:
    payload = {
        "input_hash": input_hash,
        "stage": stage_id,
        "params": dict(params),
        "upstream_key": upstream_key,
        "stage_version": stage_version,
    }
    return hashlib.sha256(canonical_json(payload).encode("utf-8")).hexdigest()


def build_cache_entry(
    *,
    stage_id: str,
    input_hash: str,
    params: Mapping[str, int | float],
    upstream_key: str,
    stage_version: int,
    output: np.ndarray,
    elapsed_sec: float,
) -> CacheEntry:
    immutable_output = np.ascontiguousarray(output).copy()
    immutable_output.setflags(write=False)
    key = make_cache_key(
        input_hash=input_hash,
        stage_id=stage_id,
        params=params,
        upstream_key=upstream_key,
        stage_version=stage_version,
    )
    return CacheEntry(
        stage=stage_id,
        cache_key=key,
        upstream_key=upstream_key,
        input_hash=input_hash,
        stage_version=stage_version,
        params=dict(params),
        output=immutable_output,
        elapsed_sec=elapsed_sec,
        output_hash=hash_array(immutable_output),
        committed_at=datetime.now(timezone.utc).isoformat(),
    )


class StageCache:
    """Valid stage outputs for one input and one parameter state."""

    def __init__(self) -> None:
        self._entries: dict[str, CacheEntry] = {}
        self._lock = threading.RLock()

    def get(self, stage_id: str) -> CacheEntry | None:
        with self._lock:
            return self._entries.get(stage_id)

    def put(self, entry: CacheEntry) -> None:
        with self._lock:
            self._entries[entry.stage] = entry

    def clear(self) -> None:
        with self._lock:
            self._entries.clear()

    def invalidate_from(self, stage_id: str) -> tuple[str, ...]:
        affected = GRAPH.affected(stage_id)
        with self._lock:
            for affected_stage in affected:
                self._entries.pop(affected_stage, None)
        return affected

    def replace_prefix(self, entries: Iterable[CacheEntry]) -> None:
        replacement = {entry.stage: entry for entry in entries}
        with self._lock:
            self._entries = replacement

    @property
    def entries(self) -> dict[str, CacheEntry]:
        with self._lock:
            return dict(self._entries)

    def ordered_entries(self) -> list[CacheEntry]:
        with self._lock:
            return [
                self._entries[stage_id]
                for stage_id in STAGE_IDS
                if stage_id in self._entries
            ]

    def valid_prefix(self) -> list[CacheEntry]:
        prefix: list[CacheEntry] = []
        for stage_id in STAGE_IDS:
            entry = self.get(stage_id)
            if entry is None:
                break
            prefix.append(entry)
        return prefix

    def __len__(self) -> int:
        with self._lock:
            return len(self._entries)

