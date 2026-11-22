"""Software renderer for transparent interfaces and single scattering."""

from __future__ import annotations

import contextlib
import hashlib
import math
import time
from dataclasses import dataclass, field
from typing import Any, Callable

import numpy as np

from .errors import parameter_error
from .fixedpoint import FixedAccumulator, LENGTH_SCALE, OPTICAL_SCALE
from .geometry import (
    Hit,
    camera_rays,
    nearest_scene_hit,
    trace_medium_path,
)
from .scattering import (
    VALID_STRATEGIES,
    hg_phase,
    integrate_segment,
    phase_cosine,
    preview_error_bound,
)
from .scene import Scene
from .resources import ResourceGovernor
from .util import canonical_json, normalize, sha256_json

ALGORITHM_VERSION = "tvol-1.0.0"
_UNRESOLVED_ENTRY = object()


@dataclass(frozen=True)
class RenderConfig:
    block_width: int = 16
    block_height: int = 12
    step_size: float = 0.05
    sampling_strategy: str = "midpoint"
    max_segments: int = 8
    preview_step_size: float | None = None

    def __post_init__(self) -> None:
        if isinstance(self.block_width, bool) or not isinstance(self.block_width, int) or self.block_width <= 0:
            raise parameter_error("render.block_width", "block width must be a positive integer")
        if isinstance(self.block_height, bool) or not isinstance(self.block_height, int) or self.block_height <= 0:
            raise parameter_error("render.block_height", "block height must be a positive integer")
        if not math.isfinite(self.step_size) or self.step_size <= 0.0 or self.step_size > 100.0:
            raise parameter_error("render.step_size", "step size must be finite and in (0, 100]")
        if self.sampling_strategy not in VALID_STRATEGIES:
            raise parameter_error("render.sampling_strategy", f"expected one of {VALID_STRATEGIES}")
        if isinstance(self.max_segments, bool) or not isinstance(self.max_segments, int) or self.max_segments <= 0:
            raise parameter_error("render.max_segments", "max_segments must be a positive integer")
        if self.preview_step_size is not None:
            if not math.isfinite(self.preview_step_size) or self.preview_step_size <= 0.0 or self.preview_step_size > 100.0:
                raise parameter_error("render.preview_step_size", "preview step size must be finite and in (0, 100]")

    def effective_step(self, mode: str) -> float:
        if mode == "preview" and self.preview_step_size is not None:
            return float(self.preview_step_size)
        return float(self.step_size)

    def to_dict(self) -> dict[str, Any]:
        return {
            "algorithm_version": ALGORITHM_VERSION,
            "block_width": self.block_width,
            "block_height": self.block_height,
            "step_size": self.step_size,
            "sampling_strategy": self.sampling_strategy,
            "max_segments": self.max_segments,
            "preview_step_size": self.preview_step_size,
        }

    @property
    def config_hash(self) -> str:
        return sha256_json(self.to_dict())


@dataclass
class BlockWindow:
    index: int
    row: int
    col: int
    x0: int
    y0: int
    x1: int
    y1: int


@dataclass
class PixelTrace:
    hit: bool
    medium_id: str | None
    exit_point: np.ndarray | None
    segments: list[Any]
    path_units: int
    optical_units: int
    path_quantization_error: float
    optical_quantization_error: float
    transmission: float
    tir_count: int
    complete: bool


@dataclass
class BlockResult:
    window: BlockWindow
    transmission: np.ndarray
    scattering: np.ndarray
    path_units: np.ndarray
    optical_units: np.ndarray
    hit_mask: np.ndarray
    media_ids: tuple[str, ...]
    light_ids: tuple[str, ...]
    max_path_quantization_error: float
    max_optical_quantization_error: float
    tir_pixels: int
    duration_ms: float = 0.0

    @property
    def index(self) -> int:
        return self.window.index

    @property
    def buffer_checksum(self) -> str:
        return hashlib.sha256(
            self.transmission.tobytes()
            + self.scattering.tobytes()
            + self.path_units.tobytes()
            + self.optical_units.tobytes()
        ).hexdigest()


