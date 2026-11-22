from __future__ import annotations

import json
import math
from collections import OrderedDict
from pathlib import Path
from typing import Any

import numpy as np

from .errors import decode_error, schema_error
from .model import (
    EDITS_FORMAT,
    MAX_MATERIAL_LAYERS,
    PROJECT_FORMAT,
    HeightSample,
    ScatterPoint,
    TerrainProject,
    TerrainTile,
    Truth,
)
from .util import atomic_write_json, read_json

SCATTER_FORMAT = "terrain-scatter/1"
TRUTH_FORMAT = "terrain-truth/1"
KNOWN_KINDS = {"tree", "bush", "rock", "prop"}


def load_project(project_dir: str | Path, edits_file: str | Path | None = None) -> TerrainProject:
    root = Path(project_dir).resolve()
    manifest_path = root / "manifest.json"
    manifest = _read_json(manifest_path, field_name="manifest")
    if not isinstance(manifest, dict):
        raise schema_error("manifest must be a JSON object", field_name="manifest")
    if manifest.get("format") != PROJECT_FORMAT:
        raise schema_error(
            f"unsupported project format; expected {PROJECT_FORMAT}",
            field_name="format",
        )

    seed = _require_int(manifest, "seed", field_name="seed")
    tile_size = _require_int(manifest, "tile_size", field_name="tile_size")
    if tile_size <= 0:
        raise schema_error("tile_size must be positive", field_name="tile_size")
    grid_tiles = _require(manifest, "grid_tiles", field_name="grid_tiles")
    if (
        not isinstance(grid_tiles, list)
        or len(grid_tiles) != 2
        or any(isinstance(v, bool) or not isinstance(v, int) or v <= 0 for v in grid_tiles)
    ):
        raise schema_error(
            "grid_tiles must be [positive_x, positive_y]",
            field_name="grid_tiles",
        )
    gx_count, gy_count = int(grid_tiles[0]), int(grid_tiles[1])
    default_subdivisions = 4
    if "subdivisions" in manifest:
        default_subdivisions = _require_int(manifest, "subdivisions", field_name="subdivisions")
        if not 1 <= default_subdivisions <= 32:
            raise schema_error("subdivisions must be in [1, 32]", field_name="subdivisions")

    materials_raw = _require(manifest, "materials", field_name="materials")
    if not isinstance(materials_raw, list) or not 1 <= len(materials_raw) <= MAX_MATERIAL_LAYERS:
        raise schema_error(
            f"materials must contain between 1 and {MAX_MATERIAL_LAYERS} layers",
            field_name="materials",
        )
    material_names: list[str] = []
    default_material_params: dict[str, float] = {}
    for index, material in enumerate(materials_raw):
        if not isinstance(material, dict):
            raise schema_error(
                "material entry must be an object",
                field_name=f"materials[{index}]",
            )
        name = material.get("name")
        if not isinstance(name, str) or not name:
            raise schema_error(
                "material name must be a non-empty string",
                field_name=f"materials[{index}].name",
            )
        if name in default_material_params:
            raise schema_error("duplicate material name", field_name=f"materials[{index}].name")
        default_param = float(material.get("default_param", 1.0))
        if not math.isfinite(default_param) or not 0.05 <= default_param <= 8.0:
            raise schema_error(
                "material default_param must be finite and in [0.05, 8]",
                field_name=f"materials[{index}].default_param",
            )
        material_names.append(name)
        default_material_params[name] = default_param

    tiles_raw = _require(manifest, "tiles", field_name="tiles")
    if not isinstance(tiles_raw, list):
        raise schema_error("tiles must be a list", field_name="tiles")
    expected_tile_count = gx_count * gy_count
    if len(tiles_raw) != expected_tile_count:
        raise schema_error(
            f"tiles length {len(tiles_raw)} does not match grid_tiles product {expected_tile_count}",
            field_name="tiles",
        )

    tiles: OrderedDict[str, TerrainTile] = OrderedDict()
    occupied: dict[tuple[int, int], str] = {}
    expected_height_shape = (tile_size + 1, tile_size + 1)
    expected_weight_shape = (tile_size + 1, tile_size + 1, len(material_names))

    for index, tile_raw in enumerate(tiles_raw):
        prefix = f"tiles[{index}]"
        if not isinstance(tile_raw, dict):
            raise schema_error("tile entry must be an object", field_name=prefix)
        tile_id_raw = tile_raw.get("id")
        if not isinstance(tile_id_raw, str) or not tile_id_raw:
            raise schema_error("tile id must be a non-empty string", field_name=f"{prefix}.id")
        tile_id = tile_id_raw
        if tile_id in tiles:
            raise schema_error("duplicate tile id", tile_id=tile_id, field_name=f"{prefix}.id")

        grid = _require(tile_raw, "grid", tile_id=tile_id, field_name=f"{prefix}.grid")
        if (
            not isinstance(grid, list)
            or len(grid) != 2
            or any(isinstance(v, bool) or not isinstance(v, int) for v in grid)
        ):
            raise schema_error(
                "tile grid must be [x, y] integers",
                tile_id=tile_id,
                field_name=f"{prefix}.grid",
            )
        grid_x, grid_y = int(grid[0]), int(grid[1])
        if not (0 <= grid_x < gx_count and 0 <= grid_y < gy_count):
            raise schema_error(
                "tile grid coordinate is outside grid_tiles",
                tile_id=tile_id,
                field_name=f"{prefix}.grid",
            )
        cell = (grid_x, grid_y)
        if cell in occupied:
            raise schema_error(
                f"tile grid coordinate duplicates {occupied[cell]}",
                tile_id=tile_id,
                field_name=f"{prefix}.grid",
            )
        occupied[cell] = tile_id

        height_rel = _require(
            tile_raw, "height_file", tile_id=tile_id, field_name=f"{prefix}.height_file"
        )
        weights_rel = _require(
            tile_raw,
            "weights_file",
            tile_id=tile_id,
            field_name=f"{prefix}.weights_file",
        )
        scatter_rel = _require(
            tile_raw,
            "scatter_file",
            tile_id=tile_id,
            field_name=f"{prefix}.scatter_file",
        )
        truth_rel = _require(
            tile_raw, "truth_file", tile_id=tile_id, field_name=f"{prefix}.truth_file"
        )
        height_path = _resolve_path(root, height_rel, tile_id, f"{prefix}.height_file")
        weights_path = _resolve_path(root, weights_rel, tile_id, f"{prefix}.weights_file")
        scatter_path = _resolve_path(root, scatter_rel, tile_id, f"{prefix}.scatter_file")
        truth_path = _resolve_path(root, truth_rel, tile_id, f"{prefix}.truth_file")

        declared_height_shape = _require(
            tile_raw,
            "height_shape",
            tile_id=tile_id,
            field_name=f"{prefix}.height_shape",
        )
        declared_weights_shape = _require(
            tile_raw,
            "weights_shape",
            tile_id=tile_id,
            field_name=f"{prefix}.weights_shape",
        )

        height = _load_npy(height_path, tile_id, "height")
        if not (
            np.issubdtype(height.dtype, np.floating) or np.issubdtype(height.dtype, np.integer)
        ):
            raise schema_error(
                f"height dtype must be numeric, got {height.dtype}",
                tile_id=tile_id,
                field_name="height.dtype",
            )
        height = np.ascontiguousarray(height, dtype=np.float64)
        if tuple(height.shape) != expected_height_shape:
            raise schema_error(
                f"height shape {tuple(height.shape)} does not match tile_size+1 {expected_height_shape}",
                tile_id=tile_id,
                field_name="height.shape",
            )
        if _shape_list(
            declared_height_shape,
            tile_id=tile_id,
            field_name=f"{prefix}.height_shape",
        ) != list(expected_height_shape):
            raise schema_error(
                f"declared height_shape {declared_height_shape} does not match expected {list(expected_height_shape)}",
                tile_id=tile_id,
                field_name="height_shape",
            )
        if not np.all(np.isfinite(height)):
            raise schema_error(
                "height contains non-finite values",
                tile_id=tile_id,
                field_name="height",
            )

        weights = _load_npy(weights_path, tile_id, "weights")
        if not np.issubdtype(weights.dtype, np.integer) or np.issubdtype(weights.dtype, np.bool_):
            raise schema_error(
                f"weights dtype must be quantized integer, got {weights.dtype}",
                tile_id=tile_id,
                field_name="weights.dtype",
            )
        if tuple(weights.shape) != expected_weight_shape:
            raise schema_error(
                f"weight shape {tuple(weights.shape)} does not match {expected_weight_shape}",
                tile_id=tile_id,
                field_name="weights.shape",
            )
        if _shape_list(
            declared_weights_shape,
            tile_id=tile_id,
            field_name=f"{prefix}.weights_shape",
        ) != list(expected_weight_shape):
            raise schema_error(
                f"declared weights_shape {declared_weights_shape} does not match expected {list(expected_weight_shape)}",
                tile_id=tile_id,
                field_name="weights_shape",
            )
        minimum = int(weights.min()) if weights.size else 0
        maximum = int(weights.max()) if weights.size else 0
        if minimum < 0 or maximum > 255:
            raise schema_error(
                f"weight values must be in [0, 255], got [{minimum}, {maximum}]",
                tile_id=tile_id,
                field_name="weights",
                details={"min": minimum, "max": maximum},
            )
        weights = np.ascontiguousarray(weights, dtype=np.uint8)

        material_params = dict(default_material_params)
        override = tile_raw.get("material_params", {})
        if not isinstance(override, dict):
            raise schema_error(
                "material_params must be an object",
                tile_id=tile_id,
                field_name=f"{prefix}.material_params",
            )
        for layer, value in override.items():
            if layer not in default_material_params:
                raise schema_error(
                    f"material_params contains unknown layer {layer!r}",
                    tile_id=tile_id,
                    field_name=f"{prefix}.material_params.{layer}",
                )
            try:
                numeric = float(value)
            except (TypeError, ValueError) as exc:
                raise schema_error(
                    "material parameter must be numeric",
                    tile_id=tile_id,
                    field_name=f"{prefix}.material_params.{layer}",
                ) from exc
            if not math.isfinite(numeric) or not 0.05 <= numeric <= 8.0:
                raise schema_error(
                    "material parameter must be finite and in [0.05, 8]",
                    tile_id=tile_id,
                    field_name=f"{prefix}.material_params.{layer}",
                )
            material_params[layer] = numeric

        scatter = _load_scatter(scatter_path, tile_id, material_names)
        truth = _load_truth(truth_path, tile_id, tile_size, material_names)

        tile = TerrainTile(
            tile_id=tile_id,
            grid_x=grid_x,
            grid_y=grid_y,
            tile_size=tile_size,
            height=height,
            weights=weights,
            scatter=scatter,
            truth=truth,
            material_params=material_params,
            source_paths={
                "height": height_path.as_posix(),
                "weights": weights_path.as_posix(),
                "scatter": scatter_path.as_posix(),
                "truth": truth_path.as_posix(),
            },
        )
        tiles[tile_id] = tile

    missing_cells = [
        [x, y] for y in range(gy_count) for x in range(gx_count) if (x, y) not in occupied
    ]
    if missing_cells:
        raise schema_error(
            "manifest does not provide one tile for every grid cell",
            field_name="tiles",
            details={"missing_cells": missing_cells},
        )

    project = TerrainProject(
        root=root,
        seed=seed,
        tile_size=tile_size,
        grid_tiles=(gx_count, gy_count),
        material_names=tuple(material_names),
        default_material_params=default_material_params,
        tiles=tiles,
        manifest=manifest,
        default_subdivisions=default_subdivisions,
    )
    project.recompute_hashes()

    if edits_file is not None and Path(edits_file).is_file():
        apply_edits_file(project, Path(edits_file))
    return project


