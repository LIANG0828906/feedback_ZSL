from __future__ import annotations

import hashlib
import io
import json
import os
import re
import shutil
import tempfile
import threading
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Self

import numpy as np

from .errors import WorkbenchError
from .flatten import FlatContour
from .models import Glyph
from .params import ALGORITHM_VERSION, RenderParams
from .raster import RasterComponent, RasterSpec, coverage_counts_from_bits

try:
    import fcntl
except ImportError:  # pragma: no cover - the target environment is Linux
    fcntl = None

CHECKPOINT_FORMAT_VERSION = 1
_LOCK_REGISTRY: dict[str, threading.RLock] = {}
_LOCK_REGISTRY_GUARD = threading.Lock()


@dataclass
class CheckpointLoad:
    valid: bool
    components: list[RasterComponent]
    flats: list[FlatContour]
    committed_count: int
    commit_sequence: list[int]
    persisted_states: dict[str, Any] | None
    invalidated: bool = False
    reason: str | None = None


def _safe_name(value: str) -> str:
    return re.sub(r"[^A-Za-z0-9_.-]+", "_", value)


def _sha256_bytes(payload: bytes) -> str:
    return hashlib.sha256(payload).hexdigest()


def _sha256_file(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as handle:
        while chunk := handle.read(1024 * 1024):
            digest.update(chunk)
    return digest.hexdigest()


def _lock_for(path: Path) -> threading.RLock:
    key = str(path.resolve())
    with _LOCK_REGISTRY_GUARD:
        return _LOCK_REGISTRY.setdefault(key, threading.RLock())


def _atomic_write(path: Path, payload: bytes) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    descriptor, temporary_name = tempfile.mkstemp(
        prefix=f".{path.name}.", suffix=".tmp", dir=path.parent
    )
    temporary = Path(temporary_name)
    try:
        with os.fdopen(descriptor, "wb") as handle:
            handle.write(payload)
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temporary, path)
        directory_fd = os.open(path.parent, os.O_RDONLY)
        try:
            os.fsync(directory_fd)
        finally:
            os.close(directory_fd)
    finally:
        if temporary.exists():
            temporary.unlink()


def _json_bytes(payload: dict[str, Any]) -> bytes:
    return (
        json.dumps(payload, ensure_ascii=True, sort_keys=True, indent=2) + "\n"
    ).encode("utf-8")


class _FileLock:
    def __init__(self, path: Path) -> None:
        self.path = path
        self._thread_lock = _lock_for(path)
        self._handle = None

    def __enter__(self) -> Self:
        self._thread_lock.acquire()
        self.path.parent.mkdir(parents=True, exist_ok=True)
        self._handle = self.path.open("a+b")
        if fcntl is not None:
            fcntl.flock(self._handle.fileno(), fcntl.LOCK_EX)
        return self

    def __exit__(self, exc_type, exc, traceback) -> None:
        try:
            if self._handle is not None:
                if fcntl is not None:
                    fcntl.flock(self._handle.fileno(), fcntl.LOCK_UN)
                self._handle.close()
        finally:
            self._handle = None
            self._thread_lock.release()


