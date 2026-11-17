from __future__ import annotations

from dataclasses import dataclass
import hashlib
import io
import json
from pathlib import Path
import threading
import time
from typing import Any

import numpy as np
from PIL import Image

from . import RENDERER_VERSION
from .compare import compare_outputs
from .errors import OutputMismatchError, RenderError
from .renderer import Renderer, TileResult
from .scene import Scene, load_scene
from .snapshot import (
    atomic_write_bytes,
    atomic_write_json,
    completed_block_summary,
    load_snapshot,
    write_snapshot,
)


CHECKPOINT_NAME = "checkpoint.json"
RECOVERY_NAME = "recovery_report.json"
REPORT_NAME = "report.json"
PROJECTION_NAME = "projection_points.json"


@dataclass(frozen=True)
class RunResult:
    status: str
    scene_digest: str
    output_dir: Path
    completed_blocks: int
    tile_count: int
    report_path: Path | None


def _assemble(
    scene: Scene,
    tiles: dict[int, TileResult],
    coordinates: list[tuple[int, int, int, int]],
) -> tuple[np.ndarray, np.ndarray]:
    if len(tiles) != len(coordinates):
        raise RenderError(
            "INCOMPLETE_TILES",
            "cannot assemble an image with missing tiles",
            completed_blocks=len(tiles),
            tile_count=len(coordinates),
        )
    rgba = np.zeros((scene.height, scene.width, 4), dtype=np.uint8)
    layer_map = np.full((scene.height, scene.width), -1, dtype=np.int32)
    for index, coordinate in enumerate(coordinates):
        x, y, width, height = coordinate
        tile = tiles[index]
        rgba[y : y + height, x : x + width] = tile.rgba
        layer_map[y : y + height, x : x + width] = tile.layer_map
    return rgba, layer_map


def _png_bytes(image: Image.Image) -> bytes:
    buffer = io.BytesIO()
    image.save(buffer, format="PNG", compress_level=9, optimize=False)
    return buffer.getvalue()


def _npy_bytes(array: np.ndarray) -> bytes:
    buffer = io.BytesIO()
    np.save(buffer, np.ascontiguousarray(array), allow_pickle=False)
    return buffer.getvalue()


def _write_pixels(
    output: Path,
    rgba: np.ndarray,
    layer_map: np.ndarray,
    layer_indexes: list[int],
) -> dict[str, Any]:
    rgba_path = output / "render_rgba.npy"
    layer_path = output / "layer_map.npy"
    rgba_payload = _npy_bytes(rgba)
    layer_payload = _npy_bytes(layer_map)
    atomic_write_bytes(rgba_path, rgba_payload)
    atomic_write_bytes(layer_path, layer_payload)

    rgba_png = _png_bytes(Image.fromarray(rgba, mode="RGBA"))
    atomic_write_bytes(output / "render.png", rgba_png)

    png_codes = np.zeros(layer_map.shape, dtype=np.uint32)
    png_codes[layer_map == -2] = 1
    for layer_index in layer_indexes:
        png_codes[layer_map == layer_index] = min(65535, layer_index + 2)
    layer_png_rgba = np.zeros((layer_map.shape[0], layer_map.shape[1], 4), dtype=np.uint8)
    layer_png_rgba[..., 0] = (png_codes & 0xFF).astype(np.uint8)
    layer_png_rgba[..., 1] = ((png_codes >> 8) & 0xFF).astype(np.uint8)
    layer_png_rgba[..., 2] = np.where(png_codes > 0, 255, 0).astype(np.uint8)
    layer_png_rgba[..., 3] = 255
    layer_png = _png_bytes(Image.fromarray(layer_png_rgba, mode="RGBA"))
    atomic_write_bytes(output / "layer_map.png", layer_png)

    legend: dict[str, Any] = {
        "-2": "ground receiver",
        "-1": "background",
    }
    for layer_index in layer_indexes:
        legend[str(layer_index)] = f"layer {layer_index} (PNG code {layer_index + 2})"
    return {
        "rgba_sha256": hashlib.sha256(rgba.tobytes()).hexdigest(),
        "layer_map_sha256": hashlib.sha256(layer_map.tobytes()).hexdigest(),
        "render_png_sha256": hashlib.sha256(rgba_png).hexdigest(),
        "layer_map_png_sha256": hashlib.sha256(layer_png).hexdigest(),
        "render_rgba_npy_sha256": hashlib.sha256(rgba_payload).hexdigest(),
        "layer_map_npy_sha256": hashlib.sha256(layer_payload).hexdigest(),
        "png_codes": {
            "background": 0,
            "ground": 1,
            "layer": "layer_index + 2",
            "encoding": "layer_map.png: code = R + 256*G, occupancy byte B",
        },
        "legend": legend,
    }


