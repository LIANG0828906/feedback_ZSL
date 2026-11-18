from __future__ import annotations

import hashlib
import json
import os
import zipfile
from dataclasses import dataclass
from pathlib import Path
from typing import Any

import numpy as np

from .errors import SnapshotError
from .raster import ChunkResult, RENDERER_VERSION, chunk_bounds, chunk_count
from .scene import Scene

SNAPSHOT_MAGIC = "glass_verify_snapshot_v1"


def render_digest(scene: Scene) -> str:
    payload = {
        "renderer_version": RENDERER_VERSION,
        "scene_digest": scene.digest,
    }
    encoded = json.dumps(
        payload, ensure_ascii=False, sort_keys=True, separators=(",", ":")
    ).encode("utf-8")
    return "sha256:" + hashlib.sha256(encoded).hexdigest()


def _array_digest(array: np.ndarray, dtype: str | None = None) -> str:
    if dtype is not None:
        array = np.ascontiguousarray(array, dtype=dtype)
    else:
        array = np.ascontiguousarray(array)
    return "sha256:" + hashlib.sha256(array.tobytes(order="C")).hexdigest()


def _metrics_bytes(metrics: dict[str, Any]) -> bytes:
    return json.dumps(
        metrics,
        ensure_ascii=False,
        allow_nan=False,
        sort_keys=True,
        separators=(",", ":"),
    ).encode("utf-8")


def _payload_digest(results: dict[int, ChunkResult]) -> str:
    digest = hashlib.sha256()
    for index in sorted(results):
        result = results[index]
        digest.update(index.to_bytes(8, "big"))
        digest.update(np.ascontiguousarray(result.rgb, dtype=np.uint8).tobytes(order="C"))
        digest.update(np.ascontiguousarray(result.labels, dtype="<u2").tobytes(order="C"))
        digest.update(_metrics_bytes(result.metrics))
    return "sha256:" + digest.hexdigest()


def save_snapshot(
    path: Path,
    scene: Scene,
    results: dict[int, ChunkResult],
    recovery_count: int,
    metadata_extra: dict[str, Any] | None = None,
) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    combined = dict(metadata_extra or {})
    combined.update(
        {
            "magic": SNAPSHOT_MAGIC,
            "renderer_version": RENDERER_VERSION,
            "scene_digest": scene.digest,
            "render_digest": render_digest(scene),
            "width": scene.camera.width,
            "height": scene.camera.height,
            "chunk_size": list(scene.render.chunk_size),
            "chunk_grid": list(chunk_count(scene)[:2]),
            "total_chunks": chunk_count(scene)[2],
            "completed_chunks": sorted(results),
            "recovery_count": int(recovery_count),
            "payload_digest": _payload_digest(results),
            "chunks": [
                {
                    "index": index,
                    "rgb_digest": _array_digest(results[index].rgb, "uint8"),
                    "labels_digest": _array_digest(results[index].labels, "<u2"),
                    "metrics_digest": "sha256:"
                    + hashlib.sha256(_metrics_bytes(results[index].metrics)).hexdigest(),
                }
                for index in sorted(results)
            ],
        }
    )
    meta_bytes = json.dumps(
        combined,
        ensure_ascii=False,
        allow_nan=False,
        sort_keys=True,
        separators=(",", ":"),
    ).encode("utf-8")
    arrays: dict[str, np.ndarray] = {
        "meta": np.frombuffer(meta_bytes, dtype=np.uint8).copy(),
    }
    for index, result in sorted(results.items()):
        arrays[f"chunk_{index}_rgb"] = np.ascontiguousarray(result.rgb, dtype=np.uint8)
        arrays[f"chunk_{index}_labels"] = np.ascontiguousarray(result.labels, dtype="<u2")
        arrays[f"chunk_{index}_metrics"] = np.frombuffer(
            _metrics_bytes(result.metrics), dtype=np.uint8
        ).copy()
    temporary = path.with_name(f".{path.stem}.{os.getpid()}.tmp.npz")
    try:
        with temporary.open("wb") as handle:
            np.savez_compressed(handle, **arrays)
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temporary, path)
        try:
            directory_fd = os.open(path.parent, os.O_RDONLY)
            try:
                os.fsync(directory_fd)
            finally:
                os.close(directory_fd)
        except OSError:
            pass
    finally:
        if temporary.exists():
            temporary.unlink()


@dataclass
class SnapshotLoad:
    results: dict[int, ChunkResult]
    recovery_count: int
    discarded_reason: str | None
    status: str


