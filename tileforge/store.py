"""Immutable published versions with an atomic CURRENT pointer."""

from __future__ import annotations

import hashlib
import json
import os
import re
from dataclasses import dataclass
from pathlib import Path
from typing import Any

from .binary import atomic_write_bytes, parse_sequence, serialize_sequence
from .errors import E_IO, E_VERSION_STALE, error
from .locks import FileMutex
from .model import TileSequence


def _canonical(value: Any) -> bytes:
    return json.dumps(value, sort_keys=True, separators=(",", ":"), ensure_ascii=True, allow_nan=False).encode("utf-8")


@dataclass(frozen=True)
class Snapshot:
    manifest: dict[str, Any]
    sequence: TileSequence

    @property
    def version_id(self) -> int:
        return int(self.manifest["version_id"])

    @property
    def version_hash(self) -> str:
        return str(self.manifest["version_hash"])


class VersionStore:
    def __init__(self, root: str | Path) -> None:
        self.root = Path(root)
        self.versions_dir = self.root / "versions"
        self.manifests_dir = self.root / "manifests"
        self.history_path = self.root / "history.jsonl"
        self.current_path = self.root / "CURRENT.json"
        self.lock_path = self.root / ".publish.lock"
        self.runs_dir = self.root / "runs"
        self.traces_dir = self.root / "traces"
        for directory in (self.versions_dir, self.manifests_dir, self.runs_dir, self.traces_dir):
            directory.mkdir(parents=True, exist_ok=True)

    def next_version_id(self) -> int:
        maximum = 0
        if self.manifests_dir.exists():
            for path in self.manifests_dir.glob("v*.json"):
                match = re.fullmatch(r"v(\d{12})\.json", path.name)
                if match:
                    maximum = max(maximum, int(match.group(1)))
        if self.current_path.exists():
            try:
                maximum = max(maximum, int(self._read_json(self.current_path)["version_id"]))
            except (KeyError, ValueError, OSError, E_VERSION_STALE):
                pass
        return maximum + 1

    def _read_json(self, path: Path) -> dict[str, Any]:
        try:
            value = json.loads(path.read_text(encoding="utf-8"))
        except (OSError, json.JSONDecodeError, UnicodeError) as exc:
            raise error(E_VERSION_STALE, "cannot read version metadata", field="version", path=str(path), reason=str(exc)) from exc
        if not isinstance(value, dict):
            raise error(E_VERSION_STALE, "version metadata is not an object", field="version", path=str(path))
        return value

    def _manifest_hash(self, manifest: dict[str, Any]) -> str:
        content = {key: value for key, value in manifest.items() if key != "manifest_hash"}
        return hashlib.sha256(_canonical(content)).hexdigest()

    def _load_manifest_path(self, path: Path, expected_manifest_hash: str | None = None) -> dict[str, Any]:
        manifest = self._read_json(path)
        actual = self._manifest_hash(manifest)
        if manifest.get("manifest_hash") != actual:
            raise error(
                E_VERSION_STALE,
                "manifest hash mismatch",
                field="manifest_hash",
                actual=manifest.get("manifest_hash"),
                expected=actual,
                path=str(path),
            )
        if expected_manifest_hash is not None and expected_manifest_hash != actual:
            raise error(
                E_VERSION_STALE,
                "manifest changed while opening the snapshot",
                field="manifest_hash",
                actual=actual,
                expected=expected_manifest_hash,
            )
        return manifest

    def load_manifest(self, version_id: int) -> dict[str, Any]:
        path = self.manifests_dir / f"v{version_id:012d}.json"
        if not path.exists():
            raise error(E_VERSION_STALE, "requested version does not exist", field="version_id", version_id=version_id)
        return self._load_manifest_path(path)

    def _load_snapshot_from_manifest(self, manifest: dict[str, Any]) -> Snapshot:
        relative = manifest.get("file")
        if not isinstance(relative, str):
            raise error(E_VERSION_STALE, "manifest has no sequence file", field="file")
        path = self.root / relative
        try:
            data = path.read_bytes()
        except OSError as exc:
            raise error(E_VERSION_STALE, "published sequence is missing", field="file", path=str(path), reason=str(exc)) from exc
        actual_file_hash = hashlib.sha256(data).hexdigest()
        if actual_file_hash != manifest.get("version_hash"):
            raise error(
                E_VERSION_STALE,
                "published sequence hash mismatch",
                field="version_hash",
                actual=actual_file_hash,
                expected=manifest.get("version_hash"),
            )
        sequence = parse_sequence(path)
        return Snapshot(manifest=manifest, sequence=sequence)

    def load_current(self, expected_version_hash: str | None = None) -> Snapshot:
        if not self.current_path.exists():
            raise error(E_VERSION_STALE, "no version has been published", field="CURRENT")
        pointer = self._read_json(self.current_path)
        pointer_hash = pointer.get("version_hash")
        if expected_version_hash is not None and pointer_hash != expected_version_hash:
            raise error(
                E_VERSION_STALE,
                "query expected an older or newer published version",
                field="version_hash",
                actual=pointer_hash,
                expected=expected_version_hash,
            )
        manifest_relative = pointer.get("manifest")
        if not isinstance(manifest_relative, str):
            raise error(E_VERSION_STALE, "CURRENT has no manifest path", field="manifest")
        manifest_path = self.root / manifest_relative
        manifest = self._load_manifest_path(manifest_path, pointer.get("manifest_hash"))
        if manifest.get("version_hash") != pointer_hash:
            raise error(
                E_VERSION_STALE,
                "CURRENT pointer and manifest disagree",
                field="version_hash",
                actual=manifest.get("version_hash"),
                expected=pointer_hash,
            )
        return self._load_snapshot_from_manifest(manifest)

    def load_version(self, version_id: int, expected_version_hash: str | None = None) -> Snapshot:
        manifest = self.load_manifest(version_id)
        if expected_version_hash is not None and manifest.get("version_hash") != expected_version_hash:
            raise error(
                E_VERSION_STALE,
                "historical version hash mismatch",
                field="version_hash",
                actual=manifest.get("version_hash"),
                expected=expected_version_hash,
            )
        return self._load_snapshot_from_manifest(manifest)

    def load_manifest_relative(self, relative: str) -> dict[str, Any]:
        path = (self.root / relative).resolve()
        root = self.root.resolve()
        if root not in path.parents:
            raise error(E_VERSION_STALE, "manifest path escapes the store", field="manifest", actual=relative)
        return self._load_manifest_path(path)

    def load_snapshot_from_manifest(self, manifest: dict[str, Any]) -> Snapshot:
        return self._load_snapshot_from_manifest(manifest)

    def publish_sequence(
        self,
        sequence: TileSequence,
        *,
        config: dict[str, Any],
        config_hash: str,
        conversion_id: int,
        input_hash: str,
        sync_marker: str,
        observation: dict[str, Any],
        affected_tiles: list[int],
        reused_tiles: list[int],
        downgrade_stages: dict[int, str],
        tile_records: list[dict[str, Any]],
        parameter_version: int | None = None,
        base_manifest: dict[str, Any] | None = None,
    ) -> dict[str, Any]:
        with FileMutex(self.lock_path):
            data = serialize_sequence(sequence)
            version_hash = hashlib.sha256(data).hexdigest()
            version_id = self.next_version_id()
            finalized_tile_records = []
            for record in tile_records:
                finalized = dict(record)
                if finalized.get("origin_version") is None:
                    finalized["origin_version"] = version_id
                finalized_tile_records.append(finalized)
            version_relative = f"versions/v{version_id:012d}.tcseq"
            version_path = self.root / version_relative
            atomic_write_bytes(version_path, data)

            manifest = {
                "schema": 1,
                "version_id": version_id,
                "version_hash": version_hash,
                "file": version_relative,
                "parent_version_id": int(base_manifest["version_id"]) if base_manifest else None,
                "parent_version_hash": str(base_manifest["version_hash"]) if base_manifest else None,
                "input_hash": input_hash,
                "config": config,
                "config_hash": config_hash,
                "conversion_id": conversion_id,
                "parameter_version": parameter_version,
                "sync_marker": sync_marker,
                "observation": observation,
                "affected_tiles": sorted(affected_tiles),
                "reused_tiles": sorted(reused_tiles),
                "recomputed_tiles": sorted(affected_tiles),
                "downgrade_stages": {str(key): value for key, value in sorted(downgrade_stages.items())},
                "tile_records": sorted(finalized_tile_records, key=lambda item: int(item["tile_id"])),
            }
            manifest["manifest_hash"] = self._manifest_hash(manifest)
            manifest_relative = f"manifests/v{version_id:012d}.json"
            manifest_path = self.root / manifest_relative
            atomic_write_bytes(manifest_path, json.dumps(manifest, sort_keys=True, indent=2, ensure_ascii=True).encode("utf-8") + b"\n")
            pointer = {
                "version_id": version_id,
                "version_hash": version_hash,
                "manifest": manifest_relative,
                "manifest_hash": manifest["manifest_hash"],
            }
            atomic_write_bytes(self.current_path, json.dumps(pointer, sort_keys=True, indent=2, ensure_ascii=True).encode("utf-8") + b"\n")
            self._append_history(
                {
                    "event": "publish",
                    "version_id": version_id,
                    "version_hash": version_hash,
                    "parent_version_id": manifest["parent_version_id"],
                    "affected_tiles": manifest["affected_tiles"],
                    "reused_tiles": manifest["reused_tiles"],
                }
            )
            return manifest

    def rollback(
        self,
        version_id: int,
        expected_current_hash: str | None = None,
    ) -> dict[str, Any]:
        with FileMutex(self.lock_path):
            current = self.load_current(expected_current_hash)
            target = self.load_manifest(version_id)
            target_path = self.manifests_dir / f"v{version_id:012d}.json"
            pointer = {
                "version_id": version_id,
                "version_hash": target["version_hash"],
                "manifest": f"manifests/{target_path.name}",
                "manifest_hash": target["manifest_hash"],
            }
            atomic_write_bytes(self.current_path, json.dumps(pointer, sort_keys=True, indent=2, ensure_ascii=True).encode("utf-8") + b"\n")
            self._append_history(
                {
                    "event": "rollback",
                    "from_version_id": current.version_id,
                    "from_version_hash": current.version_hash,
                    "to_version_id": version_id,
                    "to_version_hash": target["version_hash"],
                }
            )
            return target

    def list_versions(self) -> list[dict[str, Any]]:
        versions: list[dict[str, Any]] = []
        for path in sorted(self.manifests_dir.glob("v*.json")):
            try:
                versions.append(self._load_manifest_path(path))
            except Exception:
                continue
        return versions

    def _append_history(self, record: dict[str, Any]) -> None:
        try:
            with self.history_path.open("a", encoding="utf-8", newline="\n") as handle:
                handle.write(json.dumps(record, sort_keys=True, separators=(",", ":"), ensure_ascii=True) + "\n")
                handle.flush()
                os.fsync(handle.fileno())
        except OSError as exc:
            raise error(E_IO, "cannot append version history", field="history", path=str(self.history_path), reason=str(exc)) from exc
