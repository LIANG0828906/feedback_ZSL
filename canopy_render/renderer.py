from __future__ import annotations

import os
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Callable

import numpy as np
from PIL import Image

from .camera import Camera
from .raster import LayerGeometry, build_layer_geometry, rasterize_layer
from .scene import scene_digest
from .shading import ShadedLayer, shade_layer
from .shadows import (
    projection_catalogue,
    projection_rule_text,
    render_wall_tile,
    shadow_measurements,
)
from .snapshot import (
    SnapshotRejected,
    SnapshotState,
    TileLayout,
    TileResult,
    load_snapshot,
    save_snapshot,
)
from .tables import TABLE_SIZE, material_tables
from .util import sha256_array, sha256_bytes, stable_round, write_json_atomic
from .validation import SceneData, ValidationIssue, validate_scene


class SceneValidationError(Exception):
    def __init__(self, issues: list[ValidationIssue]):
        super().__init__("scene validation failed")
        self.issues = issues


@dataclass
class RenderResult:
    status: str
    exit_code: int
    report: dict[str, Any]
    scene: SceneData
    layout: TileLayout


def _round_float(value: float, digits: int = 8) -> float:
    return stable_round(float(value), digits)


def _to_u8(values: np.ndarray) -> np.ndarray:
    return np.clip(np.floor(values * 255.0 + 0.5), 0, 255).astype(np.uint8)


def _layer_stats(layer: ShadedLayer) -> dict[str, Any]:
    return {
        "visible_fragments": int(layer.visible_fragments),
        "transmission_energy": float(layer.transmission_energy),
        "reflection_energy": float(layer.reflection_energy),
        "effective_transmittance_sum": float(layer.effective_transmittance_sum),
    }


def render_tile(
    scene: SceneData,
    camera: Camera,
    geometries: list[tuple[int, LayerGeometry]],
    layout: TileLayout,
    tile_index: int,
) -> TileResult:
    x0, y0, width, height = layout.rect(tile_index)
    rays = camera.rays(x0, y0, width, height)
    wall = render_wall_tile(scene, camera, x0, y0, width, height)

    output_rgb = np.empty((height, width, 3), dtype=np.float64)
    output_rgb[:] = scene.background_rgba[:3]
    output_alpha = np.full((height, width), scene.background_rgba[3], dtype=np.float64)
    output_rgb = (
        wall.rgb * wall.alpha[:, :, None]
        + output_rgb * (1.0 - wall.alpha[:, :, None])
    )
    output_alpha = wall.alpha + output_alpha * (1.0 - wall.alpha)

    owner = np.zeros((height, width), dtype=np.uint16)
    layer_rgba: dict[int, np.ndarray] = {}
    stats_layers: dict[str, dict[str, Any]] = {
        str(layer.layer_index): {
            "visible_fragments": 0,
            "transmission_energy": 0.0,
            "reflection_energy": 0.0,
            "effective_transmittance_sum": 0.0,
        }
        for layer in scene.layers
    }

    for layer_index, geometry in geometries:
        layer = next(
            layer for layer in scene.layers if layer.layer_index == layer_index
        )
        hits = rasterize_layer(camera, geometry, rays, x0, y0)
        shaded = shade_layer(scene, layer, hits, camera)
        premultiplied_u8 = _to_u8(shaded.premultiplied)
        premultiplied_u8[:, :, :3] = np.minimum(
            premultiplied_u8[:, :, :3],
            premultiplied_u8[:, :, 3:4],
        )
        layer_rgba[layer_index] = premultiplied_u8
        source_alpha = shaded.alpha[:, :, None]
        output_rgb = (
            shaded.premultiplied[:, :, :3]
            + output_rgb * (1.0 - source_alpha)
        )
        output_alpha = shaded.alpha + output_alpha * (1.0 - shaded.alpha)
        visible_owner = shaded.alpha > scene.owner_alpha_threshold
        owner[visible_owner] = np.uint16(layer.layer_index)

        tile_layer_stats = _layer_stats(shaded)
        destination = stats_layers[str(layer.layer_index)]
        destination["visible_fragments"] += tile_layer_stats["visible_fragments"]
        destination["transmission_energy"] += tile_layer_stats[
            "transmission_energy"
        ]
        destination["reflection_energy"] += tile_layer_stats["reflection_energy"]
        destination["effective_transmittance_sum"] += tile_layer_stats[
            "effective_transmittance_sum"
        ]

    fabric_rgb = np.zeros((height, width, 3), dtype=np.float64)
    fabric_alpha = np.zeros((height, width), dtype=np.float64)
    for layer_index, _ in geometries:
        source = layer_rgba[layer_index].astype(np.float64) / 255.0
        source_alpha = source[:, :, 3:4]
        fabric_rgb = source[:, :, :3] + fabric_rgb * (1.0 - source_alpha)
        fabric_alpha = source[:, :, 3] + fabric_alpha * (1.0 - source[:, :, 3])

    rgba = np.empty((height, width, 4), dtype=np.uint8)
    rgba[:, :, :3] = _to_u8(output_rgb)
    rgba[:, :, 3] = _to_u8(output_alpha)
    fabric = np.empty((height, width, 4), dtype=np.uint8)
    fabric[:, :, :3] = _to_u8(fabric_rgb)
    fabric[:, :, 3] = _to_u8(fabric_alpha)
    stats = {
        "layers": stats_layers,
        "shadow_rays_evaluated": int(wall.rays_evaluated),
        "fabric_pixels": int(np.count_nonzero(owner)),
    }
    return TileResult(
        tile_index=tile_index,
        rgba=rgba,
        owner=owner,
        fabric=fabric,
        layer_rgba=layer_rgba,
        stats=stats,
    )


