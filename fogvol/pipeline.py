"""Chunked rendering, resumable snapshots, verification, and reporting."""

from __future__ import annotations

import json
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Callable

import numpy as np

from .imageio import (
    atomic_write_bytes,
    npy_bytes,
    png_bytes_gray16,
    png_bytes_rgba,
    rgba8,
)
from .render import (
    RAY_START_RULE,
    VolumetricRenderer,
    chunk_id_for_pixel,
    iter_chunks,
)
from .scene import Scene
from .snapshot import (
    SNAPSHOT_NAME,
    SnapshotState,
    load_snapshot,
    write_snapshot,
)


RGBA_PNG = "rgba.png"
RGBA_NPY = "rgba.npy"
OPTICAL_DEPTH_NPY = "optical_depth.npy"
OPTICAL_DEPTH_PNG = "optical_depth.png"
OPACITY_NPY = "opacity.npy"
OPACITY_PNG = "opacity.png"
RADIANCE_NPY = "radiance.npy"
REPORT_JSON = "report.json"
RECOVERY_REPORT_JSON = "recovery_report.json"
RESUME_DIFF_JSON = "resume_diff.json"
OPTICAL_DEPTH_PNG_SCALE = 8192.0
LUMINANCE_WEIGHTS = np.asarray([0.2126, 0.7152, 0.0722], dtype=np.float64)


@dataclass(frozen=True)
class PipelineResult:
    status: str
    output_dir: Path
    report_path: Path | None = None
    recovery_report_path: Path | None = None
    diff_path: Path | None = None
    error: dict[str, Any] | None = None

    @property
    def exit_code(self) -> int:
        if self.status in {"complete", "interrupted"}:
            if self.error:
                return 3
            return 0
        return 3


def _empty_state(scene: Scene) -> SnapshotState:
    height = scene.camera.height
    width = scene.camera.width
    return SnapshotState(
        radiance=np.zeros((height, width, 3), dtype=np.float64),
        optical_depth=np.zeros((height, width), dtype=np.float64),
        light_radiance=np.zeros(
            (len(scene.lights), height, width, 3), dtype=np.float64
        ),
        completed=np.zeros(
            (
                (height + scene.render.chunk_size[1] - 1)
                // scene.render.chunk_size[1],
                (width + scene.render.chunk_size[0] - 1)
                // scene.render.chunk_size[0],
            ),
            dtype=bool,
        ),
        recovery_count=0,
    )


def _json_bytes(document: dict[str, Any], *, pretty: bool = True) -> bytes:
    if pretty:
        text = json.dumps(
            document,
            ensure_ascii=False,
            allow_nan=False,
            sort_keys=True,
            indent=2,
        )
    else:
        text = json.dumps(
            document,
            ensure_ascii=False,
            allow_nan=False,
            sort_keys=True,
            separators=(",", ":"),
        )
    return (text + "\n").encode("utf-8")


def _write_json(path: Path, document: dict[str, Any]) -> None:
    atomic_write_bytes(path, _json_bytes(document))


def _remove_stale_snapshot_temps(output: Path) -> None:
    for path in output.glob(f".{SNAPSHOT_NAME}.*.tmp.npz"):
        try:
            path.unlink()
        except FileNotFoundError:
            pass


def _render_metadata(scene: Scene) -> dict[str, Any]:
    return {
        "alpha_definition": "1-exp(-optical_depth)",
        "channel_max": float(scene.render.channel_max),
        "chunk_size": list(scene.render.chunk_size),
        "gamma": float(scene.render.gamma),
        "max_samples": int(scene.render.max_samples),
        "optical_depth_png_scale": float(OPTICAL_DEPTH_PNG_SCALE),
        "ray_start_rule": RAY_START_RULE,
        "scene_digest": scene.digest,
        "step": float(scene.render.step),
        "substeps": int(scene.render.substeps),
        "integration_step": float(
            scene.render.step / scene.render.substeps
        ),
        "tonemap": scene.render.tonemap,
    }