def _write_textures(output: Path, renderer: Renderer) -> list[dict[str, Any]]:
    directory = output / "textures"
    directory.mkdir(parents=True, exist_ok=True)
    records: list[dict[str, Any]] = []
    for layer in renderer.scene.silk_layers:
        texture = renderer.textures[layer.layer_index]
        prefix = f"layer_{layer.layer_index:02d}_{layer.layer_id}"
        transmission_path = directory / f"{prefix}_transmission.npy"
        color_path = directory / f"{prefix}_color.npy"
        atomic_write_bytes(transmission_path, _npy_bytes(texture.transmission))
        atomic_write_bytes(color_path, _npy_bytes(texture.color))
        records.append(
            {
                "layer_index": layer.layer_index,
                "id": layer.layer_id,
                "pattern_seed": int(layer.material["pattern_seed"]),
                "texture_resolution": list(texture.resolution),
                "texture_digest": texture.digest,
                "transmission_file": str(transmission_path.relative_to(output)),
                "color_file": str(color_path.relative_to(output)),
            }
        )
    return records


def _penumbra_comparison(projection: dict[str, Any]) -> list[dict[str, Any]]:
    comparison: list[dict[str, Any]] = []
    for case in projection["cases"]:
        samples = case["edge_profile"]["samples"]
        if len(samples) > 5:
            indices = np.linspace(0, len(samples) - 1, 5, dtype=np.int64)
            selected = [samples[int(index)] for index in indices]
        else:
            selected = samples
        comparison.append(
            {
                "case_id": case["case_id"],
                "target_kind": case["target_kind"],
                "receiver_distance": case["receiver_distance"],
                "light_distance": case["light_distance"],
                "penumbra_width": case["penumbra_width"],
                "transition_width_10_90": case["edge_profile"][
                    "transition_width_10_90"
                ],
                "brightness_samples": [
                    {
                        "offset": sample["offset"],
                        "brightness": sample["brightness"],
                    }
                    for sample in selected
                ],
            }
        )
    return comparison


def _write_final_outputs(
    scene: Scene,
    renderer: Renderer,
    output: Path,
    tiles: dict[int, TileResult],
    coordinates: list[tuple[int, int, int, int]],
) -> Path:
    rgba, layer_map = _assemble(scene, tiles, coordinates)
    pixel_summary = _write_pixels(
        output,
        rgba,
        layer_map,
        [layer.layer_index for layer in scene.layers],
    )
    textures = _write_textures(output, renderer)

    projection_payload = json.dumps(
        renderer.projection,
        indent=2,
        sort_keys=True,
        ensure_ascii=False,
        allow_nan=False,
    ).encode("utf-8") + b"\n"
    atomic_write_bytes(output / PROJECTION_NAME, projection_payload)

    layer_stats, silk_shadow_evaluations, ground_shadow_evaluations = (
        renderer.layer_statistics()
    )
    projection = renderer.projection
    shadow_cases = projection["cases"]
    tile_columns = (scene.width + scene.tile_size - 1) // scene.tile_size
    tile_rows = (scene.height + scene.tile_size - 1) // scene.tile_size

    report = {
        "format_version": 1,
        "renderer_version": RENDERER_VERSION,
        "scene_digest": scene.digest,
        "image": {
            "width": scene.width,
            "height": scene.height,
            "tile_size": scene.tile_size,
            "tile_grid": [tile_columns, tile_rows],
            "tile_count": len(coordinates),
            **pixel_summary,
        },
        "layer_map_legend": pixel_summary["legend"],
        "layers": layer_stats,
        "textures": textures,
        "shadows": {
            "sample_density": scene.shadow.sample_density,
            "sample_density_semantics": (
                "Controls deterministic quantisation of the soft-edge ramp and "
                "the number of points in each reported edge profile."
            ),
            "projection_case_count": projection["projection_case_count"],
            "projected_triangle_count": projection["projected_triangle_count"],
            "in_bounds_triangle_count": projection["in_bounds_triangle_count"],
            "mullion_projection_sample_points": projection[
                "edge_profile_sample_points"
            ],
            "silk_shadow_receiver_sample_points": silk_shadow_evaluations,
            "ground_shadow_receiver_sample_points": ground_shadow_evaluations,
            "penumbra_widths": [case["penumbra_width"] for case in shadow_cases],
            "edge_transition_widths_10_90": [
                case["edge_profile"]["transition_width_10_90"]
                for case in shadow_cases
            ],
            "penumbra_trend": projection["penumbra_trend"],
            "edge_brightness_comparison": _penumbra_comparison(projection),
        },
        "projection_points_file": PROJECTION_NAME,
        "projection_points_sha256": hashlib.sha256(projection_payload).hexdigest(),
        "tile_size": scene.tile_size,
        "tile_commit_policy": (
            "row-major prefix; every committed tile is atomically stored with its "
            "scene digest, coordinates, RGBA/layer digests, and base64 payload"
        ),
        "resume_policy": (
            "only an exact contiguous prefix with matching scene/renderer digests "
            "is reused; any corrupt or mismatched snapshot is discarded wholesale"
        ),
        "alpha_mode": "display-space premultiplied RGBA",
        "final_image_sha256": pixel_summary["rgba_sha256"],
        "final_layer_map_sha256": pixel_summary["layer_map_sha256"],
    }
    report_path = output / REPORT_NAME
    atomic_write_json(report_path, report)
    return report_path


