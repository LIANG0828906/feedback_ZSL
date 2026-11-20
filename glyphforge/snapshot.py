from __future__ import annotations

import base64
import math
import threading
from dataclasses import dataclass
from pathlib import Path
from typing import Any

import numpy as np

from .errors import ErrorCode, GlyphForgeError
from .raster import CoverageSummary, TileCoverage, coverage_content_hash
from .util import atomic_write_json, read_json, sha256_json

SNAPSHOT_FORMAT = "glyphforge.snapshot/1"
SNAPSHOT_VERSION = 1


def _snapshot_error(message: str, **details: Any) -> GlyphForgeError:
    return GlyphForgeError(ErrorCode.E_SNAPSHOT_MISMATCH, message, details)


def _tile_key(tile: dict[str, Any]) -> str:
    return f"{tile['glyph']}:{int(tile['row'])}:{int(tile['col'])}"


def _summary_to_dict(summary: CoverageSummary) -> dict[str, Any]:
    return {
        "samples": summary.samples,
        "pixels": summary.pixels,
        "raw_coverage_sum": summary.raw_coverage_sum,
        "quantized_coverage_sum": summary.quantized_coverage_sum,
        "quantization_abs_error": summary.quantization_abs_error,
        "quantization_squared_error": summary.quantization_squared_error,
        "max_abs_error": summary.max_abs_error,
        "quant_step": summary.quant_step,
    }


def _summary_from_dict(value: Any) -> CoverageSummary:
    if not isinstance(value, dict):
        raise _snapshot_error("snapshot tile summary is missing")
    try:
        return CoverageSummary(
            samples=int(value["samples"]),
            pixels=int(value["pixels"]),
            raw_coverage_sum=float(value["raw_coverage_sum"]),
            quantized_coverage_sum=float(value["quantized_coverage_sum"]),
            quantization_abs_error=float(value["quantization_abs_error"]),
            quantization_squared_error=float(value["quantization_squared_error"]),
            max_abs_error=float(value["max_abs_error"]),
            quant_step=float(value["quant_step"]),
        )
    except (KeyError, TypeError, ValueError) as exc:
        raise _snapshot_error("snapshot tile summary is invalid", error=str(exc)) from None


def _chain_payload(run_config: dict[str, Any], tile: dict[str, Any]) -> dict[str, Any]:
    return {
        "run_config": run_config,
        "tile": {
            "glyph": tile["glyph"],
            "row": int(tile["row"]),
            "col": int(tile["col"]),
            "x": int(tile["x"]),
            "y": int(tile["y"]),
            "width": int(tile["width"]),
            "height": int(tile["height"]),
            "batch_size": int(tile["batch_size"]),
            "summary": tile["summary"],
            "content_hash": tile["content_hash"],
        },
    }


def _ordered_tiles(tiles: list[dict[str, Any]], glyph_order: list[str]) -> list[dict[str, Any]]:
    glyph_rank = {name: index for index, name in enumerate(glyph_order)}

    def key(tile: dict[str, Any]) -> tuple[int, int, int, str]:
        return (
            glyph_rank.get(str(tile.get("glyph")), 1 << 30),
            int(tile.get("row", 0)),
            int(tile.get("col", 0)),
            str(tile.get("glyph", "")),
        )

    return sorted(tiles, key=key)


def _build_chain(
    run_config: dict[str, Any],
    tiles: list[dict[str, Any]],
    glyph_order: list[str],
) -> tuple[list[dict[str, Any]], str]:
    ordered = _ordered_tiles(tiles, glyph_order)
    previous = sha256_json(
        {
            "format": SNAPSHOT_FORMAT,
            "version": SNAPSHOT_VERSION,
            "run_config": run_config,
        }
    )
    chain: list[dict[str, Any]] = []
    for index, tile in enumerate(ordered):
        payload = _chain_payload(run_config, tile)
        chain_hash = sha256_json({"previous": previous, "payload": payload})
        chain.append(
            {
                "index": index,
                "key": _tile_key(tile),
                "content_hash": tile["content_hash"],
                "chain_hash": chain_hash,
            }
        )
        previous = chain_hash
    return chain, previous


