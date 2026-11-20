"""Atomic, versioned on-disk point-cloud store.

Directory layout::

    root/
      manifest.json          single source of truth, atomically replaced
      versions/v000001.pclb  immutable version files
      tmp/                   staging files, cleaned on abort

Commit protocol:

1. serialize the new cloud into ``tmp/<unique>.pclb`` and fsync it;
2. ``os.replace`` onto ``versions/vNNNNNN.pclb`` (atomic on POSIX);
3. update the in-memory manifest and write it via the same
   temp-file + atomic-replace protocol, fsyncing the directory.

Any failure before step 3 leaves the previous manifest and every previously
committed version byte-identical; the caller receives ``E_COMMIT_FAILED``.
"""

from __future__ import annotations

import json
import os
import threading
from dataclasses import dataclass

import numpy as np

from .cloud import PointCloud
from .errors import E_COMMIT_FAILED, E_UNKNOWN_VERSION, PointCloudError
from .format import compute_bbox, content_hash, encode_cloud, read_cloud

MANIFEST_NAME = "manifest.json"
VERSIONS_DIR = "versions"
TMP_DIR = "tmp"


def canonical_json(payload: dict) -> bytes:
    """Deterministic JSON encoding (sorted keys, fixed separators, LF)."""
    return (json.dumps(payload, sort_keys=True, separators=(",", ":"),
                       ensure_ascii=False) + "\n").encode("utf-8")


def _atomic_write(path: str, blob: bytes) -> None:
    directory = os.path.dirname(path) or "."
    os.makedirs(directory, exist_ok=True)
    tmp_path = os.path.join(
        directory, f".{os.path.basename(path)}.{os.getpid()}.{id(blob)}.tmp")
    try:
        with open(tmp_path, "wb") as handle:
            handle.write(blob)
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(tmp_path, path)
        dir_fd = os.open(directory, os.O_RDONLY)
        try:
            os.fsync(dir_fd)
        finally:
            os.close(dir_fd)
    except OSError:
        try:
            if os.path.exists(tmp_path):
                os.unlink(tmp_path)
        except OSError:
            pass
        raise


@dataclass
class VersionEntry:
    version: int
    filename: str
    sha256: str
    parent_version: int | None
    record: dict

    def to_dict(self) -> dict:
        return {
            "version": self.version,
            "filename": self.filename,
            "sha256": self.sha256,
            "parent_version": self.parent_version,
            "record": self.record,
        }

    @classmethod
    def from_dict(cls, payload: dict) -> "VersionEntry":
        return cls(
            version=int(payload["version"]),
            filename=payload["filename"],
            sha256=payload["sha256"],
            parent_version=payload.get("parent_version"),
            record=payload.get("record", {}),
        )