def _encoded_images(
    scene: Scene,
    renderer: VolumetricRenderer,
    state: SnapshotState,
) -> tuple[
    np.ndarray,
    np.ndarray,
    np.ndarray,
    np.ndarray,
    np.ndarray,
    np.ndarray,
    dict[str, int | float],
]:
    encoded, clip_stats = renderer.tone_map(state.radiance)
    opacity = renderer.opacity_from_depth(state.optical_depth)
    pixels = rgba8(encoded, opacity)
    rgba_float = np.concatenate(
        [encoded, opacity[..., None]], axis=-1
    ).astype(np.float64)
    depth16 = np.rint(
        np.clip(
            state.optical_depth * OPTICAL_DEPTH_PNG_SCALE,
            0.0,
            65535.0,
        )
    ).astype(np.uint16)
    opacity16 = np.rint(
        np.clip(opacity, 0.0, 1.0) * 65535.0
    ).astype(np.uint16)
    return (
        encoded,
        opacity,
        pixels,
        rgba_float,
        depth16,
        opacity16,
        clip_stats,
    )


def _first_difference(
    name: str,
    first: np.ndarray,
    second: np.ndarray,
    channel_names: list[str],
) -> dict[str, Any] | None:
    if np.array_equal(first, second):
        return None
    difference = first != second
    flat_index = int(np.flatnonzero(difference.reshape(-1))[0])
    coordinates = list(np.unravel_index(flat_index, first.shape))
    if first.ndim == 3 and first.shape[-1] == len(channel_names):
        y, x, channel = coordinates
        return {
            "array": name,
            "pixel": [int(x), int(y)],
            "channels": [channel_names[int(channel)]],
            "first_value": float(first[y, x, channel]),
            "second_value": float(second[y, x, channel]),
        }
    if first.ndim == 2:
        y, x = coordinates
        return {
            "array": name,
            "pixel": [int(x), int(y)],
            "channels": channel_names,
            "first_value": float(first[y, x]),
            "second_value": float(second[y, x]),
        }
    return {
        "array": name,
        "flat_index": flat_index,
        "channels": channel_names,
    }


def _verify_resumed(
    scene: Scene,
    renderer: VolumetricRenderer,
    state: SnapshotState,
    snapshot_reused_chunks: int,
) -> tuple[dict[str, Any], dict[str, Any] | None]:
    fresh_radiance, fresh_depth, fresh_lights = renderer.render_all(
        float(scene.render.step)
    )
    fresh_state = SnapshotState(
        radiance=fresh_radiance,
        optical_depth=fresh_depth,
        light_radiance=fresh_lights,
        completed=np.ones_like(state.completed),
        recovery_count=state.recovery_count,
    )
    main = _encoded_images(scene, renderer, state)
    fresh = _encoded_images(scene, renderer, fresh_state)
    comparisons: list[tuple[str, np.ndarray, np.ndarray, list[str]]] = [
        ("radiance", state.radiance, fresh_radiance, ["R", "G", "B"]),
        (
            "optical_depth",
            state.optical_depth,
            fresh_depth,
            ["optical_depth"],
        ),
        (
            "light_radiance",
            state.light_radiance,
            fresh_lights,
            ["R", "G", "B"],
        ),
        ("rgba8", main[2], fresh[2], ["R", "G", "B", "A"]),
        ("optical_depth16", main[4], fresh[4], ["optical_depth"]),
        ("opacity16", main[5], fresh[5], ["opacity"]),
    ]
    for name, first, second, channels in comparisons:
        difference = _first_difference(name, first, second, channels)
        if difference is not None:
            pixel = difference.get("pixel")
            if pixel is not None:
                difference["chunk_id"] = chunk_id_for_pixel(
                    int(pixel[0]),
                    int(pixel[1]),
                    scene.render.chunk_size,
                )
            document = {
                "ok": False,
                "error": {
                    "code": "RESUME_PIXEL_MISMATCH",
                    "path": "resume",
                    "message": "续算结果与一次性完整渲染不一致",
                    **difference,
                },
                "scene_digest": scene.digest,
            }
            return (
                {
                    "performed": True,
                    "exact": False,
                    "reused_chunks": snapshot_reused_chunks,
                    "fresh_chunks": int(np.prod(state.completed.shape)),
                },
                document,
            )
    return (
        {
            "performed": True,
            "exact": True,
            "reused_chunks": snapshot_reused_chunks,
            "fresh_chunks": int(np.prod(state.completed.shape)),
        },
        None,
    )


