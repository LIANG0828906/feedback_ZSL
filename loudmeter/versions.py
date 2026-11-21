"""Immutable published versions and isolated queries.

Measurements and gating conclusions are published as immutable versions.
While a new version is being built, the old version stays readable; the
swap is a single atomic pointer replacement, so queries never observe
partially updated block values or half of a gating conclusion. Query
results carry the version hash actually hit. A version hash that does not
match the publish manifest raises E_VERSION_STALE; a retired handle raises
E_STALE_HANDLE.
"""

from __future__ import annotations

import hashlib
import json
import threading
from types import MappingProxyType

from .errors import StaleHandleError, VersionStaleError


def _freeze(obj):
    if isinstance(obj, dict):
        return MappingProxyType({k: _freeze(v) for k, v in obj.items()})
    if isinstance(obj, (list, tuple)):
        return tuple(_freeze(v) for v in obj)
    return obj


def version_hash(manifest: dict) -> str:
    canonical = json.dumps(manifest, sort_keys=True, separators=(",", ":"),
                           ensure_ascii=False)
    return hashlib.sha256(canonical.encode("utf-8")).hexdigest()


class QueryResult:
    __slots__ = ("version_hash", "manifest")

    def __init__(self, vhash: str, manifest):
        self.version_hash = vhash  # hash actually hit by this query
        self.manifest = manifest


class VersionStore:
    def __init__(self):
        self._lock = threading.Lock()
        self._manifests: dict[str, object] = {}   # version_hash -> frozen manifest
        self._handles: dict[int, str] = {}        # handle_id -> version_hash
        self._retired: set[int] = set()
        self._current_hash: str | None = None
        self._next_handle = 0

    def publish(self, manifest: dict) -> int:
        """Publish a manifest as one atomic replacement; returns a handle."""
        vhash = version_hash(manifest)
        frozen = _freeze(manifest)
        with self._lock:
            self._manifests[vhash] = frozen
            handle = self._next_handle
            self._next_handle += 1
            self._handles[handle] = vhash
            self._current_hash = vhash  # single-pointer atomic swap
        return handle

    def query(self, handle: int, expected_version_hash: str | None = None) -> QueryResult:
        with self._lock:
            if handle in self._retired or handle not in self._handles:
                raise StaleHandleError(f"handle {handle} is retired or unknown")
            vhash = self._handles[handle]
            if expected_version_hash is not None and expected_version_hash != vhash:
                raise VersionStaleError(
                    f"query hash {expected_version_hash} != published {vhash}")
            manifest = self._manifests[vhash]
        return QueryResult(vhash, manifest)

    def query_current(self) -> QueryResult:
        with self._lock:
            if self._current_hash is None:
                raise StaleHandleError("no version published yet")
            return QueryResult(self._current_hash,
                               self._manifests[self._current_hash])

    def retire(self, handle: int) -> None:
        with self._lock:
            if handle not in self._handles:
                raise StaleHandleError(f"handle {handle} unknown")
            self._retired.add(handle)
