"""Versioned, hash-chained snapshots with atomic replacement."""

from __future__ import annotations

import base64
import hashlib
import json
import os
import tempfile
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Iterable

import numpy as np

from .errors import E_SNAPSHOT_MISMATCH, fail
from .raster import GlyphRenderPlan, PixelBox, SamplingSpec, TileRaster, coverage_sha256

SNAPSHOT_FORMAT = "glyph-atlas-snapshot/1"
SNAPSHOT_VERSION = 1
GENESIS_HASH = hashlib.sha256(b"glyph-atlas-snapshot-genesis").hexdigest()


def canonical_json_bytes(value: Any) -> bytes:
    return json.dumps(
        value,
        sort_keys=True,
        separators=(",", ":"),
        ensure_ascii=False,
        allow_nan=False,
    ).encode("utf-8")


def digest_json(value: Any) -> str:
    return hashlib.sha256(canonical_json_bytes(value)).hexdigest()


def atomic_write_bytes(path: str | Path, data: bytes, temp_dir: str | Path | None = None) -> None:
    target = Path(path)
    target.parent.mkdir(parents=True, exist_ok=True)
    staging_dir = Path(temp_dir) if temp_dir is not None else target.parent
    staging_dir.mkdir(parents=True, exist_ok=True)
    descriptor, temporary_name = tempfile.mkstemp(
        prefix=f".{target.name}.",
        suffix=".tmp",
        dir=str(staging_dir),
    )
    temporary_path = Path(temporary_name)
    try:
        with os.fdopen(descriptor, "wb") as handle:
            handle.write(data)
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temporary_path, target)
        _fsync_directory(target.parent)
    finally:
        try:
            temporary_path.unlink()
        except FileNotFoundError:
            pass


def atomic_write_json(
    path: str | Path,
    value: Any,
    temp_dir: str | Path | None = None,
) -> None:
    atomic_write_bytes(path, canonical_json_bytes(value) + b"\n", temp_dir=temp_dir)


def ensure_writable_directory(path: str | Path) -> Path:
    directory = Path(path)
    directory.mkdir(parents=True, exist_ok=True)
    descriptor, temporary_name = tempfile.mkstemp(prefix=".write-check.", dir=str(directory))
    os.close(descriptor)
    Path(temporary_name).unlink()
    return directory.resolve()


@dataclass(frozen=True)
class SnapshotTile:
    index: int
    coord: tuple[int, int]
    box: PixelBox
    coverage: np.ndarray
    raw_sum: float
    quantized_sum: float
    quantization_error: float
    max_quantization_error: float
    sample_points: int
    memory_bytes: int
    content_hash: str
    prev_hash: str
    chain_hash: str

    def as_dict(self) -> dict[str, Any]:
        return {
            "index": self.index,
            "coord": list(self.coord),
            "box": [self.box.x, self.box.y, self.box.width, self.box.height],
            "sample_points": self.sample_points,
            "memory_bytes": self.memory_bytes,
            "raw_sum": self.raw_sum,
            "quantized_sum": self.quantized_sum,
            "quantization_error": self.quantization_error,
            "max_quantization_error": self.max_quantization_error,
            "content_hash": self.content_hash,
            "prev_hash": self.prev_hash,
            "chain_hash": self.chain_hash,
            "coverage_b64": base64.b64encode(np.ascontiguousarray(self.coverage, dtype=np.uint8).tobytes()).decode("ascii"),
        }


@dataclass(frozen=True)
class SnapshotState:
    tiles: tuple[SnapshotTile, ...]
    completed_indices: tuple[int, ...]
    head_hash: str
    outputs: dict[str, Any] | None

    @property
    def coverage_hash(self) -> str:
        return self.head_hash