@dataclass
class SnapshotData:
    path: str
    run_config: dict[str, Any]
    tiles: dict[str, TileCoverage]
    tile_records: dict[str, dict[str, Any]]
    completed_indices: list[int]
    chain_hash: str


def _tile_from_record(record: Any) -> TileCoverage:
    if not isinstance(record, dict):
        raise _snapshot_error("snapshot tile record is not an object")
    required = (
        "glyph",
        "row",
        "col",
        "x",
        "y",
        "width",
        "height",
        "batch_size",
        "summary",
        "coverage_b64",
        "content_hash",
    )
    missing = [name for name in required if name not in record]
    if missing:
        raise _snapshot_error("snapshot tile record is incomplete", missing=missing)
    try:
        width = int(record["width"])
        height = int(record["height"])
        raw = base64.b64decode(record["coverage_b64"], validate=True)
    except (TypeError, ValueError) as exc:
        raise _snapshot_error("snapshot coverage encoding is invalid", error=str(exc)) from None
    if width < 0 or height < 0 or len(raw) != width * height:
        raise _snapshot_error(
            "snapshot coverage byte count does not match tile dimensions",
            glyph=record["glyph"],
            row=record["row"],
            col=record["col"],
        )
    coverage = np.frombuffer(raw, dtype=np.uint8).reshape(height, width).copy()
    actual_hash = coverage_content_hash(coverage)
    if actual_hash != record["content_hash"]:
        raise _snapshot_error(
            "snapshot tile content hash mismatch",
            glyph=record["glyph"],
            row=record["row"],
            col=record["col"],
            expected=record["content_hash"],
            actual=actual_hash,
        )
    return TileCoverage(
        glyph_name=str(record["glyph"]),
        row=int(record["row"]),
        col=int(record["col"]),
        x=int(record["x"]),
        y=int(record["y"]),
        width=width,
        height=height,
        coverage=coverage,
        summary=_summary_from_dict(record["summary"]),
        batch_size=int(record["batch_size"]),
        content_hash=actual_hash,
    )


def load_snapshot(path: str | Path) -> SnapshotData:
    source = Path(path)
    try:
        document = read_json(source)
    except FileNotFoundError:
        raise _snapshot_error("snapshot file does not exist", path=str(source)) from None
    except (OSError, UnicodeError, ValueError) as exc:
        raise _snapshot_error("snapshot file is not valid JSON", path=str(source), error=str(exc)) from None
    if not isinstance(document, dict):
        raise _snapshot_error("snapshot root must be an object")
    if document.get("format") != SNAPSHOT_FORMAT:
        raise _snapshot_error("snapshot format is unsupported", format=document.get("format"))
    if document.get("version") != SNAPSHOT_VERSION:
        raise _snapshot_error(
            "snapshot version is unsupported",
            expected=SNAPSHOT_VERSION,
            actual=document.get("version"),
        )
    run_config = document.get("run_config")
    if not isinstance(run_config, dict):
        raise _snapshot_error("snapshot run configuration is missing")
    glyph_order = run_config.get("glyph_order")
    if not isinstance(glyph_order, list) or not all(
        isinstance(name, str) for name in glyph_order
    ):
        raise _snapshot_error("snapshot glyph order is invalid")
    records = document.get("tiles")
    if not isinstance(records, list):
        raise _snapshot_error("snapshot tile table is missing")
    tiles: dict[str, TileCoverage] = {}
    record_map: dict[str, dict[str, Any]] = {}
    for record in records:
        tile = _tile_from_record(record)
        key = f"{tile.glyph_name}:{tile.row}:{tile.col}"
        if key in tiles:
            raise _snapshot_error("snapshot contains a duplicate tile", key=key)
        tiles[key] = tile
        record_map[key] = record

    expected_chain, expected_chain_hash = _build_chain(
        run_config, records, glyph_order
    )
    if document.get("hash_chain") != expected_chain:
        raise _snapshot_error("snapshot hash chain mismatch")
    if document.get("chain_hash") != expected_chain_hash:
        raise _snapshot_error("snapshot terminal hash mismatch")
    completed_indices = document.get("completed_indices")
    expected_indices = [entry["index"] for entry in expected_chain]
    if completed_indices != expected_indices:
        raise _snapshot_error(
            "snapshot completed tile index table mismatch",
            expected=expected_indices,
            actual=completed_indices,
        )
    if document.get("tile_count") != len(records):
        raise _snapshot_error("snapshot tile count mismatch")
    return SnapshotData(
        path=str(source),
        run_config=run_config,
        tiles=tiles,
        tile_records=record_map,
        completed_indices=completed_indices,
        chain_hash=expected_chain_hash,
    )


