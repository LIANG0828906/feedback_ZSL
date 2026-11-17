from __future__ import annotations

import io
import shutil
from dataclasses import dataclass
from pathlib import Path
from typing import Any

import numpy as np

from . import RENDERER_VERSION, __version__
from .io_utils import (
    atomic_write_bytes,
    atomic_write_json,
    canonical_json_bytes,
    load_json,
    sha256_bytes,
)
from .renderer import ChunkLayout, TileResult, chunk_layout_hash
from .scene import Scene

STATE_DIR_NAME = ".volverify-state"
MANIFEST_SCHEMA_VERSION = 1
CHUNK_ARRAY_NAMES = (
    "rgba",
    "depth",
    "entry_t",
    "exit_t",
    "peak_density",
    "peak_t",
    "peak_index",
    "index_min",
    "index_max",
)


@dataclass
class SnapshotState:
    manifest: dict[str, Any]
    resumed_chunks: int
    recovery_event: dict[str, Any] | None


def _tile_file_name(chunk_id: int) -> str:
    return f"chunk-{chunk_id:06d}.npz"


def _serialize_tile(tile: TileResult) -> bytes:
    buffer = io.BytesIO()
    np.savez_compressed(
        buffer,
        rgba=tile.rgba,
        depth=tile.depth,
        entry_t=tile.entry_t,
        exit_t=tile.exit_t,
        peak_density=tile.peak_density,
        peak_t=tile.peak_t,
        peak_index=tile.peak_index,
        index_min=tile.index_min,
        index_max=tile.index_max,
    )
    return buffer.getvalue()


def _array_hash(array: np.ndarray) -> str:
    return sha256_bytes(np.ascontiguousarray(array).tobytes())


def _chunk_summary(tile: TileResult, file_sha256: str) -> dict[str, Any]:
    return {
        "chunk_id": tile.chunk_id,
        "x0": tile.x0,
        "y0": tile.y0,
        "width": tile.width,
        "height": tile.height,
        "file_sha256": file_sha256,
        "rgba_sha256": _array_hash(tile.rgba),
        "depth_sha256": _array_hash(tile.depth),
        "sample_count": tile.sample_count,
        "actual_step_min": tile.actual_step_min,
        "actual_step_max": tile.actual_step_max,
        "self_rgb": tile.self_rgb.tolist(),
        "light_rgb": {
            light_id: values.tolist()
            for light_id, values in sorted(tile.light_rgb.items())
        },
    }


def _empty_cumulative(light_ids: list[str]) -> dict[str, Any]:
    return {
        "completed_chunks": 0,
        "completed_pixels": 0,
        "sample_count": 0,
        "actual_step_min": None,
        "actual_step_max": None,
        "self_rgb": [0.0, 0.0, 0.0],
        "light_rgb": {light_id: [0.0, 0.0, 0.0] for light_id in light_ids},
    }


def _recompute_cumulative(
    completed: list[dict[str, Any]], light_ids: list[str]
) -> dict[str, Any]:
    cumulative = _empty_cumulative(light_ids)
    for summary in sorted(completed, key=lambda item: item["chunk_id"]):
        cumulative["completed_chunks"] += 1
        cumulative["completed_pixels"] += summary["width"] * summary["height"]
        cumulative["sample_count"] += summary["sample_count"]
        if summary["sample_count"]:
            chunk_min = summary["actual_step_min"]
            chunk_max = summary["actual_step_max"]
            if cumulative["actual_step_min"] is None:
                cumulative["actual_step_min"] = chunk_min
            else:
                cumulative["actual_step_min"] = min(
                    cumulative["actual_step_min"], chunk_min
                )
            if cumulative["actual_step_max"] is None:
                cumulative["actual_step_max"] = chunk_max
            else:
                cumulative["actual_step_max"] = max(
                    cumulative["actual_step_max"], chunk_max
                )
        cumulative["self_rgb"] = [
            float(left + right)
            for left, right in zip(
                cumulative["self_rgb"], summary["self_rgb"]
            )
        ]
        for light_id in light_ids:
            values = summary["light_rgb"].get(light_id, [0.0, 0.0, 0.0])
            cumulative["light_rgb"][light_id] = [
                float(left + right)
                for left, right in zip(
                    cumulative["light_rgb"][light_id], values
                )
            ]
    return cumulative