def _density_stats(scene: Scene) -> dict[str, Any]:
    density = scene.volume.density
    minimum_index = tuple(
        int(value) for value in np.unravel_index(np.argmin(density), density.shape)
    )
    maximum_index = tuple(
        int(value) for value in np.unravel_index(np.argmax(density), density.shape)
    )

    def world(index: tuple[int, int, int]) -> list[float]:
        coordinate = scene.volume.origin + (
            np.asarray(index, dtype=np.float64) + 0.5
        ) * scene.volume.voxel_size
        return [round(float(value), 9) for value in coordinate]

    return {
        "dimensions": list(scene.volume.dimensions),
        "mean": round(float(np.mean(density)), 9),
        "min": round(float(np.min(density)), 9),
        "min_voxel": list(minimum_index),
        "min_world": world(minimum_index),
        "max": round(float(np.max(density)), 9),
        "max_voxel": list(maximum_index),
        "max_world": world(maximum_index),
        "range": [
            round(float(scene.volume.density_range[0]), 9),
            round(float(scene.volume.density_range[1]), 9),
        ],
    }


def _light_contribution_stats(
    scene: Scene, state: SnapshotState
) -> tuple[dict[str, Any], np.ndarray]:
    per_pixel = state.light_radiance @ LUMINANCE_WEIGHTS
    totals = np.sum(per_pixel, axis=(1, 2))
    grand_total = float(np.sum(totals))
    lights: dict[str, Any] = {}
    for index, light in enumerate(scene.lights):
        share = float(totals[index] / grand_total) if grand_total > 0.0 else 0.0
        lights[light.light_id] = {
            "contribution_share": round(share, 9),
            "integrated_luminance": round(float(totals[index]), 9),
            "kind": light.kind,
        }
    return (
        {
            "total_scattered_luminance": round(grand_total, 9),
            "lights": lights,
        },
        per_pixel,
    )