def _write_recovery(
    output: Path,
    scene: Scene,
    tiles: dict[int, TileResult],
    coordinates: list[tuple[int, int, int, int]],
    status: str,
    resume_status: str,
    resume_reason: str | None,
) -> None:
    payload = {
        "status": status,
        "scene_digest": scene.digest,
        "completed_count": len(tiles),
        "tile_count": len(coordinates),
        "next_block": len(tiles),
        "completed_blocks": completed_block_summary(tiles, coordinates),
        "snapshot": CHECKPOINT_NAME,
        "resume_status": resume_status,
        "resume_reason": resume_reason,
    }
    if status == "interrupted":
        payload["code"] = "INTERRUPTED_GRACEFULLY"
        payload["exit_code"] = 0
        payload["message"] = (
            "The current tile was committed after the interrupt request; "
            "rerun the same command to resume from this checkpoint."
        )
    else:
        payload["code"] = "COMPLETE"
        payload["message"] = "All tiles and final outputs are complete."
    atomic_write_json(output / RECOVERY_NAME, payload)


def run_render(
    scene_path: str | Path,
    output_dir: str | Path,
    *,
    fresh: bool = False,
    stop_event: threading.Event | None = None,
    block_delay_ms: int = 0,
    verify_baseline: str | Path | None = None,
) -> RunResult:
    started = time.perf_counter()
    scene = load_scene(scene_path)
    output = Path(output_dir)
    output.mkdir(parents=True, exist_ok=True)
    renderer = Renderer(scene)
    coordinates = renderer.tile_coordinates()
    checkpoint_path = output / CHECKPOINT_NAME

    if fresh:
        tiles: dict[int, TileResult] = {}
        resume_status = "fresh"
        resume_reason = "fresh run requested"
    else:
        loaded = load_snapshot(checkpoint_path, scene, coordinates)
        tiles = loaded.tiles
        resume_status = loaded.status
        resume_reason = loaded.reason
    initial_completed = len(tiles)

    if stop_event is not None and stop_event.is_set() and len(tiles) < len(coordinates):
        write_snapshot(checkpoint_path, scene, tiles, coordinates, complete=False)
        _write_recovery(
            output,
            scene,
            tiles,
            coordinates,
            "interrupted",
            resume_status,
            resume_reason,
        )
        return RunResult(
            status="interrupted",
            scene_digest=scene.digest,
            output_dir=output,
            completed_blocks=len(tiles),
            tile_count=len(coordinates),
            report_path=None,
        )

    for index in range(len(tiles), len(coordinates)):
        x, y, width, height = coordinates[index]
        tile = renderer.render_tile(x, y, width, height)
        if block_delay_ms > 0:
            time.sleep(block_delay_ms / 1000.0)
        tiles[index] = tile
        write_snapshot(checkpoint_path, scene, tiles, coordinates, complete=False)
        if stop_event is not None and stop_event.is_set() and len(tiles) < len(coordinates):
            _write_recovery(
                output,
                scene,
                tiles,
                coordinates,
                "interrupted",
                resume_status,
                resume_reason,
            )
            return RunResult(
                status="interrupted",
                scene_digest=scene.digest,
                output_dir=output,
                completed_blocks=len(tiles),
                tile_count=len(coordinates),
                report_path=None,
            )

    write_snapshot(checkpoint_path, scene, tiles, coordinates, complete=True)
    report_path = _write_final_outputs(
        scene, renderer, output, tiles, coordinates
    )
    _write_recovery(
        output,
        scene,
        tiles,
        coordinates,
        "complete",
        resume_status,
        resume_reason,
    )
    elapsed = time.perf_counter() - started
    atomic_write_json(
        output / "timing.json",
        {
            "elapsed_seconds": round(elapsed, 6),
            "blocks_rendered_this_process": len(coordinates) - initial_completed,
        },
    )

    if verify_baseline is not None:
        differences = compare_outputs(verify_baseline, output)
        if differences:
            raise OutputMismatchError(
                "completed output differs from the uninterrupted baseline",
                differences,
            )

    return RunResult(
        status="complete",
        scene_digest=scene.digest,
        output_dir=output,
        completed_blocks=len(tiles),
        tile_count=len(coordinates),
        report_path=report_path,
    )
