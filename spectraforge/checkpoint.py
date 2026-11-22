"""Atomic segment checkpoints with whole-snapshot validation."""

from __future__ import annotations

import json
import os
import tempfile
import threading
import uuid
from collections.abc import Iterator
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

import numpy as np

from .errors import WorkbenchError
from .models import Scene, SegmentResult, SegmentSpec
from .parameters import parameter_version
from .segmentation import segmentation_key
from .state import SegmentRecord, SegmentState, SegmentStateMachine
from .util import (
    atomic_write_json,
    hash_arrays,
    read_json,
)
from .versions import (
    ALGORITHM_VERSION,
    CHECKPOINT_FORMAT,
    CHECKPOINT_SCHEMA_VERSION,
)


def _utc_now() -> str:
    return datetime.now(timezone.utc).isoformat(timespec="milliseconds")


class InterProcessFileLock:
    """Exclusive lock preventing two workbench processes from sharing a snapshot."""

    def __init__(self, path: Path):
        self.path = path
        self._handle = None

    def acquire(self) -> None:
        self.path.parent.mkdir(parents=True, exist_ok=True)
        self._handle = self.path.open("a+b")
        try:
            if os.name == "nt":
                import msvcrt

                self._handle.seek(0, os.SEEK_END)
                if self._handle.tell() == 0:
                    self._handle.write(b"\0")
                    self._handle.flush()
                self._handle.seek(0)
                msvcrt.locking(self._handle.fileno(), msvcrt.LK_NBLCK, 1)
            else:
                import fcntl

                fcntl.flock(self._handle.fileno(), fcntl.LOCK_EX | fcntl.LOCK_NB)
        except (OSError, BlockingIOError) as exc:
            self._handle.close()
            self._handle = None
            raise WorkbenchError(
                "E_LOCK",
                "Checkpoint snapshot is locked by another process",
                {"lock": str(self.path)},
            ) from exc

    def release(self) -> None:
        if self._handle is None:
            return
        try:
            if os.name == "nt":
                import msvcrt

                self._handle.seek(0)
                msvcrt.locking(self._handle.fileno(), msvcrt.LK_UNLCK, 1)
            else:
                import fcntl

                fcntl.flock(self._handle.fileno(), fcntl.LOCK_UN)
        finally:
            self._handle.close()
            self._handle = None


