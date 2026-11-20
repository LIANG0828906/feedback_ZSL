from __future__ import annotations

import contextlib
import hashlib
import json
import os
import shutil
import tempfile
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, Iterator

from .errors import (
    CommitError,
    E_COMMIT_FAILED,
    E_RECOVERY_INVALID,
    E_VERSION_STALE,
    RecoveryError,
    StaleVersionError,
)
from .formats import ExportReport, export_mesh, parse_obj, serialize_obj
from .model import Mesh


MANIFEST_SCHEMA = "mesh-editor-manifest-v1"


def canonical_json(value: Any) -> str:
    return json.dumps(value, sort_keys=True, separators=(",", ":"), ensure_ascii=True)


def sha256_bytes(payload: bytes) -> str:
    return "sha256:" + hashlib.sha256(payload).hexdigest()


def sha256_file(path: str | Path) -> str:
    digest = hashlib.sha256()
    with open(path, "rb") as handle:
        for chunk in iter(lambda: handle.read(1024 * 1024), b""):
            digest.update(chunk)
    return "sha256:" + digest.hexdigest()


def _fsync_file(path: Path) -> None:
    with open(path, "rb") as handle:
        os.fsync(handle.fileno())


def _fsync_directory(path: Path) -> None:
    try:
        descriptor = os.open(path, os.O_RDONLY)
    except OSError:
        return
    try:
        os.fsync(descriptor)
    except OSError:
        pass
    finally:
        os.close(descriptor)


class DependencyCache:
    """Exact-key cache for edit dependencies.

    The key includes the base version identity and content hash, tolerance,
    format version and operation signature. Rollback clears every entry so a
    later edit cannot reuse a descendant's result.
    """

    def __init__(self) -> None:
        self._values: dict[str, Any] = {}
        self.hits = 0
        self.misses = 0
        self.invalidations = 0

    @staticmethod
    def make_key(
        *,
        base_version: int,
        base_content_hash: str,
        weld_tolerance: float,
        format_version: int,
        operation_signature: dict[str, Any],
    ) -> str:
        payload = canonical_json(
            {
                "base_version": int(base_version),
                "base_content_hash": base_content_hash,
                "weld_tolerance": float(weld_tolerance),
                "format_version": int(format_version),
                "operation_signature": operation_signature,
            }
        ).encode("utf-8")
        return "sha256:" + hashlib.sha256(payload).hexdigest()

    def get(self, key: str) -> Any | None:
        if key in self._values:
            self.hits += 1
            return self._values[key]
        self.misses += 1
        return None

    def put(self, key: str, value: Any) -> None:
        self._values[key] = value

    def clear(self) -> None:
        self._values.clear()
        self.invalidations += 1

    def stats(self) -> dict[str, int]:
        return {
            "entries": len(self._values),
            "hits": self.hits,
            "misses": self.misses,
            "invalidations": self.invalidations,
        }


@dataclass
class RecoveryReport:
    recovered_version: int
    content_hash: str
    discarded_staging: int
    discarded_orphan_versions: int
    status: str = "RECOVERED_COMMITTED_STATE"

    def to_dict(self) -> dict[str, Any]:
        return {
            "status": self.status,
            "recovered_version": self.recovered_version,
            "content_hash": self.content_hash,
            "discarded_staging": self.discarded_staging,
            "discarded_orphan_versions": self.discarded_orphan_versions,
            "exit_report_for_interrupted_edit": None,
            "note": "Only the last committed snapshot was recovered; interrupted edits were discarded.",
        }