def apply_edits_file(project: TerrainProject, edits_path: Path) -> None:
    payload = _read_json(edits_path, field_name="edits")
    if not isinstance(payload, dict):
        raise schema_error("edits must be a JSON object", field_name="edits")
    if payload.get("format") != EDITS_FORMAT:
        raise schema_error(
            f"unsupported edits format; expected {EDITS_FORMAT}",
            field_name="edits.format",
        )
    height_edits = payload.get("height_edits", [])
    material_edits = payload.get("material_edits", [])
    if not isinstance(height_edits, list):
        raise schema_error("height_edits must be a list", field_name="height_edits")
    if not isinstance(material_edits, list):
        raise schema_error("material_edits must be a list", field_name="material_edits")

    for index, edit in enumerate(height_edits):
        if not isinstance(edit, dict):
            raise schema_error("height edit must be an object", field_name=f"height_edits[{index}]")
        tile_id = edit.get("tile")
        if not isinstance(tile_id, str) or not tile_id:
            raise schema_error(
                "height edit tile must be a string",
                field_name=f"height_edits[{index}].tile",
            )
        try:
            x = int(edit["x"])
            y = int(edit["y"])
            value = float(edit["value"])
        except (KeyError, TypeError, ValueError) as exc:
            raise schema_error(
                "height edit requires integer x/y and numeric value",
                tile_id=tile_id,
                field_name=f"height_edits[{index}]",
            ) from exc
        project.apply_height_edit(tile_id, x, y, value, record=True)

    for index, edit in enumerate(material_edits):
        if not isinstance(edit, dict):
            raise schema_error(
                "material edit must be an object", field_name=f"material_edits[{index}]"
            )
        tile_id = edit.get("tile")
        layer = edit.get("layer")
        if not isinstance(tile_id, str) or not tile_id:
            raise schema_error(
                "material edit tile must be a string",
                field_name=f"material_edits[{index}].tile",
            )
        if not isinstance(layer, str) or not layer:
            raise schema_error(
                "material edit layer must be a string",
                field_name=f"material_edits[{index}].layer",
            )
        try:
            value = float(edit["value"])
        except (KeyError, TypeError, ValueError) as exc:
            raise schema_error(
                "material edit requires numeric value",
                tile_id=tile_id,
                field_name=f"material_edits[{index}].value",
            ) from exc
        project.apply_material_edit(tile_id, layer, value, record=True)


