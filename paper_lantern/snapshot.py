from __future__ import annotations

import json
import struct
from dataclasses import dataclass
from pathlib import Path

import numpy as np

from .scene import Scene
from .state import RenderState, block_bounds, empty_state, summarize_block_arrays
from .util import (
    atomic_write_bytes,
    canonical_json,
    sha256_bytes,
)

SNAPSHOT_MAGIC = b"PLSNP001"
SNAPSHOT_FORMAT = "paper-lantern-snapshot-v1"


@dataclass(frozen=True)
class SnapshotLoad:
    status: str
    state: RenderState
    reason: str | None = None


def snapshot_arrays(
    scene: Scene, state: RenderState
) -> list[tuple[str, np.ndarray]]:
    return [
        ("rgba", np.asarray(state.rgba, dtype="<f8")),
        ("depth", np.asarray(state.depth, dtype="<f8")),
        ("layer_rgba", np.asarray(state.layer_rgba, dtype="<f8")),
        ("visible_fragments", np.asarray(state.visible_fragments, dtype="<i8")),
        ("paper_fragments", np.asarray(state.paper_fragments, dtype="<i8")),
        (
            "effective_transmission_sum",
            np.asarray(state.effective_transmission_sum, dtype="<f8"),
        ),
        ("cutout_fragments", np.asarray(state.cutout_fragments, dtype="<i8")),
    ]


def save_snapshot(path: str | Path, scene: Scene, state: RenderState) -> None:
    arrays = snapshot_arrays(scene, state)
    payload_parts: list[bytes] = []
    array_specs: list[dict] = []
    for name, array in arrays:
        contiguous = np.ascontiguousarray(array)
        data = contiguous.tobytes(order="C")
        payload_parts.append(data)
        array_specs.append(
            {
                "name": name,
                "dtype": contiguous.dtype.str,
                "shape": list(contiguous.shape),
                "nbytes": len(data),
            }
        )
    payload = b"".join(payload_parts)
    header = {
        "format": SNAPSHOT_FORMAT,
        "scene_digest": scene.digest,
        "image_width": scene.image.width,
        "image_height": scene.image.height,
        "tile_width": scene.image.tile_width,
        "tile_height": scene.image.tile_height,
        "tile_columns": scene.tile_columns,
        "tile_rows": scene.tile_rows,
        "total_blocks": scene.tile_count,
        "completed_blocks": list(state.completed_blocks),
        "block_summaries": list(state.block_summaries),
        "recovery_count": int(state.recovery_count),
        "shadow_sample_rays": int(state.shadow_sample_rays),
        "shadow_occluder_tests": int(state.shadow_occluder_tests),
        "arrays": array_specs,
        "payload_sha256": sha256_bytes(payload),
    }
    header_bytes = canonical_json(header)
    file_bytes = (
        SNAPSHOT_MAGIC
        + struct.pack("<Q", len(header_bytes))
        + header_bytes
        + payload
    )
    atomic_write_bytes(Path(path), file_bytes)


def _discard(scene: Scene, status: str, reason: str) -> SnapshotLoad:
    return SnapshotLoad(status=status, state=empty_state(scene), reason=reason)


def _expected_array_specs(scene: Scene) -> dict[str, tuple[str, tuple[int, ...]]]:
    height = scene.image.height
    width = scene.image.width
    layers = len(scene.layers)
    return {
        "rgba": ("<f8", (height, width, 4)),
        "depth": ("<f8", (height, width)),
        "layer_rgba": ("<f8", (layers, height, width, 4)),
        "visible_fragments": ("<i8", (layers,)),
        "paper_fragments": ("<i8", (layers,)),
        "effective_transmission_sum": ("<f8", (layers,)),
        "cutout_fragments": ("<i8", (layers,)),
    }