class CheckpointManager:
    def __init__(
        self,
        workspace: str | Path,
        scene: Scene,
        segments: list[SegmentSpec],
        segmentation_config: dict[str, Any],
    ):
        self.workspace = Path(workspace).resolve()
        self.scene = scene
        self.metadata = scene.metadata
        self.segments = segments
        self.segmentation_config = dict(segmentation_config)
        self.segmentation_config["segments"] = [segment.to_dict() for segment in segments]
        self._commit_lock = threading.RLock()
        safe_id = "".join(ch if ch.isalnum() or ch in "-_" else "_" for ch in self.metadata.scene_id)
        key = f"{safe_id}_{segmentation_key(segmentation_config)}"
        self.snapshot_key = key
        self.snapshot_dir = self.workspace / "checkpoints" / key
        self.manifest_path = self.snapshot_dir / "snapshot.json"
        self.segments_dir = self.snapshot_dir / "segments"
        self.invalidated_reason: dict[str, Any] | None = None
        self._lock = InterProcessFileLock(self.workspace / "checkpoints" / ".locks" / f"{key}.lock")
        self._lock.acquire()
        try:
            self.snapshot_dir.mkdir(parents=True, exist_ok=True)
            self.segments_dir.mkdir(parents=True, exist_ok=True)
            self.manifest = self._load_or_create()
            records = {
                segment.index: SegmentRecord(index=segment.index)
                for segment in self.segments
            }
            for key_text, entry in self.manifest.get("segments", {}).items():
                index = int(key_text)
                record = records[index]
                record.state = SegmentState(entry["state"])
                record.param_version = entry.get("param_version")
                record.content_hash = entry.get("content_hash")
                record.file = entry.get("file")
                record.stats = entry.get("stats", {})
                record.error = entry.get("error")
                if record.state == SegmentState.COMMITTED:
                    loaded = self._read_segment(record.file, index)
                    record.params = loaded["meta"]["params"]
            self.state = SegmentStateMachine(records)
        except Exception:
            self._lock.release()
            raise

    def _base_manifest(self) -> dict[str, Any]:
        now = _utc_now()
        return {
            "schema_version": CHECKPOINT_SCHEMA_VERSION,
            "checkpoint_format": CHECKPOINT_FORMAT,
            "snapshot_id": uuid.uuid4().hex,
            "scene_id": self.metadata.scene_id,
            "scene_title": self.metadata.title,
            "metadata_version": self.metadata.metadata_version,
            "metadata_hash": self.metadata.metadata_hash,
            "algorithm_version": ALGORITHM_VERSION,
            "segmentation": self.segmentation_config,
            "parameter_snapshots": {},
            "segments": {},
            "state_version": 0,
            "created_at": now,
            "updated_at": now,
            "run_status": "incomplete",
        }

    def _sort_manifest(self) -> None:
        segments = self.manifest.get("segments", {})
        self.manifest["segments"] = {
            key: segments[key] for key in sorted(segments, key=lambda item: int(item))
        }
        snapshots = self.manifest.get("parameter_snapshots", {})
        self.manifest["parameter_snapshots"] = {
            key: snapshots[key] for key in sorted(snapshots)
        }

    def _load_or_create(self) -> dict[str, Any]:
        if not self.manifest_path.exists():
            manifest = self._base_manifest()
            atomic_write_json(self.manifest_path, manifest)
            return manifest
        try:
            manifest = read_json(self.manifest_path, code="E_IO", label="checkpoint manifest")
            self._validate_manifest(manifest)
            return manifest
        except WorkbenchError as exc:
            self.invalidated_reason = exc.to_dict()
            self._archive_invalid_snapshot()
            manifest = self._base_manifest()
            atomic_write_json(self.manifest_path, manifest)
            return manifest
        except Exception as exc:
            self.invalidated_reason = WorkbenchError(
                "E_STATE",
                "Checkpoint snapshot validation failed",
                {"reason": str(exc), "type": type(exc).__name__},
            ).to_dict()
            self._archive_invalid_snapshot()
            manifest = self._base_manifest()
            atomic_write_json(self.manifest_path, manifest)
            return manifest

    def _archive_invalid_snapshot(self) -> None:
        archive_root = self.workspace / "checkpoints" / "_invalid"
        archive_root.mkdir(parents=True, exist_ok=True)
        stamp = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%S%fZ")
        target = archive_root / f"{self.snapshot_key}-{stamp}-{uuid.uuid4().hex[:8]}"
        try:
            os.replace(self.snapshot_dir, target)
        except OSError as exc:
            raise WorkbenchError(
                "E_IO",
                "Could not quarantine an invalid checkpoint snapshot",
                {"snapshot": str(self.snapshot_dir), "reason": str(exc)},
            ) from exc
        self.snapshot_dir.mkdir(parents=True, exist_ok=True)
        self.segments_dir.mkdir(parents=True, exist_ok=True)

    def _validate_manifest(self, manifest: dict[str, Any]) -> None:
        expected_pairs = {
            "schema_version": CHECKPOINT_SCHEMA_VERSION,
            "checkpoint_format": CHECKPOINT_FORMAT,
            "scene_id": self.metadata.scene_id,
            "metadata_version": self.metadata.metadata_version,
            "metadata_hash": self.metadata.metadata_hash,
            "algorithm_version": ALGORITHM_VERSION,
        }
        for field, expected in expected_pairs.items():
            actual = manifest.get(field)
            if actual != expected:
                raise WorkbenchError(
                    "E_STATE",
                    "Checkpoint snapshot version does not match the current scene",
                    {"field": field, "expected": expected, "actual": actual},
                )
        if manifest.get("segmentation") != self.segmentation_config:
            raise WorkbenchError(
                "E_STATE",
                "Checkpoint segmentation differs from the requested segmentation",
                {"field": "segmentation"},
            )
        snapshots = manifest.get("parameter_snapshots", {})
        if not isinstance(snapshots, dict):
            raise WorkbenchError("E_STATE", "Checkpoint parameter snapshot catalog is invalid")
        for version, params in snapshots.items():
            actual = parameter_version(params, self.scene.exposure_count)
            if version != actual:
                raise WorkbenchError(
                    "E_STATE",
                    "Checkpoint parameter snapshot hash is invalid",
                    {"field": "parameter_snapshots", "param_version": version, "actual": actual},
                )
        entries = manifest.get("segments", {})
        if not isinstance(entries, dict):
            raise WorkbenchError("E_STATE", "Checkpoint segment table is invalid")
        seen: set[int] = set()
        for key_text, entry in entries.items():
            index = int(key_text)
            if index < 0 or index >= len(self.segments) or index in seen:
                raise WorkbenchError("E_STATE", "Checkpoint segment index is invalid", {"segment_index": index})
            seen.add(index)
            state = entry.get("state")
            if state not in {SegmentState.COMMITTED.value, SegmentState.FAILED.value}:
                raise WorkbenchError(
                    "E_STATE",
                    "Checkpoint contains an uncommitted transient state",
                    {"segment_index": index, "state": state},
                )
            if state == SegmentState.COMMITTED.value:
                file = entry.get("file")
                if not file:
                    raise WorkbenchError("E_STATE", "Committed checkpoint has no file", {"segment_index": index})
                loaded = self._read_segment(file, index)
                if entry.get("content_hash") != loaded["content_hash"]:
                    raise WorkbenchError(
                        "E_STATE",
                        "Checkpoint manifest content hash does not match its segment",
                        {"segment_index": index},
                    )
                if entry.get("param_version") != loaded["meta"]["param_version"]:
                    raise WorkbenchError(
                        "E_STATE",
                        "Checkpoint parameter version does not match its segment",
                        {"segment_index": index},
                    )
                if entry.get("param_version") not in snapshots:
                    raise WorkbenchError(
                        "E_STATE",
                        "Committed segment references a missing parameter snapshot",
                        {"segment_index": index, "param_version": entry.get("param_version")},
                    )

    def _segment_path(self, index: int) -> str:
        return f"segments/{index:05d}.npz"

    def _checkpoint_basis(
        self,
        spec: SegmentSpec,
        arrays: dict[str, np.ndarray],
        meta: dict[str, Any],
    ) -> str:
        basis = {
            "checkpoint_format": CHECKPOINT_FORMAT,
            "segment": spec.to_dict(),
            "param_version": meta["param_version"],
            "params": meta["params"],
            "metadata_version": meta["metadata_version"],
            "metadata_hash": meta["metadata_hash"],
            "algorithm_version": meta["algorithm_version"],
            "stats": meta["stats"],
            "alignment_residual": meta["alignment_residual"],
        }
        return hash_arrays(
            [
                ("radiance", arrays["radiance"]),
                ("absolute_rgb", arrays["absolute_rgb"]),
                ("rgb_chroma", arrays["rgb_chroma"]),
                ("linear_rgb", arrays["linear_rgb"]),
                ("display", arrays["display"]),
                ("gamut_channel_mask", arrays["gamut_channel_mask"]),
            ],
            extra=basis,
        )

    def _write_segment(
        self,
        spec: SegmentSpec,
        arrays: dict[str, np.ndarray],
        meta: dict[str, Any],
        content_hash: str,
    ) -> str:
        relative = self._segment_path(spec.index)
        target = self.snapshot_dir / relative
        target.parent.mkdir(parents=True, exist_ok=True)
        meta_with_hash = {**meta, "content_hash": content_hash}
        fd, temp_name = tempfile.mkstemp(prefix=f".{spec.index:05d}.", suffix=".npz.tmp", dir=target.parent)
        temp_path = Path(temp_name)
        try:
            with os.fdopen(fd, "wb") as handle:
                np.savez(
                    handle,
                    radiance=np.ascontiguousarray(arrays["radiance"], dtype="<f4"),
                    absolute_rgb=np.ascontiguousarray(arrays["absolute_rgb"], dtype="<f4"),
                    rgb_chroma=np.ascontiguousarray(arrays["rgb_chroma"], dtype="<f4"),
                    linear_rgb=np.ascontiguousarray(arrays["linear_rgb"], dtype="<f4"),
                    display=np.ascontiguousarray(arrays["display"], dtype="u1"),
                    gamut_channel_mask=np.ascontiguousarray(arrays["gamut_channel_mask"], dtype="u1"),
                    meta=np.array(
                        json.dumps(meta_with_hash, sort_keys=True, separators=(",", ":"), ensure_ascii=False)
                    ),
                )
                handle.flush()
                os.fsync(handle.fileno())
            os.replace(temp_path, target)
            if os.name != "nt":
                try:
                    dir_fd = os.open(target.parent, os.O_RDONLY)
                    os.fsync(dir_fd)
                    os.close(dir_fd)
                except OSError:
                    pass
        except Exception:
            temp_path.unlink(missing_ok=True)
            raise
        return relative

    def _read_segment(self, relative: str, index: int) -> dict[str, Any]:
        path = self.snapshot_dir / relative
        try:
            with path.open("rb") as handle:
                with np.load(handle, allow_pickle=False) as archive:
                    arrays = {
                        "radiance": archive["radiance"],
                        "absolute_rgb": archive["absolute_rgb"],
                        "rgb_chroma": archive["rgb_chroma"],
                        "linear_rgb": archive["linear_rgb"],
                        "display": archive["display"],
                        "gamut_channel_mask": archive["gamut_channel_mask"],
                    }
                    meta = json.loads(str(archive["meta"].item()))
        except Exception as exc:
            raise WorkbenchError(
                "E_STATE",
                "Segment checkpoint is missing or corrupt",
                {"segment_index": index, "file": relative, "reason": str(exc)},
            ) from exc
        for field, expected in (
            ("metadata_version", self.metadata.metadata_version),
            ("metadata_hash", self.metadata.metadata_hash),
            ("algorithm_version", ALGORITHM_VERSION),
            ("checkpoint_format", CHECKPOINT_FORMAT),
        ):
            if meta.get(field) != expected:
                raise WorkbenchError(
                    "E_STATE",
                    "Segment checkpoint metadata version mismatch",
                    {"segment_index": index, "field": field, "expected": expected, "actual": meta.get(field)},
                )
        if int(meta.get("segment", {}).get("index", -1)) != index:
            raise WorkbenchError("E_STATE", "Segment checkpoint index mismatch", {"segment_index": index})
        actual_param_version = parameter_version(meta["params"], self.scene.exposure_count)
        if actual_param_version != meta.get("param_version"):
            raise WorkbenchError(
                "E_STATE",
                "Segment checkpoint parameter snapshot hash is invalid",
                {"segment_index": index},
            )
        spec = self.segments[index]
        recomputed = self._checkpoint_basis(spec, arrays, meta)
        if recomputed != meta.get("content_hash"):
            raise WorkbenchError(
                "E_STATE",
                "Segment checkpoint content hash mismatch",
                {"segment_index": index, "declared": meta.get("content_hash"), "actual": recomputed},
            )
        expected_radiance_shape = (spec.height, spec.width)
        expected_color_shape = (spec.height, spec.width, 3)
        shape_checks = {
            "radiance": expected_radiance_shape,
            "absolute_rgb": expected_color_shape,
            "rgb_chroma": expected_color_shape,
            "linear_rgb": expected_color_shape,
            "display": expected_color_shape,
            "gamut_channel_mask": expected_color_shape,
        }
        for name, expected_shape in shape_checks.items():
            if arrays[name].shape != expected_shape:
                raise WorkbenchError(
                    "E_STATE",
                    "Segment checkpoint buffer shape is invalid",
                    {"segment_index": index, "buffer": name, "expected": list(expected_shape), "actual": list(arrays[name].shape)},
                )
        return {"arrays": arrays, "meta": meta, "content_hash": recomputed}

    def _result_arrays(self, result: SegmentResult) -> dict[str, np.ndarray]:
        expected_color = (result.spec.height, result.spec.width, 3)
        expected_radiance = (result.spec.height, result.spec.width)
        arrays = {
            "radiance": np.ascontiguousarray(result.radiance, dtype="<f4"),
            "absolute_rgb": np.ascontiguousarray(result.absolute_rgb, dtype="<f4"),
            "rgb_chroma": np.ascontiguousarray(result.rgb_chroma, dtype="<f4"),
            "linear_rgb": np.ascontiguousarray(result.linear_rgb, dtype="<f4"),
            "display": np.ascontiguousarray(result.display, dtype="u1"),
            "gamut_channel_mask": np.ascontiguousarray(result.gamut_channel_mask, dtype="u1"),
        }
        if arrays["radiance"].shape != expected_radiance:
            raise WorkbenchError("E_STATE", "Segment result radiance shape is invalid", {"segment_index": result.spec.index})
        for name in ("absolute_rgb", "rgb_chroma", "linear_rgb", "display", "gamut_channel_mask"):
            if arrays[name].shape != expected_color:
                raise WorkbenchError(
                    "E_STATE",
                    "Segment result color buffer shape is invalid",
                    {"segment_index": result.spec.index, "buffer": name},
                )
        return arrays

    def commit_segment(self, result: SegmentResult) -> SegmentRecord:
        with self._commit_lock:
            record = self.state.records[result.spec.index]
            if record.state != SegmentState.PROCESSING:
                raise WorkbenchError(
                    "E_STATE",
                    "Segment must be processing before commit",
                    {"segment_index": result.spec.index, "state": record.state.value},
                )
            arrays = self._result_arrays(result)
            meta = {
                "checkpoint_format": CHECKPOINT_FORMAT,
                "segment": result.spec.to_dict(),
                "param_version": result.param_version,
                "params": result.params,
                "metadata_version": self.metadata.metadata_version,
                "metadata_hash": self.metadata.metadata_hash,
                "algorithm_version": ALGORITHM_VERSION,
                "stats": result.stats,
                "alignment_residual": float(result.alignment_residual),
            }
            actual_param_version = parameter_version(result.params, self.scene.exposure_count)
            if actual_param_version != result.param_version:
                raise WorkbenchError(
                    "E_STATE",
                    "Segment result parameter version is invalid",
                    {"segment_index": result.spec.index},
                )
            content_hash = self._checkpoint_basis(result.spec, arrays, meta)
            relative = self._write_segment(result.spec, arrays, meta, content_hash)
            self.manifest.setdefault("parameter_snapshots", {})[result.param_version] = result.params
            self.manifest.setdefault("segments", {})[str(result.spec.index)] = {
                "state": SegmentState.COMMITTED.value,
                "file": relative,
                "content_hash": content_hash,
                "param_version": result.param_version,
                "stats": result.stats,
                "error": None,
            }
            self.manifest["state_version"] = int(self.manifest.get("state_version", 0)) + 1
            self.manifest["updated_at"] = _utc_now()
            self.manifest["run_status"] = "incomplete"
            self._sort_manifest()
            atomic_write_json(self.manifest_path, self.manifest)
            self.state.transition(result.spec.index, SegmentState.COMMITTED)
            record.param_version = result.param_version
            record.params = result.params
            record.content_hash = content_hash
            record.file = relative
            record.stats = result.stats
            record.error = None
            return record

    def fail_segment(
        self,
        index: int,
        error: WorkbenchError,
        *,
        params: dict[str, Any] | None = None,
        param_version_value: str | None = None,
    ) -> SegmentRecord:
        with self._commit_lock:
            record = self.state.records[index]
            if record.state == SegmentState.PROCESSING:
                self.state.transition(index, SegmentState.FAILED)
            elif record.state != SegmentState.FAILED:
                raise WorkbenchError(
                    "E_STATE",
                    "Only a processing segment can fail",
                    {"segment_index": index, "state": record.state.value},
                )
            if params is not None and param_version_value is not None:
                self.manifest.setdefault("parameter_snapshots", {})[param_version_value] = params
            self.manifest.setdefault("segments", {})[str(index)] = {
                "state": SegmentState.FAILED.value,
                "file": None,
                "content_hash": None,
                "param_version": param_version_value,
                "stats": {},
                "error": error.to_dict(),
            }
            self.manifest["state_version"] = int(self.manifest.get("state_version", 0)) + 1
            self.manifest["updated_at"] = _utc_now()
            self.manifest["run_status"] = "incomplete"
            self._sort_manifest()
            atomic_write_json(self.manifest_path, self.manifest)
            record.error = error.to_dict()
            record.param_version = param_version_value
            record.params = params
            return record

    def committed_data(self) -> Iterator[tuple[SegmentRecord, dict[str, Any]]]:
        with self._commit_lock:
            items = []
            for index in sorted(self.state.records):
                record = self.state.records[index]
                if record.state == SegmentState.COMMITTED:
                    loaded = self._read_segment(record.file, index)
                    items.append((record, loaded))
            yield from items

    def load_segment(self, index: int) -> dict[str, Any]:
        with self._commit_lock:
            record = self.state.records.get(index)
            if record is None:
                raise WorkbenchError("E_STATE", "Unknown segment", {"segment_index": index})
            if record.state != SegmentState.COMMITTED:
                raise WorkbenchError(
                    "E_INCOMPLETE",
                    "Segment has no committed buffers",
                    {"segment_index": index, "state": record.state.value},
                )
            loaded = self._read_segment(record.file, index)
            loaded["record"] = record
            return loaded

    def progress(self) -> dict[str, Any]:
        counts = self.state.counts()
        total = len(self.segments)
        return {
            "committed": counts[SegmentState.COMMITTED.value],
            "failed": counts[SegmentState.FAILED.value],
            "processing": counts[SegmentState.PROCESSING.value],
            "pending": counts[SegmentState.PENDING.value],
            "total": total,
            "fraction": (counts[SegmentState.COMMITTED.value] / total) if total else 0.0,
            "states": counts,
        }

    def is_complete(self) -> bool:
        return all(record.state == SegmentState.COMMITTED for record in self.state.records.values())

    def set_run_status(self, status: str) -> None:
        if status not in {"complete", "paused", "incomplete"}:
            raise WorkbenchError("E_STATE", "Invalid run status", {"status": status})
        with self._commit_lock:
            self.manifest["run_status"] = status
            self.manifest["updated_at"] = _utc_now()
            self._sort_manifest()
            atomic_write_json(self.manifest_path, self.manifest)

    def reset(self) -> None:
        with self._commit_lock:
            self._archive_invalid_snapshot()
            self.manifest = self._base_manifest()
            atomic_write_json(self.manifest_path, self.manifest)
            for index in self.state.records:
                self.state.records[index] = SegmentRecord(index=index)

    def close(self) -> None:
        self._lock.release()

    def __enter__(self) -> "CheckpointManager":
        return self

    def __exit__(self, *_args: Any) -> None:
        self.close()