def save_edits(project: TerrainProject, edits_path: Path) -> None:
    atomic_write_json(Path(edits_path), project.edits_dict())


def _load_scatter(path: Path, tile_id: str, material_names: list[str]) -> tuple[ScatterPoint, ...]:
    payload = _read_json(path, tile_id=tile_id, field_name="scatter")
    if not isinstance(payload, dict):
        raise schema_error(
            "scatter file must contain an object", tile_id=tile_id, field_name="scatter"
        )
    if payload.get("format") != SCATTER_FORMAT:
        raise schema_error(
            f"unsupported scatter format; expected {SCATTER_FORMAT}",
            tile_id=tile_id,
            field_name="scatter.format",
        )
    points_raw = payload.get("points")
    if not isinstance(points_raw, list):
        raise schema_error(
            "scatter.points must be a list",
            tile_id=tile_id,
            field_name="scatter.points",
        )
    result: list[ScatterPoint] = []
    seen_ids: set[str] = set()
    for index, point in enumerate(points_raw):
        prefix = f"scatter.points[{index}]"
        if not isinstance(point, dict):
            raise schema_error(
                "scatter point must be an object", tile_id=tile_id, field_name=prefix
            )
        point_id = point.get("id")
        if not isinstance(point_id, str) or not point_id:
            raise schema_error(
                "scatter point id must be a string",
                tile_id=tile_id,
                field_name=f"{prefix}.id",
            )
        if point_id in seen_ids:
            raise schema_error(
                "duplicate scatter point id", tile_id=tile_id, field_name=f"{prefix}.id"
            )
        seen_ids.add(point_id)
        kind = point.get("kind")
        if kind not in KNOWN_KINDS:
            raise schema_error(
                f"unknown scatter kind {kind!r}",
                tile_id=tile_id,
                field_name=f"{prefix}.kind",
            )
        layer = point.get("layer")
        if layer not in material_names:
            raise schema_error(
                f"unknown scatter layer {layer!r}",
                tile_id=tile_id,
                field_name=f"{prefix}.layer",
            )
        try:
            x = float(point["x"])
            y = float(point["y"])
            scale = float(point.get("scale", 1.0))
            rotation = float(point.get("rotation", 0.0))
            variant = float(point.get("variant", 0.0))
        except (KeyError, TypeError, ValueError) as exc:
            raise schema_error(
                "scatter point requires numeric x/y and optional numeric scale/rotation/variant",
                tile_id=tile_id,
                field_name=prefix,
            ) from exc
        if not all(math.isfinite(v) for v in (x, y, scale, rotation, variant)):
            raise schema_error(
                "scatter point values must be finite",
                tile_id=tile_id,
                field_name=prefix,
            )
        if scale <= 0.0:
            raise schema_error(
                "scatter point scale must be > 0",
                tile_id=tile_id,
                field_name=f"{prefix}.scale",
            )
        result.append(
            ScatterPoint(
                point_id=point_id,
                kind=kind,
                layer=layer,
                x=x,
                y=y,
                scale=scale,
                rotation=rotation,
                variant=variant,
            )
        )
    result.sort(key=lambda p: p.point_id)
    return tuple(result)


