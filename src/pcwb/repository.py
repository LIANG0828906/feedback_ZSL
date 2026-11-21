"""Atomic, versioned point-cloud repository.

The manifest is the commit record.  A point-cloud object only becomes visible
after it has been written, fsynced, atomically renamed into the object
directory without replacing an existing version, and then listed in an
atomically replaced manifest.  Recovery ignores and removes objects not
referenced by the durable manifest.
"""

from __future__ import annotations

import copy
import ctypes
import errno
import fcntl
import os
import tempfile
from collections.abc import Iterator
from contextlib import contextmanager
from dataclasses import dataclass
from pathlib import Path
from typing import Any

from .errors import CommitFailed, RepositoryInvalid
from .format import parse_point_cloud, serialize_point_cloud
from .model import PointCloud, PointCloudSnapshot
from .operators import OperationRecord
from .util import fsync_directory, pretty_json_bytes, sha256_hex

SCHEMA = "pcwb-manifest-v1"


@dataclass(frozen=True, slots=True)
class CommitResult:
    snapshot: PointCloudSnapshot
    object_path: Path
    manifest_path: Path
    version: int
    output_hash: str


@dataclass(frozen=True, slots=True)
class RecoveryReport:
    current_version: int
    removed_temporary_files: int
    removed_orphan_objects: int