class SnapshotStore:
    """One glyph/target snapshot; every committed tile rewrites atomically."""

    def __init__(
        self,
        path: str | Path,
        plan: GlyphRenderPlan,
        sampling: SamplingSpec,
        tile_width: int,
        tile_height: int,
        budget: dict[str, Any],
        font_sha256: str,
        temp_dir: str | Path | None = None,
    ) -> None:
        self.path = Path(path)
        self.plan = plan
        self.sampling = sampling
        self.tile_width = tile_width
        self.tile_height = tile_height
        self.budget = budget
        self.font_sha256 = font_sha256
        self.temp_dir = temp_dir
        self.tile_plan = plan.tile_boxes(tile_width, tile_height)
        self.signature = snapshot_signature(plan, sampling, tile_width, tile_height, font_sha256)

    def write(self, tiles: Iterable[SnapshotTile], outputs: dict[str, Any] | None = None) -> str:
        entries = list(tiles)
        head = entries[-1].chain_hash if entries else GENESIS_HASH
        document = {
            "format": SNAPSHOT_FORMAT,
            "version": SNAPSHOT_VERSION,
            "font_sha256": self.font_sha256,
            "signature": self.signature,
            "sampling": {
                "samples_x": self.sampling.samples_x,
                "samples_y": self.sampling.samples_y,
                "quantization_step": self.sampling.quantization_step,
            },
            "fill_rule": self.plan.fill_rule,
            "budget": self.budget,
            "tile_plan": [
                {
                    "index": index,
                    "coord": list(coord),
                    "box": [box.x, box.y, box.width, box.height],
                }
                for index, (coord, box) in enumerate(self.tile_plan)
            ],
            "completed_indices": [entry.index for entry in entries],
            "tiles": [entry.as_dict() for entry in entries],
            "head_hash": head,
            "outputs": outputs,
        }
        atomic_write_json(self.path, document, temp_dir=self.temp_dir)
        return head

    def load(self) -> SnapshotState:
        try:
            document = json.loads(self.path.read_text(encoding="utf-8"))
        except FileNotFoundError:
            fail(E_SNAPSHOT_MISMATCH, "Snapshot file does not exist", path=str(self.path))
        except (OSError, UnicodeDecodeError, json.JSONDecodeError) as exc:
            fail(E_SNAPSHOT_MISMATCH, "Snapshot cannot be read", path=str(self.path), error=str(exc))
        if not isinstance(document, dict):
            fail(E_SNAPSHOT_MISMATCH, "Snapshot root must be an object", path=str(self.path))
        if document.get("format") != SNAPSHOT_FORMAT or document.get("version") != SNAPSHOT_VERSION:
            fail(
                E_SNAPSHOT_MISMATCH,
                "Snapshot format or version mismatch",
                path=str(self.path),
                expected_format=SNAPSHOT_FORMAT,
                expected_version=SNAPSHOT_VERSION,
                actual_format=document.get("format"),
                actual_version=document.get("version"),
            )
        if document.get("font_sha256") != self.font_sha256:
            fail(E_SNAPSHOT_MISMATCH, "Snapshot font hash mismatch", path=str(self.path))
        if document.get("signature") != self.signature:
            fail(
                E_SNAPSHOT_MISMATCH,
                "Snapshot render signature mismatch",
                path=str(self.path),
                expected=self.signature,
                actual=document.get("signature"),
            )
        if document.get("sampling") != {
            "samples_x": self.sampling.samples_x,
            "samples_y": self.sampling.samples_y,
            "quantization_step": self.sampling.quantization_step,
        }:
            fail(E_SNAPSHOT_MISMATCH, "Snapshot sampling or quantization mismatch", path=str(self.path))
        if document.get("fill_rule") != self.plan.fill_rule:
            fail(E_SNAPSHOT_MISMATCH, "Snapshot fill rule mismatch", path=str(self.path))

        raw_tiles = document.get("tiles")
        if not isinstance(raw_tiles, list):
            fail(E_SNAPSHOT_MISMATCH, "Snapshot tiles must be an array", path=str(self.path))
        entries: list[SnapshotTile] = []
        previous = GENESIS_HASH
        for position, raw_entry in enumerate(raw_tiles):
            if not isinstance(raw_entry, dict):
                fail(E_SNAPSHOT_MISMATCH, "Snapshot tile entry must be an object", position=position)
            expected_index = position
            if raw_entry.get("index") != expected_index:
                fail(E_SNAPSHOT_MISMATCH, "Snapshot tile indices are not contiguous", position=position)
            if expected_index >= len(self.tile_plan):
                fail(E_SNAPSHOT_MISMATCH, "Snapshot contains more tiles than planned", position=position)
            expected_coord, expected_box = self.tile_plan[expected_index]
            if raw_entry.get("coord") != list(expected_coord):
                fail(E_SNAPSHOT_MISMATCH, "Snapshot tile coordinate mismatch", position=position)
            if raw_entry.get("box") != [expected_box.x, expected_box.y, expected_box.width, expected_box.height]:
                fail(E_SNAPSHOT_MISMATCH, "Snapshot tile box mismatch", position=position)
            if raw_entry.get("prev_hash") != previous:
                fail(E_SNAPSHOT_MISMATCH, "Snapshot hash chain is broken", position=position)
            try:
                coverage_bytes = base64.b64decode(raw_entry["coverage_b64"], validate=True)
            except (KeyError, ValueError) as exc:
                fail(E_SNAPSHOT_MISMATCH, "Snapshot tile coverage payload is missing", position=position, error=str(exc))
            expected_length = expected_box.area
            if len(coverage_bytes) != expected_length:
                fail(
                    E_SNAPSHOT_MISMATCH,
                    "Snapshot tile coverage length mismatch",
                    position=position,
                    expected=expected_length,
                    actual=len(coverage_bytes),
                )
            coverage = np.frombuffer(coverage_bytes, dtype=np.uint8).copy().reshape(
                expected_box.height,
                expected_box.width,
            )
            content_hash = coverage_sha256(coverage)
            if raw_entry.get("content_hash") != content_hash:
                fail(E_SNAPSHOT_MISMATCH, "Snapshot tile content hash mismatch", position=position)
            chain_payload = _chain_payload(raw_entry)
            expected_chain = _chain_hash(previous, chain_payload)
            if raw_entry.get("chain_hash") != expected_chain:
                fail(E_SNAPSHOT_MISMATCH, "Snapshot tile chain hash mismatch", position=position)
            entry = SnapshotTile(
                index=expected_index,
                coord=expected_coord,
                box=expected_box,
                coverage=coverage,
                raw_sum=float(raw_entry.get("raw_sum", 0.0)),
                quantized_sum=float(raw_entry.get("quantized_sum", 0.0)),
                quantization_error=float(raw_entry.get("quantization_error", 0.0)),
                max_quantization_error=float(raw_entry.get("max_quantization_error", 0.0)),
                sample_points=int(raw_entry.get("sample_points", 0)),
                memory_bytes=int(raw_entry.get("memory_bytes", 0)),
                content_hash=content_hash,
                prev_hash=previous,
                chain_hash=expected_chain,
            )
            entries.append(entry)
            previous = expected_chain

        completed = document.get("completed_indices")
        if completed != [entry.index for entry in entries]:
            fail(E_SNAPSHOT_MISMATCH, "Snapshot completed index list mismatch", path=str(self.path))
        if document.get("head_hash") != previous:
            fail(E_SNAPSHOT_MISMATCH, "Snapshot head hash mismatch", path=str(self.path))
        outputs = document.get("outputs")
        if outputs is not None and not isinstance(outputs, dict):
            fail(E_SNAPSHOT_MISMATCH, "Snapshot outputs section is invalid", path=str(self.path))
        if outputs is not None and "glyph_coverage_sha256" in outputs:
            assembled = np.zeros((self.plan.height, self.plan.width), dtype=np.uint8)
            for entry in entries:
                assembled[
                    entry.box.y : entry.box.y + entry.box.height,
                    entry.box.x : entry.box.x + entry.box.width,
                ] = entry.coverage
            actual_coverage_hash = coverage_sha256(assembled)
            if outputs.get("glyph_coverage_sha256") != actual_coverage_hash:
                fail(
                    E_SNAPSHOT_MISMATCH,
                    "Snapshot output coverage hash mismatch",
                    path=str(self.path),
                    expected=outputs.get("glyph_coverage_sha256"),
                    actual=actual_coverage_hash,
                )
        if outputs is not None and outputs.get("glyph_snapshot_head") != previous:
            fail(
                E_SNAPSHOT_MISMATCH,
                "Snapshot output head hash mismatch",
                path=str(self.path),
                expected=outputs.get("glyph_snapshot_head"),
                actual=previous,
            )
        return SnapshotState(
            tiles=tuple(entries),
            completed_indices=tuple(entry.index for entry in entries),
            head_hash=previous,
            outputs=outputs,
        )

    def attach_outputs(self, outputs: dict[str, Any]) -> None:
        state = self.load()
        self.write(state.tiles, outputs=outputs)