def _load_truth(path: Path, tile_id: str, tile_size: int, material_names: list[str]) -> Truth:
    payload = _read_json(path, tile_id=tile_id, field_name="truth")
    if not isinstance(payload, dict):
        raise schema_error("truth file must contain an object", tile_id=tile_id, field_name="truth")
    if payload.get("format") != TRUTH_FORMAT:
        raise schema_error(
            f"unsupported truth format; expected {TRUTH_FORMAT}",
            tile_id=tile_id,
            field_name="truth.format",
        )
    samples_raw = payload.get("height_samples")
    if not isinstance(samples_raw, list) or not samples_raw:
        raise schema_error(
            "truth.height_samples must be a non-empty list",
            tile_id=tile_id,
            field_name="truth.height_samples",
        )
    samples: list[HeightSample] = []
    for index, sample in enumerate(samples_raw):
        if not isinstance(sample, dict):
            raise schema_error(
                "height sample must be an object",
                tile_id=tile_id,
                field_name=f"truth.height_samples[{index}]",
            )
        try:
            x = int(sample["x"])
            y = int(sample["y"])
            height = float(sample["height"])
        except (KeyError, TypeError, ValueError) as exc:
            raise schema_error(
                "height sample requires integer x/y and numeric height",
                tile_id=tile_id,
                field_name=f"truth.height_samples[{index}]",
            ) from exc
        if not 0 <= x <= tile_size or not 0 <= y <= tile_size:
            raise schema_error(
                "height sample is outside the tile",
                tile_id=tile_id,
                field_name=f"truth.height_samples[{index}]",
            )
        if not math.isfinite(height):
            raise schema_error(
                "height sample is not finite",
                tile_id=tile_id,
                field_name=f"truth.height_samples[{index}].height",
            )
        samples.append(HeightSample(x=x, y=y, height=height))
    coverage_raw = payload.get("material_coverage")
    if not isinstance(coverage_raw, dict):
        raise schema_error(
            "truth.material_coverage must be an object",
            tile_id=tile_id,
            field_name="truth.material_coverage",
        )
    coverage: dict[str, float] = {}
    for layer in material_names:
        if layer not in coverage_raw:
            raise schema_error(
                f"truth.material_coverage is missing layer {layer!r}",
                tile_id=tile_id,
                field_name=f"truth.material_coverage.{layer}",
            )
        value = float(coverage_raw[layer])
        if not math.isfinite(value) or not 0.0 <= value <= 1.0:
            raise schema_error(
                "material coverage must be finite and in [0, 1]",
                tile_id=tile_id,
                field_name=f"truth.material_coverage.{layer}",
            )
        coverage[layer] = value
    extra = sorted(set(coverage_raw) - set(material_names))
    if extra:
        raise schema_error(
            f"truth.material_coverage contains unknown layers {extra}",
            tile_id=tile_id,
            field_name="truth.material_coverage",
        )
    scatter_count_raw = payload.get("scatter_count")
    if isinstance(scatter_count_raw, bool) or not isinstance(scatter_count_raw, int):
        raise schema_error(
            "truth.scatter_count must be an integer",
            tile_id=tile_id,
            field_name="truth.scatter_count",
        )
    if scatter_count_raw < 0:
        raise schema_error(
            "truth.scatter_count must be >= 0",
            tile_id=tile_id,
            field_name="truth.scatter_count",
        )
    return Truth(
        height_samples=tuple(samples),
        material_coverage=coverage,
        scatter_count=int(scatter_count_raw),
    )


