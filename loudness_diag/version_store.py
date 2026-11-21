"""Immutable, atomically published diagnostic versions and isolated queries."""

from __future__ import annotations

import copy
import hashlib
import json
import threading
from pathlib import Path
from typing import Any

from .errors import (
    E_SNAPSHOT_RELEASED,
    E_VERSION_MISMATCH,
    DiagnosticError,
)
from .jsonutil import (
    atomic_write_bytes,
    atomic_write_json,
    canonical_bytes,
    sha256_bytes,
    sha256_json,
)

_SCHEMA = 1
_VERSION_DOMAIN = b"loudness-diag/version/v1\0"


def _version_hash(report_bytes: bytes) -> str:
    digest = hashlib.sha256()
    digest.update(_VERSION_DOMAIN)
    digest.update(report_bytes)
    return digest.hexdigest()


def _report_hash_from_value(report: dict[str, Any]) -> str | None:
    claimed = report.get("report_hash")
    if not isinstance(claimed, str):
        return None
    unsigned = copy.deepcopy(report)
    unsigned.pop("report_hash", None)
    return sha256_bytes(canonical_bytes(unsigned))


class SnapshotHandle:
    """A reference-counted immutable view of one published version."""

    def __init__(
        self,
        store: VersionedStore,
        version_hash: str,
        manifest_hash: str,
        report: dict[str, Any],
    ) -> None:
        self._store = store
        self.version_hash = version_hash
        self.manifest_hash = manifest_hash
        self._report = report
        self._closed = False
        self._lock = threading.Lock()

    @property
    def report(self) -> dict[str, Any]:
        with self._lock:
            if self._closed:
                raise DiagnosticError(
                    E_SNAPSHOT_RELEASED, "snapshot handle has already been released"
                )
            return copy.deepcopy(self._report)

    @property
    def content_hash(self) -> str:
        return self.version_hash

    @property
    def closed(self) -> bool:
        with self._lock:
            return self._closed

    def close(self) -> None:
        with self._lock:
            if self._closed:
                return
            self._closed = True
        self._store._release_snapshot()

    def __enter__(self) -> SnapshotHandle:
        return self

    def __exit__(self, *args: object) -> None:
        self.close()