def _tile_to_record(tile: TileCoverage) -> dict[str, Any]:
    return {
        "glyph": tile.glyph_name,
        "row": int(tile.row),
        "col": int(tile.col),
        "x": int(tile.x),
        "y": int(tile.y),
        "width": int(tile.width),
        "height": int(tile.height),
        "batch_size": int(tile.batch_size),
        "summary": _summary_to_dict(tile.summary),
        "coverage_b64": base64.b64encode(tile.coverage.tobytes()).decode("ascii"),
        "content_hash": tile.content_hash,
    }


class SnapshotStore:
    def __init__(
        self,
        path: str | Path,
        run_config: dict[str, Any],
        *,
        initial_tiles: dict[str, TileCoverage] | None = None,
    ):
        self.path = Path(path)
        self.run_config = run_config
        self._glyph_order = list(run_config["glyph_order"])
        self._tiles: dict[str, TileCoverage] = dict(initial_tiles or {})
        self._lock = threading.RLock()
        self.save()

    def _document(self) -> dict[str, Any]:
        records = [_tile_to_record(tile) for tile in self._tiles.values()]
        chain, chain_hash = _build_chain(
            self.run_config, records, self._glyph_order
        )
        return {
            "format": SNAPSHOT_FORMAT,
            "version": SNAPSHOT_VERSION,
            "run_config": self.run_config,
            "tile_count": len(records),
            "tiles": records,
            "completed_indices": [entry["index"] for entry in chain],
            "hash_chain": chain,
            "chain_hash": chain_hash,
        }

    def save(self) -> None:
        with self._lock:
            document = self._document()
            atomic_write_json(self.path, document)

    def add_tile(self, tile: TileCoverage) -> None:
        key = f"{tile.glyph_name}:{tile.row}:{tile.col}"
        with self._lock:
            existing = self._tiles.get(key)
            if existing is not None:
                if existing.content_hash != tile.content_hash:
                    raise _snapshot_error(
                        "refusing to overwrite a completed tile with different content",
                        key=key,
                    )
                return
            self._tiles[key] = tile
            document = self._document()
            atomic_write_json(self.path, document)

    def get(self, key: str) -> TileCoverage | None:
        with self._lock:
            return self._tiles.get(key)

    @property
    def completed_count(self) -> int:
        with self._lock:
            return len(self._tiles)

    @property
    def tiles(self) -> dict[str, TileCoverage]:
        with self._lock:
            return dict(self._tiles)

    @property
    def chain_hash(self) -> str:
        with self._lock:
            _, chain_hash = _build_chain(
                self.run_config,
                [_tile_to_record(tile) for tile in self._tiles.values()],
                self._glyph_order,
            )
            return chain_hash


def snapshot_is_finite(value: Any) -> bool:
    if isinstance(value, float):
        return math.isfinite(value)
    if isinstance(value, dict):
        return all(snapshot_is_finite(item) for item in value.values())
    if isinstance(value, list):
        return all(snapshot_is_finite(item) for item in value)
    return True
