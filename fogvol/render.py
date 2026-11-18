"""Vectorized camera-ray volumetric integration."""

from __future__ import annotations

import math
from typing import Iterator

import numpy as np

from .scene import Light, Scene


RAY_START_RULE = (
    "camera_position + (ray_box_entry + 0.5*integration_step)*"
    "normalized_pixel_ray; integration_step=step/substeps; "
    "pixel centers use (x+0.5, y+0.5)"
)


def iter_chunks(
    width: int, height: int, chunk_size: tuple[int, int]
) -> Iterator[tuple[str, int, int, int, int, int, int]]:
    chunk_width, chunk_height = chunk_size
    columns = (width + chunk_width - 1) // chunk_width
    rows = (height + chunk_height - 1) // chunk_height
    for chunk_y in range(rows):
        for chunk_x in range(columns):
            x0 = chunk_x * chunk_width
            y0 = chunk_y * chunk_height
            x1 = min(width, x0 + chunk_width)
            y1 = min(height, y0 + chunk_height)
            chunk_id = f"r{chunk_y:03d}_c{chunk_x:03d}"
            yield chunk_id, chunk_y, chunk_x, x0, x1, y0, y1


def chunk_id_for_pixel(
    x: int, y: int, chunk_size: tuple[int, int]
) -> str:
    chunk_width, chunk_height = chunk_size
    return f"r{y // chunk_height:03d}_c{x // chunk_width:03d}"


