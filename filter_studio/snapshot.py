"""Atomic single-file persistence for stage outputs and provenance."""

from __future__ import annotations

import io
import json
import os
import tempfile
import zipfile
from dataclasses import dataclass
from datetime import datetime, timezone
from pathlib import Path
from typing import Any, Mapping

import numpy as np

from .cache import CacheEntry, hash_array


SNAPSHOT_FORMAT = 1
_MANIFEST_NAME = "manifest.json"


@dataclass(frozen=True)
class SnapshotData:
    manifest: dict[str, Any]
    outputs: dict[str, np.ndarray]

    @property
    def complete(self) -> bool:
        return bool(self.manifest.get("chain_complete", False))


class SnapshotStore:
    """Write snapshots with temp-file + fsync + os.replace semantics."""

    def __init__(self, path: str | Path, *, max_bytes: int = 512 * 1024 * 1024) -> None:
        self.path = Path(path)
        self.max_bytes = max_bytes
        self.last_write_ok = False
        self.last_error: str | None = None
        self._cleanup_stale_temps()

    def _cleanup_stale_temps(self) -> None:
        if not self.path.parent.exists():
            return
        pattern = f".{self.path.name}.*.tmp"
        for candidate in self.path.parent.glob(pattern):
            try:
                candidate.unlink()
            except OSError:
                pass

    def exists(self) -> bool:
        return self.path.is_file()

    def discard(self) -> None:
        try:
            self.path.unlink(missing_ok=True)
        except OSError:
            pass
        self.last_write_ok = False

    def save(
        self,
        *,
        image_metadata: Mapping[str, Any],
        params: Mapping[str, Mapping[str, int | float]],
        params_fingerprint: str,
        stage_versions: Mapping[str, int],
        entries: list[CacheEntry],
        chain_complete: bool,
        last_run: Mapping[str, Any] | None = None,
        last_error: Mapping[str, Any] | None = None,
    ) -> dict[str, Any]:
        now = datetime.now(timezone.utc).isoformat()
        entry_metadata: dict[str, Any] = {}
        payloads: dict[str, bytes] = {}
        for entry in entries:
            array_buffer = io.BytesIO()
            np.save(array_buffer, entry.output, allow_pickle=False)
            raw = array_buffer.getvalue()
            if len(raw) > self.max_bytes:
                raise OSError(
                    f"阶段 {entry.stage} 输出超过快照缓冲上限 {self.max_bytes} 字节"
                )
            payloads[entry.stage] = raw
            metadata = entry.metadata()
            metadata["output_file"] = f"stages/{entry.stage}.npy"
            entry_metadata[entry.stage] = metadata

        manifest: dict[str, Any] = {
            "format": SNAPSHOT_FORMAT,
            "created_at": now,
            "updated_at": now,
            "image": dict(image_metadata),
            "params": {
                stage_id: dict(stage_params)
                for stage_id, stage_params in params.items()
            },
            "params_fingerprint": params_fingerprint,
            "params_version": params_fingerprint,
            "stage_versions": dict(stage_versions),
            "chain_complete": bool(chain_complete),
            "completed_stages": [entry.stage for entry in entries],
            "entries": entry_metadata,
            "last_run": dict(last_run or {}),
            "last_error": dict(last_error) if last_error else None,
        }

        self.path.parent.mkdir(parents=True, exist_ok=True)
        fd, temporary_name = tempfile.mkstemp(
            prefix=f".{self.path.name}.",
            suffix=".tmp",
            dir=self.path.parent,
        )
        temporary_path = Path(temporary_name)
        try:
            with os.fdopen(fd, "wb") as raw_file:
                with zipfile.ZipFile(
                    raw_file, mode="w", compression=zipfile.ZIP_DEFLATED
                ) as archive:
                    archive.writestr(
                        _MANIFEST_NAME,
                        json.dumps(
                            manifest,
                            ensure_ascii=False,
                            sort_keys=True,
                            separators=(",", ":"),
                        ),
                    )
                    for stage_id in manifest["completed_stages"]:
                        archive.writestr(
                            f"stages/{stage_id}.npy",
                            payloads[stage_id],
                            compress_type=zipfile.ZIP_STORED,
                        )
                raw_file.flush()
                os.fsync(raw_file.fileno())
            os.replace(temporary_path, self.path)
            self._fsync_parent()
            self.last_write_ok = True
            self.last_error = None
            return manifest
        except Exception as exc:
            self.last_write_ok = False
            self.last_error = str(exc)
            try:
                temporary_path.unlink(missing_ok=True)
            except OSError:
                pass
            raise

    def _fsync_parent(self) -> None:
        if os.name == "nt":
            return
        try:
            descriptor = os.open(self.path.parent, os.O_RDONLY)
            try:
                os.fsync(descriptor)
            finally:
                os.close(descriptor)
        except OSError:
            pass

    def load(self) -> SnapshotData | None:
        if not self.exists():
            return None
        try:
            if self.path.stat().st_size > self.max_bytes * 2:
                raise ValueError("快照文件超过读取上限")
            with zipfile.ZipFile(self.path, mode="r") as archive:
                manifest = json.loads(archive.read(_MANIFEST_NAME).decode("utf-8"))
                if manifest.get("format") != SNAPSHOT_FORMAT:
                    raise ValueError("快照格式版本不匹配")
                outputs: dict[str, np.ndarray] = {}
                for stage_id in manifest.get("completed_stages", []):
                    metadata = manifest["entries"][stage_id]
                    output_file = metadata["output_file"]
                    raw = archive.read(output_file)
                    array = np.load(io.BytesIO(raw), allow_pickle=False)
                    if not isinstance(array, np.ndarray):
                        raise ValueError("快照阶段输出不是 ndarray")
                    outputs[stage_id] = array
            return SnapshotData(manifest=manifest, outputs=outputs)
        except Exception as exc:
            self.last_error = str(exc)
            return None

    def peek_manifest(self) -> dict[str, Any] | None:
        """Read only the manifest, used by the UI to restore last-run controls."""
        if not self.exists():
            return None
        try:
            with zipfile.ZipFile(self.path, mode="r") as archive:
                payload = archive.read(_MANIFEST_NAME)
            manifest = json.loads(payload.decode("utf-8"))
            if manifest.get("format") != SNAPSHOT_FORMAT:
                return None
            return manifest
        except Exception:
            return None

    def validate_outputs(self, data: SnapshotData) -> bool:
        for stage_id, array in data.outputs.items():
            metadata = data.manifest.get("entries", {}).get(stage_id, {})
            if str(array.dtype) != metadata.get("dtype"):
                return False
            if list(array.shape) != metadata.get("shape"):
                return False
            if hash_array(array) != metadata.get("output_hash"):
                return False
        return True