def block_windows(width: int, height: int, block_width: int, block_height: int) -> list[BlockWindow]:
    windows: list[BlockWindow] = []
    columns = math.ceil(width / block_width)
    rows = math.ceil(height / block_height)
    index = 0
    for row in range(rows):
        for col in range(columns):
            windows.append(
                BlockWindow(
                    index=index,
                    row=row,
                    col=col,
                    x0=col * block_width,
                    y0=row * block_height,
                    x1=min(width, (col + 1) * block_width),
                    y1=min(height, (row + 1) * block_height),
                )
            )
            index += 1
    return windows


def _geometry_trace(
    scene: Scene,
    origin: np.ndarray,
    direction: np.ndarray,
    wavelength_index: int,
    max_segments: int,
    entry: Hit | None | object = _UNRESOLVED_ENTRY,
) -> PixelTrace:
    if entry is _UNRESOLVED_ENTRY:
        entry = nearest_scene_hit(scene, origin, direction)
    if entry is None:
        return PixelTrace(
            hit=False,
            medium_id=None,
            exit_point=None,
            segments=[],
            path_units=0,
            optical_units=0,
            path_quantization_error=0.0,
            optical_quantization_error=0.0,
            transmission=1.0,
            tir_count=0,
            complete=True,
        )
    medium = scene.media_by_id[entry.medium_id]
    path = trace_medium_path(medium, entry, direction, wavelength_index, max_segments)
    sigma_t = float(medium["absorption"][wavelength_index]) + float(medium["scattering"][wavelength_index])
    accumulator = FixedAccumulator()
    for segment in path.segments:
        accumulator.add_segment(segment.length, sigma_t)
    transmission = math.exp(-accumulator.optical_depth)
    return PixelTrace(
        hit=True,
        medium_id=medium["id"],
        exit_point=path.exit_point.copy(),
        segments=path.segments,
        path_units=accumulator.path_units,
        optical_units=accumulator.optical_units,
        path_quantization_error=accumulator.path_quantization_error,
        optical_quantization_error=accumulator.optical_quantization_error,
        transmission=transmission,
        tir_count=path.tir_count,
        complete=path.complete,
    )


def _scattering_trace(
    scene: Scene,
    trace: PixelTrace,
    wavelength_index: int,
    step_size: float,
    sampling_strategy: str,
) -> tuple[float, tuple[str, ...], dict[str, float]]:
    if not trace.hit or trace.medium_id is None or not trace.segments:
        return 0.0, (), {}
    medium = scene.media_by_id[trace.medium_id]
    sigma_s = float(medium["scattering"][wavelength_index])
    sigma_a = float(medium["absorption"][wavelength_index])
    sigma_t = sigma_a + sigma_s
    if sigma_s <= 0.0:
        return 0.0, (), {}

    used_lights: list[str] = []
    contributions: dict[str, float] = {}
    total = 0.0
    base_optical_depth = 0.0
    for segment in trace.segments:
        for light in scene.lights:
            radiance = float(light["radiance"][wavelength_index])
            if radiance <= 0.0:
                continue
            anisotropy = float(light["anisotropy"][wavelength_index])
            cosine = phase_cosine(segment.direction, np.asarray(light["direction"], dtype=np.float64))
            phase = hg_phase(cosine, anisotropy)
            contribution = integrate_segment(
                length=segment.length,
                sigma_s=sigma_s,
                sigma_t=sigma_t,
                radiance=radiance,
                phase=phase,
                step_size=step_size,
                strategy=sampling_strategy,
                base_optical_depth=base_optical_depth,
            )
            total += contribution
            contributions[light["id"]] = contributions.get(light["id"], 0.0) + contribution
            if light["id"] not in used_lights:
                used_lights.append(light["id"])
        base_optical_depth += sigma_t * segment.length
    return total, tuple(sorted(used_lights)), contributions


