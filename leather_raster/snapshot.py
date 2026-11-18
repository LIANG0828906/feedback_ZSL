from __future__ import annotations

import base64
import binascii
import json
import zlib
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

import numpy as np

from .render import BlockResult, BlockSpec, block_specs
from .scene import Scene
from .util import (
    atomic_write_json,
    canonical_json_bytes,
    sha256_bytes,
    stable_float,
)


SNAPSHOT_FORMAT = "leather-shadow-raster-checkpoint"
SNAPSHOT_VERSION = 1


def snapshot_file(out_dir: str | Path) -> Path:
    return Path(out_dir) / ".snapshot" / "checkpoint.json"


def block_color_summary(rgba: np.ndarray) -> dict[str, Any]:
    values = np.asarray(rgba, dtype=np.uint8)
    return {
        "sum": [int(v) for v in np.sum(values, axis=(0, 1), dtype=np.int64)],
        "min": [int(v) for v in np.min(values, axis=(0, 1))],
        "max": [int(v) for v in np.max(values, axis=(0, 1))],
        "mean": [stable_float(v, 6) for v in np.mean(values, axis=(0, 1))],
    }


def _layout(scene: Scene) -> dict[str, Any]:
    return {
        "width": scene.width,
        "height": scene.height,
        "block_width": scene.render.block_width,
        "block_height": scene.render.block_height,
        "supersampling": scene.render.supersampling,
        "layer_count": len(scene.layers),
    }


def _encode_block(result: BlockResult) -> dict[str, Any]:
    rgba_bytes = np.asarray(result.rgba, dtype=np.uint8).tobytes(order="C")
    layer_bytes = np.asarray(result.layer_map, dtype=np.uint8).tobytes(order="C")
    raw = rgba_bytes + layer_bytes
    payload = base64.b64encode(zlib.compress(raw, level=6)).decode("ascii")
    result.rgba_sha256 = sha256_bytes(rgba_bytes)
    result.layer_sha256 = sha256_bytes(layer_bytes)
    result.color_summary = block_color_summary(result.rgba)
    return {
        "block_id": result.spec.block_id,
        "row": result.spec.row,
        "col": result.spec.col,
        "x0": result.spec.x0,
        "y0": result.spec.y0,
        "x1": result.spec.x1,
        "y1": result.spec.y1,
        "rgba_sha256": result.rgba_sha256,
        "layer_map_sha256": result.layer_sha256,
        "color_summary": result.color_summary,
        "stats": result.stats,
        "payload_encoding": "zlib+base64",
        "payload": payload,
    }


def _decode_block(
    entry: dict[str, Any],
    expected: BlockSpec,
    layer_count: int,
) -> BlockResult:
    for name, value in (
        ("row", expected.row),
        ("col", expected.col),
        ("x0", expected.x0),
        ("y0", expected.y0),
        ("x1", expected.x1),
        ("y1", expected.y1),
    ):
        if entry.get(name) != value:
            raise ValueError(f"block layout mismatch for {expected.block_id}: {name}")
    if entry.get("payload_encoding") != "zlib+base64":
        raise ValueError(f"unsupported payload encoding for {expected.block_id}")
    payload = entry.get("payload")
    if not isinstance(payload, str):
        raise ValueError(f"missing payload for {expected.block_id}")
    raw = zlib.decompress(base64.b64decode(payload, validate=True))
    rgb_bytes = expected.width * expected.height * 4
    layer_bytes = expected.width * expected.height
    if len(raw) != rgb_bytes + layer_bytes:
        raise ValueError(f"payload size mismatch for {expected.block_id}")
    rgba = np.frombuffer(raw[:rgb_bytes], dtype=np.uint8).reshape(
        expected.height,
        expected.width,
        4,
    ).copy()
    layer_map = np.frombuffer(raw[rgb_bytes:], dtype=np.uint8).reshape(
        expected.height,
        expected.width,
    ).copy()
    rgba_sha = sha256_bytes(rgba.tobytes(order="C"))
    layer_sha = sha256_bytes(layer_map.tobytes(order="C"))
    if rgba_sha != entry.get("rgba_sha256"):
        raise ValueError(f"RGBA checksum mismatch for {expected.block_id}")
    if layer_sha != entry.get("layer_map_sha256"):
        raise ValueError(f"layer-map checksum mismatch for {expected.block_id}")
    if block_color_summary(rgba) != entry.get("color_summary"):
        raise ValueError(f"color summary mismatch for {expected.block_id}")
    stats = entry.get("stats")
    if not isinstance(stats, dict):
        raise ValueError(f"missing stats for {expected.block_id}")
    samples = stats.get("layer_samples")
    if not isinstance(samples, list) or len(samples) != layer_count:
        raise ValueError(f"invalid layer stats for {expected.block_id}")
    return BlockResult(
        spec=expected,
        rgba=rgba,
        layer_map=layer_map,
        rgba_sha256=rgba_sha,
        layer_sha256=layer_sha,
        color_summary=entry.get("color_summary"),
        stats=stats,
    )


