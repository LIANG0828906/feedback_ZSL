from __future__ import annotations

import hashlib
import math
import os
import struct
from collections import OrderedDict
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

import numpy as np

from .errors import schema_error
from .util import canonical_json_bytes, sha256_bytes

ALGORITHM_VERSION = "terrain-bake-v1"
SNAPSHOT_FORMAT = "terrain-render-snapshot/1"
PROJECT_FORMAT = "openworld-terrain-project/1"
EDITS_FORMAT = "terrain-edits/1"
MAX_MATERIAL_LAYERS = 4
ABSOLUTE_MAX_WORKERS = 4


@dataclass(frozen=True)
class ScatterPoint:
    point_id: str
    kind: str
    layer: str
    x: float
    y: float
    scale: float
    rotation: float
    variant: float

    def to_dict(self) -> dict[str, Any]:
        return {
            "id": self.point_id,
            "kind": self.kind,
            "layer": self.layer,
            "x": self.x,
            "y": self.y,
            "scale": self.scale,
            "rotation": self.rotation,
            "variant": self.variant,
        }


@dataclass(frozen=True)
class HeightSample:
    x: int
    y: int
    height: float


@dataclass(frozen=True)
class Truth:
    height_samples: tuple[HeightSample, ...]
    material_coverage: dict[str, float]
    scatter_count: int


@dataclass
class TerrainTile:
    tile_id: str
    grid_x: int
    grid_y: int
    tile_size: int
    height: np.ndarray
    weights: np.ndarray
    scatter: tuple[ScatterPoint, ...]
    truth: Truth
    material_params: dict[str, float]
    source_paths: dict[str, str]
    content_hash: str = ""

    @property
    def height_shape(self) -> tuple[int, int]:
        return tuple(int(v) for v in self.height.shape)

    @property
    def weights_shape(self) -> tuple[int, int, int]:
        return tuple(int(v) for v in self.weights.shape)

    def recompute_content_hash(self, material_names: tuple[str, ...]) -> str:
        digest = hashlib.sha256()
        digest.update(b"terrain-tile-content-v1\0")
        digest.update(self.tile_id.encode("utf-8"))
        digest.update(
            struct.pack("<iiii", self.grid_x, self.grid_y, self.tile_size, len(material_names))
        )
        for name in material_names:
            encoded = name.encode("utf-8")
            digest.update(struct.pack("<I", len(encoded)))
            digest.update(encoded)
        contiguous_height = np.ascontiguousarray(self.height, dtype="<f8")
        digest.update(struct.pack("<Q", contiguous_height.size))
        digest.update(contiguous_height.tobytes(order="C"))
        contiguous_weights = np.ascontiguousarray(self.weights, dtype="u1")
        digest.update(struct.pack("<Q", contiguous_weights.size))
        digest.update(contiguous_weights.tobytes(order="C"))
        digest.update(canonical_json_bytes([p.to_dict() for p in self.scatter]))
        digest.update(
            canonical_json_bytes(
                {
                    name: _canonical_float(self.material_params.get(name, 1.0))
                    for name in material_names
                }
            )
        )
        self.content_hash = digest.hexdigest()
        return self.content_hash


def _canonical_float(value: float) -> float:
    value = float(value)
    if value == 0.0:
        return 0.0
    return value


@dataclass(frozen=True)
class BakeParams:
    subdivisions: int = 4
    normal_strength: float = 1.0
    min_layer_weight: float = 0.12
    max_vertices_per_thread: int = 262_144
    max_workers: int = 4

    def validate(self) -> None:
        if isinstance(self.subdivisions, bool) or not isinstance(self.subdivisions, int):
            raise schema_error("subdivisions must be an integer", field_name="subdivisions")
        if not 1 <= self.subdivisions <= 32:
            raise schema_error("subdivisions must be in [1, 32]", field_name="subdivisions")
        if not math.isfinite(self.normal_strength) or self.normal_strength <= 0.0:
            raise schema_error(
                "normal_strength must be finite and > 0", field_name="normal_strength"
            )
        if not math.isfinite(self.min_layer_weight) or not 0.0 <= self.min_layer_weight <= 1.0:
            raise schema_error(
                "min_layer_weight must be finite and in [0, 1]",
                field_name="min_layer_weight",
            )
        if isinstance(self.max_vertices_per_thread, bool) or not isinstance(
            self.max_vertices_per_thread, int
        ):
            raise schema_error(
                "max_vertices_per_thread must be an integer",
                field_name="max_vertices_per_thread",
            )
        if not 1 <= self.max_vertices_per_thread <= 16_777_216:
            raise schema_error(
                "max_vertices_per_thread must be in [1, 16777216]",
                field_name="max_vertices_per_thread",
            )
        if isinstance(self.max_workers, bool) or not isinstance(self.max_workers, int):
            raise schema_error("max_workers must be an integer", field_name="max_workers")
        if not 1 <= self.max_workers <= ABSOLUTE_MAX_WORKERS:
            raise schema_error(
                f"max_workers must be in [1, {ABSOLUTE_MAX_WORKERS}]",
                field_name="max_workers",
            )

    def output_dict(self) -> dict[str, Any]:
        """Parameters that affect bytes, excluding resource-only settings."""

        return {
            "algorithm": ALGORITHM_VERSION,
            "subdivisions": self.subdivisions,
            "normal_strength": _canonical_float(self.normal_strength),
            "min_layer_weight": _canonical_float(self.min_layer_weight),
        }

    @property
    def bake_version(self) -> str:
        digest = sha256_bytes(canonical_json_bytes(self.output_dict()))
        return f"{ALGORITHM_VERSION}:{digest[:16]}"

    def resource_dict(self) -> dict[str, Any]:
        return {
            "max_vertices_per_thread": self.max_vertices_per_thread,
            "requested_workers": self.max_workers,
            "effective_worker_cap": _effective_worker_cap(self.max_workers),
        }


