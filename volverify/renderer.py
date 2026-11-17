from __future__ import annotations

import math
from dataclasses import dataclass, field
from typing import Callable

import numpy as np

from .errors import RenderInterrupted
from .scene import Scene, SceneLight, Volume

RAY_START_RULE = (
    "pixel_center; t_enter=max(0,AABB_entry); each interval starts at "
    "t_enter+k*step; sample at interval midpoint; final interval uses the "
    "remaining distance after floor(distance/step) full intervals"
)
RAY_DIRECTION_RULE = (
    "perspective pinhole through pixel centers; forward plus tangent-scaled "
    "right/up offsets, normalized"
)


@dataclass
class TileResult:
    chunk_id: int
    x0: int
    y0: int
    width: int
    height: int
    rgba: np.ndarray
    depth: np.ndarray
    radiance: np.ndarray
    entry_t: np.ndarray
    exit_t: np.ndarray
    peak_density: np.ndarray
    peak_t: np.ndarray
    peak_index: np.ndarray
    index_min: np.ndarray
    index_max: np.ndarray
    sample_count: int
    actual_step_min: float
    actual_step_max: float
    self_rgb: np.ndarray
    light_rgb: dict[str, np.ndarray] = field(default_factory=dict)


@dataclass
class ImageResult:
    rgba: np.ndarray
    depth: np.ndarray
    radiance: np.ndarray
    entry_t: np.ndarray
    exit_t: np.ndarray
    peak_density: np.ndarray
    peak_t: np.ndarray
    peak_index: np.ndarray
    index_min: np.ndarray
    index_max: np.ndarray
    sample_count: int
    actual_step_min: float
    actual_step_max: float
    self_rgb: np.ndarray
    light_rgb: dict[str, np.ndarray]


@dataclass(frozen=True)
class ChunkLayout:
    chunk_id: int
    x0: int
    y0: int
    width: int
    height: int

    def to_dict(self) -> dict:
        return {
            "chunk_id": self.chunk_id,
            "x0": self.x0,
            "y0": self.y0,
            "width": self.width,
            "height": self.height,
        }


def make_chunk_layout(
    width: int, height: int, chunk_width: int, chunk_height: int
) -> list[ChunkLayout]:
    chunks: list[ChunkLayout] = []
    chunk_id = 0
    for y0 in range(0, height, chunk_height):
        for x0 in range(0, width, chunk_width):
            chunks.append(
                ChunkLayout(
                    chunk_id=chunk_id,
                    x0=x0,
                    y0=y0,
                    width=min(chunk_width, width - x0),
                    height=min(chunk_height, height - y0),
                )
            )
            chunk_id += 1
    return chunks


def chunk_layout_hash(chunks: list[ChunkLayout]) -> str:
    from .io_utils import canonical_json_bytes, sha256_bytes

    return sha256_bytes(
        canonical_json_bytes([chunk.to_dict() for chunk in chunks])
    )


def _camera_rays(
    scene: Scene, x0: int, y0: int, width: int, height: int
) -> tuple[np.ndarray, np.ndarray]:
    camera = scene.camera
    xs = np.arange(x0, x0 + width, dtype=np.float64) + 0.5
    ys = np.arange(y0, y0 + height, dtype=np.float64) + 0.5
    unnormalized_x = (xs / camera.width) * 2.0 - 1.0
    unnormalized_y = 1.0 - (ys / camera.height) * 2.0
    tangent = math.tan(math.radians(camera.fov_y_degrees) * 0.5)
    grid_x, grid_y = np.meshgrid(unnormalized_x, unnormalized_y)
    directions = (
        camera.forward[None, :]
        + (grid_x.reshape(-1, 1) * camera.aspect_ratio * tangent)
        * camera.right[None, :]
        + (grid_y.reshape(-1, 1) * tangent) * camera.up[None, :]
    )
    directions /= np.linalg.norm(directions, axis=1, keepdims=True)
    origins = np.broadcast_to(
        camera.position, (width * height, 3)
    ).astype(np.float64, copy=True)
    return origins, directions


