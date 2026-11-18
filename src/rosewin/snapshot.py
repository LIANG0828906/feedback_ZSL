"""Atomic resumable chunk snapshots stored as compressed NPZ archives."""

from __future__ import annotations

import json
import os
from dataclasses import dataclass
from pathlib import Path

import numpy as np

from .rasterizer import ChunkResult, Rasterizer


@dataclass
class SnapshotLoad:
    found: bool
    used: bool
    discarded: bool
    discard_reason: str | None
    resume_count: int
    chunk_done: np.ndarray
    arrays: dict[str, np.ndarray]
    meta: dict


class SnapshotStore:
    def __init__(self, path: str | Path, scene_digest_hex: str, rasterizer: Rasterizer):
        self.path = Path(path)
        self.scene_digest = scene_digest_hex
        self.rasterizer = rasterizer
        width = rasterizer.width
        height = rasterizer.height
        facet_count = len(rasterizer.model.facets)
        self.initial_arrays = {
            "rgb": np.zeros((height, width, 3), dtype=np.uint8),
            "transmission": np.zeros((height, width, 3), dtype=np.uint8),
            "reflection": np.zeros((height, width, 3), dtype=np.uint8),
            "shadow": np.zeros((height, width, 3), dtype=np.uint8),
            "owner": np.zeros((height, width), dtype=np.uint16),
            "owner_lanes": np.zeros((height, width, rasterizer.samples), dtype=np.uint16),
            "lead": np.zeros((height, width), dtype=np.uint8),
            "chunk_done": np.zeros(rasterizer.chunk_count, dtype=np.uint8),
            "facet_visible": np.zeros(facet_count + 1, dtype=np.int64),
            "facet_glass": np.zeros(facet_count + 1, dtype=np.int64),
            "facet_transmission_sum": np.zeros((facet_count + 1, 3), dtype=np.float64),
            "facet_reflection_sum": np.zeros((facet_count + 1, 3), dtype=np.float64),
        }

    def load(self) -> SnapshotLoad:
        if not self.path.exists():
            return self._missing()
        try:
            with np.load(self.path) as archive:
                arrays = {key: archive[key].copy() for key in archive.files if key != "meta"}
                meta = json.loads(bytes(archive["meta"]).decode("utf-8"))
        except Exception as exc:  # corrupted npz or malformed json
            self.discard()
            load = self._missing()
            load.found = True
            load.discarded = True
            load.discard_reason = f"corrupt_snapshot: {type(exc).__name__}"
            return load

        if meta.get("scene_sha256") != self.scene_digest:
            self.discard()
            load = self._missing()
            load.found = True
            load.discarded = True
            load.discard_reason = "scene_digest_mismatch"
            return load

        expected_shapes = {key: value.shape for key, value in self.initial_arrays.items()}
        if any(arrays.get(key) is None or arrays[key].shape != shape for key, shape in expected_shapes.items()):
            self.discard()
            load = self._missing()
            load.found = True
            load.discarded = True
            load.discard_reason = "array_shape_mismatch"
            return load

        chunk_done = arrays["chunk_done"]
        resume_count = int(meta.get("resume_count", 0)) + 1
        return SnapshotLoad(
            found=True,
            used=bool(chunk_done.any()),
            discarded=False,
            discard_reason=None,
            resume_count=resume_count,
            chunk_done=chunk_done,
            arrays=arrays,
            meta=meta,
        )

    def _missing(self) -> SnapshotLoad:
        arrays = {key: value.copy() for key, value in self.initial_arrays.items()}
        return SnapshotLoad(
            found=False,
            used=False,
            discarded=False,
            discard_reason=None,
            resume_count=0,
            chunk_done=arrays["chunk_done"],
            arrays=arrays,
            meta={"chunk_summaries": []},
        )

    def save(
        self,
        arrays: dict[str, np.ndarray],
        chunk_summaries: list[dict],
        resume_count: int,
        extra_meta: dict | None = None,
    ) -> None:
        meta = {
            "scene_sha256": self.scene_digest,
            "chunk_count": self.rasterizer.chunk_count,
            "chunk_size": self.rasterizer.chunk_size,
            "resume_count": resume_count,
            "chunk_summaries": chunk_summaries,
        }
        if extra_meta:
            meta.update(extra_meta)
        payload = dict(arrays)
        payload["meta"] = np.frombuffer(json.dumps(meta, separators=(",", ":")).encode("utf-8"), dtype=np.uint8)
        tmp_path = self.path.with_suffix(self.path.suffix + ".tmp")
        with open(tmp_path, "wb") as handle:
            np.savez_compressed(handle, **payload)
        os.replace(tmp_path, self.path)

    def discard(self) -> None:
        if self.path.exists():
            self.path.unlink()


def chunk_summary(
    chunk_index: int,
    result: ChunkResult,
    rgb8: np.ndarray,
    owner: np.ndarray,
    shadow_band: np.ndarray,
) -> dict:
    from hashlib import sha256

    return {
        "chunk_id": f"c{chunk_index:04d}",
        "row_start": int(result.row_start),
        "row_end": int(result.row_end),
        "rgb_sha256": sha256(rgb8.tobytes()).hexdigest(),
        "owner_sha256": sha256(owner.astype("<u2").tobytes()).hexdigest(),
        "shadow_sha256": sha256(shadow_band.tobytes()).hexdigest(),
        "color_min_rgb8": [int(v) for v in rgb8.reshape(-1, 3).min(axis=0)],
        "color_max_rgb8": [int(v) for v in rgb8.reshape(-1, 3).max(axis=0)],
        "color_mean_rgb8": [float(v) for v in rgb8.reshape(-1, 3).mean(axis=0)],
    }