def _effective_worker_cap(requested: int) -> int:
    cpu = os.cpu_count() or 1
    return max(1, min(int(requested), int(cpu), ABSOLUTE_MAX_WORKERS))


@dataclass
class TerrainProject:
    root: Path
    seed: int
    tile_size: int
    grid_tiles: tuple[int, int]
    material_names: tuple[str, ...]
    default_material_params: dict[str, float]
    tiles: OrderedDict[str, TerrainTile]
    manifest: dict[str, Any]
    default_subdivisions: int = 4
    has_edits: bool = False
    global_content_hash: str = ""
    height_edits: list[dict[str, Any]] = field(default_factory=list)
    material_edits: list[dict[str, Any]] = field(default_factory=list)

    def recompute_hashes(self) -> None:
        for tile in self.tiles.values():
            tile.recompute_content_hash(self.material_names)
        payload = {
            "format": PROJECT_FORMAT,
            "seed": self.seed,
            "tile_size": self.tile_size,
            "grid_tiles": list(self.grid_tiles),
            "materials": [
                {
                    "name": name,
                    "default_param": _canonical_float(self.default_material_params[name]),
                }
                for name in self.material_names
            ],
            "tiles": [
                {
                    "id": tile.tile_id,
                    "grid": [tile.grid_x, tile.grid_y],
                    "content_hash": tile.content_hash,
                }
                for tile in self.tiles.values()
            ],
        }
        self.global_content_hash = sha256_bytes(canonical_json_bytes(payload))

    def tile_at(self, gx: int, gy: int) -> TerrainTile | None:
        for tile in self.tiles.values():
            if tile.grid_x == gx and tile.grid_y == gy:
                return tile
        return None

    def neighbors(self, tile: TerrainTile) -> dict[str, TerrainTile]:
        result: dict[str, TerrainTile] = {}
        for direction, dx, dy in (("east", 1, 0), ("south", 0, 1)):
            neighbor = self.tile_at(tile.grid_x + dx, tile.grid_y + dy)
            if neighbor is not None:
                result[direction] = neighbor
        return result

    def apply_height_edit(
        self,
        tile_id: str,
        x: int,
        y: int,
        value: float,
        *,
        record: bool = True,
    ) -> tuple[str, ...]:
        tile = self._require_tile(tile_id)
        if not isinstance(x, int) or not isinstance(y, int):
            raise schema_error(
                "height edit coordinates must be integers",
                tile_id=tile_id,
                field_name="height_edit",
            )
        if not 0 <= x <= self.tile_size or not 0 <= y <= self.tile_size:
            raise schema_error(
                "height edit coordinates are outside the tile",
                tile_id=tile_id,
                field_name="height_edit",
            )
        value = float(value)
        if not math.isfinite(value):
            raise schema_error(
                "height edit value must be finite",
                tile_id=tile_id,
                field_name="height_edit",
            )
        world_x = tile.grid_x * self.tile_size + x
        world_y = tile.grid_y * self.tile_size + y
        changed = self._set_world_height(world_x, world_y, value)
        if record:
            self.height_edits.append(
                {
                    "tile": tile_id,
                    "x": int(x),
                    "y": int(y),
                    "value": _canonical_float(value),
                }
            )
            self.has_edits = True
        self.recompute_hashes()
        return tuple(sorted(changed))

    def apply_material_edit(
        self, tile_id: str, layer: str, value: float, *, record: bool = True
    ) -> None:
        tile = self._require_tile(tile_id)
        if layer not in self.material_names:
            raise schema_error(
                f"unknown material layer {layer!r}",
                tile_id=tile_id,
                field_name="material_edit.layer",
            )
        value = float(value)
        if not math.isfinite(value) or not 0.05 <= value <= 8.0:
            raise schema_error(
                "material parameter must be finite and in [0.05, 8]",
                tile_id=tile_id,
                field_name="material_edit.value",
            )
        tile.material_params[layer] = _canonical_float(value)
        if record:
            self.material_edits.append(
                {"tile": tile_id, "layer": layer, "value": _canonical_float(value)}
            )
            self.has_edits = True
        self.recompute_hashes()

    def _set_world_height(self, world_x: int, world_y: int, value: float) -> set[str]:
        changed: set[str] = set()
        for tile in self.tiles.values():
            x = world_x - tile.grid_x * self.tile_size
            y = world_y - tile.grid_y * self.tile_size
            if 0 <= x <= self.tile_size and 0 <= y <= self.tile_size:
                tile.height[y, x] = value
                changed.add(tile.tile_id)
        return changed

    def _require_tile(self, tile_id: str) -> TerrainTile:
        try:
            return self.tiles[tile_id]
        except KeyError as exc:
            raise schema_error("unknown tile id", tile_id=tile_id, field_name="tile_id") from exc

    def edits_dict(self) -> dict[str, Any]:
        return {
            "format": EDITS_FORMAT,
            "height_edits": list(self.height_edits),
            "material_edits": list(self.material_edits),
        }