def _stop_is_requested(
    stop_requested: Callable[[], bool] | None,
) -> bool:
    return bool(stop_requested()) if stop_requested is not None else False


def _aggregate_layer_stats(
    scene: SceneData,
    tiles: dict[int, TileResult],
) -> list[dict[str, Any]]:
    totals: list[dict[str, Any]] = []
    for layer in scene.layers:
        visible = 0
        transmission = 0.0
        reflection = 0.0
        effective_sum = 0.0
        for tile_index in sorted(tiles):
            item = tiles[tile_index].stats["layers"][str(layer.layer_index)]
            visible += int(item["visible_fragments"])
            transmission += float(item["transmission_energy"])
            reflection += float(item["reflection_energy"])
            effective_sum += float(item["effective_transmittance_sum"])
        total_energy = transmission + reflection
        tables = material_tables(layer.pattern_seed)
        totals.append(
            {
                "layer_index": layer.layer_index,
                "pattern_seed": layer.pattern_seed,
                "pattern_table_resolution": [TABLE_SIZE, TABLE_SIZE],
                "fold_normal_strength": _round_float(
                    scene.shading["fold_normal_strength"]
                ),
                "pattern_table_sha256": sha256_array(tables.pattern),
                "fold_normal_table_sha256": sha256_array(
                    np.stack((tables.normal_u, tables.normal_v), axis=0)
                ),
                "thickness": _round_float(layer.thickness),
                "transmission_reference_thickness": _round_float(
                    scene.shading["transmission_reference_thickness"]
                ),
                "material_transmittance": _round_float(layer.transmittance),
                "mean_effective_transmittance": (
                    _round_float(effective_sum / visible)
                    if visible
                    else 0.0
                ),
                "visible_fragments": visible,
                "transmission_energy": _round_float(transmission),
                "reflection_energy": _round_float(reflection),
                "transmission_ratio": (
                    _round_float(transmission / total_energy)
                    if total_energy > 0.0
                    else 0.0
                ),
                "reflection_ratio": (
                    _round_float(reflection / total_energy)
                    if total_energy > 0.0
                    else 0.0
                ),
            }
        )
    return totals


def _block_summaries(
    layout: TileLayout,
    tiles: dict[int, TileResult],
) -> list[dict[str, Any]]:
    summaries: list[dict[str, Any]] = []
    for tile_index in sorted(tiles):
        tile = tiles[tile_index]
        x0, y0, width, height = layout.rect(tile_index)
        summaries.append(
            {
                "tile_index": tile_index,
                "x": x0,
                "y": y0,
                "width": width,
                "height": height,
                "rgba_sha256": sha256_array(tile.rgba),
                "owner_sha256": sha256_array(tile.owner),
                "fabric_sha256": sha256_array(tile.fabric),
                "tile_color_sha256": tile.color_digest,
            }
        )
    return summaries