def load_snapshot(path: str | Path, scene: Scene) -> SnapshotLoad:
    snapshot_path = Path(path)
    if not snapshot_path.exists():
        return SnapshotLoad(status="none", state=empty_state(scene))
    try:
        file_bytes = snapshot_path.read_bytes()
    except OSError as exc:
        return _discard(scene, "discarded_corrupt", f"read_error:{exc.__class__.__name__}")
    if len(file_bytes) < len(SNAPSHOT_MAGIC) + 8 + 2:
        return _discard(scene, "discarded_corrupt", "file_too_short")
    if file_bytes[: len(SNAPSHOT_MAGIC)] != SNAPSHOT_MAGIC:
        return _discard(scene, "discarded_corrupt", "bad_magic")
    header_length = struct.unpack(
        "<Q", file_bytes[len(SNAPSHOT_MAGIC) : len(SNAPSHOT_MAGIC) + 8]
    )[0]
    header_start = len(SNAPSHOT_MAGIC) + 8
    header_end = header_start + header_length
    if header_length <= 0 or header_length > 16 * 1024 * 1024 or header_end > len(file_bytes):
        return _discard(scene, "discarded_corrupt", "bad_header_length")
    try:
        header = json.loads(file_bytes[header_start:header_end].decode("utf-8"))
    except (UnicodeDecodeError, json.JSONDecodeError):
        return _discard(scene, "discarded_corrupt", "bad_header_json")
    if not isinstance(header, dict) or header.get("format") != SNAPSHOT_FORMAT:
        return _discard(scene, "discarded_corrupt", "bad_format")
    if header.get("scene_digest") != scene.digest:
        return _discard(scene, "discarded_scene_mismatch", "scene_digest_mismatch")
    if (
        header.get("image_width") != scene.image.width
        or header.get("image_height") != scene.image.height
        or header.get("tile_width") != scene.image.tile_width
        or header.get("tile_height") != scene.image.tile_height
        or header.get("total_blocks") != scene.tile_count
    ):
        return _discard(scene, "discarded_scene_mismatch", "image_layout_mismatch")

    payload = file_bytes[header_end:]
    if sha256_bytes(payload) != header.get("payload_sha256"):
        return _discard(scene, "discarded_corrupt", "payload_checksum_mismatch")

    raw_specs = header.get("arrays")
    if not isinstance(raw_specs, list):
        return _discard(scene, "discarded_corrupt", "array_specs_missing")
    expected_specs = _expected_array_specs(scene)
    if len(raw_specs) != len(expected_specs):
        return _discard(scene, "discarded_corrupt", "array_count_mismatch")
    arrays: dict[str, np.ndarray] = {}
    offset = 0
    for spec in raw_specs:
        if not isinstance(spec, dict):
            return _discard(scene, "discarded_corrupt", "bad_array_spec")
        name = spec.get("name")
        if name not in expected_specs or name in arrays:
            return _discard(scene, "discarded_corrupt", "unexpected_array_name")
        dtype_string, shape = expected_specs[name]
        if spec.get("dtype") != dtype_string or tuple(spec.get("shape", ())) != shape:
            return _discard(scene, "discarded_corrupt", "array_type_mismatch")
        expected_bytes = int(np.prod(shape)) * np.dtype(dtype_string).itemsize
        if spec.get("nbytes") != expected_bytes:
            return _discard(scene, "discarded_corrupt", "array_size_mismatch")
        end = offset + expected_bytes
        if end > len(payload):
            return _discard(scene, "discarded_corrupt", "payload_too_short")
        array = np.frombuffer(payload[offset:end], dtype=dtype_string).reshape(shape).copy()
        arrays[name] = array
        offset = end
    if offset != len(payload) or set(arrays) != set(expected_specs):
        return _discard(scene, "discarded_corrupt", "payload_length_mismatch")

    completed = header.get("completed_blocks")
    summaries = header.get("block_summaries")
    if not isinstance(completed, list) or completed != list(range(len(completed))):
        return _discard(scene, "discarded_corrupt", "noncontiguous_completed_blocks")
    if len(completed) > scene.tile_count or not isinstance(summaries, list):
        return _discard(scene, "discarded_corrupt", "invalid_completed_blocks")
    if len(summaries) != len(completed):
        return _discard(scene, "discarded_corrupt", "block_summary_count_mismatch")
    recovery_count = header.get("recovery_count")
    shadow_sample_rays = header.get("shadow_sample_rays")
    shadow_occluder_tests = header.get("shadow_occluder_tests")
    if (
        not isinstance(recovery_count, int)
        or recovery_count < 0
        or not isinstance(shadow_sample_rays, int)
        or shadow_sample_rays < 0
        or not isinstance(shadow_occluder_tests, int)
        or shadow_occluder_tests < 0
    ):
        return _discard(scene, "discarded_corrupt", "invalid_scalar_stats")

    state = RenderState(
        rgba=arrays["rgba"],
        depth=arrays["depth"],
        layer_rgba=arrays["layer_rgba"],
        visible_fragments=arrays["visible_fragments"],
        paper_fragments=arrays["paper_fragments"],
        effective_transmission_sum=arrays["effective_transmission_sum"],
        cutout_fragments=arrays["cutout_fragments"],
        shadow_sample_rays=shadow_sample_rays,
        shadow_occluder_tests=shadow_occluder_tests,
        completed_blocks=list(completed),
        block_summaries=list(summaries),
        recovery_count=recovery_count,
    )
    for block in completed:
        expected_summary = summarize_block_arrays(scene, state.rgba, state.depth, block)
        if expected_summary != summaries[block]:
            return _discard(scene, "discarded_corrupt", f"block_summary_mismatch:{block}")
        column, row, x_start, y_start, x_end, y_end = block_bounds(scene, block)
        del column, row
        region = state.rgba[y_start:y_end, x_start:x_end]
        if not np.all(np.isfinite(region)):
            return _discard(scene, "discarded_corrupt", f"nonfinite_block:{block}")
    for block in range(len(completed), scene.tile_count):
        column, row, x_start, y_start, x_end, y_end = block_bounds(scene, block)
        del column, row
        if (
            np.any(state.rgba[y_start:y_end, x_start:x_end] != 0.0)
            or np.any(state.depth[y_start:y_end, x_start:x_end] != 0.0)
            or np.any(state.layer_rgba[:, y_start:y_end, x_start:x_end] != 0.0)
        ):
            return _discard(scene, "discarded_corrupt", "stale_uncommitted_block_data")
    if (
        np.any(state.visible_fragments < 0)
        or np.any(state.paper_fragments < 0)
        or np.any(state.cutout_fragments < 0)
        or not np.all(np.isfinite(state.effective_transmission_sum))
        or np.any(state.effective_transmission_sum < 0.0)
        or not np.all(np.isfinite(state.depth))
        or not np.all(np.isfinite(state.layer_rgba))
    ):
        return _discard(scene, "discarded_corrupt", "invalid_layer_stats")
    return SnapshotLoad(status="resumed", state=state)