@dataclass
class LoadResult:
    status: str
    blocks: dict[str, BlockResult] = field(default_factory=dict)
    reason: str | None = None
    discarded_blocks: int = 0


def load_snapshot(scene: Scene, out_dir: str | Path) -> LoadResult:
    path = snapshot_file(out_dir)
    if not path.exists():
        return LoadResult(status="empty")
    try:
        value = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, UnicodeError, json.JSONDecodeError) as exc:
        return LoadResult(status="rejected", reason=f"snapshot is unreadable: {exc}")
    if not isinstance(value, dict):
        return LoadResult(status="rejected", reason="snapshot root is not an object")
    checksum = value.get("checkpoint_sha256")
    if not isinstance(checksum, str):
        return LoadResult(status="rejected", reason="snapshot checksum is missing")
    unsigned = {key: item for key, item in value.items() if key != "checkpoint_sha256"}
    expected_checksum = sha256_bytes(canonical_json_bytes(unsigned))
    if checksum != expected_checksum:
        return LoadResult(status="rejected", reason="snapshot checksum mismatch")
    if value.get("format") != SNAPSHOT_FORMAT:
        return LoadResult(status="rejected", reason="snapshot format mismatch")
    if value.get("version") != SNAPSHOT_VERSION:
        return LoadResult(status="rejected", reason="snapshot version mismatch")
    if value.get("scene_sha256") != scene.digest:
        return LoadResult(status="rejected", reason="scene digest mismatch")
    if value.get("layout") != _layout(scene):
        return LoadResult(status="rejected", reason="render layout mismatch")
    entries = value.get("blocks")
    if not isinstance(entries, list):
        return LoadResult(status="rejected", reason="snapshot blocks are missing")
    expected_by_id = {spec.block_id: spec for spec in block_specs(scene)}
    blocks: dict[str, BlockResult] = {}
    try:
        for entry in entries:
            if not isinstance(entry, dict):
                raise ValueError("snapshot block entry is not an object")
            block_id = entry.get("block_id")
            if not isinstance(block_id, str) or block_id not in expected_by_id:
                raise ValueError(f"unknown block id: {block_id!r}")
            if block_id in blocks:
                raise ValueError(f"duplicate block id: {block_id}")
            blocks[block_id] = _decode_block(
                entry,
                expected_by_id[block_id],
                len(scene.layers),
            )
    except (ValueError, TypeError, KeyError, zlib.error, binascii.Error) as exc:
        return LoadResult(
            status="rejected",
            reason=f"snapshot block validation failed: {exc}",
            discarded_blocks=len(entries) if isinstance(entries, list) else 0,
        )
    return LoadResult(status="loaded", blocks=blocks)


class SnapshotWriter:
    def __init__(self, scene: Scene, out_dir: str | Path) -> None:
        self.scene = scene
        self.path = snapshot_file(out_dir)
        self.entries: list[dict[str, Any]] = []
        self.block_ids: set[str] = set()

    def seed(self, blocks: dict[str, BlockResult]) -> None:
        expected = {spec.block_id: spec for spec in block_specs(self.scene)}
        for block_id in sorted(blocks):
            if block_id not in expected:
                raise ValueError(f"cannot seed unknown block {block_id}")
            if block_id in self.block_ids:
                raise ValueError(f"cannot seed duplicate block {block_id}")
            self.entries.append(_encode_block(blocks[block_id]))
            self.block_ids.add(block_id)
        self._write()

    def commit(self, result: BlockResult) -> None:
        if result.spec.block_id in self.block_ids:
            raise ValueError(f"block {result.spec.block_id} was already committed")
        self.entries.append(_encode_block(result))
        self.block_ids.add(result.spec.block_id)
        self._write()

    def _write(self) -> None:
        payload: dict[str, Any] = {
            "format": SNAPSHOT_FORMAT,
            "version": SNAPSHOT_VERSION,
            "scene_sha256": self.scene.digest,
            "layout": _layout(self.scene),
            "blocks": self.entries,
        }
        payload["checkpoint_sha256"] = sha256_bytes(canonical_json_bytes(payload))
        atomic_write_json(self.path, payload)