def _image_summary(
    rgba: np.ndarray,
    owner: np.ndarray,
    fabric: np.ndarray,
) -> dict[str, Any]:
    fabric_mask = owner > 0
    if np.any(fabric_mask):
        fabric_luminance = np.mean(
            rgba[:, :, :3][fabric_mask].astype(np.float64),
            axis=1,
        )
        dark_threshold = float(np.quantile(fabric_luminance, 0.25))
        dark_quartile = fabric_luminance[fabric_luminance <= dark_threshold]
        fabric_rgb_max = [
            int(value)
            for value in np.max(rgba[:, :, :3][fabric_mask], axis=0)
        ]
        fabric_luminance_stats = {
            "minimum": _round_float(float(np.min(fabric_luminance)) / 255.0, 6),
            "maximum": _round_float(float(np.max(fabric_luminance)) / 255.0, 6),
            "mean": _round_float(float(np.mean(fabric_luminance)) / 255.0, 6),
            "stddev": _round_float(float(np.std(fabric_luminance)) / 255.0, 6),
            "dark_quartile_stddev": _round_float(
                float(np.std(dark_quartile)) / 255.0,
                6,
            ),
        }
    else:
        fabric_rgb_max = [0, 0, 0]
        fabric_luminance_stats = {
            "minimum": 0.0,
            "maximum": 0.0,
            "mean": 0.0,
            "stddev": 0.0,
            "dark_quartile_stddev": 0.0,
        }
    return {
        "rgba_pixel_sha256": sha256_array(rgba),
        "owner_pixel_sha256": sha256_array(owner),
        "fabric_pixel_sha256": sha256_array(fabric),
        "mean_rgba": [
            _round_float(float(value) / 255.0, 6)
            for value in np.mean(rgba.reshape(-1, 4), axis=0)
        ],
        "fabric_pixel_count": int(np.count_nonzero(owner)),
        "fabric_rgb_max": fabric_rgb_max,
        "fabric_luminance": fabric_luminance_stats,
        "rgb_max": int(np.max(rgba[:, :, :3])),
        "rgb_min": int(np.min(rgba[:, :, :3])),
        "alpha_max": int(np.max(rgba[:, :, 3])),
        "alpha_min": int(np.min(rgba[:, :, 3])),
    }


def _assemble_arrays(
    layout: TileLayout,
    tiles: dict[int, TileResult],
) -> tuple[
    np.ndarray,
    np.ndarray,
    np.ndarray,
    dict[int, np.ndarray],
]:
    if set(tiles) != set(range(layout.total)):
        raise RuntimeError("cannot assemble an incomplete tile set")
    rgba = np.zeros((layout.height, layout.width, 4), dtype=np.uint8)
    owner = np.zeros((layout.height, layout.width), dtype=np.uint16)
    fabric = np.zeros((layout.height, layout.width, 4), dtype=np.uint8)
    layer_indices = sorted(tiles[0].layer_rgba)
    layers = {
        layer_index: np.zeros(
            (layout.height, layout.width, 4),
            dtype=np.uint8,
        )
        for layer_index in layer_indices
    }
    for tile_index in range(layout.total):
        x0, y0, width, height = layout.rect(tile_index)
        tile = tiles[tile_index]
        rgba[y0 : y0 + height, x0 : x0 + width] = tile.rgba
        owner[y0 : y0 + height, x0 : x0 + width] = tile.owner
        fabric[y0 : y0 + height, x0 : x0 + width] = tile.fabric
        for layer_index in layer_indices:
            layers[layer_index][
                y0 : y0 + height,
                x0 : x0 + width,
            ] = tile.layer_rgba[layer_index]
    return rgba, owner, fabric, layers


def _save_png(path: Path, array: np.ndarray) -> None:
    image = Image.fromarray(array)
    image.save(path, format="PNG", compress_level=6)


def _report_base(
    scene: SceneData,
    layout: TileLayout,
    tiles: dict[int, TileResult],
    *,
    status: str,
    business_code: str,
    recovery_count: int,
    snapshot_status: str,
    snapshot_rejection_reason: str | None,
) -> dict[str, Any]:
    return {
        "schema_version": "1.0",
        "status": status,
        "business_code": business_code,
        "scene_name": scene.canonical.get("scene_name"),
        "scene_digest": scene.digest,
        "canvas": {"width": scene.width, "height": scene.height},
        "tile_layout": {
            "width": layout.width,
            "height": layout.height,
            "tile_width": layout.tile_width,
            "tile_height": layout.tile_height,
            "tiles_x": layout.tiles_x,
            "tiles_y": layout.tiles_y,
            "total_tiles": layout.total,
            "completed_tile_indices": sorted(tiles),
            "completed_tile_count": len(tiles),
            "complete": len(tiles) == layout.total,
        },
        "recovery": {
            "recovery_count": int(recovery_count),
            "snapshot_status": snapshot_status,
            "snapshot_rejection_reason": snapshot_rejection_reason,
        },
        "block_summaries": _block_summaries(layout, tiles),
    }