def _intersect_volume(
    origins: np.ndarray,
    directions: np.ndarray,
    volume: Volume,
) -> tuple[np.ndarray, np.ndarray, np.ndarray]:
    count = directions.shape[0]
    near = np.full(count, -np.inf, dtype=np.float64)
    far = np.full(count, np.inf, dtype=np.float64)
    impossible = np.zeros(count, dtype=bool)

    for axis in range(3):
        origin = origins[:, axis]
        direction = directions[:, axis]
        minimum = float(volume.bounds_min[axis])
        maximum = float(volume.bounds_max[axis])
        parallel = np.abs(direction) < 1e-15
        impossible |= parallel & ((origin < minimum) | (origin > maximum))
        safe_direction = np.where(parallel, 1.0, direction)
        t0 = (minimum - origin) / safe_direction
        t1 = (maximum - origin) / safe_direction
        axis_near = np.minimum(t0, t1)
        axis_far = np.maximum(t0, t1)
        near = np.maximum(near, axis_near)
        far = np.minimum(far, axis_far)

    hit = (~impossible) & (far >= near) & (far >= 0.0)
    entry = np.where(hit, np.maximum(near, 0.0), -1.0)
    exit_value = np.where(hit, far, -1.0)
    hit &= exit_value > entry
    entry = np.where(hit, entry, -1.0)
    exit_value = np.where(hit, exit_value, -1.0)
    return entry, exit_value, hit


def _sample_volume(
    volume: Volume, positions: np.ndarray
) -> tuple[np.ndarray, np.ndarray, np.ndarray]:
    dims = np.asarray(volume.dims, dtype=np.float64)
    span = volume.bounds_max - volume.bounds_min
    normalized = (positions - volume.bounds_min[None, :]) / span[None, :]
    coordinates = normalized * (dims[None, :] - 1.0)
    inside = np.all(
        (coordinates >= 0.0) & (coordinates <= dims[None, :] - 1.0), axis=1
    )
    density = np.zeros(positions.shape[0], dtype=np.float64)
    indices = np.full(positions.shape, -1, dtype=np.int32)
    if not np.any(inside):
        return density, indices, inside

    valid_coordinates = np.clip(
        coordinates[inside], 0.0, dims[None, :] - 1.0
    )
    lower = np.floor(valid_coordinates).astype(np.int32)
    upper = np.minimum(lower + 1, np.asarray(volume.dims, dtype=np.int32) - 1)
    fraction = valid_coordinates - lower
    indices[inside] = lower

    nx, ny, nz = volume.dims
    flat = np.asarray(volume.data, dtype=np.float64).ravel()

    def corner(ix: np.ndarray, iy: np.ndarray, iz: np.ndarray) -> np.ndarray:
        return flat[ix * (ny * nz) + iy * nz + iz]

    x0, y0, z0 = lower[:, 0], lower[:, 1], lower[:, 2]
    x1, y1, z1 = upper[:, 0], upper[:, 1], upper[:, 2]
    fx, fy, fz = fraction[:, 0], fraction[:, 1], fraction[:, 2]
    c000 = corner(x0, y0, z0)
    c100 = corner(x1, y0, z0)
    c010 = corner(x0, y1, z0)
    c110 = corner(x1, y1, z0)
    c001 = corner(x0, y0, z1)
    c101 = corner(x1, y0, z1)
    c011 = corner(x0, y1, z1)
    c111 = corner(x1, y1, z1)
    c00 = c000 * (1.0 - fx) + c100 * fx
    c10 = c010 * (1.0 - fx) + c110 * fx
    c01 = c001 * (1.0 - fx) + c101 * fx
    c11 = c011 * (1.0 - fx) + c111 * fx
    c0 = c00 * (1.0 - fy) + c10 * fy
    c1 = c01 * (1.0 - fy) + c11 * fy
    density[inside] = c0 * (1.0 - fz) + c1 * fz
    return density, indices, inside


def _phase(cos_theta: np.ndarray, g: float) -> np.ndarray:
    denominator = np.maximum(1.0 + g * g - 2.0 * g * cos_theta, 1e-8)
    return (1.0 - g * g) / (4.0 * math.pi * denominator**1.5)