class PointCloudRepository:
    def __init__(self, root: str | os.PathLike[str], *, recover: bool = True) -> None:
        self.root = Path(root)
        self.objects_dir = self.root / "objects"
        self.manifest_path = self.root / "manifest.json"
        self.lock_path = self.root / ".lock"
        self._snapshot_cache: dict[int, PointCloudSnapshot] = {}
        self._manifest: dict[str, Any] | None = None
        if recover:
            self.recover()

    @classmethod
    def create(
        cls,
        root: str | os.PathLike[str],
        cloud: PointCloud,
        *,
        source_name: str = "initial",
    ) -> PointCloudRepository:
        repository = cls(root, recover=False)
        repository._initialize(cloud, source_name=source_name)
        return repository

    def _ensure_layout(self) -> None:
        self.root.mkdir(parents=True, exist_ok=True)
        self.objects_dir.mkdir(parents=True, exist_ok=True)
        self.lock_path.touch(exist_ok=True)

    @contextmanager
    def _file_lock(self, *, exclusive: bool = True) -> Iterator[None]:
        self._ensure_layout()
        with self.lock_path.open("a+b") as lock_file:
            operation = fcntl.LOCK_EX if exclusive else fcntl.LOCK_SH
            fcntl.flock(lock_file.fileno(), operation)
            try:
                yield
            finally:
                fcntl.flock(lock_file.fileno(), fcntl.LOCK_UN)

    def _object_name(self, version: int) -> str:
        return f"v{version:08d}.pcwb"

    def _object_path(self, version: int) -> Path:
        return self.objects_dir / self._object_name(version)

    def _atomic_write(self, target: Path, payload: bytes) -> None:
        fd, temporary_name = tempfile.mkstemp(
            prefix=f".{target.name}.", suffix=".tmp", dir=target.parent
        )
        temporary = Path(temporary_name)
        try:
            with os.fdopen(fd, "wb") as stream:
                stream.write(payload)
                stream.flush()
                os.fsync(stream.fileno())
            os.replace(temporary, target)
            fsync_directory(target.parent)
        except Exception:
            temporary.unlink(missing_ok=True)
            raise

    def _write_manifest_atomic(self, manifest: dict[str, Any]) -> None:
        self._atomic_write(self.manifest_path, pretty_json_bytes(manifest))

    def _read_manifest_file(self) -> dict[str, Any]:
        if not self.manifest_path.exists():
            raise RepositoryInvalid("repository manifest does not exist")
        try:
            import json

            manifest = json.loads(self.manifest_path.read_text(encoding="utf-8"))
        except (OSError, ValueError) as exc:
            raise RepositoryInvalid(f"cannot read repository manifest: {exc}") from exc
        self._validate_manifest(manifest)
        return manifest

    def _validate_manifest(self, manifest: Any) -> None:
        if not isinstance(manifest, dict) or manifest.get("schema") != SCHEMA:
            raise RepositoryInvalid("unsupported repository manifest schema")
        versions = manifest.get("versions")
        if not isinstance(versions, list):
            raise RepositoryInvalid("manifest versions must be a list")
        seen: set[int] = set()
        for entry in versions:
            if not isinstance(entry, dict):
                raise RepositoryInvalid("manifest version entry must be an object")
            version = entry.get("version")
            if not isinstance(version, int) or version <= 0 or version in seen:
                raise RepositoryInvalid("manifest contains an invalid version number")
            seen.add(version)
            if (
                not isinstance(entry.get("output_hash"), str)
                or len(entry["output_hash"]) != 64
            ):
                raise RepositoryInvalid("manifest contains an invalid output hash")
            if entry.get("file") != self._object_name(version):
                raise RepositoryInvalid("manifest object filename is inconsistent")
        current = manifest.get("current_version")
        maximum = manifest.get("max_version")
        if current not in seen or maximum not in seen:
            raise RepositoryInvalid("manifest current/max version is invalid")
        if maximum != max(seen):
            raise RepositoryInvalid("manifest max_version is inconsistent")

    def _load_manifest_locked(self) -> dict[str, Any]:
        manifest = self._read_manifest_file()
        self._manifest = manifest
        return manifest

    def _initialize(self, cloud: PointCloud, *, source_name: str) -> PointCloudSnapshot:
        payload = serialize_point_cloud(cloud)
        output_hash = sha256_hex(payload)
        with self._file_lock():
            if self.manifest_path.exists():
                raise CommitFailed("repository already initialized")
            final_path = self._object_path(1)
            self._publish_object(final_path, payload)
            entry = {
                "version": 1,
                "parent_version": 0,
                "file": final_path.name,
                "output_hash": output_hash,
                "operator": "import",
                "params": {"source": source_name},
                "input_version": 0,
                "input_hash": "",
                "input_count": 0,
                "output_count": cloud.count,
                "selection_count": 0,
            }
            manifest = {
                "schema": SCHEMA,
                "current_version": 1,
                "max_version": 1,
                "versions": [entry],
            }
            try:
                self._write_manifest_atomic(manifest)
            except Exception as exc:
                final_path.unlink(missing_ok=True)
                raise CommitFailed(f"initial manifest commit failed: {exc}") from exc
            self._manifest = manifest
        return PointCloudSnapshot(
            version=1,
            content_hash=output_hash,
            cloud=cloud,
            source_path=str(final_path),
            parent_version=0,
        )

    def _publish_object(self, final_path: Path, payload: bytes) -> None:
        """Durably publish one immutable object without overwriting."""

        fd, temporary_name = tempfile.mkstemp(
            prefix=f".{final_path.name}.", suffix=".tmp", dir=final_path.parent
        )
        temporary = Path(temporary_name)
        published = False
        try:
            with os.fdopen(fd, "wb") as stream:
                stream.write(payload)
                stream.flush()
                os.fsync(stream.fileno())
            try:
                self._rename_noreplace(temporary, final_path)
            except FileExistsError as exc:
                raise CommitFailed(
                    "immutable version object already exists",
                    version_path=str(final_path),
                ) from exc
            published = True
            fsync_directory(final_path.parent)
        except Exception:
            if published:
                final_path.unlink(missing_ok=True)
            raise
        finally:
            temporary.unlink(missing_ok=True)

    @staticmethod
    def _rename_noreplace(source: Path, target: Path) -> None:
        """Atomically rename without replacing an existing target.

        Linux ``renameat2(RENAME_NOREPLACE)`` provides the exact primitive.
        Platforms without it fall back to a same-directory hard link followed
        by removal of the staged name.
        """

        try:
            libc = ctypes.CDLL(None, use_errno=True)
            renameat2 = libc.renameat2
        except AttributeError:
            renameat2 = None
        if renameat2 is not None:
            renameat2.argtypes = [
                ctypes.c_int,
                ctypes.c_char_p,
                ctypes.c_int,
                ctypes.c_char_p,
                ctypes.c_uint,
            ]
            renameat2.restype = ctypes.c_int
            result = renameat2(
                -100,  # AT_FDCWD
                os.fsencode(source),
                -100,
                os.fsencode(target),
                1,  # RENAME_NOREPLACE
            )
            if result == 0:
                return
            error = ctypes.get_errno()
            if error == errno.EEXIST:
                raise FileExistsError(error, "target already exists", target)
            if error not in {errno.ENOSYS, errno.EINVAL, errno.ENOTSUP}:
                raise OSError(error, os.strerror(error), str(target))
        os.link(source, target)
        source.unlink()

    def _entry_for(self, manifest: dict[str, Any], version: int) -> dict[str, Any]:
        for entry in manifest["versions"]:
            if entry["version"] == version:
                return entry
        raise RepositoryInvalid(f"version {version} does not exist")

    def _load_entry_locked(self, entry: dict[str, Any]) -> PointCloudSnapshot:
        version = int(entry["version"])
        cached = self._snapshot_cache.get(version)
        if cached is not None and cached.content_hash == entry["output_hash"]:
            return cached
        path = self.objects_dir / entry["file"]
        try:
            payload = path.read_bytes()
        except OSError as exc:
            raise RepositoryInvalid(
                f"committed object for version {version} is missing: {exc}"
            ) from exc
        digest = sha256_hex(payload)
        if digest != entry["output_hash"]:
            raise RepositoryInvalid(
                f"committed object for version {version} failed hash verification"
            )
        cloud = parse_point_cloud(payload)
        snapshot = PointCloudSnapshot(
            version=version,
            content_hash=digest,
            cloud=cloud,
            source_path=str(path),
            parent_version=int(entry.get("parent_version", 0)),
        )
        self._snapshot_cache[version] = snapshot
        return snapshot

    def commit(
        self,
        cloud: PointCloud,
        *,
        base_version: int,
        record: OperationRecord,
    ) -> CommitResult:
        payload = serialize_point_cloud(cloud)
        output_hash = sha256_hex(payload)
        if output_hash != record.output_hash:
            raise CommitFailed("operator record output hash does not match the cloud")
        if record.input_version != base_version:
            raise CommitFailed("operator input version does not match the commit base")

        with self._file_lock():
            manifest = self._load_manifest_locked()
            if int(manifest["current_version"]) != base_version:
                raise CommitFailed(
                    "commit base is not the repository current version",
                    current_version=manifest["current_version"],
                    base_version=base_version,
                )
            base_entry = self._entry_for(manifest, base_version)
            if base_entry["output_hash"] != record.input_hash:
                raise CommitFailed("operator input hash does not match the commit base")
            next_version = int(manifest["max_version"]) + 1
            final_path = self._object_path(next_version)
            self._publish_object(final_path, payload)
            entry = {
                "version": next_version,
                "parent_version": base_version,
                "file": final_path.name,
                "output_hash": output_hash,
                "operator": record.operator,
                "params": copy.deepcopy(record.params),
                "input_version": record.input_version,
                "input_hash": record.input_hash,
                "input_count": record.input_count,
                "output_count": record.output_count,
                "selection_count": record.selection_count,
            }
            updated = copy.deepcopy(manifest)
            updated["versions"].append(entry)
            updated["current_version"] = next_version
            updated["max_version"] = next_version
            try:
                self._write_manifest_atomic(updated)
            except Exception as exc:
                final_path.unlink(missing_ok=True)
                fsync_directory(final_path.parent)
                raise CommitFailed(f"manifest commit failed: {exc}") from exc
            self._manifest = updated
            snapshot = PointCloudSnapshot(
                version=next_version,
                content_hash=output_hash,
                cloud=cloud,
                source_path=str(final_path),
                parent_version=base_version,
            )
            self._snapshot_cache[next_version] = snapshot
            return CommitResult(
                snapshot=snapshot,
                object_path=final_path,
                manifest_path=self.manifest_path,
                version=next_version,
                output_hash=output_hash,
            )

    @property
    def current_snapshot(self) -> PointCloudSnapshot:
        with self._file_lock(exclusive=False):
            manifest = self._load_manifest_locked()
            entry = self._entry_for(manifest, int(manifest["current_version"]))
            return self._load_entry_locked(entry)

    def load_version(self, version: int) -> PointCloudSnapshot:
        with self._file_lock(exclusive=False):
            manifest = self._load_manifest_locked()
            return self._load_entry_locked(self._entry_for(manifest, int(version)))

    def manifest(self) -> dict[str, Any]:
        with self._file_lock(exclusive=False):
            return copy.deepcopy(self._load_manifest_locked())

    def versions(self) -> list[dict[str, Any]]:
        return copy.deepcopy(self.manifest()["versions"])

    def checkout(self, version: int) -> PointCloudSnapshot:
        """Move the active recovery pointer without deleting later versions."""

        with self._file_lock():
            manifest = self._load_manifest_locked()
            snapshot = self._load_entry_locked(self._entry_for(manifest, int(version)))
            updated = copy.deepcopy(manifest)
            updated["current_version"] = int(version)
            self._write_manifest_atomic(updated)
            self._manifest = updated
            return snapshot

    def recover(self) -> RecoveryReport:
        with self._file_lock():
            removed_temporary = 0
            removed_orphans = 0
            for path in list(self.objects_dir.glob("*.tmp")):
                path.unlink(missing_ok=True)
                removed_temporary += 1
            for path in list(self.root.glob(".manifest.json.*.tmp")):
                path.unlink(missing_ok=True)
                removed_temporary += 1
            if not self.manifest_path.exists():
                self._manifest = None
                return RecoveryReport(0, removed_temporary, 0)
            manifest = self._load_manifest_locked()
            referenced = {entry["file"] for entry in manifest["versions"]}
            for path in list(self.objects_dir.glob("v*.pcwb")):
                if path.name not in referenced:
                    path.unlink(missing_ok=True)
                    removed_orphans += 1
            current = self._entry_for(manifest, int(manifest["current_version"]))
            self._load_entry_locked(current)
            return RecoveryReport(
                int(manifest["current_version"]),
                removed_temporary,
                removed_orphans,
            )