class SnapshotManager:
    def __init__(
        self,
        out_dir: Path,
        scene: Scene,
        fingerprint: str,
        layout: list[ChunkLayout],
    ):
        self.out_dir = out_dir
        self.scene = scene
        self.fingerprint = fingerprint
        self.layout = layout
        self.layout_hash = chunk_layout_hash(layout)
        self.state_dir = out_dir / STATE_DIR_NAME
        self.chunks_dir = self.state_dir / "chunks"
        self.manifest_path = self.state_dir / "manifest.json"
        self.recovery_path = out_dir / "recovery_report.json"
        self.resume_path = out_dir / "resume_report.json"
        self.light_ids = [light.id for light in scene.lights]

    def _base_manifest(self) -> dict[str, Any]:
        return {
            "schema_version": MANIFEST_SCHEMA_VERSION,
            "renderer": {
                "name": "volverify",
                "version": __version__,
                "renderer_version": RENDERER_VERSION,
            },
            "frame_id": self.scene.frame_id,
            "fingerprint": self.fingerprint,
            "scene_json_sha256": self.scene.scene_json_sha256,
            "volume_sha256": self.scene.volume.data_sha256,
            "chunk_layout_hash": self.layout_hash,
            "chunk_layout": [chunk.to_dict() for chunk in self.layout],
            "status": "running",
            "completed": [],
            "cumulative": _empty_cumulative(self.light_ids),
        }

    def _write_manifest(self, manifest: dict[str, Any]) -> None:
        manifest["completed"] = sorted(
            manifest["completed"], key=lambda item: item["chunk_id"]
        )
        manifest["cumulative"] = _recompute_cumulative(
            manifest["completed"], self.light_ids
        )
        atomic_write_json(self.manifest_path, manifest)

    def _record_recovery(self, event: dict[str, Any]) -> None:
        atomic_write_json(self.recovery_path, event)

    def _write_resume_report(self, event: dict[str, Any]) -> None:
        atomic_write_json(self.resume_path, event)

    def _invalidate_state(
        self,
        code: str,
        message: str,
        details: dict[str, Any],
    ) -> dict[str, Any]:
        completed_count = 0
        old_fingerprint = None
        old_layout_hash = None
        if self.manifest_path.exists():
            try:
                old_manifest = load_json(self.manifest_path)
                completed_count = len(old_manifest.get("completed", []))
                old_fingerprint = old_manifest.get("fingerprint")
                old_layout_hash = old_manifest.get("chunk_layout_hash")
            except (OSError, ValueError):
                completed_count = 0
        archive_name = (
            f"invalidated-state-{str(old_fingerprint or self.fingerprint)[:12]}"
        )
        archive = self.out_dir / archive_name
        suffix = 0
        while archive.exists():
            suffix += 1
            archive = self.out_dir / f"{archive_name}-{suffix}"
        if self.state_dir.exists():
            shutil.move(str(self.state_dir), str(archive))
        event = {
            "schema_version": 1,
            "frame_id": self.scene.frame_id,
            "code": code,
            "status": "invalidated",
            "message": message,
            "invalidated_chunks": completed_count,
            "old_fingerprint": old_fingerprint,
            "new_fingerprint": self.fingerprint,
            "old_chunk_layout_hash": old_layout_hash,
            "new_chunk_layout_hash": self.layout_hash,
            "details": details,
        }
        self._record_recovery(event)
        return event

    def _manifest_mismatch_details(
        self, manifest: dict[str, Any]
    ) -> dict[str, Any] | None:
        if manifest.get("schema_version") != MANIFEST_SCHEMA_VERSION:
            return {
                "field": "schema_version",
                "expected": MANIFEST_SCHEMA_VERSION,
                "actual": manifest.get("schema_version"),
            }
        if manifest.get("fingerprint") != self.fingerprint:
            return {
                "field": "fingerprint",
                "expected": self.fingerprint,
                "actual": manifest.get("fingerprint"),
            }
        if manifest.get("chunk_layout_hash") != self.layout_hash:
            return {
                "field": "chunk_layout_hash",
                "expected": self.layout_hash,
                "actual": manifest.get("chunk_layout_hash"),
            }
        if manifest.get("chunk_layout") != [
            chunk.to_dict() for chunk in self.layout
        ]:
            return {
                "field": "chunk_layout",
                "expected": [chunk.to_dict() for chunk in self.layout],
                "actual": manifest.get("chunk_layout"),
            }
        if manifest.get("scene_json_sha256") != self.scene.scene_json_sha256:
            return {
                "field": "scene_json_sha256",
                "expected": self.scene.scene_json_sha256,
                "actual": manifest.get("scene_json_sha256"),
            }
        if manifest.get("volume_sha256") != self.scene.volume.data_sha256:
            return {
                "field": "volume_sha256",
                "expected": self.scene.volume.data_sha256,
                "actual": manifest.get("volume_sha256"),
            }
        completed = manifest.get("completed")
        if not isinstance(completed, list):
            return {
                "field": "completed",
                "expected": "array",
                "actual": type(completed).__name__,
            }
        expected_by_id = {chunk.chunk_id: chunk for chunk in self.layout}
        seen: set[int] = set()
        for summary in completed:
            if not isinstance(summary, dict):
                return {"field": "completed", "reason": "non-object entry"}
            chunk_id = summary.get("chunk_id")
            if not isinstance(chunk_id, int) or chunk_id not in expected_by_id:
                return {
                    "field": "completed.chunk_id",
                    "reason": "chunk id is not in the current layout",
                    "chunk_id": chunk_id,
                }
            if chunk_id in seen:
                return {
                    "field": "completed.chunk_id",
                    "reason": "duplicate chunk id",
                    "chunk_id": chunk_id,
                }
            seen.add(chunk_id)
            expected = expected_by_id[chunk_id]
            for field in ("x0", "y0", "width", "height"):
                if summary.get(field) != getattr(expected, field):
                    return {
                        "field": f"chunk[{chunk_id}].{field}",
                        "expected": getattr(expected, field),
                        "actual": summary.get(field),
                    }
            chunk_path = self.chunks_dir / _tile_file_name(chunk_id)
            if not chunk_path.exists():
                return {
                    "field": "chunk_file",
                    "reason": "missing persisted chunk",
                    "chunk_id": chunk_id,
                }
            from .io_utils import sha256_file

            if sha256_file(chunk_path) != summary.get("file_sha256"):
                return {
                    "field": "chunk_file_sha256",
                    "reason": "persisted chunk file is incomplete or changed",
                    "chunk_id": chunk_id,
                }
        return None

    def initialize(self, resume: bool) -> SnapshotState:
        self.out_dir.mkdir(parents=True, exist_ok=True)
        if resume and self.state_dir.exists():
            try:
                manifest = load_json(self.manifest_path)
                if not isinstance(manifest, dict):
                    raise ValueError("manifest root is not an object")
            except (OSError, ValueError) as exc:
                event = self._invalidate_state(
                    "SNAPSHOT_CORRUPT",
                    "snapshot manifest could not be read; all old progress was invalidated",
                    {"error": str(exc)},
                )
                return self._create_fresh(event)

            mismatch = self._manifest_mismatch_details(manifest)
            if mismatch is not None:
                event = self._invalidate_state(
                    "SNAPSHOT_INPUT_MISMATCH",
                    "persisted snapshot does not match the current input; all old progress was invalidated",
                    mismatch,
                )
                return self._create_fresh(event)

            resumed_chunks = len(manifest.get("completed", []))
            manifest["status"] = "running"
            manifest.pop("interrupt_signal", None)
            self._write_manifest(manifest)
            if resumed_chunks:
                self._write_resume_report(
                    {
                        "schema_version": 1,
                        "frame_id": self.scene.frame_id,
                        "code": "SNAPSHOT_RESUMED",
                        "resumed_chunks": resumed_chunks,
                        "remaining_chunks": len(self.layout) - resumed_chunks,
                        "fingerprint": self.fingerprint,
                        "chunk_layout_hash": self.layout_hash,
                    }
                )
            return SnapshotState(
                manifest=manifest,
                resumed_chunks=resumed_chunks,
                recovery_event=None,
            )

        if self.state_dir.exists():
            shutil.rmtree(self.state_dir)
        return self._create_fresh(None)

    def _create_fresh(self, event: dict[str, Any] | None) -> SnapshotState:
        self.state_dir.mkdir(parents=True, exist_ok=True)
        self.chunks_dir.mkdir(parents=True, exist_ok=True)
        manifest = self._base_manifest()
        self._write_manifest(manifest)
        return SnapshotState(
            manifest=manifest,
            resumed_chunks=0,
            recovery_event=event,
        )

    def save_chunk(self, tile: TileResult, manifest: dict[str, Any]) -> None:
        file_bytes = _serialize_tile(tile)
        file_sha256 = sha256_bytes(file_bytes)
        chunk_path = self.chunks_dir / _tile_file_name(tile.chunk_id)
        atomic_write_bytes(chunk_path, file_bytes)
        manifest["completed"].append(_chunk_summary(tile, file_sha256))
        self._write_manifest(manifest)

    def load_chunk(
        self, summary: dict[str, Any]
    ) -> dict[str, np.ndarray]:
        chunk_id = int(summary["chunk_id"])
        chunk_path = self.chunks_dir / _tile_file_name(chunk_id)
        raw = chunk_path.read_bytes()
        if sha256_bytes(raw) != summary.get("file_sha256"):
            raise ValueError(f"chunk {chunk_id} file hash mismatch")
        arrays: dict[str, np.ndarray] = {}
        with np.load(io.BytesIO(raw), allow_pickle=False) as archive:
            for name in CHUNK_ARRAY_NAMES:
                if name not in archive:
                    raise ValueError(
                        f"chunk {chunk_id} is missing array {name}"
                    )
                arrays[name] = np.array(archive[name], copy=True)
        height = int(summary["height"])
        width = int(summary["width"])
        expected_shapes = {
            "rgba": (height, width, 4),
            "depth": (height, width),
            "entry_t": (height, width),
            "exit_t": (height, width),
            "peak_density": (height, width),
            "peak_t": (height, width),
            "peak_index": (height, width, 3),
            "index_min": (height, width, 3),
            "index_max": (height, width, 3),
        }
        for name, shape in expected_shapes.items():
            if arrays[name].shape != shape:
                raise ValueError(
                    f"chunk {chunk_id} array {name} has shape "
                    f"{arrays[name].shape}, expected {shape}"
                )
        return arrays

    def mark_interrupted(
        self,
        manifest: dict[str, Any],
        signal_number: int,
        phase: str,
        error: str | None = None,
    ) -> dict[str, Any]:
        manifest["status"] = "interrupted"
        manifest["interrupt_signal"] = signal_number
        self._write_manifest(manifest)
        completed = len(manifest.get("completed", []))
        and_next = (
            manifest["completed"][-1]["chunk_id"] + 1
            if manifest.get("completed")
            else 0
        )
        event = {
            "schema_version": 1,
            "frame_id": self.scene.frame_id,
            "code": "INTERRUPTED",
            "status": "interrupted",
            "signal": signal_number,
            "phase": phase,
            "completed_chunks": completed,
            "total_chunks": len(self.layout),
            "next_chunk_id": and_next,
            "fingerprint": self.fingerprint,
            "chunk_layout_hash": self.layout_hash,
            "error": error,
        }
        self._record_recovery(event)
        return event

    def mark_complete(
        self,
        manifest: dict[str, Any],
        frame_summary_sha256: str,
        image_sha256: str,
        depth_sha256: str,
    ) -> None:
        manifest["status"] = "completed"
        manifest["frame_summary_sha256"] = frame_summary_sha256
        manifest["image_sha256"] = image_sha256
        manifest["depth_sha256"] = depth_sha256
        self._write_manifest(manifest)