def _load_npy(path: Path, tile_id: str, field_name: str) -> np.ndarray:
    if not path.is_file():
        raise decode_error(
            "referenced terrain file is missing",
            tile_id=tile_id,
            field_name=f"{field_name}.file",
            details={"path": path.as_posix()},
        )
    try:
        array = np.load(path, allow_pickle=False)
    except (OSError, ValueError, EOFError) as exc:
        raise decode_error(
            "failed to decode NumPy terrain array",
            tile_id=tile_id,
            field_name=f"{field_name}.file",
            details={"path": path.as_posix(), "reason": type(exc).__name__},
        ) from exc
    if not isinstance(array, np.ndarray):
        raise decode_error(
            "NumPy terrain payload is not an array",
            tile_id=tile_id,
            field_name=f"{field_name}.file",
        )
    return array


def _read_json(
    path: Path,
    *,
    tile_id: str | None = None,
    field_name: str | None = None,
) -> Any:
    if not path.is_file():
        raise decode_error(
            "project JSON file is missing",
            tile_id=tile_id,
            field_name=field_name,
            details={"path": path.as_posix()},
        )
    try:
        return read_json(path)
    except (OSError, UnicodeDecodeError, json.JSONDecodeError) as exc:
        raise decode_error(
            "failed to decode project JSON",
            tile_id=tile_id,
            field_name=field_name,
            details={"path": path.as_posix(), "reason": type(exc).__name__},
        ) from exc