def _complete_report(
    scene: SceneData,
    layout: TileLayout,
    tiles: dict[int, TileResult],
    camera: Camera,
    out_dir: Path,
    recovery_count: int,
    snapshot_status: str,
    snapshot_rejection_reason: str | None,
) -> dict[str, Any]:
    rgba, owner, fabric, layer_arrays = _assemble_arrays(layout, tiles)
    render_path = out_dir / "render.png"
    owner_path = out_dir / "ownership.png"
    fabric_path = out_dir / "fabric_composite.png"
    layer_files: list[dict[str, Any]] = []
    _save_png(render_path, rgba)
    _save_png(owner_path, owner)
    _save_png(fabric_path, fabric)
    for layer in scene.layers:
        filename = f"layer_{layer.layer_index:04d}_premultiplied.png"
        layer_path = out_dir / filename
        _save_png(layer_path, layer_arrays[layer.layer_index])
        layer_files.append(
            {
                "layer_index": layer.layer_index,
                "file": filename,
                "encoding": "premultiplied RGBA over transparent",
                "sha256": sha256_bytes(layer_path.read_bytes()),
                "pixel_sha256": sha256_array(layer_arrays[layer.layer_index]),
            }
        )

    report = _report_base(
        scene,
        layout,
        tiles,
        status="complete",
        business_code="OK",
        recovery_count=recovery_count,
        snapshot_status=snapshot_status,
        snapshot_rejection_reason=snapshot_rejection_reason,
    )
    report["image"] = {
        "file": "render.png",
        "width": layout.width,
        "height": layout.height,
        "sha256": sha256_bytes(render_path.read_bytes()),
    }
    report["ownership"] = {
        "file": "ownership.png",
        "encoding": "uint16 grayscale, 0 = no fabric, N = layer_index",
        "sha256": sha256_bytes(owner_path.read_bytes()),
    }
    report["fabric_composite"] = {
        "file": "fabric_composite.png",
        "encoding": "premultiplied RGBA over transparent",
        "sha256": sha256_bytes(fabric_path.read_bytes()),
    }
    report["premultiplied_layers"] = {
        "composition_order": [
            layer.layer_index for layer in scene.layers
        ],
        "files": layer_files,
    }
    report["transmission_model"] = {
        "model": "beer_lambert_path_length",
        "formula": (
            "T_effective = T_material ** "
            "((layer.thickness / reference_thickness) / "
            "max(abs(dot(normal, view)), min_cosine))"
        ),
        "reference_thickness": _round_float(
            scene.shading["transmission_reference_thickness"]
        ),
        "min_cosine": _round_float(scene.shading["min_cosine"]),
    }
    report["image_summary"] = _image_summary(rgba, owner, fabric)
    report["layers"] = _aggregate_layer_stats(scene, tiles)
    report["soft_shadows"] = {
        "samples_per_light": scene.shadows["samples_per_light"],
        "soft_shadow_sample_count": int(
            sum(tile.stats["shadow_rays_evaluated"] for tile in tiles.values())
        ),
        "sample_pattern": scene.shadows["sample_pattern"],
        "edge_scan_spacing_px": scene.shadows["edge_scan_spacing_px"],
        "edge_scan_max_px": scene.shadows["edge_scan_max_px"],
        "tassel_count": len(scene.tassels),
        "occluder_segment_count": sum(
            len(tassel.segments) for tassel in scene.tassels
        ),
        "rays_evaluated": int(
            sum(tile.stats["shadow_rays_evaluated"] for tile in tiles.values())
        ),
        "measurement_ambient": 0.16,
        "measurements": shadow_measurements(scene, camera),
    }
    projections = projection_catalogue(scene)
    report["projection"] = {
        "rule": projection_rule_text(),
        "coordinate_space": "wall origin + wall_uv[0]*u_axis + wall_uv[1]*v_axis",
        "count": len(projections),
        "points": projections,
    }
    return report


def _interrupted_report(
    scene: SceneData,
    layout: TileLayout,
    tiles: dict[int, TileResult],
    recovery_count: int,
    snapshot_status: str,
    snapshot_rejection_reason: str | None,
) -> dict[str, Any]:
    report = _report_base(
        scene,
        layout,
        tiles,
        status="interrupted",
        business_code="INTERRUPTED",
        recovery_count=recovery_count,
        snapshot_status=snapshot_status,
        snapshot_rejection_reason=snapshot_rejection_reason,
    )
    report["image"] = None
    report["ownership"] = None
    report["fabric_composite"] = None
    report["image_summary"] = None
    report["layers"] = _aggregate_layer_stats(scene, tiles)
    report["soft_shadows"] = {
        "samples_per_light": scene.shadows["samples_per_light"],
        "soft_shadow_sample_count": int(
            sum(tile.stats["shadow_rays_evaluated"] for tile in tiles.values())
        ),
        "sample_pattern": scene.shadows["sample_pattern"],
        "edge_scan_spacing_px": scene.shadows["edge_scan_spacing_px"],
        "edge_scan_max_px": scene.shadows["edge_scan_max_px"],
        "rays_evaluated": int(
            sum(tile.stats["shadow_rays_evaluated"] for tile in tiles.values())
        ),
        "measurements": [],
    }
    report["projection"] = {
        "rule": projection_rule_text(),
        "coordinate_space": "wall origin + wall_uv[0]*u_axis + wall_uv[1]*v_axis",
        "count": 0,
        "points": [],
    }
    return report