@dataclass
class BakedTile:
    tile_id: str
    cache_key: str
    content_hash: str
    bake_version: str
    dependencies: tuple[dict[str, Any], ...]
    artifacts: dict[str, bytes]
    artifact_hashes: dict[str, str]
    block_manifest: dict[str, Any]
    block_manifest_bytes: bytes
    vertex_count: int
    triangle_count: int
    instance_count: int
    bounds: tuple[float, float, float, float, float, float]
    truth_check: dict[str, Any]


@dataclass
class SnapshotEntry:
    tile_id: str
    cache_key: str
    content_hash: str
    block_dir: str
    block_manifest_sha256: str
    artifact_hashes: dict[str, str]
    dependencies: tuple[dict[str, Any], ...]
    vertex_count: int
    instance_count: int
    bounds: tuple[float, float, float, float, float, float]

    def to_dict(self) -> dict[str, Any]:
        return {
            "tile_id": self.tile_id,
            "cache_key": self.cache_key,
            "content_hash": self.content_hash,
            "block_dir": self.block_dir,
            "block_manifest_sha256": self.block_manifest_sha256,
            "artifact_hashes": dict(sorted(self.artifact_hashes.items())),
            "dependencies": list(self.dependencies),
            "vertex_count": self.vertex_count,
            "instance_count": self.instance_count,
            "bounds": list(self.bounds),
        }


@dataclass
class InvalidationReport:
    trigger: list[dict[str, Any]]
    affected: list[str]
    reused: list[str]
    changed: list[str]
    graph_edges: list[dict[str, str]]
    reasons: dict[str, list[dict[str, str]]]
    topological_order: list[str]
    mode: str
    startup_reason: str | None = None

    def to_dict(self) -> dict[str, Any]:
        return {
            "format": "terrain-invalidation-report/1",
            "mode": self.mode,
            "trigger": list(self.trigger),
            "changed_tiles": list(self.changed),
            "affected_tiles": list(self.affected),
            "reused_tiles": list(self.reused),
            "graph_edges": list(self.graph_edges),
            "reasons": {key: list(value) for key, value in sorted(self.reasons.items())},
            "topological_order": list(self.topological_order),
            "startup_reason": self.startup_reason,
        }


@dataclass
class BakeResult:
    success: bool
    snapshot: dict[str, Any] | None
    invalidations: InvalidationReport
    rebuilt: list[str]
    reused: list[str]
    recovered: list[str]
    failed: dict[str, dict[str, Any]]
    timings_ms: dict[str, float]
    total_ms: float
    effective_workers: int
    startup_reason: str | None = None
    error: dict[str, Any] | None = None

    def diagnostics_dict(self) -> dict[str, Any]:
        return {
            "format": "terrain-run-diagnostics/1",
            "success": self.success,
            "startup_reason": self.startup_reason,
            "affected_tiles": list(self.invalidations.affected),
            "reused_tiles": list(self.reused),
            "rebuilt_tiles": list(self.rebuilt),
            "recovered_tiles": list(self.recovered),
            "failed": dict(sorted(self.failed.items())),
            "timings_ms": dict(sorted(self.timings_ms.items())),
            "total_ms": self.total_ms,
            "effective_workers": self.effective_workers,
            "error": self.error,
        }