def _discard(reason: str) -> SnapshotLoad:
    return SnapshotLoad({}, 0, reason, "discarded")


def load_snapshot(path: Path, scene: Scene) -> SnapshotLoad:
    if not path.exists():
        return SnapshotLoad({}, 0, None, "absent")
    try:
        with np.load(path, allow_pickle=False) as archive:
            if "meta" not in archive.files:
                raise SnapshotError("snapshot has no metadata")
            meta_bytes = np.ascontiguousarray(archive["meta"], dtype=np.uint8).tobytes()
            meta = json.loads(meta_bytes.decode("utf-8"))
            if not isinstance(meta, dict) or meta.get("magic") != SNAPSHOT_MAGIC:
                raise SnapshotError("snapshot magic does not match")
            if meta.get("scene_digest") != scene.digest:
                raise SnapshotError("scene digest mismatch")
            if meta.get("render_digest") != render_digest(scene):
                raise SnapshotError("renderer digest mismatch")
            if meta.get("width") != scene.camera.width or meta.get("height") != scene.camera.height:
                raise SnapshotError("snapshot dimensions mismatch")
            if meta.get("chunk_size") != list(scene.render.chunk_size):
                raise SnapshotError("chunk size mismatch")
            columns, rows, total = chunk_count(scene)
            if meta.get("chunk_grid") != [columns, rows] or meta.get("total_chunks") != total:
                raise SnapshotError("chunk grid mismatch")
            completed = meta.get("completed_chunks")
            if not isinstance(completed, list) or completed != list(range(len(completed))):
                raise SnapshotError("completed chunk list is not a contiguous prefix")
            if not completed:
                return SnapshotLoad({}, int(meta.get("recovery_count", 0)), None, "valid_empty")
            chunk_meta = meta.get("chunks")
            if not isinstance(chunk_meta, list) or len(chunk_meta) != len(completed):
                raise SnapshotError("snapshot chunk metadata is incomplete")
            entries_by_index = {
                int(entry.get("index")): entry
                for entry in chunk_meta
                if isinstance(entry, dict) and isinstance(entry.get("index"), int)
            }
            if sorted(entries_by_index) != completed:
                raise SnapshotError("snapshot chunk metadata indices do not match")

            results: dict[int, ChunkResult] = {}
            recovered_count = int(meta.get("recovery_count", 0))
            for index in completed:
                x0, y0, width, height = chunk_bounds(scene, int(index))
                rgb_key = f"chunk_{index}_rgb"
                labels_key = f"chunk_{index}_labels"
                metrics_key = f"chunk_{index}_metrics"
                if any(key not in archive.files for key in (rgb_key, labels_key, metrics_key)):
                    raise SnapshotError(f"snapshot chunk {index} payload is incomplete")
                rgb = np.ascontiguousarray(archive[rgb_key], dtype=np.uint8)
                labels = np.ascontiguousarray(archive[labels_key], dtype="<u2")
                metrics_raw = np.ascontiguousarray(archive[metrics_key], dtype=np.uint8).tobytes()
                if rgb.shape != (height, width, 3) or labels.shape != (height, width):
                    raise SnapshotError(f"snapshot chunk {index} shape mismatch")
                entry = entries_by_index[int(index)]
                if entry.get("rgb_digest") != _array_digest(rgb, "uint8"):
                    raise SnapshotError(f"snapshot chunk {index} RGB digest mismatch")
                if entry.get("labels_digest") != _array_digest(labels, "<u2"):
                    raise SnapshotError(f"snapshot chunk {index} label digest mismatch")
                if entry.get("metrics_digest") != "sha256:" + hashlib.sha256(metrics_raw).hexdigest():
                    raise SnapshotError(f"snapshot chunk {index} metrics digest mismatch")
                metrics = json.loads(metrics_raw.decode("utf-8"))
                results[int(index)] = ChunkResult(
                    int(index), x0, y0, width, height, rgb, labels, metrics
                )
            if meta.get("payload_digest") != _payload_digest(results):
                raise SnapshotError("snapshot payload digest mismatch")
            return SnapshotLoad(results, recovered_count, None, "valid")
    except (
        SnapshotError,
        OSError,
        ValueError,
        KeyError,
        TypeError,
        json.JSONDecodeError,
        EOFError,
        zipfile.BadZipFile,
    ) as exc:
        return _discard(str(exc) or exc.__class__.__name__)