class VolumetricRenderer:
    """Deterministic single-scattering stage-fog integrator."""

    def __init__(self, scene: Scene) -> None:
        self.scene = scene
        self.volume = scene.volume
        self.camera_data = scene.camera
        self.render = scene.render
        self.lights = scene.lights
        self._density = self.volume.density
        self._dims = np.asarray(self.volume.dimensions, dtype=np.int64)
        self._inv_voxel = 1.0 / self.volume.voxel_size
        self._box_min = self.volume.origin
        self._box_max = self.volume.maximum

        forward = self.camera_data.target - self.camera_data.position
        self._forward = forward / np.linalg.norm(forward)
        right = np.cross(self._forward, self.camera_data.up)
        self._right = right / np.linalg.norm(right)
        self._up = np.cross(self._right, self._forward)
        self._tan_half_fov = math.tan(
            math.radians(self.camera_data.fov_deg) * 0.5
        )
        self._outer_cos = np.asarray(
            [
                math.cos(math.radians(light.outer_cone_deg))
                for light in self.lights
            ],
            dtype=np.float64,
        )
        self._inner_cos = np.asarray(
            [
                math.cos(math.radians(light.inner_cone_deg))
                for light in self.lights
            ],
            dtype=np.float64,
        )

    def integration_step(self, declared_step: float) -> float:
        return float(declared_step) / float(self.render.substeps)

    def camera_rays(
        self, x0: int, x1: int, y0: int, y1: int
    ) -> tuple[np.ndarray, np.ndarray]:
        xs = np.arange(x0, x1, dtype=np.float64)
        ys = np.arange(y0, y1, dtype=np.float64)
        width = float(self.camera_data.width)
        height = float(self.camera_data.height)
        screen_x = (
            (2.0 * (xs + 0.5) / width - 1.0)
            * self.camera_data.aspect
            * self._tan_half_fov
        )
        screen_y = (
            (1.0 - 2.0 * (ys + 0.5) / height) * self._tan_half_fov
        )
        directions = (
            self._forward[None, None, :]
            + screen_x[None, :, None] * self._right[None, None, :]
            + screen_y[:, None, None] * self._up[None, None, :]
        )
        directions /= np.linalg.norm(directions, axis=-1, keepdims=True)
        origins = np.broadcast_to(
            self.camera_data.position,
            directions.shape,
        ).copy()
        return origins, directions

    def _ray_box(
        self, origins: np.ndarray, directions: np.ndarray
    ) -> tuple[np.ndarray, np.ndarray, np.ndarray]:
        zero = np.abs(directions) <= 1.0e-15
        inverse = np.zeros_like(directions)
        np.divide(1.0, directions, out=inverse, where=~zero)

        t_low = (self._box_min[None, :] - origins) * inverse
        t_high = (self._box_max[None, :] - origins) * inverse
        for axis in range(3):
            if np.any(zero[:, axis]):
                inside_axis = (
                    (origins[:, axis] >= self._box_min[axis])
                    & (origins[:, axis] <= self._box_max[axis])
                )
                t_low[:, axis] = np.where(
                    zero[:, axis],
                    np.where(inside_axis, -np.inf, np.inf),
                    t_low[:, axis],
                )
                t_high[:, axis] = np.where(
                    zero[:, axis],
                    np.where(inside_axis, np.inf, -np.inf),
                    t_high[:, axis],
                )
        swap = t_low > t_high
        low = np.where(swap, t_high, t_low)
        high = np.where(swap, t_low, t_high)
        entry = np.max(low, axis=1)
        exit_value = np.min(high, axis=1)
        entry = np.maximum(entry, 0.0)
        hit = exit_value > entry
        return entry, exit_value, hit

    def sample_density(self, points: np.ndarray) -> np.ndarray:
        """Trilinearly sample the density field; all points outside are zero."""

        original_shape = points.shape[:-1]
        flat = np.asarray(points, dtype=np.float64).reshape(-1, 3)
        if flat.size == 0:
            return np.zeros(original_shape, dtype=np.float64)

        local = (flat - self.volume.origin[None, :]) * self._inv_voxel[
            None, :
        ] - 0.5
        base = np.floor(local).astype(np.int64)
        frac = local - base
        nx, ny, nz = (int(value) for value in self._dims)
        in_box = np.all(
            (flat >= self._box_min[None, :])
            & (flat <= self._box_max[None, :]),
            axis=1,
        )

        x0, y0, z0 = base[:, 0], base[:, 1], base[:, 2]
        x1, y1, z1 = x0 + 1, y0 + 1, z0 + 1
        fx, fy, fz = frac[:, 0], frac[:, 1], frac[:, 2]

        valid_x0 = (x0 >= 0) & (x0 < nx)
        valid_x1 = (x1 >= 0) & (x1 < nx)
        valid_y0 = (y0 >= 0) & (y0 < ny)
        valid_y1 = (y1 >= 0) & (y1 < ny)
        valid_z0 = (z0 >= 0) & (z0 < nz)
        valid_z1 = (z1 >= 0) & (z1 < nz)

        cx0 = np.clip(x0, 0, nx - 1)
        cx1 = np.clip(x1, 0, nx - 1)
        cy0 = np.clip(y0, 0, ny - 1)
        cy1 = np.clip(y1, 0, ny - 1)
        cz0 = np.clip(z0, 0, nz - 1)
        cz1 = np.clip(z1, 0, nz - 1)

        w000 = (1 - fx) * (1 - fy) * (1 - fz) * valid_x0 * valid_y0 * valid_z0
        w100 = fx * (1 - fy) * (1 - fz) * valid_x1 * valid_y0 * valid_z0
        w010 = (1 - fx) * fy * (1 - fz) * valid_x0 * valid_y1 * valid_z0
        w110 = fx * fy * (1 - fz) * valid_x1 * valid_y1 * valid_z0
        w001 = (1 - fx) * (1 - fy) * fz * valid_x0 * valid_y0 * valid_z1
        w101 = fx * (1 - fy) * fz * valid_x1 * valid_y0 * valid_z1
        w011 = (1 - fx) * fy * fz * valid_x0 * valid_y1 * valid_z1
        w111 = fx * fy * fz * valid_x1 * valid_y1 * valid_z1

        result = (
            self._density[cx0, cy0, cz0] * w000
            + self._density[cx1, cy0, cz0] * w100
            + self._density[cx0, cy1, cz0] * w010
            + self._density[cx1, cy1, cz0] * w110
            + self._density[cx0, cy0, cz1] * w001
            + self._density[cx1, cy0, cz1] * w101
            + self._density[cx0, cy1, cz1] * w011
            + self._density[cx1, cy1, cz1] * w111
        )
        result *= in_box
        return result.reshape(original_shape)

    def _light_irradiance(
        self, points: np.ndarray, light: Light
    ) -> tuple[np.ndarray, np.ndarray]:
        vector = light.position[None, :] - points
        distance = np.linalg.norm(vector, axis=1)
        safe_distance = np.maximum(distance, 1.0e-12)
        to_light = vector / safe_distance[:, None]
        from_light = -to_light
        cosine = from_light @ light.direction

        outer = math.cos(math.radians(light.outer_cone_deg))
        inner = math.cos(math.radians(light.inner_cone_deg))
        denominator = max(inner - outer, 1.0e-7)
        cone_t = np.clip((cosine - outer) / denominator, 0.0, 1.0)
        cone = cone_t * cone_t * (3.0 - 2.0 * cone_t)

        fade_width = max(light.range * 0.06, 1.0e-4)
        range_t = np.clip((light.range - distance) / fade_width, 0.0, 1.0)
        range_window = range_t * range_t * (3.0 - 2.0 * range_t)

        falloff = light.intensity / (
            0.25 + np.power(safe_distance, light.falloff)
        )
        scalar = falloff * cone * range_window
        radiance = scalar[:, None] * light.color[None, :]
        return radiance, scalar > 0.0

    def _phase(self, cos_theta: np.ndarray) -> np.ndarray:
        g = self.render.phase_g
        denominator = np.maximum(
            1.0 + g * g - 2.0 * g * cos_theta, 1.0e-6
        )
        return (1.0 - g * g) / (
            4.0 * math.pi * np.power(denominator, 1.5)
        )

    def _light_transmittance(
        self, points: np.ndarray, light: Light
    ) -> np.ndarray:
        if points.shape[0] == 0:
            return np.ones(0, dtype=np.float64)
        vector = light.position[None, :] - points
        distance = np.linalg.norm(vector, axis=1)
        step_count = np.ceil(distance / self.render.light_step).astype(np.int64)
        step_count = np.clip(
            step_count, 1, self.render.light_max_samples
        )
        max_steps = int(np.max(step_count))
        index = np.arange(max_steps, dtype=np.float64)[None, :]
        fractions = (index + 0.5) / step_count[:, None]
        valid = index < step_count[:, None]
        samples = (
            points[:, None, :]
            + vector[:, None, :] * fractions[:, :, None]
        )
        density = self.sample_density(samples)
        path_step = distance / step_count
        optical_depth = np.sum(density * valid, axis=1) * (
            self.render.extinction * path_step
        )
        return np.exp(-optical_depth)

    def render_chunk(
        self,
        x0: int,
        x1: int,
        y0: int,
        y1: int,
        step: float,
    ) -> tuple[np.ndarray, np.ndarray, np.ndarray]:
        origins, direction_grid = self.camera_rays(x0, x1, y0, y1)
        shape = direction_grid.shape[:2]
        directions = direction_grid.reshape(-1, 3)
        origins = origins.reshape(-1, 3)
        point_count = directions.shape[0]

        entry, exit_value, hit = self._ray_box(origins, directions)
        sample_counts = np.ceil(
            np.maximum(exit_value - entry, 0.0) / step
        ).astype(np.int64)
        sample_counts = np.minimum(sample_counts, self.render.max_samples)
        sample_counts[~hit] = 0
        maximum_samples = int(np.max(sample_counts, initial=0))

        radiance = np.zeros((point_count, 3), dtype=np.float64)
        optical_depth = np.zeros(point_count, dtype=np.float64)
        light_radiance = np.zeros(
            (len(self.lights), point_count, 3), dtype=np.float64
        )
        if maximum_samples == 0:
            return (
                radiance.reshape(shape[0], shape[1], 3),
                optical_depth.reshape(shape),
                light_radiance.reshape(
                    len(self.lights), shape[0], shape[1], 3
                ),
            )

        sample_index = np.arange(maximum_samples, dtype=np.float64)[None, :]
        sample_t = entry[:, None] + (sample_index + 0.5) * step
        valid = sample_index < sample_counts[:, None]
        points = origins[:, None, :] + directions[:, None, :] * sample_t[
            :, :, None
        ]
        density = self.sample_density(points)
        density *= valid

        transmittance = np.ones(point_count, dtype=np.float64)
        scattering_ratio = self.render.scattering / self.render.extinction
        ambient = self.render.ambient
        for sample in range(maximum_samples):
            active = valid[:, sample] & (density[:, sample] > 0.0)
            if not np.any(active):
                continue
            local_density = density[:, sample]
            sigma_t = local_density * self.render.extinction
            segment_alpha = -np.expm1(-sigma_t * step)
            camera_weight = transmittance * segment_alpha * scattering_ratio

            radiance += camera_weight[:, None] * ambient[None, :]
            optical_depth += sigma_t * step

            sample_points = points[:, sample]
            for light_index, light in enumerate(self.lights):
                incident, candidate = self._light_irradiance(
                    sample_points, light
                )
                candidate &= active
                if not np.any(candidate):
                    continue
                candidate_indices = np.flatnonzero(candidate)
                selected_points = sample_points[candidate_indices]
                selected_directions = directions[candidate_indices]
                selected_incident = incident[candidate_indices]
                shadow = self._light_transmittance(selected_points, light)
                cos_theta = np.sum(
                    selected_directions
                    * (light.position[None, :] - selected_points),
                    axis=1,
                ) / np.maximum(
                    np.linalg.norm(
                        light.position[None, :] - selected_points, axis=1
                    ),
                    1.0e-12,
                )
                cos_theta = -cos_theta
                phase = self._phase(cos_theta)
                weight = (
                    camera_weight[candidate_indices]
                    * shadow
                    * phase
                )
                contribution = weight[:, None] * selected_incident
                radiance[candidate_indices] += contribution
                light_radiance[light_index, candidate_indices] += contribution

            transmittance *= np.exp(-sigma_t * step)

        return (
            radiance.reshape(shape[0], shape[1], 3),
            optical_depth.reshape(shape),
            light_radiance.reshape(len(self.lights), shape[0], shape[1], 3),
        )

    def render_all(
        self,
        step: float,
    ) -> tuple[np.ndarray, np.ndarray, np.ndarray]:
        height = self.camera_data.height
        width = self.camera_data.width
        radiance = np.zeros((height, width, 3), dtype=np.float64)
        optical_depth = np.zeros((height, width), dtype=np.float64)
        light_radiance = np.zeros(
            (len(self.lights), height, width, 3), dtype=np.float64
        )
        for _, _, _, x0, x1, y0, y1 in iter_chunks(
            width, height, self.render.chunk_size
        ):
            chunk_rgb, chunk_depth, chunk_lights = self.render_chunk(
                x0, x1, y0, y1, self.integration_step(step)
            )
            radiance[y0:y1, x0:x1] = chunk_rgb
            optical_depth[y0:y1, x0:x1] = chunk_depth
            light_radiance[:, y0:y1, x0:x1] = chunk_lights
        return radiance, optical_depth, light_radiance

    def tone_map(
        self, radiance: np.ndarray
    ) -> tuple[np.ndarray, dict[str, int | float]]:
        exposed = np.maximum(radiance, 0.0) * self.render.exposure
        if self.render.tonemap == "linear":
            mapped = exposed
        elif self.render.tonemap == "reinhard":
            mapped = exposed / (1.0 + exposed)
        else:
            mapped = (
                exposed * (2.51 * exposed + 0.03)
            ) / (exposed * (2.43 * exposed + 0.59) + 0.14)
        gamma_encoded = np.power(
            np.maximum(mapped, 0.0), 1.0 / self.render.gamma
        )
        clipped_channels = gamma_encoded > self.render.channel_max
        clipped_pixels = np.any(clipped_channels, axis=-1)
        encoded = np.clip(
            gamma_encoded, 0.0, self.render.channel_max
        )
        stats: dict[str, int | float] = {
            "clipped_channels": int(np.count_nonzero(clipped_channels)),
            "clipped_pixels": int(np.count_nonzero(clipped_pixels)),
            "total_pixels": int(encoded.shape[0] * encoded.shape[1]),
            "channel_max": float(self.render.channel_max),
        }
        return encoded, stats

    @staticmethod
    def opacity_from_depth(optical_depth: np.ndarray) -> np.ndarray:
        return -np.expm1(-optical_depth)

    def world_midpoint_for_pixel(self, x: int, y: int) -> np.ndarray:
        origins, directions = self.camera_rays(x, x + 1, y, y + 1)
        origin = origins.reshape(1, 3)
        direction = directions.reshape(1, 3)
        entry, exit_value, hit = self._ray_box(origin, direction)
        if not bool(hit[0]):
            return np.full(3, np.nan, dtype=np.float64)
        t = 0.5 * (entry[0] + exit_value[0])
        return origin[0] + direction[0] * t