def _light_sample_contribution(
    light: SceneLight,
    positions: np.ndarray,
    ray_directions: np.ndarray,
    density: np.ndarray,
) -> np.ndarray:
    if light.kind == "point":
        assert light.position is not None
        to_light = light.position[None, :] - positions
        distance = np.linalg.norm(to_light, axis=1)
        safe_distance = np.maximum(distance, light.min_distance)
        to_light_unit = to_light / np.maximum(
            distance[:, None], light.min_distance
        )
        cosine = np.sum(ray_directions * to_light_unit, axis=1)
        attenuation = 1.0 / (safe_distance * safe_distance)
    else:
        assert light.direction is not None
        to_light_unit = np.broadcast_to(-light.direction, positions.shape)
        cosine = np.sum(ray_directions * to_light_unit, axis=1)
        attenuation = np.ones_like(density)
    phase = _phase(np.clip(cosine, -1.0, 1.0), light.phase_g)
    scalar = density * light.intensity * attenuation * phase
    return scalar[:, None] * light.color[None, :]


def _tone_map_rgba(radiance: np.ndarray, depth: np.ndarray, scene: Scene) -> np.ndarray:
    exposure = scene.render.exposure
    gamma = scene.render.gamma
    mapped = 1.0 - np.exp(-np.maximum(radiance, 0.0) * exposure)
    encoded = np.power(mapped, 1.0 / gamma)
    rgb = np.floor(np.clip(encoded, 0.0, 1.0) * 255.0 + 0.5).astype(np.uint8)
    opacity = -np.expm1(-np.maximum(depth, 0.0))
    alpha = np.floor(np.clip(opacity, 0.0, 1.0) * 255.0 + 0.5).astype(
        np.uint8
    )
    return np.concatenate([rgb, alpha[:, None]], axis=1)