class VersionedStore:
    """Publish immutable diagnostic reports and query old versions safely."""

    def __init__(self, root: str | Path) -> None:
        self.root = Path(root)
        self._manifest_path = self.root / "publish_manifest.json"
        self._versions_dir = self.root / "versions"
        self._versions_dir.mkdir(parents=True, exist_ok=True)
        self._lock = threading.RLock()
        self._snapshot_lock = threading.Lock()
        self._active_snapshots = 0
        self._version_bytes: dict[str, bytes] = {}
        self._reports: dict[str, dict[str, Any]] = {}
        self._manifest: dict[str, Any] = self._empty_manifest()
        if self._manifest_path.exists():
            self._load_from_disk()

    @staticmethod
    def _empty_manifest() -> dict[str, Any]:
        return {
            "schema_version": _SCHEMA,
            "current_version_hash": None,
            "publication_order": [],
            "versions": [],
            "manifest_hash": sha256_json(
                {
                    "schema_version": _SCHEMA,
                    "current_version_hash": None,
                    "publication_order": [],
                    "versions": [],
                }
            ),
        }

    @property
    def current_version_hash(self) -> str | None:
        with self._lock:
            return self._manifest["current_version_hash"]

    @property
    def active_snapshot_count(self) -> int:
        with self._snapshot_lock:
            return self._active_snapshots

    def publish(self, report: dict[str, Any]) -> str:
        if not isinstance(report, dict):
            raise DiagnosticError(
                E_VERSION_MISMATCH, "a diagnostic report must be a JSON object"
            )
        report_value = copy.deepcopy(report)
        report_bytes = canonical_bytes(report_value)
        version_hash = _version_hash(report_bytes)
        claimed_report_hash = _report_hash_from_value(report_value)
        if report_value.get("report_hash") is not None and claimed_report_hash != report_value.get(
            "report_hash"
        ):
            raise DiagnosticError(
                E_VERSION_MISMATCH, "report_hash does not match report content"
            )

        version_path = self._versions_dir / f"{version_hash}.json"
        with self._lock:
            if version_path.exists():
                existing = version_path.read_bytes()
                if _version_hash(existing) != version_hash:
                    raise DiagnosticError(
                        E_VERSION_MISMATCH,
                        f"version file {version_path.name} is inconsistent",
                    )
            else:
                atomic_write_bytes(version_path, report_bytes)

            manifest = self._manifest
            known = {entry["version_hash"] for entry in manifest["versions"]}
            if version_hash not in known:
                manifest["versions"].append(
                    {
                        "version_hash": version_hash,
                        "report_hash": claimed_report_hash,
                        "file": f"versions/{version_hash}.json",
                    }
                )
                manifest["publication_order"].append(version_hash)
            manifest["current_version_hash"] = version_hash
            self._seal_and_write_manifest(manifest)
            self._version_bytes[version_hash] = report_bytes
            self._reports[version_hash] = copy.deepcopy(report_value)
            return version_hash

    def query(self, version_hash: str | None = None) -> SnapshotHandle:
        with self._lock:
            manifest = self._read_and_verify_manifest()
            requested = (
                version_hash
                if version_hash is not None
                else manifest["current_version_hash"]
            )
            if requested is None:
                raise DiagnosticError(
                    E_VERSION_MISMATCH, "no version has been published"
                )
            entry = next(
                (
                    item
                    for item in manifest["versions"]
                    if item["version_hash"] == requested
                ),
                None,
            )
            if entry is None:
                raise DiagnosticError(
                    E_VERSION_MISMATCH,
                    f"version {requested} is absent from the publish manifest",
                )
            report_bytes = self._load_version_bytes(entry)
            actual_version_hash = _version_hash(report_bytes)
            if actual_version_hash != requested or actual_version_hash != entry["version_hash"]:
                raise DiagnosticError(
                    E_VERSION_MISMATCH,
                    "queried version hash does not match the on-disk version",
                )
            report = json.loads(report_bytes)
            actual_report_hash = _report_hash_from_value(report)
            if entry.get("report_hash") is not None and actual_report_hash != entry[
                "report_hash"
            ]:
                raise DiagnosticError(
                    E_VERSION_MISMATCH,
                    "report hash does not match the publish manifest",
                )
            manifest_hash = manifest["manifest_hash"]
            with self._snapshot_lock:
                self._active_snapshots += 1
            return SnapshotHandle(
                self,
                version_hash=actual_version_hash,
                manifest_hash=manifest_hash,
                report=report,
            )

    def query_report(
        self, version_hash: str | None = None
    ) -> tuple[dict[str, Any], str]:
        with self.query(version_hash) as snapshot:
            return snapshot.report, snapshot.version_hash

    def verify_manifest(self) -> dict[str, Any]:
        with self._lock:
            return copy.deepcopy(self._read_and_verify_manifest())

    def _release_snapshot(self) -> None:
        with self._snapshot_lock:
            self._active_snapshots = max(0, self._active_snapshots - 1)

    def _load_version_bytes(self, entry: dict[str, Any]) -> bytes:
        path = self.root / entry["file"]
        try:
            report_bytes = path.read_bytes()
        except OSError as exc:
            raise DiagnosticError(
                E_VERSION_MISMATCH, f"cannot read version file: {exc}"
            ) from exc
        if _version_hash(report_bytes) != entry["version_hash"]:
            raise DiagnosticError(
                E_VERSION_MISMATCH, "version file content hash mismatch"
            )
        try:
            parsed = json.loads(report_bytes)
        except json.JSONDecodeError as exc:
            raise DiagnosticError(
                E_VERSION_MISMATCH, "version file is not valid JSON"
            ) from exc
        if canonical_bytes(parsed) != report_bytes:
            raise DiagnosticError(
                E_VERSION_MISMATCH, "version file is not canonical JSON"
            )
        return report_bytes

    def _read_and_verify_manifest(self) -> dict[str, Any]:
        try:
            manifest_bytes = self._manifest_path.read_bytes()
            manifest = json.loads(manifest_bytes)
        except (OSError, json.JSONDecodeError) as exc:
            raise DiagnosticError(
                E_VERSION_MISMATCH, f"cannot read publish manifest: {exc}"
            ) from exc
        if not isinstance(manifest, dict):
            raise DiagnosticError(E_VERSION_MISMATCH, "publish manifest is not an object")
        claimed = manifest.get("manifest_hash")
        unsigned = copy.deepcopy(manifest)
        unsigned.pop("manifest_hash", None)
        actual = sha256_json(unsigned)
        if claimed != actual:
            raise DiagnosticError(
                E_VERSION_MISMATCH, "publish manifest hash mismatch"
            )
        if canonical_bytes(manifest) != manifest_bytes:
            raise DiagnosticError(
                E_VERSION_MISMATCH, "publish manifest is not canonical JSON"
            )
        versions = manifest.get("versions")
        order = manifest.get("publication_order")
        current = manifest.get("current_version_hash")
        if not isinstance(versions, list) or not isinstance(order, list):
            raise DiagnosticError(
                E_VERSION_MISMATCH, "publish manifest has invalid version lists"
            )
        listed = [entry.get("version_hash") for entry in versions]
        if len(set(listed)) != len(listed) or listed != order:
            raise DiagnosticError(
                E_VERSION_MISMATCH,
                "publish manifest version order is inconsistent",
            )
        if current is not None and current not in listed:
            raise DiagnosticError(
                E_VERSION_MISMATCH,
                "current version is absent from the publish manifest",
            )
        return manifest

    def _seal_and_write_manifest(self, manifest: dict[str, Any]) -> None:
        unsigned = copy.deepcopy(manifest)
        unsigned.pop("manifest_hash", None)
        manifest["manifest_hash"] = sha256_json(unsigned)
        atomic_write_json(self._manifest_path, manifest)
        self._manifest = copy.deepcopy(manifest)

    def _load_from_disk(self) -> None:
        manifest = self._read_and_verify_manifest()
        loaded_bytes: dict[str, bytes] = {}
        loaded_reports: dict[str, dict[str, Any]] = {}
        for entry in manifest["versions"]:
            report_bytes = self._load_version_bytes(entry)
            report = json.loads(report_bytes)
            actual_report_hash = _report_hash_from_value(report)
            if entry.get("report_hash") is not None and actual_report_hash != entry[
                "report_hash"
            ]:
                raise DiagnosticError(
                    E_VERSION_MISMATCH,
                    f"report hash mismatch for version {entry['version_hash']}",
                )
            loaded_bytes[entry["version_hash"]] = report_bytes
            loaded_reports[entry["version_hash"]] = report
        self._manifest = manifest
        self._version_bytes = loaded_bytes
        self._reports = loaded_reports