def trace_ray(
    scene: Scene,
    origin: np.ndarray,
    direction: np.ndarray,
    wavelength_index: int,
    config: RenderConfig,
    *,
    step_size: float | None = None,
) -> dict[str, Any]:
    """Public deterministic single-ray API used by analytic-truth tests."""

    if not 0 <= wavelength_index < scene.band_count:
        raise parameter_error("wavelength_index", "wavelength index is out of range")
    incident = normalize(np.asarray(direction, dtype=np.float64), "ray.direction")
    origin = np.asarray(origin, dtype=np.float64)
    trace = _geometry_trace(scene, origin, incident, wavelength_index, config.max_segments)
    actual_step = config.step_size if step_size is None else step_size
    scattering, lights, contributions = _scattering_trace(
        scene, trace, wavelength_index, actual_step, config.sampling_strategy
    )
    return {
        "hit": trace.hit,
        "medium_id": trace.medium_id,
        "exit_point": trace.exit_point,
        "transmission": trace.transmission,
        "scattering": scattering,
        "scattering_by_light": contributions,
        "path_units": trace.path_units,
        "optical_units": trace.optical_units,
        "path_length_quantized": trace.path_units / LENGTH_SCALE,
        "optical_depth_quantized": trace.optical_units / OPTICAL_SCALE,
        "path_quantization_error": trace.path_quantization_error,
        "optical_quantization_error": trace.optical_quantization_error,
        "tir_count": trace.tir_count,
        "complete": trace.complete,
        "light_ids": lights,
        "actual_step": actual_step,
    }