class CheckpointStore:
    def __init__(
        self,
        persist_root: str | Path,
        glyph: Glyph,
        params: RenderParams,
    ) -> None:
        params.validate()
        self.persist_root = Path(persist_root)
        self.glyph = glyph
        self.params = params
        self.content_hash = glyph.content_hash()
        self.params_hash = params.params_hash()
        self.glyph_root = self.persist_root / "checkpoints" / _safe_name(glyph.glyph_id)
        identity = f"{self.content_hash[:20]}-{self.params_hash[:20]}"
        self.snapshot_dir = self.glyph_root / identity
        self.manifest_path = self.snapshot_dir / "manifest.json"
        self.states_path = self.snapshot_dir / "states.json"
        self.lock_path = self.persist_root / "checkpoints" / ".checkpoint.lock"
        self._lock = _FileLock(self.lock_path)

    def _identity_dict(self) -> dict[str, Any]:
        return {
            "format_version": CHECKPOINT_FORMAT_VERSION,
            "glyph_id": self.glyph.glyph_id,
            "content_hash": self.content_hash,
            "params_hash": self.params_hash,
            "algorithm_version": ALGORITHM_VERSION,
        }

    def _identity_matches(self, raw: dict[str, Any]) -> bool:
        expected = self._identity_dict()
        return all(raw.get(key) == value for key, value in expected.items())

    def _read_json(self, path: Path) -> dict[str, Any] | None:
        try:
            return json.loads(path.read_text(encoding="utf-8"))
        except (OSError, json.JSONDecodeError):
            return None

    def _remove_snapshot_dir(self, path: Path) -> None:
        if path.exists():
            shutil.rmtree(path, ignore_errors=True)

    def validate_or_invalidate(self) -> CheckpointLoad:
        with self._lock:
            self.glyph_root.mkdir(parents=True, exist_ok=True)
            invalidated = False
            reason = None
            for child in list(self.glyph_root.iterdir()):
                if not child.is_dir():
                    continue
                if child.name != self.snapshot_dir.name:
                    other_manifest = self._read_json(child / "manifest.json")
                    if other_manifest is not None and not self._identity_matches(
                        other_manifest
                    ):
                        invalidated = True
                        reason = "another snapshot identity is incompatible"
            manifest = self._read_json(self.manifest_path)
            states = self._read_json(self.states_path)
            if manifest is None or not self._identity_matches(manifest):
                if self.snapshot_dir.exists():
                    self._remove_snapshot_dir(self.snapshot_dir)
                    invalidated = True
                    reason = "content, parameter, or algorithm version mismatch"
                return CheckpointLoad(
                    valid=False,
                    components=[],
                    flats=[],
                    committed_count=0,
                    commit_sequence=[],
                    persisted_states=states,
                    invalidated=invalidated,
                    reason=reason,
                )
            total = int(manifest.get("total_segments", -1))
            segments = manifest.get("segments", [])
            if total <= 0 or len(segments) != total:
                self._remove_snapshot_dir(self.snapshot_dir)
                return CheckpointLoad(
                    valid=False,
                    components=[],
                    flats=[],
                    committed_count=0,
                    commit_sequence=[],
                    persisted_states=states,
                    invalidated=True,
                    reason="manifest segment matrix is incomplete",
                )
            if [entry.get("index") for entry in segments] != list(range(total)):
                self._remove_snapshot_dir(self.snapshot_dir)
                return CheckpointLoad(
                    valid=False,
                    components=[],
                    flats=[],
                    committed_count=0,
                    commit_sequence=[],
                    persisted_states=states,
                    invalidated=True,
                    reason="manifest segment indices are not canonical",
                )
            committed_indices = [
                int(entry["index"])
                for entry in segments
                if entry.get("committed") is True
            ]
            if committed_indices != list(range(len(committed_indices))):
                self._remove_snapshot_dir(self.snapshot_dir)
                return CheckpointLoad(
                    valid=False,
                    components=[],
                    flats=[],
                    committed_count=0,
                    commit_sequence=[],
                    persisted_states=states,
                    invalidated=True,
                    reason="committed checkpoint segments are not a stable prefix",
                )
            components: list[RasterComponent] = []
            flats: list[FlatContour] = []
            for index in range(len(committed_indices)):
                entry = segments[index]
                try:
                    component, flat = self._load_segment(entry)
                except WorkbenchError:
                    self._remove_snapshot_dir(self.snapshot_dir)
                    return CheckpointLoad(
                        valid=False,
                        components=[],
                        flats=[],
                        committed_count=0,
                        commit_sequence=[],
                        persisted_states=states,
                        invalidated=True,
                        reason="segment cache hash or payload validation failed",
                    )
                components.append(component)
                flats.append(flat)
            expected_sequence = list(range(len(components)))
            sequence = [int(value) for value in manifest.get("commit_sequence", [])]
            if sequence and sequence != expected_sequence:
                self._remove_snapshot_dir(self.snapshot_dir)
                return CheckpointLoad(
                    valid=False,
                    components=[],
                    flats=[],
                    committed_count=0,
                    commit_sequence=[],
                    persisted_states=states,
                    invalidated=True,
                    reason="commit sequence is not canonical",
                )
            self._cleanup_orphans({Path(entry["cache_file"]).name for entry in segments[:len(components)]})
            return CheckpointLoad(
                valid=True,
                components=components,
                flats=flats,
                committed_count=len(components),
                commit_sequence=expected_sequence,
                persisted_states=states,
                invalidated=invalidated,
                reason=reason,
            )

    def _cleanup_orphans(self, referenced: set[str]) -> None:
        for path in self.snapshot_dir.glob("segment_*.npz"):
            if path.name not in referenced:
                path.unlink(missing_ok=True)

    def _load_segment(
        self, entry: dict[str, Any]
    ) -> tuple[RasterComponent, FlatContour]:
        cache_name = str(entry["cache_file"])
        if Path(cache_name).name != cache_name:
            raise WorkbenchError("E_STATE", "segment cache path escapes snapshot")
        cache_file = self.snapshot_dir / cache_name
        if not cache_file.is_file():
            raise WorkbenchError("E_STATE", "segment cache file is missing")
        if _sha256_file(cache_file) != entry.get("cache_sha256"):
            raise WorkbenchError("E_STATE", "segment cache checksum mismatch")
        try:
            with np.load(cache_file, allow_pickle=False) as archive:
                points = np.array(archive["points"], dtype=np.float64)
                packed = np.array(archive["packed_bits"], dtype=np.uint8)
                width = int(archive["width"])
                height = int(archive["height"])
                grid = int(archive["grid"])
                tolerance = float(archive["tolerance"])
                curve_segments = int(archive["curve_segment_count"])
                flat_count = int(archive["flat_count"])
                bbox = tuple(float(value) for value in archive["bbox"])
                content_hash = str(archive["content_hash"].item())
                params_hash = str(archive["params_hash"].item())
                algorithm = str(archive["algorithm"].item())
                counts = np.array(archive["coverage_counts"], dtype=np.uint16)
        except (OSError, KeyError, ValueError) as exc:
            raise WorkbenchError("E_STATE", f"cannot read segment cache: {exc}") from exc
        if (
            content_hash != self.content_hash
            or params_hash != self.params_hash
            or algorithm != ALGORITHM_VERSION
        ):
            raise WorkbenchError("E_STATE", "segment cache identity mismatch")
        spec = RasterSpec(
            width=width,
            height=height,
            grid=grid,
            pixels_per_unit=self.params.pixels_per_unit,
            bbox=bbox,
        )
        if grid != self.params.grid or tolerance != self.params.tolerance:
            raise WorkbenchError("E_STATE", "segment cache parameters mismatch")
        expected_bits = int(spec.fine_width * spec.fine_height)
        if packed.size != (expected_bits + 7) // 8:
            raise WorkbenchError("E_STATE", "segment cache bitmap dimensions mismatch")
        recalculated_counts = coverage_counts_from_bits(
            np.unpackbits(packed)[:expected_bits], spec
        )
        if not np.array_equal(recalculated_counts, counts):
            raise WorkbenchError("E_STATE", "segment cache coverage mismatch")
        component = RasterComponent(
            contour_index=int(entry["contour_index"]),
            packed_bits=packed,
            coverage_counts=counts,
            flat_count=flat_count,
            content_hash=str(entry["component_hash"]),
        )
        flat = FlatContour(
            contour_index=int(entry["contour_index"]),
            points=points,
            curve_segment_count=curve_segments,
            tolerance=tolerance,
            closed=True,
        )
        return component, flat

    def persist_states(self, state_payload: dict[str, Any]) -> None:
        with self._lock:
            self.snapshot_dir.mkdir(parents=True, exist_ok=True)
            manifest = self._read_json(self.manifest_path)
            if manifest is None:
                manifest = self._identity_dict() | {
                    "total_segments": state_payload["total"],
                    "committed_count": 0,
                    "segments": [
                        {
                            "index": record["index"],
                            "state": record["state"],
                            "committed": False,
                        }
                        for record in state_payload["segments"]
                    ],
                    "commit_sequence": [],
                }
            manifest["states"] = state_payload
            _atomic_write(self.states_path, _json_bytes(state_payload))
            _atomic_write(self.manifest_path, _json_bytes(manifest))

    def commit_segment(
        self,
        index: int,
        flat: FlatContour,
        component: RasterComponent,
        spec: RasterSpec,
        states: dict[str, Any],
    ) -> dict[str, Any]:
        with self._lock:
            self.snapshot_dir.mkdir(parents=True, exist_ok=True)
            manifest = self._read_json(self.manifest_path)
            if manifest is not None and not self._identity_matches(manifest):
                raise WorkbenchError(
                    "E_STATE",
                    "checkpoint identity changed during commit",
                    segment_index=index,
                )
            if manifest is None:
                manifest = self._identity_dict() | {
                    "total_segments": states["total"],
                    "committed_count": 0,
                    "segments": [
                        {
                            "index": record["index"],
                            "state": "PENDING_FLATTEN",
                            "committed": False,
                        }
                        for record in states["segments"]
                    ],
                    "commit_sequence": [],
                }
            total = int(manifest["total_segments"])
            if total != states["total"] or not (0 <= index < total):
                raise WorkbenchError(
                    "E_STATE", "checkpoint segment count mismatch", segment_index=index
                )
            entries = manifest["segments"]
            existing = entries[index]
            if existing.get("committed") is True:
                if existing.get("component_hash") == component.content_hash:
                    return existing
                raise WorkbenchError(
                    "E_STATE",
                    "refusing to silently rewrite a committed segment cache",
                    segment_index=index,
                )
            committed_prefix = int(manifest.get("committed_count", 0))
            if index != committed_prefix:
                raise WorkbenchError(
                    "E_STATE",
                    "segments must commit in stable contour order",
                    segment_index=index,
                    expected_index=committed_prefix,
                )
            payload = self._encode_segment(index, flat, component, spec)
            cache_sha = _sha256_bytes(payload)
            cache_name = f"segment_{index:06d}_{cache_sha[:20]}.npz"
            cache_path = self.snapshot_dir / cache_name
            if cache_path.exists() and _sha256_file(cache_path) != cache_sha:
                raise WorkbenchError(
                    "E_STATE",
                    "conflicting uncommitted segment cache exists",
                    segment_index=index,
                )
            if not cache_path.exists():
                _atomic_write(cache_path, payload)
            committed_states = {
                key: value for key, value in states.items() if key != "segments"
            }
            committed_states["segments"] = [
                dict(record) for record in states["segments"]
            ]
            record = committed_states["segments"][index]
            record["state"] = "COMMITTED"
            record["state_label"] = "已提交"
            record["committed"] = True
            record["flat_count"] = component.flat_count
            record["cache_hash"] = component.content_hash
            record["error_code"] = None
            record["error_message"] = None
            committed_states["committed_count"] = index + 1
            committed_states["commit_sequence"] = list(range(index + 1))
            committed_states["progress"] = f"{index + 1}/{states['total']}"
            entry = {
                "index": index,
                "contour_index": component.contour_index,
                "state": "COMMITTED",
                "committed": True,
                "cache_file": cache_name,
                "cache_sha256": cache_sha,
                "component_hash": component.content_hash,
                "flat_count": component.flat_count,
                "buffer_bytes": component.buffer_bytes,
            }
            entries[index] = entry
            manifest["committed_count"] = index + 1
            manifest["commit_sequence"] = list(range(index + 1))
            manifest["states"] = committed_states
            manifest["width"] = spec.width
            manifest["height"] = spec.height
            manifest["bbox"] = list(spec.bbox)
            _atomic_write(self.manifest_path, _json_bytes(manifest))
            _atomic_write(self.states_path, _json_bytes(committed_states))
            return entry

    def _encode_segment(
        self,
        index: int,
        flat: FlatContour,
        component: RasterComponent,
        spec: RasterSpec,
    ) -> bytes:
        buffer = io.BytesIO()
        np.savez_compressed(
            buffer,
            points=np.ascontiguousarray(flat.points, dtype=np.float64),
            packed_bits=np.ascontiguousarray(component.packed_bits, dtype=np.uint8),
            coverage_counts=np.ascontiguousarray(
                component.coverage_counts, dtype=np.uint16
            ),
            width=np.asarray(spec.width, dtype=np.int64),
            height=np.asarray(spec.height, dtype=np.int64),
            grid=np.asarray(spec.grid, dtype=np.int64),
            bbox=np.asarray(spec.bbox, dtype=np.float64),
            tolerance=np.asarray(flat.tolerance, dtype=np.float64),
            curve_segment_count=np.asarray(flat.curve_segment_count, dtype=np.int64),
            flat_count=np.asarray(component.flat_count, dtype=np.int64),
            content_hash=np.asarray(self.content_hash),
            params_hash=np.asarray(self.params_hash),
            algorithm=np.asarray(ALGORITHM_VERSION),
            index=np.asarray(index, dtype=np.int64),
        )
        return buffer.getvalue()

    def reset(self) -> None:
        with self._lock:
            for child in list(self.glyph_root.iterdir()) if self.glyph_root.exists() else []:
                if child.is_dir():
                    self._remove_snapshot_dir(child)

    def prune_other_snapshots(self) -> None:
        with self._lock:
            if not self.glyph_root.exists():
                return
            for child in list(self.glyph_root.iterdir()):
                if child.is_dir() and child.name != self.snapshot_dir.name:
                    self._remove_snapshot_dir(child)

