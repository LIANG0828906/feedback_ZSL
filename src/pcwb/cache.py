"""Content-addressed operator cache with hash and lineage validation."""

from __future__ import annotations

import copy
import hashlib
import threading
from collections.abc import Mapping
from dataclasses import dataclass
from typing import Any

from .format import parse_point_cloud, serialize_point_cloud
from .model import PointCloud, PointCloudSnapshot
from .util import canonical_json_bytes, normalize_json_value, sha256_hex


@dataclass(frozen=True, slots=True)
class CacheEntry:
    input_hash: str
    source_version: int
    operator: str
    params: dict[str, Any]
    output_hash: str
    payload: bytes


@dataclass(frozen=True, slots=True)
class CacheStats:
    hits: int
    misses: int
    invalid: int
    evictions: int


class OperationCache:
    """In-memory cache whose keys include the immutable input content hash.

    Entries produced by a later version are never accepted after a rollback to
    an earlier version.  Every hit is also verified against the SHA-256 of the
    cached payload, so cache corruption cannot alter committed results.
    """

    def __init__(self, *, max_entries: int = 32) -> None:
        if max_entries <= 0:
            raise ValueError("max_entries must be positive")
        self._max_entries = int(max_entries)
        self._entries: dict[str, CacheEntry] = {}
        self._order: list[str] = []
        self._lock = threading.RLock()
        self._hits = 0
        self._misses = 0
        self._invalid = 0
        self._evictions = 0

    @staticmethod
    def _params_bytes(params: Mapping[str, Any]) -> bytes:
        return canonical_json_bytes(normalize_json_value(dict(params)))

    def make_key(
        self,
        snapshot: PointCloudSnapshot,
        operator: str,
        params: Mapping[str, Any],
    ) -> str:
        material = (
            str(snapshot.version).encode("ascii")
            + b"\x00"
            + snapshot.content_hash.encode("ascii")
            + b"\x00"
            + operator.encode("utf-8")
            + b"\x00"
            + self._params_bytes(params)
        )
        return hashlib.sha256(material).hexdigest()

    def get(
        self,
        snapshot: PointCloudSnapshot,
        operator: str,
        params: Mapping[str, Any],
    ) -> PointCloud | None:
        key = self.make_key(snapshot, operator, params)
        normalized_params = normalize_json_value(dict(params))
        with self._lock:
            entry = self._entries.get(key)
            if entry is None:
                self._misses += 1
                return None
            if (
                entry.input_hash != snapshot.content_hash
                or entry.source_version > snapshot.version
                or entry.operator != operator
                or entry.params != normalized_params
            ):
                self._invalid += 1
                self._misses += 1
                self._entries.pop(key, None)
                self._order = [item for item in self._order if item != key]
                return None
            if sha256_hex(entry.payload) != entry.output_hash:
                self._invalid += 1
                self._misses += 1
                self._entries.pop(key, None)
                self._order = [item for item in self._order if item != key]
                return None
            cloud = parse_point_cloud(entry.payload)
            if cloud.content_hash != entry.output_hash:
                self._invalid += 1
                self._misses += 1
                self._entries.pop(key, None)
                self._order = [item for item in self._order if item != key]
                return None
            self._hits += 1
            self._order.remove(key)
            self._order.append(key)
            return cloud

    def put(
        self,
        snapshot: PointCloudSnapshot,
        operator: str,
        params: Mapping[str, Any],
        cloud: PointCloud,
    ) -> str:
        key = self.make_key(snapshot, operator, params)
        payload = serialize_point_cloud(cloud)
        output_hash = sha256_hex(payload)
        normalized_params = normalize_json_value(dict(params))
        entry = CacheEntry(
            input_hash=snapshot.content_hash,
            source_version=snapshot.version,
            operator=operator,
            params=copy.deepcopy(normalized_params),
            output_hash=output_hash,
            payload=payload,
        )
        with self._lock:
            if key in self._entries:
                self._order.remove(key)
            self._entries[key] = entry
            self._order.append(key)
            while len(self._order) > self._max_entries:
                oldest = self._order.pop(0)
                self._entries.pop(oldest, None)
                self._evictions += 1
        return output_hash

    def clear(self) -> None:
        with self._lock:
            self._entries.clear()
            self._order.clear()

    def invalidate_after_version(self, version: int) -> int:
        """Discard entries computed from versions newer than ``version``."""

        with self._lock:
            remove = [
                key
                for key, entry in self._entries.items()
                if entry.source_version > version
            ]
            for key in remove:
                self._entries.pop(key, None)
                self._order.remove(key)
            return len(remove)

    def stats(self) -> CacheStats:
        with self._lock:
            return CacheStats(
                hits=self._hits,
                misses=self._misses,
                invalid=self._invalid,
                evictions=self._evictions,
            )