def render_block(
    scene: Scene,
    config: RenderConfig,
    window: BlockWindow,
    *,
    sample_index: int,
    mode: str = "full",
    transmission_callback: Callable[[], None] | None = None,
    governor: ResourceGovernor | None = None,
) -> BlockResult:
    width = window.x1 - window.x0
    height = window.y1 - window.y0
    band_count = scene.band_count
    transmission = np.zeros((height, width, band_count), dtype=np.float64)
    scattering = np.zeros((height, width, band_count), dtype=np.float64)
    path_units = np.zeros((height, width, band_count), dtype=np.int64)
    optical_units = np.zeros((height, width, band_count), dtype=np.int64)
    hit_mask = np.zeros((height, width), dtype=np.bool_)
    traces: list[list[PixelTrace]] = [[None for _ in range(band_count)] for _ in range(height * width)]  # type: ignore[list-item]
    max_path_error = 0.0
    max_optical_error = 0.0
    media_ids: set[str] = set()
    tir_pixels = 0
    start = time.perf_counter()
    origins, directions = camera_rays(scene.camera, sample_index, scene.camera["width"], scene.camera["height"])
    scene_cache_prefix = scene.content_hash.encode("ascii")
    pixel_count = height * width
    if governor is None:
        chunk_pixels = pixel_count
    else:
        chunk_pixels = max(
            1,
            min(
                64,
                governor.limits.intersection_cache_budget,
                governor.limits.per_thread_sample_buffer,
            ),
        )
    actual_step = config.effective_step(mode)
    estimated_samples_per_pixel = max(1, int(math.ceil(4.0 / actual_step)))
    for chunk_start in range(0, pixel_count, chunk_pixels):
        chunk_end = min(pixel_count, chunk_start + chunk_pixels)
        lease = (
            governor.lease(
                chunk_end - chunk_start,
                (chunk_end - chunk_start) * estimated_samples_per_pixel,
            )
            if governor is not None
            else _null_context()
        )
        with lease:
            for pixel_index in range(chunk_start, chunk_end):
                local_y, local_x = divmod(pixel_index, width)
                global_y = window.y0 + local_y
                global_x = window.x0 + local_x
                origin = origins[global_y, global_x]
                direction = directions[global_y, global_x]
                cache_key = scene_cache_prefix + origin.tobytes() + direction.tobytes()
                for band in range(band_count):
                    if governor is not None:
                        cached, entry = governor.intersection_cache.get(cache_key)
                        if not cached:
                            entry = nearest_scene_hit(scene, origin, direction)
                            governor.intersection_cache.put(cache_key, entry)
                    else:
                        entry = nearest_scene_hit(scene, origin, direction)
                    trace = _geometry_trace(
                        scene,
                        origin,
                        direction,
                        band,
                        config.max_segments,
                        entry=entry,
                    )
                    traces[pixel_index][band] = trace
                    transmission[local_y, local_x, band] = trace.transmission
                    path_units[local_y, local_x, band] = trace.path_units
                    optical_units[local_y, local_x, band] = trace.optical_units
                    max_path_error = max(max_path_error, trace.path_quantization_error)
                    max_optical_error = max(max_optical_error, trace.optical_quantization_error)
                    if trace.hit:
                        hit_mask[local_y, local_x] = True
                        media_ids.add(trace.medium_id)  # type: ignore[arg-type]
                        if trace.tir_count:
                            tir_pixels += 1
    if transmission_callback is not None:
        transmission_callback()

    used_lights: set[str] = set()
    for chunk_start in range(0, pixel_count, chunk_pixels):
        chunk_end = min(pixel_count, chunk_start + chunk_pixels)
        lease = (
            governor.lease(
                chunk_end - chunk_start,
                (chunk_end - chunk_start) * estimated_samples_per_pixel,
            )
            if governor is not None
            else _null_context()
        )
        with lease:
            for pixel_index in range(chunk_start, chunk_end):
                local_y, local_x = divmod(pixel_index, width)
                for band in range(band_count):
                    value, lights, _ = _scattering_trace(
                        scene,
                        traces[pixel_index][band],
                        band,
                        actual_step,
                        config.sampling_strategy,
                    )
                    scattering[local_y, local_x, band] = value
                    used_lights.update(lights)
    return BlockResult(
        window=window,
        transmission=transmission,
        scattering=scattering,
        path_units=path_units,
        optical_units=optical_units,
        hit_mask=hit_mask,
        media_ids=tuple(sorted(media_ids)),
        light_ids=tuple(sorted(used_lights)),
        max_path_quantization_error=max_path_error,
        max_optical_quantization_error=max_optical_error,
        tir_pixels=tir_pixels,
        duration_ms=(time.perf_counter() - start) * 1000.0,
    )


@contextlib.contextmanager
def _null_context() -> Any:
    yield


def _empty_arrays(width: int, height: int, bands: int) -> tuple[np.ndarray, np.ndarray, np.ndarray, np.ndarray, np.ndarray]:
    return (
        np.zeros((height, width, bands), dtype=np.float64),
        np.zeros((height, width, bands), dtype=np.float64),
        np.zeros((height, width, bands), dtype=np.int64),
        np.zeros((height, width, bands), dtype=np.int64),
        np.zeros((height, width), dtype=np.bool_),
    )


def assemble_blocks(
    scene: Scene,
    results: dict[int, BlockResult],
    config: RenderConfig,
) -> tuple[np.ndarray, np.ndarray, np.ndarray, np.ndarray, np.ndarray]:
    width = int(scene.camera["width"])
    height = int(scene.camera["height"])
    bands = scene.band_count
    transmission, scattering, path_units, optical_units, hit_mask = _empty_arrays(width, height, bands)
    for result in results.values():
        window = result.window
        transmission[window.y0 : window.y1, window.x0 : window.x1, :] = result.transmission
        scattering[window.y0 : window.y1, window.x0 : window.x1, :] = result.scattering
        path_units[window.y0 : window.y1, window.x0 : window.x1, :] = result.path_units
        optical_units[window.y0 : window.y1, window.x0 : window.x1, :] = result.optical_units
        hit_mask[window.y0 : window.y1, window.x0 : window.x1] |= result.hit_mask
    return transmission, scattering, path_units, optical_units, hit_mask