def render_tile(
    scene: Scene,
    step: float,
    chunk: ChunkLayout,
    should_stop: Callable[[], bool] | None = None,
) -> TileResult:
    origins, directions = _camera_rays(
        scene, chunk.x0, chunk.y0, chunk.width, chunk.height
    )
    entry_t, exit_t, hit = _intersect_volume(origins, directions, scene.volume)
    pixel_count = origins.shape[0]

    radiance = np.zeros((pixel_count, 3), dtype=np.float64)
    depth = np.zeros(pixel_count, dtype=np.float64)
    transmittance = np.ones(pixel_count, dtype=np.float64)
    self_rgb = np.zeros(3, dtype=np.float64)
    light_rgb = {
        light.id: np.zeros(3, dtype=np.float64) for light in scene.lights
    }

    entry_output = entry_t.copy()
    exit_output = exit_t.copy()
    peak_density = np.full(pixel_count, -1.0, dtype=np.float64)
    peak_t = np.full(pixel_count, -1.0, dtype=np.float64)
    peak_index = np.full((pixel_count, 3), -1, dtype=np.int16)
    index_min = np.full((pixel_count, 3), 32767, dtype=np.int16)
    index_max = np.full((pixel_count, 3), -1, dtype=np.int16)

    lengths = np.where(hit, np.maximum(exit_t - entry_t, 0.0), 0.0)
    ratio = lengths / step
    full_intervals = np.floor(ratio + 1e-12).astype(np.int64)
    remainder = lengths - full_intervals * step
    has_partial = remainder > max(1e-12, step * 1e-12)
    intervals = full_intervals + has_partial.astype(np.int64)
    intervals = np.where(hit & (lengths > 0.0), intervals, 0)
    maximum_intervals = int(intervals.max()) if intervals.size else 0

    total_samples = 0
    actual_step_min = math.inf
    actual_step_max = 0.0
    dims = np.asarray(scene.volume.dims, dtype=np.float64)
    span = scene.volume.bounds_max - scene.volume.bounds_min

    for interval_index in range(maximum_intervals):
        if (
            should_stop is not None
            and interval_index % 4 == 0
            and should_stop()
        ):
            raise RenderInterrupted(_signal_from_stop(should_stop))
        active = hit & (interval_index < intervals)
        active_indices = np.flatnonzero(active)
        if active_indices.size == 0:
            continue

        active_full = full_intervals[active_indices]
        active_remainder = lengths[active_indices] - active_full * step
        partial = interval_index >= active_full
        interval_step = np.full(active_indices.size, step, dtype=np.float64)
        interval_step[partial] = active_remainder[partial]
        interval_start = (
            entry_t[active_indices] + interval_index * step
        )
        sample_t = interval_start + 0.5 * interval_step
        active_origins = origins[active_indices]
        active_directions = directions[active_indices]
        positions = (
            active_origins + active_directions * sample_t[:, None]
        )
        density, grid_indices, inside = _sample_volume(scene.volume, positions)
        if not np.any(inside):
            continue

        inside_indices = np.flatnonzero(inside)
        pixel_indices = active_indices[inside_indices]
        sample_density = density[inside_indices]
        sample_step = interval_step[inside_indices]
        sample_positions = positions[inside_indices]
        sample_directions = active_directions[inside_indices]
        sample_time = sample_t[inside_indices]

        absorption, emission = scene.transfer.evaluate(sample_density)
        local_self = emission * sample_density[:, None]
        local_source = local_self.copy()
        interval_transmittance = transmittance[pixel_indices]

        for light in scene.lights:
            contribution = _light_sample_contribution(
                light, sample_positions, sample_directions, sample_density
            )
            local_source += contribution
            light_rgb[light.id] += np.sum(
                interval_transmittance[:, None] * contribution * sample_step[:, None],
                axis=0,
            )

        self_rgb += np.sum(
            interval_transmittance[:, None] * local_self * sample_step[:, None],
            axis=0,
        )
        radiance[pixel_indices] += (
            interval_transmittance[:, None]
            * local_source
            * sample_step[:, None]
        )
        interval_optical_depth = absorption * sample_step
        depth[pixel_indices] += interval_optical_depth
        transmittance[pixel_indices] *= np.exp(-interval_optical_depth)
        total_samples += pixel_indices.size
        actual_step_min = min(
            actual_step_min, float(np.min(sample_step))
        )
        actual_step_max = max(
            actual_step_max, float(np.max(sample_step))
        )

        sample_coordinates = (
            (sample_positions - scene.volume.bounds_min[None, :])
            / span[None, :]
            * (dims[None, :] - 1.0)
        )
        sample_cell = np.floor(sample_coordinates).astype(np.int16)
        sample_cell = np.clip(
            sample_cell, 0, np.asarray(scene.volume.dims, np.int16) - 1
        )
        index_min[pixel_indices] = np.minimum(
            index_min[pixel_indices], sample_cell
        )
        index_max[pixel_indices] = np.maximum(
            index_max[pixel_indices], sample_cell
        )
        update_peak = sample_density > peak_density[pixel_indices]
        if np.any(update_peak):
            peak_pixels = pixel_indices[update_peak]
            peak_density[peak_pixels] = sample_density[update_peak]
            peak_t[peak_pixels] = sample_time[update_peak]
            peak_index[peak_pixels] = sample_cell[update_peak]

    rgba = _tone_map_rgba(radiance, depth, scene).reshape(
        chunk.height, chunk.width, 4
    )
    depth_output = depth.astype(np.float32).reshape(chunk.height, chunk.width)
    no_index = np.any(index_max < 0, axis=1)
    index_min[no_index] = -1
    index_max[no_index] = -1
    if actual_step_min is math.inf:
        actual_step_min = 0.0

    return TileResult(
        chunk_id=chunk.chunk_id,
        x0=chunk.x0,
        y0=chunk.y0,
        width=chunk.width,
        height=chunk.height,
        rgba=rgba,
        depth=depth_output,
        radiance=radiance.reshape(chunk.height, chunk.width, 3),
        entry_t=entry_output.reshape(chunk.height, chunk.width),
        exit_t=exit_output.reshape(chunk.height, chunk.width),
        peak_density=peak_density.reshape(chunk.height, chunk.width),
        peak_t=peak_t.reshape(chunk.height, chunk.width),
        peak_index=peak_index.reshape(chunk.height, chunk.width, 3),
        index_min=index_min.reshape(chunk.height, chunk.width, 3),
        index_max=index_max.reshape(chunk.height, chunk.width, 3),
        sample_count=total_samples,
        actual_step_min=actual_step_min,
        actual_step_max=actual_step_max,
        self_rgb=self_rgb,
        light_rgb=light_rgb,
    )