def _sample_points(
    scene: Scene,
    renderer: VolumetricRenderer,
    state: SnapshotState,
    encoded: np.ndarray,
    opacity: np.ndarray,
    per_light_luminance: np.ndarray,
    pixels: np.ndarray,
) -> list[dict[str, Any]]:
    candidates: list[tuple[int, int]] = []
    for light_index in range(len(scene.lights)):
        flat_index = int(np.argmax(per_light_luminance[light_index]))
        y, x = np.unravel_index(flat_index, per_light_luminance.shape[1:])
        if per_light_luminance[light_index, y, x] > 0.0:
            candidates.append((int(y), int(x)))
    total_luminance = np.sum(per_light_luminance, axis=0)
    top = np.argsort(total_luminance.reshape(-1), kind="stable")[-3:]
    for flat_index in top[::-1]:
        if total_luminance.reshape(-1)[flat_index] > 0.0:
            y, x = np.unravel_index(int(flat_index), total_luminance.shape)
            candidates.append((int(y), int(x)))
    ordered = sorted(set(candidates))
    if not ordered:
        ordered = [
            (scene.camera.height // 2, scene.camera.width // 2)
        ]
    result: list[dict[str, Any]] = []
    for y, x in ordered:
        per_light = {
            light.light_id: round(
                float(per_light_luminance[index, y, x]), 9
            )
            for index, light in enumerate(scene.lights)
        }
        dominant = max(per_light, key=lambda key: per_light[key])
        if per_light[dominant] <= 0.0:
            dominant = None
        world = renderer.world_midpoint_for_pixel(x, y)
        result.append(
            {
                "pixel": [x, y],
                "world": [
                    None if not np.isfinite(value) else round(float(value), 9)
                    for value in world
                ],
                "density_at_world": (
                    None
                    if not np.all(np.isfinite(world))
                    else round(float(renderer.sample_density(world[None, :])[0]), 9)
                ),
                "rgba8": [int(value) for value in pixels[y, x]],
                "encoded_rgb": [
                    round(float(value), 9) for value in encoded[y, x]
                ],
                "luminance": round(
                    float(encoded[y, x] @ LUMINANCE_WEIGHTS), 9
                ),
                "opacity": round(float(opacity[y, x]), 9),
                "optical_depth": round(float(state.optical_depth[y, x]), 9),
                "per_light_luminance": per_light,
                "dominant_light": dominant,
            }
        )
    return result


def _percentile(values: np.ndarray, q: float) -> float:
    if values.size == 0:
        return 0.0
    return round(float(np.percentile(values, q)), 9)


def _smoothness_report(
    scene: Scene,
    encoded: np.ndarray,
    opacity: np.ndarray,
) -> dict[str, Any]:
    luminance = encoded @ LUMINANCE_WEIGHTS
    far = opacity >= 0.5
    horizontal_values: list[np.ndarray] = []
    vertical_values: list[np.ndarray] = []
    if luminance.shape[1] > 1:
        pair = far[:, 1:] & far[:, :-1]
        horizontal_values.append(np.abs(np.diff(luminance, axis=1))[pair])
    if luminance.shape[0] > 1:
        pair = far[1:, :] & far[:-1, :]
        vertical_values.append(np.abs(np.diff(luminance, axis=0))[pair])
    horizontal = (
        np.concatenate(horizontal_values)
        if horizontal_values
        else np.zeros(0)
    )
    vertical = (
        np.concatenate(vertical_values)
        if vertical_values
        else np.zeros(0)
    )
    all_gradient = np.concatenate([horizontal, vertical])

    chunk_width, chunk_height = scene.render.chunk_size
    seam_values: list[np.ndarray] = []
    for seam_x in range(chunk_width, scene.camera.width, chunk_width):
        seam_values.append(np.abs(luminance[:, seam_x] - luminance[:, seam_x - 1]))
    for seam_y in range(chunk_height, scene.camera.height, chunk_height):
        seam_values.append(np.abs(luminance[seam_y, :] - luminance[seam_y - 1, :]))
    seams = np.concatenate(seam_values) if seam_values else np.zeros(0)
    return {
        "dense_region_opacity_floor": 0.5,
        "dense_region_adjacent_luminance_delta": {
            "p50": _percentile(all_gradient, 50.0),
            "p95": _percentile(all_gradient, 95.0),
            "max": round(float(np.max(all_gradient)), 9)
            if all_gradient.size
            else 0.0,
        },
        "chunk_seam_luminance_delta": {
            "p95": _percentile(seams, 95.0),
            "max": round(float(np.max(seams)), 9) if seams.size else 0.0,
        },
        "step_banding_note": (
            "积分不按块归一化；块边界只负责像素归属，沿光线按同一步长"
            "进行全局透射累积。"
        ),
    }


def _convergence_report(
    scene: Scene,
    encoded: np.ndarray,
    half_encoded: np.ndarray,
) -> dict[str, Any]:
    base_luminance = encoded @ LUMINANCE_WEIGHTS
    half_luminance = half_encoded @ LUMINANCE_WEIGHTS
    average = 0.5 * (base_luminance + half_luminance)
    visible = (base_luminance > 1.0e-8) | (half_luminance > 1.0e-8)
    relative = np.zeros_like(average)
    relative[visible] = np.abs(
        base_luminance[visible] - half_luminance[visible]
    ) / np.maximum(average[visible], 1.0e-5)
    if np.any(visible):
        mean_relative = float(np.mean(relative[visible]))
        finite_indices = np.flatnonzero(visible.reshape(-1))
        worst_flat = int(finite_indices[np.argmax(relative.reshape(-1)[finite_indices])])
        worst_y, worst_x = np.unravel_index(worst_flat, relative.shape)
        max_relative = float(relative[worst_y, worst_x])
    else:
        mean_relative = 0.0
        max_relative = 0.0
        worst_y, worst_x = 0, 0
    tolerance = float(scene.render.convergence_tolerance)
    return {
        "declared_step": float(scene.render.step),
        "half_step": float(scene.render.step) * 0.5,
        "substeps": scene.render.substeps,
        "integration_step": float(
            scene.render.step / scene.render.substeps
        ),
        "half_integration_step": float(
            scene.render.step / (2.0 * scene.render.substeps)
        ),
        "mean_relative_deviation": round(mean_relative, 9),
        "max_relative_deviation": round(max_relative, 9),
        "tolerance": tolerance,
        "passed": bool(mean_relative <= tolerance),
        "visible_pixel_count": int(np.count_nonzero(visible)),
        "worst_pixel": [int(worst_x), int(worst_y)],
        "worst_pixel_chunk_id": chunk_id_for_pixel(
            int(worst_x), int(worst_y), scene.render.chunk_size
        ),
        "max_samples": scene.render.max_samples,
    }


def _write_recovery_report(
    scene: Scene,
    state: SnapshotState,
    rendered_this_run: int,
    reason: str,
) -> tuple[Path, dict[str, Any]]:
    total_chunks = int(np.prod(state.completed.shape))
    completed_count = int(np.count_nonzero(state.completed))
    remaining = [
        chunk_id
        for chunk_id, chunk_y, chunk_x, *_ in iter_chunks(
            scene.camera.width,
            scene.camera.height,
            scene.render.chunk_size,
        )
        if not state.completed[chunk_y, chunk_x]
    ]
    document = {
        "ok": True,
        "state": "interrupted",
        "reason": reason,
        "scene_id": scene.scene_id,
        "scene_digest": scene.digest,
        "snapshot": SNAPSHOT_NAME,
        "completed_chunk_count": completed_count,
        "total_chunk_count": total_chunks,
        "rendered_chunks_this_run": rendered_this_run,
        "next_chunk_id": remaining[0] if remaining else None,
        "recovery_count": int(state.recovery_count),
        "step": float(scene.render.step),
        "substeps": scene.render.substeps,
        "integration_step": float(
            scene.render.step / scene.render.substeps
        ),
        "max_samples": scene.render.max_samples,
        "chunk_size": list(scene.render.chunk_size),
        "complete_chunk_ids": [
            chunk_id
            for chunk_id, chunk_y, chunk_x, *_ in iter_chunks(
                scene.camera.width,
                scene.camera.height,
                scene.render.chunk_size,
            )
            if state.completed[chunk_y, chunk_x]
        ],
    }
    path = Path(RECOVERY_REPORT_JSON)
    return path, document


def run_render(
    scene: Scene,
    output_dir: str | Path,
    *,
    fresh: bool = False,
    interrupt_after_chunks: int | None = None,
    should_stop: Callable[[], bool] | None = None,
) -> PipelineResult:
    output = Path(output_dir)
    output.mkdir(parents=True, exist_ok=True)
    _remove_stale_snapshot_temps(output)
    snapshot_path = output / SNAPSHOT_NAME
    if fresh and snapshot_path.exists():
        snapshot_path.unlink()

    load_result = load_snapshot(snapshot_path, scene)
    if load_result.state is not None:
        state = load_result.state
        state.recovery_count += 1
        resumed = True
        reused_chunks = int(np.count_nonzero(state.completed))
    else:
        state = _empty_state(scene)
        resumed = False
        reused_chunks = 0

    renderer = VolumetricRenderer(scene)
    rendered_this_run = 0
    interrupted = False
    completion_reason = "signal"
    for chunk_id, chunk_y, chunk_x, x0, x1, y0, y1 in iter_chunks(
        scene.camera.width,
        scene.camera.height,
        scene.render.chunk_size,
    ):
        if state.completed[chunk_y, chunk_x]:
            continue
        chunk_radiance, chunk_depth, chunk_lights = renderer.render_chunk(
            x0,
            x1,
            y0,
            y1,
            renderer.integration_step(float(scene.render.step)),
        )
        state.radiance[y0:y1, x0:x1] = chunk_radiance
        state.optical_depth[y0:y1, x0:x1] = chunk_depth
        state.light_radiance[:, y0:y1, x0:x1] = chunk_lights
        state.completed[chunk_y, chunk_x] = True
        write_snapshot(snapshot_path, state, scene)
        rendered_this_run += 1
        if (
            interrupt_after_chunks is not None
            and rendered_this_run >= interrupt_after_chunks
        ):
            interrupted = True
            completion_reason = "chunk_limit"
            break
        if should_stop is not None and should_stop():
            interrupted = True
            completion_reason = "signal"
            break

    if interrupted:
        recovery_path, recovery_document = _write_recovery_report(
            scene,
            state,
            rendered_this_run,
            completion_reason,
        )
        recovery_path = output / recovery_path
        _write_json(recovery_path, recovery_document)
        return PipelineResult(
            status="interrupted",
            output_dir=output,
            recovery_report_path=recovery_path,
        )

    resume_verification: dict[str, Any] = {
        "performed": False,
        "exact": True,
        "reused_chunks": reused_chunks,
        "fresh_chunks": 0,
    }
    if resumed:
        resume_verification, difference = _verify_resumed(
            scene, renderer, state, reused_chunks
        )
        if difference is not None:
            diff_path = output / RESUME_DIFF_JSON
            _write_json(diff_path, difference)
            return PipelineResult(
                status="resume_mismatch",
                output_dir=output,
                diff_path=diff_path,
                error=difference["error"],
            )

    (
        encoded,
        opacity,
        pixels,
        rgba_float,
        depth16,
        opacity16,
        clip_stats,
    ) = _encoded_images(scene, renderer, state)
    half_radiance, _, _ = renderer.render_all(
        float(scene.render.step) * 0.5
    )
    half_encoded, _ = renderer.tone_map(half_radiance)
    convergence = _convergence_report(scene, encoded, half_encoded)
    light_stats, per_light_luminance = _light_contribution_stats(
        scene, state
    )
    sample_points = _sample_points(
        scene,
        renderer,
        state,
        encoded,
        opacity,
        per_light_luminance,
        pixels,
    )
    smoothness = _smoothness_report(scene, encoded, opacity)

    metadata = _render_metadata(scene)
    rgba_png_bytes = png_bytes_rgba(pixels, metadata)
    depth_png_bytes = png_bytes_gray16(depth16)
    opacity_png_bytes = png_bytes_gray16(opacity16)
    rgba_npy_bytes = npy_bytes(rgba_float)
    depth_npy_bytes = npy_bytes(state.optical_depth)
    opacity_npy_bytes = npy_bytes(opacity)
    radiance_npy_bytes = npy_bytes(state.radiance)

    report = {
        "ok": convergence["passed"],
        "accepted": convergence["passed"],
        "scene": {
            "scene_id": scene.scene_id,
            "seed": scene.seed,
            "scene_digest": scene.digest,
            "render_signature": scene.render_signature,
            "image_size": [scene.camera.width, scene.camera.height],
        },
        "density": _density_stats(scene),
        "render": {
            "step": float(scene.render.step),
            "substeps": scene.render.substeps,
            "integration_step": float(
                scene.render.step / scene.render.substeps
            ),
            "half_step": float(scene.render.step) * 0.5,
            "half_integration_step": float(
                scene.render.step / (2.0 * scene.render.substeps)
            ),
            "max_samples": scene.render.max_samples,
            "chunk_size": list(scene.render.chunk_size),
            "ray_start_rule": RAY_START_RULE,
            "absorption": scene.render.absorption,
            "scattering": scene.render.scattering,
            "phase_g": scene.render.phase_g,
            "light_step": scene.render.light_step,
            "light_max_samples": scene.render.light_max_samples,
        },
        "convergence": convergence,
        "tonemapping": {
            **clip_stats,
            "tonemap": scene.render.tonemap,
            "exposure": scene.render.exposure,
            "gamma": scene.render.gamma,
            "alpha_definition": "1-exp(-optical_depth)",
            "optical_depth_png_scale": OPTICAL_DEPTH_PNG_SCALE,
            "encoded_min": round(float(np.min(encoded)), 9),
            "encoded_max": round(float(np.max(encoded)), 9),
            "channel_overflow_count": int(
                np.count_nonzero(encoded > 1.0)
            ),
        },
        "lighting": light_stats,
        "sample_points": sample_points,
        "smoothness": smoothness,
        "resume": {
            "recovery_count": int(state.recovery_count),
            "resumed_from_snapshot": resumed,
            "reused_chunks": reused_chunks,
            "rendered_chunks_this_run": rendered_this_run,
            "snapshot_load_status": load_result.status,
            "snapshot_discarded": load_result.status in {"corrupt", "invalid"},
            "verification": resume_verification,
        },
        "outputs": {
            "rgba_png": RGBA_PNG,
            "rgba_npy": RGBA_NPY,
            "radiance_npy": RADIANCE_NPY,
            "optical_depth_npy": OPTICAL_DEPTH_NPY,
            "optical_depth_png": OPTICAL_DEPTH_PNG,
            "opacity_npy": OPACITY_NPY,
            "opacity_png": OPACITY_PNG,
        },
    }
    if not convergence["passed"]:
        report["error"] = {
            "code": "STEP_CONVERGENCE_FAILED",
            "path": "render.step",
            "message": "声明步长与二分之一步长的平均相对偏差超过容差",
        }

    atomic_write_bytes(output / RGBA_PNG, rgba_png_bytes)
    atomic_write_bytes(output / RGBA_NPY, rgba_npy_bytes)
    atomic_write_bytes(output / RADIANCE_NPY, radiance_npy_bytes)
    atomic_write_bytes(output / OPTICAL_DEPTH_NPY, depth_npy_bytes)
    atomic_write_bytes(output / OPTICAL_DEPTH_PNG, depth_png_bytes)
    atomic_write_bytes(output / OPACITY_NPY, opacity_npy_bytes)
    atomic_write_bytes(output / OPACITY_PNG, opacity_png_bytes)
    report_path = output / REPORT_JSON
    _write_json(report_path, report)

    if snapshot_path.exists():
        snapshot_path.unlink()
    _remove_stale_snapshot_temps(output)
    recovery_stale = output / RECOVERY_REPORT_JSON
    if recovery_stale.exists():
        recovery_stale.unlink()
    diff_stale = output / RESUME_DIFF_JSON
    if diff_stale.exists():
        diff_stale.unlink()

    return PipelineResult(
        status="complete" if convergence["passed"] else "quality_failed",
        output_dir=output,
        report_path=report_path,
        error=report.get("error"),
    )