def _resolve_path(root: Path, value: Any, tile_id: str, field_name: str) -> Path:
    if not isinstance(value, str) or not value:
        raise schema_error(
            "path field must be a non-empty string",
            tile_id=tile_id,
            field_name=field_name,
        )
    candidate = Path(value)
    if candidate.is_absolute():
        resolved = candidate.resolve()
    else:
        resolved = (root / candidate).resolve()
    try:
        resolved.relative_to(root)
    except ValueError as exc:
        raise schema_error(
            "tile path must stay inside the project directory",
            tile_id=tile_id,
            field_name=field_name,
            details={"path": value},
        ) from exc
    return resolved


def _require(
    obj: dict[str, Any],
    key: str,
    *,
    tile_id: str | None = None,
    field_name: str,
) -> Any:
    if key not in obj:
        raise schema_error("missing required field", tile_id=tile_id, field_name=field_name)
    return obj[key]


def _require_int(
    obj: dict[str, Any],
    key: str,
    *,
    tile_id: str | None = None,
    field_name: str,
) -> int:
    value = _require(obj, key, tile_id=tile_id, field_name=field_name)
    if isinstance(value, bool) or not isinstance(value, int):
        raise schema_error("field must be an integer", tile_id=tile_id, field_name=field_name)
    return int(value)


def _shape_list(
    value: Any,
    *,
    tile_id: str | None,
    field_name: str,
) -> list[int]:
    if not isinstance(value, list) or any(
        isinstance(v, bool) or not isinstance(v, int) for v in value
    ):
        raise schema_error(
            "shape fields must be lists of integers",
            tile_id=tile_id,
            field_name=field_name,
        )
    return [int(v) for v in value]