def render_stats(scene: Scene, transmission: np.ndarray, scattering: np.ndarray, hit_mask: np.ndarray) -> dict[str, Any]:
    bands: list[dict[str, Any]] = []
    for band, wavelength in enumerate(scene.wavelengths):
        trans = transmission[:, :, band]
        scat = scattering[:, :, band]
        bands.append(
            {
                "wavelength_nm": float(wavelength),
                "transmission_sum": float(np.sum(trans, dtype=np.float64)),
                "transmission_mean": float(np.mean(trans, dtype=np.float64)),
                "transmission_min": float(np.min(trans)),
                "transmission_max": float(np.max(trans)),
                "scattering_sum": float(np.sum(scat, dtype=np.float64)),
                "scattering_mean": float(np.mean(scat, dtype=np.float64)),
                "scattering_min": float(np.min(scat)),
                "scattering_max": float(np.max(scat)),
                "hit_pixels": int(np.count_nonzero(hit_mask)),
            }
        )
    return {
        "wavelength_bands": bands,
        "hit_pixels": int(np.count_nonzero(hit_mask)),
        "pixel_count": int(hit_mask.size),
    }


def conservative_preview_bound(scene: Scene, config: RenderConfig, mode: str) -> float:
    step = config.effective_step(mode)
    if mode != "preview" or config.preview_step_size is None or config.preview_step_size >= config.step_size:
        return 0.0
    max_length = 0.0
    max_sigma_t = 0.0
    max_coefficient = 0.0
    for medium in scene.media:
        boundary = medium["boundary"]
        if boundary["type"] == "sphere":
            length = 2.0 * float(boundary["radius"])
        elif boundary["type"] == "box":
            length = float(np.linalg.norm(np.asarray(boundary["max"]) - np.asarray(boundary["min"])))
        else:
            vertices = np.asarray(boundary["vertices"], dtype=np.float64)
            length = float(np.linalg.norm(np.max(vertices, axis=0) - np.min(vertices, axis=0)))
        max_length = max(max_length, length)
        for band in range(scene.band_count):
            max_sigma_t = max(
                max_sigma_t,
                float(medium["absorption"][band]) + float(medium["scattering"][band]),
            )
            sigma_s = float(medium["scattering"][band])
            for light in scene.lights:
                g = float(light["anisotropy"][band])
                phase_max = hg_phase(1.0, g)
                max_coefficient = max(
                    max_coefficient,
                    sigma_s * float(light["radiance"][band]) * phase_max,
                )
    return preview_error_bound(
        step,
        max_length,
        max_sigma_t,
        max_coefficient,
        1.0,
        1.0,
    )


@dataclass
class ReferenceResult:
    transmission: np.ndarray
    scattering: np.ndarray
    path_units: np.ndarray
    optical_units: np.ndarray
    hit_mask: np.ndarray
    stats: dict[str, Any]
    commit_order: list[int]
    statuses: dict[int, str]
    media_dependencies: dict[int, tuple[str, ...]]
    light_dependencies: dict[int, tuple[str, ...]]
    actual_step: float
    preview_error_bound: float
    algorithm_version: str = ALGORITHM_VERSION
    config_hash: str = ""
    scene_hash: str = ""
    block_windows: list[BlockWindow] = field(default_factory=list)
    complete: bool = True
    failed_blocks: dict[int, str] = field(default_factory=dict)

    def to_dict(self, include_arrays: bool = False) -> dict[str, Any]:
        result: dict[str, Any] = {
            "ok": self.complete,
            "algorithm_version": self.algorithm_version,
            "config_hash": self.config_hash,
            "scene_hash": self.scene_hash,
            "actual_step": self.actual_step,
            "preview_error_bound": self.preview_error_bound,
            "stats": self.stats,
            "commit_order": self.commit_order,
            "statuses": {str(key): value for key, value in self.statuses.items()},
            "media_dependencies": {str(key): list(value) for key, value in self.media_dependencies.items()},
            "light_dependencies": {str(key): list(value) for key, value in self.light_dependencies.items()},
            "complete": self.complete,
            "failed_blocks": {str(key): value for key, value in self.failed_blocks.items()},
        }
        if include_arrays:
            result["transmission"] = self.transmission.tolist()
            result["scattering"] = self.scattering.tolist()
            result["path_units"] = self.path_units.tolist()
            result["optical_units"] = self.optical_units.tolist()
            result["hit_mask"] = self.hit_mask.tolist()
        return result