class VersionStore:
    """Thread-safe versioned store with rollback and hash-checked cache."""

    def __init__(self, root: str):
        self.root = os.path.abspath(root)
        self.versions_dir = os.path.join(self.root, VERSIONS_DIR)
        self.tmp_dir = os.path.join(self.root, TMP_DIR)
        self.manifest_path = os.path.join(self.root, MANIFEST_NAME)
        os.makedirs(self.versions_dir, exist_ok=True)
        os.makedirs(self.tmp_dir, exist_ok=True)
        self._lock = threading.RLock()
        self._entries: dict[int, VersionEntry] = {}
        self._head: int | None = None
        self._cache: dict[int, PointCloud] = {}
        self._load_manifest()

    def _load_manifest(self) -> None:
        if not os.path.exists(self.manifest_path):
            return
        with open(self.manifest_path, "rb") as handle:
            payload = json.loads(handle.read().decode("utf-8"))
        for item in payload["versions"]:
            entry = VersionEntry.from_dict(item)
            self._entries[entry.version] = entry
        self._head = payload.get("head")

    def _write_manifest(self) -> None:
        payload = {
            "format": "pclb-manifest",
            "manifest_version": 1,
            "head": self._head,
            "versions": [
                self._entries[v].to_dict()
                for v in sorted(self._entries)
            ],
        }
        _atomic_write(self.manifest_path, canonical_json(payload))

    @property
    def head_version(self) -> int | None:
        return self._head

    def history(self) -> list[VersionEntry]:
        with self._lock:
            return [self._entries[v] for v in sorted(self._entries)]

    def entry(self, version: int) -> VersionEntry:
        with self._lock:
            try:
                return self._entries[int(version)]
            except KeyError as exc:
                raise PointCloudError(
                    E_UNKNOWN_VERSION, f"version {version} not found") from exc

    def _next_version(self) -> int:
        return (max(self._entries) + 1) if self._entries else 1

    def commit(self, xyz: np.ndarray, rgb: np.ndarray,
               record: dict | None = None,
               parent_version: int | None = None) -> PointCloud:
        """Serialize and atomically commit a new cloud version."""
        with self._lock:
            version = self._next_version()
            filename = f"v{version:06d}.pclb"
            final_path = os.path.join(self.versions_dir, filename)
            tmp_path = os.path.join(
                self.tmp_dir, f".stage-{version}-{id(xyz)}.pclb")
            if parent_version is None:
                parent_version = self._head
            bbox = compute_bbox(np.asarray(xyz, dtype=np.float64))
            try:
                blob = encode_cloud(xyz, rgb, bbox)
                with open(tmp_path, "wb") as handle:
                    handle.write(blob)
                    handle.flush()
                    os.fsync(handle.fileno())
                os.replace(tmp_path, final_path)
            except (OSError, ValueError) as exc:
                self._safe_unlink(tmp_path)
                if os.path.exists(final_path) and version not in self._entries:
                    self._safe_unlink(final_path)
                raise PointCloudError(
                    E_COMMIT_FAILED, f"staging failed: {exc}") from exc

            sha = content_hash(blob)
            entry = VersionEntry(
                version=version, filename=filename, sha256=sha,
                parent_version=parent_version, record=record or {})
            try:
                self._entries[version] = entry
                self._head = version
                self._write_manifest()
            except OSError as exc:
                self._entries.pop(version, None)
                self._head = parent_version
                self._safe_unlink(final_path)
                raise PointCloudError(
                    E_COMMIT_FAILED, f"manifest write failed: {exc}") from exc

            cloud = PointCloud.from_arrays(xyz, rgb, sha, version, bbox)
            self._cache[version] = cloud
            return cloud

    @staticmethod
    def _safe_unlink(path: str) -> None:
        try:
            if os.path.exists(path):
                os.unlink(path)
        except OSError:
            pass

    def load(self, version: int) -> PointCloud:
        """Load a version, verifying its bytes against the manifest hash.

        Cached snapshots are only reused when their stored hash matches the
        manifest hash for that version number.
        """
        with self._lock:
            entry = self.entry(version)
            cached = self._cache.get(version)
            if cached is not None and cached.sha256 == entry.sha256:
                return cached
            path = os.path.join(self.versions_dir, entry.filename)
            xyz, rgb, bbox, sha = read_cloud(path)
            if sha != entry.sha256:
                raise PointCloudError(
                    E_COMMIT_FAILED,
                    f"hash mismatch for version {version}: disk {sha} vs "
                    f"manifest {entry.sha256}")
            cloud = PointCloud.from_arrays(xyz, rgb, sha, version, bbox)
            self._cache[version] = cloud
            return cloud

    def head(self) -> PointCloud:
        with self._lock:
            if self._head is None:
                raise PointCloudError(E_UNKNOWN_VERSION, "store is empty")
            return self.load(self._head)

    def checkout(self, version: int) -> PointCloud:
        """Move the head back to a historical version.

        Later versions remain on disk for diff/inspection, but their cached
        snapshots are invalidated so subsequent edits recompute dependencies
        from the checked-out version instead of reusing stale branch data.
        """
        with self._lock:
            entry = self.entry(version)
            cloud = self.load(version)
            for cached_version in list(self._cache):
                if cached_version != version:
                    self._cache.pop(cached_version, None)
            self._head = entry.version
            self._write_manifest()
            return cloud

    def invalidate_cache(self) -> None:
        with self._lock:
            self._cache.clear()