def make_snapshot_tile(
    index: int,
    raster: TileRaster,
    previous_hash: str,
) -> SnapshotTile:
    payload = {
        "index": index,
        "coord": list(raster.coord),
        "box": [raster.box.x, raster.box.y, raster.box.width, raster.box.height],
        "sample_points": raster.sample_points,
        "memory_bytes": raster.memory_bytes,
        "raw_sum": raster.raw_sum,
        "quantized_sum": raster.quantized_sum,
        "quantization_error": raster.quantization_error,
        "max_quantization_error": raster.max_quantization_error,
        "content_hash": raster.content_hash,
        "prev_hash": previous_hash,
    }
    return SnapshotTile(
        index=index,
        coord=raster.coord,
        box=raster.box,
        coverage=raster.coverage.copy(),
        raw_sum=raster.raw_sum,
        quantized_sum=raster.quantized_sum,
        quantization_error=raster.quantization_error,
        max_quantization_error=raster.max_quantization_error,
        sample_points=raster.sample_points,
        memory_bytes=raster.memory_bytes,
        content_hash=raster.content_hash,
        prev_hash=previous_hash,
        chain_hash=_chain_hash(previous_hash, payload),
    )


def snapshot_signature(
    plan: GlyphRenderPlan,
    sampling: SamplingSpec,
    tile_width: int,
    tile_height: int,
    font_sha256: str,
) -> dict[str, Any]:
    return {
        "font_sha256": font_sha256,
        "glyph": plan.signature(),
        "sampling": {
            "samples_x": sampling.samples_x,
            "samples_y": sampling.samples_y,
            "quantization_step": sampling.quantization_step,
        },
        "tile": {"width": tile_width, "height": tile_height},
        "fill_rule": plan.fill_rule,
    }


def snapshot_coverage_hash(tiles: Iterable[SnapshotTile], plan: GlyphRenderPlan) -> str:
    digest = hashlib.sha256()
    digest.update(canonical_json_bytes(plan.signature()))
    for tile in tiles:
        digest.update(bytes([tile.coord[0], tile.coord[1]]))
        digest.update(tile.coverage.tobytes(order="C"))
    return digest.hexdigest()


def _chain_payload(entry: dict[str, Any]) -> dict[str, Any]:
    payload = {
        key: entry.get(key)
        for key in (
            "index",
            "coord",
            "box",
            "sample_points",
            "memory_bytes",
            "raw_sum",
            "quantized_sum",
            "quantization_error",
            "max_quantization_error",
            "content_hash",
            "prev_hash",
        )
    }
    return payload


def _chain_hash(previous: str, payload: dict[str, Any]) -> str:
    return hashlib.sha256(previous.encode("ascii") + b"\0" + canonical_json_bytes(payload)).hexdigest()


def _fsync_directory(path: Path) -> None:
    if os.name == "nt":
        return
    try:
        descriptor = os.open(path, os.O_RDONLY)
    except OSError:
        return
    try:
        os.fsync(descriptor)
    finally:
        os.close(descriptor)