def render_reference(scene: Scene, config: RenderConfig, *, sample_index: int = 0, mode: str = "full") -> ReferenceResult:
    width = int(scene.camera["width"])
    height = int(scene.camera["height"])
    windows = block_windows(width, height, config.block_width, config.block_height)
    results: dict[int, BlockResult] = {}
    statuses: dict[int, str] = {}
    media_dependencies: dict[int, tuple[str, ...]] = {}
    light_dependencies: dict[int, tuple[str, ...]] = {}
    for window in windows:
        statuses[window.index] = "pending_intersect"
        statuses[window.index] = "transmission_accumulating"
        statuses[window.index] = "scattering_accumulating"
        result = render_block(scene, config, window, sample_index=sample_index, mode=mode)
        results[window.index] = result
        media_dependencies[window.index] = result.media_ids
        light_dependencies[window.index] = result.light_ids
        statuses[window.index] = "committed"
    transmission, scattering, path_units, optical_units, hit_mask = assemble_blocks(scene, results, config)
    return ReferenceResult(
        transmission=transmission,
        scattering=scattering,
        path_units=path_units,
        optical_units=optical_units,
        hit_mask=hit_mask,
        stats=render_stats(scene, transmission, scattering, hit_mask),
        commit_order=[window.index for window in windows],
        statuses=statuses,
        media_dependencies=media_dependencies,
        light_dependencies=light_dependencies,
        actual_step=config.effective_step(mode),
        preview_error_bound=conservative_preview_bound(scene, config, mode),
        config_hash=config.config_hash,
        scene_hash=scene.content_hash,
        block_windows=windows,
    )


def render_heatmap_rgb(values: np.ndarray, hit_mask: np.ndarray | None = None) -> list[list[list[int]]]:
    values = np.asarray(values, dtype=np.float64)
    if values.ndim != 3:
        raise parameter_error("heatmap.values", "heatmap values must have shape (height, width, bands)")
    bands = values.shape[2]
    if bands >= 3:
        rgb = values[:, :, :3]
    else:
        repeated = np.repeat(values[:, :, :1], 3, axis=2)
        repeated[:, :, 1] = values[:, :, min(1, bands - 1)]
        repeated[:, :, 2] = values[:, :, min(2, bands - 1)]
        rgb = repeated
    if hit_mask is not None:
        rgb = rgb * np.asarray(hit_mask, dtype=np.float64)[:, :, None]
    maximum = float(np.max(rgb)) if rgb.size else 0.0
    if maximum > 0.0:
        rgb = rgb / maximum
    quantized = np.clip(np.floor(rgb * 255.0 + 0.5), 0, 255).astype(np.int64)
    return quantized.tolist()


__all__ = [
    "ALGORITHM_VERSION",
    "BlockResult",
    "BlockWindow",
    "ReferenceResult",
    "RenderConfig",
    "assemble_blocks",
    "block_windows",
    "conservative_preview_bound",
    "render_block",
    "render_heatmap_rgb",
    "render_reference",
    "render_stats",
    "trace_ray",
]