def _signal_from_stop(should_stop: Callable[[], bool]) -> int:
    signal_number = getattr(should_stop, "signal_number", None)
    return int(signal_number) if signal_number is not None else 15


def render_image(
    scene: Scene,
    step: float,
    should_stop: Callable[[], bool] | None = None,
    keep_auxiliary: bool = True,
) -> ImageResult:
    width = scene.camera.width
    height = scene.camera.height
    chunks = make_chunk_layout(
        width,
        height,
        scene.render.chunk_width,
        scene.render.chunk_height,
    )
    rgba = np.zeros((height, width, 4), dtype=np.uint8)
    depth = np.zeros((height, width), dtype=np.float32)
    radiance = np.zeros((height, width, 3), dtype=np.float64)
    entry_t = np.full((height, width), -1.0, dtype=np.float64)
    exit_t = np.full((height, width), -1.0, dtype=np.float64)
    peak_density = np.full((height, width), -1.0, dtype=np.float64)
    peak_t = np.full((height, width), -1.0, dtype=np.float64)
    peak_index = np.full((height, width, 3), -1, dtype=np.int16)
    index_min = np.full((height, width, 3), -1, dtype=np.int16)
    index_max = np.full((height, width, 3), -1, dtype=np.int16)
    self_rgb = np.zeros(3, dtype=np.float64)
    light_rgb = {
        light.id: np.zeros(3, dtype=np.float64) for light in scene.lights
    }
    total_samples = 0
    actual_step_min = math.inf
    actual_step_max = 0.0

    for chunk in chunks:
        tile = render_tile(scene, step, chunk, should_stop=should_stop)
        y1 = chunk.y0 + chunk.height
        x1 = chunk.x0 + chunk.width
        rgba[chunk.y0 : y1, chunk.x0 : x1] = tile.rgba
        depth[chunk.y0 : y1, chunk.x0 : x1] = tile.depth
        radiance[chunk.y0 : y1, chunk.x0 : x1] = tile.radiance
        if keep_auxiliary:
            entry_t[chunk.y0 : y1, chunk.x0 : x1] = tile.entry_t
            exit_t[chunk.y0 : y1, chunk.x0 : x1] = tile.exit_t
            peak_density[chunk.y0 : y1, chunk.x0 : x1] = tile.peak_density
            peak_t[chunk.y0 : y1, chunk.x0 : x1] = tile.peak_t
            peak_index[chunk.y0 : y1, chunk.x0 : x1] = tile.peak_index
            index_min[chunk.y0 : y1, chunk.x0 : x1] = tile.index_min
            index_max[chunk.y0 : y1, chunk.x0 : x1] = tile.index_max
        total_samples += tile.sample_count
        if tile.sample_count:
            actual_step_min = min(actual_step_min, tile.actual_step_min)
            actual_step_max = max(actual_step_max, tile.actual_step_max)
        self_rgb += tile.self_rgb
        for light_id, contribution in tile.light_rgb.items():
            light_rgb[light_id] += contribution

    if actual_step_min is math.inf:
        actual_step_min = 0.0
    return ImageResult(
        rgba=rgba,
        depth=depth,
        radiance=radiance,
        entry_t=entry_t,
        exit_t=exit_t,
        peak_density=peak_density,
        peak_t=peak_t,
        peak_index=peak_index,
        index_min=index_min,
        index_max=index_max,
        sample_count=total_samples,
        actual_step_min=actual_step_min,
        actual_step_max=actual_step_max,
        self_rgb=self_rgb,
        light_rgb=light_rgb,
    )