def render_scene(
    scene_value: dict[str, Any],
    out_dir: str | Path,
    *,
    snapshot_path: str | Path | None = None,
    stop_requested: Callable[[], bool] | None = None,
    interrupt_after_tiles: int | None = None,
    hard_kill_after_tiles: int | None = None,
) -> RenderResult:
    validation = validate_scene(scene_value)
    if not validation.ok or validation.data is None:
        raise SceneValidationError(validation.issues)
    scene = validation.data
    out_dir = Path(out_dir)
    out_dir.mkdir(parents=True, exist_ok=True)
    snapshot_path = (
        Path(snapshot_path)
        if snapshot_path is not None
        else out_dir / "snapshot.json"
    )
    layout = TileLayout(
        scene.width,
        scene.height,
        scene.tile_width,
        scene.tile_height,
    )

    snapshot_status = "fresh"
    snapshot_rejection_reason: str | None = None
    recovery_count = 0
    tiles: dict[int, TileResult] = {}
    if snapshot_path.exists():
        try:
            state: SnapshotState = load_snapshot(snapshot_path, scene, layout)
        except SnapshotRejected as exc:
            snapshot_status = "discarded"
            snapshot_rejection_reason = exc.reason
        else:
            tiles = state.tiles
            recovery_count = state.recovery_count + 1
            snapshot_status = "resumed"
    else:
        snapshot_rejection_reason = "snapshot_missing"

    camera = Camera.from_data(scene.camera, scene.width, scene.height)
    geometries = [
        (layer.layer_index, build_layer_geometry(layer))
        for layer in scene.layers
    ]

    graceful_stop = False
    for tile_index in range(layout.total):
        if tile_index in tiles:
            continue
        tile = render_tile(scene, camera, geometries, layout, tile_index)
        tiles[tile_index] = tile
        save_snapshot(
            snapshot_path,
            scene,
            layout,
            tiles,
            recovery_count,
        )
        if (
            hard_kill_after_tiles is not None
            and len(tiles) >= hard_kill_after_tiles
            and len(tiles) < layout.total
        ):
            os._exit(137)
        if len(tiles) < layout.total and (
            _stop_is_requested(stop_requested)
            or (
                interrupt_after_tiles is not None
                and len(tiles) >= interrupt_after_tiles
            )
        ):
            graceful_stop = True
            break

    if graceful_stop:
        report = _interrupted_report(
            scene,
            layout,
            tiles,
            recovery_count,
            snapshot_status,
            snapshot_rejection_reason,
        )
        write_json_atomic(out_dir / "recovery_report.json", report)
        return RenderResult(
            status="interrupted",
            exit_code=0,
            report=report,
            scene=scene,
            layout=layout,
        )

    if len(tiles) != layout.total:
        raise RuntimeError("render ended with an incomplete tile set")

    save_snapshot(
        snapshot_path,
        scene,
        layout,
        tiles,
        recovery_count,
    )
    report = _complete_report(
        scene,
        layout,
        tiles,
        camera,
        out_dir,
        recovery_count,
        snapshot_status,
        snapshot_rejection_reason,
    )
    write_json_atomic(out_dir / "render_report.json", report)
    recovery_report = out_dir / "recovery_report.json"
    if recovery_report.exists():
        recovery_report.unlink()
    return RenderResult(
        status="complete",
        exit_code=0,
        report=report,
        scene=scene,
        layout=layout,
    )

    fabric_rgb = np.zeros((height, width, 3), dtype=np.float64)
    fabric_alpha = np.zeros((height, width), dtype=np.float64)
    for layer_index, _ in geometries:
        source = layer_rgba[layer_index].astype(np.float64) / 255.0
        source_alpha = source[:, :, 3:4]
        fabric_rgb = source[:, :, :3] + fabric_rgb * (1.0 - source_alpha)
        fabric_alpha = source[:, :, 3] + fabric_alpha * (1.0 - source[:, :, 3])

