from __future__ import annotations

import fcntl
import json
import threading
from dataclasses import dataclass
from pathlib import Path
from typing import Any

from .archive import validate_manifest_chain
from .errors import E_VERSION_STALE, AppError
from .trace import TraceLogger
from .util import (
    atomic_write_json,
    canonical_json_bytes,
    hash_without,
    read_json,
)

VERSION_SCHEMA = "loudness-published-version"
VERSION_INDEX_SCHEMA = "loudness-version-index"


@dataclass(frozen=True)
class PublishedVersion:
    version_id: str
    content_hash: str
    manifest_chain_head: str
    data_json: bytes

    def query_result(self) -> dict[str, Any]:
        return {
            "ok": True,
            "hit_version": self.version_id,
            "content_hash": self.content_hash,
            "manifest_chain_head": self.manifest_chain_head,
            "data": json.loads(self.data_json.decode("utf-8")),
        }


def _freeze_version(version_id: str, body: dict[str, Any]) -> PublishedVersion:
    payload = {
        "schema": VERSION_SCHEMA,
        "schema_version": 1,
        "version_id": version_id,
        **body,
    }
    content_hash = hash_without(payload, "content_hash")
    payload["content_hash"] = content_hash
    data = canonical_json_bytes(payload)
    return PublishedVersion(
        version_id=version_id,
        content_hash=content_hash,
        manifest_chain_head=str(payload["manifest_chain_head"]),
        data_json=data,
    )


def build_version_body(
    manifest: dict[str, Any],
    analysis_rows: list[dict[str, Any]],
    *,
    target_version: int,
    loss_manifest: dict[str, Any] | None,
    field_mapping: list[dict[str, Any]],
) -> dict[str, Any]:
    entries = manifest.get("segments", [])
    content_hashes = {
        int(entry["index"]): {
            "content_sha256": entry["content_sha256"],
            "chain_hash": entry["chain_hash"],
        }
        for entry in entries
    }
    segments = []
    for row in analysis_rows:
        index = int(row["index"])
        segments.append(
            {
                **row,
                "manifest_hashes": content_hashes.get(index),
            }
        )
    return {
        "asset_id": manifest["asset_id"],
        "manifest_revision": manifest["manifest_revision"],
        "manifest_sha256": manifest["manifest_sha256"],
        "manifest_chain_head": manifest["chain_head"],
        "parameters_version": manifest["parameters_version"],
        "metadata_version": target_version,
        "source_wav_sha256": manifest["source_wav_sha256"],
        "loss_manifest": loss_manifest,
        "field_mapping": field_mapping,
        "segments": segments,
        "rework": [
            {
                "segment_index": int(row["index"]),
                "over_limit": bool(row["over_limit"]),
                "excess_db": row["excess_db"],
                "basis_fields": row["basis_fields"],
            }
            for row in analysis_rows
        ],
    }


class InMemoryVersionPublisher:
    """Atomic complete-snapshot publisher used by the GUI and tests."""

    def __init__(self) -> None:
        self._lock = threading.Lock()
        self._history: dict[str, PublishedVersion] = {}
        self._current: PublishedVersion | None = None
        self._sequence = 0

    def publish_body(self, body: dict[str, Any]) -> PublishedVersion:
        with self._lock:
            self._sequence += 1
            version_id = f"ver-{self._sequence:06d}"
        snapshot = _freeze_version(version_id, body)
        with self._lock:
            self._current = snapshot
            self._history[version_id] = snapshot
        return snapshot

    def publish(
        self,
        body: dict[str, Any],
        *,
        expected_manifest_chain_head: str | None = None,
    ) -> PublishedVersion:
        if (
            expected_manifest_chain_head is not None
            and body.get("manifest_chain_head") != expected_manifest_chain_head
        ):
            raise AppError(
                E_VERSION_STALE,
                "snapshot was built from a different archive manifest chain",
                field="manifest_chain_head",
                details={
                    "expected": expected_manifest_chain_head,
                    "actual": body.get("manifest_chain_head"),
                },
            )
        return self.publish_body(body)

    def query(
        self,
        version_id: str | None = None,
        *,
        expected_version_hash: str | None = None,
    ) -> dict[str, Any]:
        with self._lock:
            snapshot = (
                self._history.get(version_id)
                if version_id is not None
                else self._current
            )
        if snapshot is None:
            raise AppError(
                E_VERSION_STALE,
                "requested version is not published",
                field="version_id",
                details={"version_id": version_id},
            )
        if expected_version_hash is not None and snapshot.content_hash != expected_version_hash:
            raise AppError(
                E_VERSION_STALE,
                "published snapshot hash does not match the expected version hash",
                field="content_hash",
                details={
                    "expected": expected_version_hash,
                    "actual": snapshot.content_hash,
                },
            )
        return snapshot.query_result()

    @property
    def current(self) -> PublishedVersion | None:
        with self._lock:
            return self._current