class VersionRepository:
    def __init__(
        self,
        root: str | Path,
        *,
        workspace_id: str = "local-mesh-workspace",
        cache: DependencyCache | None = None,
    ) -> None:
        self.root = Path(root).resolve()
        self.snapshots_dir = self.root / "snapshots"
        self.manifest_path = self.root / "manifest.json"
        self.lock_path = self.root / ".lock"
        self.workspace_id = workspace_id
        self.cache = cache if cache is not None else DependencyCache()
        self.root.mkdir(parents=True, exist_ok=True)
        self.snapshots_dir.mkdir(parents=True, exist_ok=True)

    @contextlib.contextmanager
    def _locked(self) -> Iterator[None]:
        with open(self.lock_path, "a+b") as lock:
            try:
                import fcntl

                fcntl.flock(lock.fileno(), fcntl.LOCK_EX)
                try:
                    yield
                finally:
                    fcntl.flock(lock.fileno(), fcntl.LOCK_UN)
            except ImportError:
                yield

    @staticmethod
    def _entry_body(entry: dict[str, Any]) -> dict[str, Any]:
        return {key: value for key, value in entry.items() if key != "entry_hash"}

    @classmethod
    def _entry_hash(cls, previous_hash: str, entry: dict[str, Any]) -> str:
        payload = (previous_hash + "|" + canonical_json(cls._entry_body(entry))).encode("utf-8")
        return "sha256:" + hashlib.sha256(payload).hexdigest()

    def _empty_manifest(self) -> dict[str, Any]:
        return {
            "schema": MANIFEST_SCHEMA,
            "workspace_id": self.workspace_id,
            "next_version": 1,
            "current_version": 0,
            "chain_head": "",
            "events": [],
            "versions": [],
        }

    def manifest_exists(self) -> bool:
        return self.manifest_path.exists()

    def read_manifest(self, *, recovery: bool = False, check_meshes: bool = True) -> dict[str, Any]:
        if not self.manifest_path.exists():
            raise RecoveryError(
                code=E_RECOVERY_INVALID,
                message="version manifest is missing",
                file=str(self.manifest_path),
            )
        try:
            manifest = json.loads(self.manifest_path.read_text(encoding="utf-8"))
        except (OSError, UnicodeDecodeError, json.JSONDecodeError) as exc:
            raise RecoveryError(
                code=E_RECOVERY_INVALID,
                message=f"version manifest is unreadable: {exc}",
                file=str(self.manifest_path),
            ) from exc
        if recovery:
            self._validate_manifest(manifest, check_meshes=check_meshes)
        return manifest

    def _validate_manifest(self, manifest: dict[str, Any], *, check_meshes: bool) -> None:
        try:
            if manifest.get("schema") != MANIFEST_SCHEMA:
                raise ValueError("schema marker does not match")
            versions = manifest["versions"]
            events = manifest.get("events", [])
            if not isinstance(versions, list) or not versions:
                raise ValueError("version list is empty")
            if manifest.get("next_version") != len(versions) + 1:
                raise ValueError("next_version does not follow the version list")
            current = manifest.get("current_version")
            if not isinstance(current, int) or current < 1 or current > len(versions):
                raise ValueError("current_version is outside the version list")
            if not isinstance(events, list):
                raise ValueError("event list is invalid")
            previous_hash = ""
            version_map: dict[int, dict[str, Any]] = {}
            for index, entry in enumerate(versions):
                expected = index + 1
                if entry.get("version") != expected:
                    raise ValueError(f"version number at position {index} is {entry.get('version')}, expected {expected}")
                if entry.get("previous_entry_hash") != previous_hash:
                    raise ValueError(f"hash chain is broken at version {expected}")
                calculated = self._entry_hash(previous_hash, entry)
                if entry.get("entry_hash") != calculated:
                    raise ValueError(f"entry hash mismatch at version {expected}")
                parent = entry.get("parent")
                if expected == 1:
                    if parent != 0:
                        raise ValueError("baseline parent must be zero")
                elif not isinstance(parent, int) or parent < 1 or parent >= expected:
                    raise ValueError(f"invalid parent at version {expected}")
                dependencies = entry.get("dependencies")
                if not isinstance(dependencies, dict):
                    raise ValueError(f"dependencies are missing at version {expected}")
                if dependencies.get("base_version") != parent:
                    raise ValueError(f"dependency base mismatch at version {expected}")
                if entry.get("format_version") not in (1, 2):
                    raise ValueError(f"invalid format version at version {expected}")
                tolerance = entry.get("weld_tolerance")
                if not isinstance(tolerance, (int, float)) or tolerance < 0:
                    raise ValueError(f"invalid weld tolerance at version {expected}")
                version_map[expected] = entry
                previous_hash = calculated
            if manifest.get("chain_head") != previous_hash:
                raise ValueError("manifest chain head does not match the final entry")
            for version, entry in version_map.items():
                parent = entry["parent"]
                if parent:
                    if parent not in version_map:
                        raise ValueError(f"version {version} references missing parent {parent}")
                    expected_base = version_map[parent]["content_hash"]
                    if entry["dependencies"].get("base_content_hash") != expected_base:
                        raise ValueError(f"version {version} dependency hash does not match parent")
            if check_meshes:
                for version, entry in version_map.items():
                    mesh_path = self._mesh_path(entry)
                    if not mesh_path.is_file():
                        raise ValueError(f"snapshot for version {version} is missing")
                    actual = sha256_file(mesh_path)
                    if actual != entry.get("content_hash"):
                        raise ValueError(f"snapshot hash mismatch at version {version}")
                    try:
                        parse_obj(mesh_path)
                    except Exception as exc:
                        raise ValueError(f"snapshot for version {version} cannot be parsed: {exc}") from exc
        except (KeyError, TypeError, ValueError) as exc:
            raise RecoveryError(
                code=E_RECOVERY_INVALID,
                message=str(exc),
                file=str(self.manifest_path),
                details={"reason": "manifest_or_hash_chain_invalid"},
            ) from exc

    def _mesh_path(self, entry: dict[str, Any]) -> Path:
        relative = Path(str(entry["mesh_relpath"]))
        if relative.is_absolute() or ".." in relative.parts:
            raise RecoveryError(
                code=E_RECOVERY_INVALID,
                message="manifest contains an unsafe snapshot path",
                file=str(self.manifest_path),
            )
        return self.snapshots_dir / relative

    def _atomic_write_manifest(self, manifest: dict[str, Any]) -> None:
        payload = (json.dumps(manifest, sort_keys=True, indent=2, ensure_ascii=False) + "\n").encode("utf-8")
        descriptor, temp_name = tempfile.mkstemp(prefix=".manifest-", suffix=".tmp", dir=self.root)
        temp_path = Path(temp_name)
        try:
            with os.fdopen(descriptor, "wb") as handle:
                handle.write(payload)
                handle.flush()
                os.fsync(handle.fileno())
            os.replace(temp_path, self.manifest_path)
            _fsync_directory(self.root)
        except Exception:
            temp_path.unlink(missing_ok=True)
            raise

    def _publish_new_version(
        self,
        *,
        manifest: dict[str, Any],
        mesh: Mesh,
        parent: int,
        metrics_before: dict[str, Any] | None,
        metrics_after: dict[str, Any],
        operation: dict[str, Any],
        weld_tolerance: float,
        format_version: int,
    ) -> dict[str, Any]:
        version = int(manifest["next_version"])
        version_dir_name = f"v{version:08d}"
        final_dir = self.snapshots_dir / version_dir_name
        if final_dir.exists():
            raise CommitError(
                code=E_COMMIT_FAILED,
                message=f"refusing to overwrite existing snapshot directory {final_dir}",
            )
        payload = serialize_obj(mesh, 2)
        content_hash = sha256_bytes(payload)
        staging = Path(tempfile.mkdtemp(prefix=".staging-", dir=self.snapshots_dir))
        published = False
        try:
            snapshot_path = staging / "mesh.obj"
            with open(snapshot_path, "wb") as handle:
                handle.write(payload)
                handle.flush()
                os.fsync(handle.fileno())
            _fsync_directory(staging)
            if os.environ.get("MESH_EDITOR_CRASH_AFTER_STAGE") == "1":
                os._exit(91)
            os.rename(staging, final_dir)
            _fsync_directory(self.snapshots_dir)
            if os.environ.get("MESH_EDITOR_CRASH_BEFORE_MANIFEST") == "1":
                os._exit(92)

            previous_hash = str(manifest["chain_head"])
            parent_entry = manifest["versions"][parent - 1] if parent else None
            base_content_hash = str(parent_entry["content_hash"]) if parent_entry else ""
            dependency_key = DependencyCache.make_key(
                base_version=parent,
                base_content_hash=base_content_hash,
                weld_tolerance=weld_tolerance,
                format_version=format_version,
                operation_signature=operation,
            )
            entry: dict[str, Any] = {
                "version": version,
                "parent": int(parent),
                "previous_entry_hash": previous_hash,
                "mesh_relpath": f"{version_dir_name}/mesh.obj",
                "format": "obj",
                "format_version": int(format_version),
                "weld_tolerance": float(weld_tolerance),
                "content_hash": content_hash,
                "metrics_before": metrics_before,
                "metrics_after": metrics_after,
                "operation": operation,
                "dependencies": {
                    "base_version": int(parent),
                    "base_content_hash": base_content_hash,
                    "weld_tolerance": float(weld_tolerance),
                    "format_version": int(format_version),
                    "dependency_key": dependency_key,
                },
            }
            entry["entry_hash"] = self._entry_hash(previous_hash, entry)
            updated = json.loads(json.dumps(manifest))
            updated["versions"].append(entry)
            updated["next_version"] = version + 1
            updated["current_version"] = version
            updated["chain_head"] = entry["entry_hash"]
            updated["events"].append(
                {"sequence": len(updated["events"]) + 1, "type": "commit", "version": version, "parent": parent}
            )
            if os.environ.get("MESH_EDITOR_FAIL_COMMIT") == "1":
                raise OSError("injected commit failure before manifest publication")
            try:
                self._atomic_write_manifest(updated)
            except Exception:
                shutil.rmtree(final_dir, ignore_errors=True)
                raise
            published = True
            return entry
        except CommitError:
            raise
        except OSError as exc:
            if not published:
                shutil.rmtree(final_dir, ignore_errors=True)
            raise CommitError(
                code=E_COMMIT_FAILED,
                message=f"atomic commit failed: {exc}",
                details={"previous_version_preserved": True},
            ) from exc
        finally:
            if staging.exists():
                shutil.rmtree(staging, ignore_errors=True)

    def create_baseline(
        self,
        mesh: Mesh,
        *,
        weld_tolerance: float = 0.0,
        format_version: int = 2,
        operation: dict[str, Any] | None = None,
    ) -> dict[str, Any]:
        with self._locked():
            if self.manifest_path.exists():
                raise CommitError(
                    code=E_COMMIT_FAILED,
                    message="baseline already exists; refusing to replace committed history",
                )
            manifest = self._empty_manifest()
            entry = self._publish_new_version(
                manifest=manifest,
                mesh=mesh,
                parent=0,
                metrics_before=None,
                metrics_after=mesh.metrics(),
                operation=operation or {"type": "baseline"},
                weld_tolerance=weld_tolerance,
                format_version=format_version,
            )
            self.cache.clear()
            return entry

    def current_entry(self, manifest: dict[str, Any] | None = None) -> dict[str, Any]:
        manifest = manifest or self.read_manifest(recovery=True)
        return manifest["versions"][int(manifest["current_version"]) - 1]

    def commit_edit(
        self,
        base_version: int,
        mesh: Mesh,
        *,
        metrics_before: dict[str, Any],
        metrics_after: dict[str, Any],
        operation: dict[str, Any],
        weld_tolerance: float,
        format_version: int = 2,
    ) -> dict[str, Any]:
        with self._locked():
            manifest = self.read_manifest(recovery=True)
            if int(manifest["current_version"]) != int(base_version):
                raise StaleVersionError(
                    code=E_VERSION_STALE,
                    message=(
                        f"edit expected base version {base_version}, "
                        f"but current committed version is {manifest['current_version']}"
                    ),
                    details={"expected": int(base_version), "current": manifest["current_version"]},
                )
            entry = self._publish_new_version(
                manifest=manifest,
                mesh=mesh,
                parent=int(base_version),
                metrics_before=metrics_before,
                metrics_after=metrics_after,
                operation=operation,
                weld_tolerance=weld_tolerance,
                format_version=format_version,
            )
            return entry

    def rollback_to(self, version: int) -> dict[str, Any]:
        with self._locked():
            manifest = self.read_manifest(recovery=True)
            if version < 1 or version > len(manifest["versions"]):
                raise StaleVersionError(
                    code=E_VERSION_STALE,
                    message=f"version {version} is not present in the manifest",
                )
            old = int(manifest["current_version"])
            if old == version:
                return manifest["versions"][version - 1]
            updated = json.loads(json.dumps(manifest))
            updated["current_version"] = int(version)
            updated["events"].append(
                {
                    "sequence": len(updated["events"]) + 1,
                    "type": "rollback",
                    "from": old,
                    "to": int(version),
                }
            )
            self._atomic_write_manifest(updated)
            self.cache.clear()
            return updated["versions"][version - 1]

    def undo_last_commit(self) -> dict[str, Any]:
        with self._locked():
            manifest = self.read_manifest(recovery=True)
            current = self.current_entry(manifest)
            parent = int(current["parent"])
            if parent < 1:
                raise StaleVersionError(
                    code=E_VERSION_STALE,
                    message="the baseline version has no committed parent to undo to",
                )
            updated = json.loads(json.dumps(manifest))
            updated["current_version"] = parent
            updated["events"].append(
                {
                    "sequence": len(updated["events"]) + 1,
                    "type": "undo",
                    "from": int(current["version"]),
                    "to": parent,
                }
            )
            self._atomic_write_manifest(updated)
            self.cache.clear()
            return updated["versions"][parent - 1]

    def load_version(self, version: int, *, verify_manifest: bool = True) -> Mesh:
        manifest = self.read_manifest(recovery=verify_manifest, check_meshes=False)
        if version < 1 or version > len(manifest["versions"]):
            raise StaleVersionError(
                code=E_VERSION_STALE,
                message=f"version {version} does not exist",
            )
        entry = manifest["versions"][version - 1]
        path = self._mesh_path(entry)
        if not path.is_file():
            raise StaleVersionError(
                code=E_VERSION_STALE,
                message=f"snapshot file for version {version} is missing",
                details={"version": version},
            )
        actual_hash = sha256_file(path)
        if actual_hash != entry["content_hash"]:
            raise StaleVersionError(
                code=E_VERSION_STALE,
                message=f"content hash mismatch for version {version}",
                details={
                    "version": version,
                    "expected_hash": entry["content_hash"],
                    "actual_hash": actual_hash,
                },
            )
        mesh = parse_obj(path)
        mesh.metadata["version"] = int(version)
        mesh.metadata["content_hash"] = entry["content_hash"]
        return mesh

    def load_current(self) -> tuple[Mesh, dict[str, Any]]:
        manifest = self.read_manifest(recovery=True, check_meshes=False)
        current = int(manifest["current_version"])
        return self.load_version(current), manifest["versions"][current - 1]

    def export_version(
        self,
        version: int,
        destination: str | Path,
        fmt: str,
        format_version: int,
        *,
        expected_hash: str | None = None,
        ascii_stl: bool = False,
        allow_lossy: bool = False,
    ) -> ExportReport:
        manifest = self.read_manifest(recovery=True, check_meshes=False)
        if version < 1 or version > len(manifest["versions"]):
            raise StaleVersionError(
                code=E_VERSION_STALE,
                message=f"version {version} does not exist",
                details={"version": version},
            )
        entry = manifest["versions"][version - 1]
        if expected_hash is not None and expected_hash != entry["content_hash"]:
            raise StaleVersionError(
                code=E_VERSION_STALE,
                message="requested version hash does not match the manifest",
                details={
                    "version": version,
                    "expected_hash": expected_hash,
                    "manifest_hash": entry["content_hash"],
                },
            )
        mesh = self.load_version(version)
        report = export_mesh(
            mesh,
            destination,
            fmt,
            format_version,
            ascii_stl=ascii_stl,
            allow_lossy=allow_lossy,
        )
        report.details["version"] = int(version)
        report.details["content_hash"] = entry["content_hash"]
        report.details["manifest_entry_hash"] = entry["entry_hash"]
        return report

    def recover(self) -> RecoveryReport:
        with self._locked():
            manifest = self.read_manifest(recovery=True)
            current = int(manifest["current_version"])
            entry = manifest["versions"][current - 1]
            referenced = {Path(e["mesh_relpath"]).parts[0] for e in manifest["versions"]}
            staging = list(self.snapshots_dir.glob(".staging-*"))
            for path in staging:
                shutil.rmtree(path, ignore_errors=True)
            orphans: list[Path] = []
            for path in self.snapshots_dir.iterdir():
                if path.is_dir() and path.name.startswith("v") and path.name not in referenced:
                    orphans.append(path)
            for path in orphans:
                shutil.rmtree(path, ignore_errors=True)
            _fsync_directory(self.snapshots_dir)
            mesh = self.load_version(current)
            return RecoveryReport(
                recovered_version=current,
                content_hash=str(entry["content_hash"]),
                discarded_staging=len(staging),
                discarded_orphan_versions=len(orphans),
            )

    def trace_records(self) -> list[dict[str, Any]]:
        manifest = self.read_manifest(recovery=True)
        records: list[dict[str, Any]] = []
        for event in manifest.get("events", []):
            version = event.get("version", event.get("to"))
            entry = manifest["versions"][version - 1] if version else None
            records.append(
                {
                    "event": event,
                    "version": entry,
                }
            )
        return records

    def write_trace(self, path: str | Path) -> None:
        path = Path(path)
        path.parent.mkdir(parents=True, exist_ok=True)
        lines = [canonical_json(record) for record in self.trace_records()]
        payload = ("\n".join(lines) + "\n").encode("utf-8") if lines else b""
        temp = path.with_name(path.name + ".tmp")
        temp.write_bytes(payload)
        os.replace(temp, path)