class FileVersionPublisher:
    """Persistent immutable versions with an atomically replaced index pointer."""

    def __init__(
        self,
        workspace: str | Path,
        trace: TraceLogger | None = None,
    ):
        self.workspace = Path(workspace)
        self.versions_dir = self.workspace / "versions"
        self.index_path = self.versions_dir / "index.json"
        self.lock_path = self.workspace / "locks" / "version-publish.lock"
        self.trace = trace

    def _read_index(self) -> dict[str, Any]:
        if not self.index_path.exists():
            return {
                "schema": VERSION_INDEX_SCHEMA,
                "schema_version": 1,
                "current_version": None,
                "versions": [],
            }
        try:
            index = read_json(self.index_path)
        except (OSError, json.JSONDecodeError) as exc:
            raise AppError(
                E_VERSION_STALE,
                f"version index is unreadable: {exc}",
                field="versions/index.json",
            ) from exc
        if index.get("schema") != VERSION_INDEX_SCHEMA:
            raise AppError(
                E_VERSION_STALE,
                "version index schema is unknown",
                field="versions/index.json.schema",
            )
        return index

    def _version_path(self, version_id: str) -> Path:
        return self.versions_dir / f"{version_id}.json"

    def publish(
        self,
        manifest: dict[str, Any],
        analysis_rows: list[dict[str, Any]],
        *,
        target_version: int,
        loss_manifest: dict[str, Any] | None,
        field_mapping: list[dict[str, Any]],
        expected_manifest_chain_head: str | None = None,
    ) -> dict[str, Any]:
        manifest_path = (
            self.workspace / "manifests" / f"{manifest.get('asset_id', '')}.json"
        )
        if not manifest_path.exists():
            raise AppError(
                E_VERSION_STALE,
                "archive manifest does not exist at publication time",
                field="manifest",
            )
        disk_manifest = read_json(manifest_path)
        validate_manifest_chain(self.workspace, disk_manifest)
        if disk_manifest.get("manifest_sha256") != hash_without(
            disk_manifest, "manifest_sha256"
        ):
            raise AppError(
                E_VERSION_STALE,
                "archive manifest checksum changed before publication",
                field="manifest_sha256",
            )
        if (
            disk_manifest.get("chain_head") != manifest.get("chain_head")
            or disk_manifest.get("manifest_revision") != manifest.get("manifest_revision")
            or disk_manifest.get("manifest_sha256") != manifest.get("manifest_sha256")
        ):
            raise AppError(
                E_VERSION_STALE,
                "version snapshot was built from a stale archive manifest",
                field="manifest_chain_head",
                details={
                    "snapshot_chain_head": manifest.get("chain_head"),
                    "disk_chain_head": disk_manifest.get("chain_head"),
                    "snapshot_revision": manifest.get("manifest_revision"),
                    "disk_revision": disk_manifest.get("manifest_revision"),
                },
            )
        body = build_version_body(
            manifest,
            analysis_rows,
            target_version=target_version,
            loss_manifest=loss_manifest,
            field_mapping=field_mapping,
        )
        if (
            expected_manifest_chain_head is not None
            and body["manifest_chain_head"] != expected_manifest_chain_head
        ):
            raise AppError(
                E_VERSION_STALE,
                "version snapshot does not match the expected manifest chain",
                field="manifest_chain_head",
                details={
                    "expected": expected_manifest_chain_head,
                    "actual": body["manifest_chain_head"],
                },
            )
        self.versions_dir.mkdir(parents=True, exist_ok=True)
        self.lock_path.parent.mkdir(parents=True, exist_ok=True)
        with open(self.lock_path, "a+b") as lock_handle:
            fcntl.flock(lock_handle.fileno(), fcntl.LOCK_EX)
            index = self._read_index()
            sequence = len(index["versions"]) + 1
            version_id = f"ver-{sequence:06d}"
            snapshot = _freeze_version(version_id, body)
            validation = _freeze_version(version_id, body)
            if validation.content_hash != snapshot.content_hash:
                raise AppError(
                    E_VERSION_STALE,
                    "internal version snapshot changed during construction",
                    field="content_hash",
                )
            atomic_write_json(self._version_path(version_id), json.loads(snapshot.data_json))
            index["versions"].append(
                {
                    "version_id": version_id,
                    "content_hash": snapshot.content_hash,
                    "manifest_chain_head": snapshot.manifest_chain_head,
                    "manifest_revision": manifest["manifest_revision"],
                    "asset_id": manifest["asset_id"],
                }
            )
            index["current_version"] = version_id
            atomic_write_json(self.index_path, index)
            fcntl.flock(lock_handle.fileno(), fcntl.LOCK_UN)
        if self.trace is not None:
            self.trace.record(
                "publish",
                asset_id=str(manifest["asset_id"]),
                version_id=version_id,
                hash=snapshot.content_hash,
                manifest_revision=manifest["manifest_revision"],
                chain_head=snapshot.manifest_chain_head,
                state="published",
            )
        return {
            "ok": True,
            "version_id": version_id,
            "content_hash": snapshot.content_hash,
            "manifest_chain_head": snapshot.manifest_chain_head,
            "index_path": str(self.index_path),
        }

    def query(
        self,
        version_id: str | None = None,
        *,
        expected_version_hash: str | None = None,
        verify_manifest: bool = True,
    ) -> dict[str, Any]:
        index = self._read_index()
        target = version_id or index.get("current_version")
        if target is None:
            raise AppError(
                E_VERSION_STALE,
                "no version has been published",
                field="version_id",
            )
        entry = next(
            (item for item in index["versions"] if item.get("version_id") == target),
            None,
        )
        if entry is None:
            raise AppError(
                E_VERSION_STALE,
                "requested version is absent from the index",
                field="version_id",
                details={"version_id": target},
            )
        path = self._version_path(target)
        if not path.exists():
            raise AppError(
                E_VERSION_STALE,
                "published version file is missing",
                field="versions_file",
                details={"version_id": target},
            )
        try:
            payload = read_json(path)
        except (OSError, json.JSONDecodeError) as exc:
            raise AppError(
                E_VERSION_STALE,
                f"published version file is unreadable: {exc}",
                field="versions_file",
            ) from exc
        actual_hash = hash_without(payload, "content_hash")
        if payload.get("content_hash") != actual_hash or entry.get("content_hash") != actual_hash:
            raise AppError(
                E_VERSION_STALE,
                "published version content hash mismatch",
                field="content_hash",
                details={
                    "index_hash": entry.get("content_hash"),
                    "actual_hash": actual_hash,
                },
            )
        if expected_version_hash is not None and actual_hash != expected_version_hash:
            raise AppError(
                E_VERSION_STALE,
                "requested version hash does not match the published hash",
                field="content_hash",
                details={
                    "expected": expected_version_hash,
                    "actual": actual_hash,
                },
            )
        if payload.get("manifest_chain_head") != entry.get("manifest_chain_head"):
            raise AppError(
                E_VERSION_STALE,
                "version manifest chain disagrees with the index",
                field="manifest_chain_head",
            )
        if verify_manifest and target == index.get("current_version"):
            manifest_path = (
                self.workspace
                / "manifests"
                / f"{payload['asset_id']}.json"
            )
            if not manifest_path.exists():
                raise AppError(
                    E_VERSION_STALE,
                    "archive manifest is missing for the current version",
                    field="manifest",
                )
            try:
                manifest = read_json(manifest_path)
                validate_manifest_chain(self.workspace, manifest)
            except AppError as exc:
                raise AppError(
                    E_VERSION_STALE,
                    f"archive manifest validation failed: {exc.message}",
                    field=exc.field,
                    details=exc.details,
                ) from exc
            if manifest.get("chain_head") != payload.get("manifest_chain_head"):
                raise AppError(
                    E_VERSION_STALE,
                    "published version is stale relative to the archive manifest",
                    field="manifest_chain_head",
                    details={
                        "version_chain_head": payload.get("manifest_chain_head"),
                        "manifest_chain_head": manifest.get("chain_head"),
                    },
                )
            if (
                manifest.get("manifest_sha256") != payload.get("manifest_sha256")
                or manifest.get("manifest_revision") != payload.get("manifest_revision")
            ):
                raise AppError(
                    E_VERSION_STALE,
                    "published version manifest metadata is stale",
                    field="manifest_sha256",
                    details={
                        "version_manifest_sha256": payload.get("manifest_sha256"),
                        "manifest_sha256": manifest.get("manifest_sha256"),
                        "version_revision": payload.get("manifest_revision"),
                        "manifest_revision": manifest.get("manifest_revision"),
                    },
                )
        return {
            "ok": True,
            "hit_version": target,
            "content_hash": actual_hash,
            "manifest_chain_head": payload["manifest_chain_head"],
            "data": payload,
        }

    def current(self) -> PublishedVersion | None:
        index = self._read_index()
        current = index.get("current_version")
        if current is None:
            return None
        result = self.query(current, verify_manifest=False)
        payload = result["data"]
        return PublishedVersion(
            version_id=current,
            content_hash=result["content_hash"],
            manifest_chain_head=result["manifest_chain_head"],
            data_json=canonical_json_bytes(payload),
        )

