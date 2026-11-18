"""World-space spotlight projection, soft penumbrae and landing lists."""

from __future__ import annotations

import math
from dataclasses import dataclass
from typing import Any, Iterable

import numpy as np

from .patterns import PatternSet
from .scene import Layer, Leaf, Light, Receiver, Scene
from .util import smoothstep


@dataclass(frozen=True)
class LayerApertureField:
    layer: int
    x_min: float
    x_max: float
    y_min: float
    y_max: float
    aperture: np.ndarray
    thickness: np.ndarray

    @property
    def width(self) -> float:
        return self.x_max - self.x_min

    @property
    def height(self) -> float:
        return self.y_max - self.y_min

    def _sample_map(
        self, image: np.ndarray, qx: np.ndarray, qy: np.ndarray, outside: float
    ) -> np.ndarray:
        qx_arr = np.asarray(qx, dtype=np.float64)
        qy_arr = np.asarray(qy, dtype=np.float64)
        height, width = image.shape[:2]
        fx = (qx_arr - self.x_min) / self.width * width - 0.5
        fy = (qy_arr - self.y_min) / self.height * height - 0.5
        x0 = np.floor(fx).astype(np.int64)
        y0 = np.floor(fy).astype(np.int64)
        x1 = np.minimum(x0 + 1, width - 1)
        y1 = np.minimum(y0 + 1, height - 1)
        wx = fx - x0
        wy = fy - y0
        x0c = np.clip(x0, 0, width - 1)
        y0c = np.clip(y0, 0, height - 1)
        x1c = np.clip(x1, 0, width - 1)
        y1c = np.clip(y1, 0, height - 1)
        top = image[y0c, x0c] * (1.0 - wx) + image[y0c, x1c] * wx
        bottom = image[y1c, x0c] * (1.0 - wx) + image[y1c, x1c] * wx
        sampled = top * (1.0 - wy) + bottom * wy
        outside_mask = (
            (qx_arr < self.x_min)
            | (qx_arr > self.x_max)
            | (qy_arr < self.y_min)
            | (qy_arr > self.y_max)
        )
        return np.where(outside_mask, outside, sampled)

    def sample_aperture(self, qx: np.ndarray, qy: np.ndarray) -> np.ndarray:
        return np.clip(self._sample_map(self.aperture, qx, qy, 1.0), 0.0, 1.0)

    def sample_thickness(self, qx: np.ndarray, qy: np.ndarray) -> np.ndarray:
        return np.maximum(self._sample_map(self.thickness, qx, qy, 0.0), 0.0)

    def sample_soft(
        self,
        qx: np.ndarray,
        qy: np.ndarray,
        radius_m: np.ndarray,
        density: int,
        kernel_offsets: np.ndarray,
        kernel_weights: np.ndarray,
    ) -> np.ndarray:
        qx_arr = np.asarray(qx, dtype=np.float64)
        qy_arr = np.asarray(qy, dtype=np.float64)
        radius = np.maximum(np.asarray(radius_m, dtype=np.float64), 0.0)
        accumulated = np.zeros_like(qx_arr, dtype=np.float64)
        for (offset_x, offset_y), weight in zip(kernel_offsets, kernel_weights):
            sampled = self.sample_aperture(
                qx_arr + radius * float(offset_x),
                qy_arr + radius * float(offset_y),
            )
            accumulated += sampled * float(weight)
        return np.clip(accumulated, 0.0, 1.0)


@dataclass
class LightContribution:
    total: np.ndarray
    by_light: dict[str, np.ndarray]
    crossed_layer_weights: dict[str, np.ndarray]
    layer_increments: dict[str, np.ndarray]


def _make_kernel(density: int) -> tuple[np.ndarray, np.ndarray]:
    coordinates = np.linspace(-1.0, 1.0, density, dtype=np.float64)
    offsets: list[tuple[float, float]] = []
    weights: list[float] = []
    for oy in coordinates:
        for ox in coordinates:
            radius_sq = float(ox * ox + oy * oy)
            if radius_sq > 1.000001:
                continue
            weight = math.exp(-1.65 * radius_sq)
            offsets.append((float(ox), float(oy)))
            weights.append(weight)
    weight_arr = np.asarray(weights, dtype=np.float64)
    weight_arr /= np.sum(weight_arr)
    return np.asarray(offsets, dtype=np.float64), weight_arr


class ProjectionSystem:
    def __init__(self, scene: Scene, patterns: PatternSet):
        self.scene = scene
        self.patterns = patterns
        self.fields = {
            layer.layer: self._build_layer_field(layer)
            for layer in sorted(scene.layers, key=lambda item: item.layer)
        }
        self.kernel_offsets, self.kernel_weights = _make_kernel(
            scene.render.shadow_sample_density
        )
        self.leaf_lookup = scene.leaf_lookup()

    def _build_layer_field(self, layer: Layer) -> LayerApertureField:
        leaves = sorted(
            [leaf for leaf in self.scene.leaves if leaf.layer == layer.layer],
            key=lambda item: item.leaf,
        )
        x_min = min(leaf.rect_x[0] for leaf in leaves) - 0.14
        x_max = max(leaf.rect_x[1] for leaf in leaves) + 0.14
        y_min = min(leaf.rect_y[0] for leaf in leaves) - 0.14
        y_max = max(leaf.rect_y[1] for leaf in leaves) + 0.14
        width = int(max(384, min(1024, self.scene.render.pattern_resolution[1] * 8)))
        height = int(max(288, min(768, self.scene.render.pattern_resolution[0] * 6)))
        aperture = np.ones((height, width), dtype=np.float32)
        thickness = np.zeros((height, width), dtype=np.float32)
        xs = x_min + (np.arange(width, dtype=np.float64) + 0.5) * (
            (x_max - x_min) / width
        )
        ys = y_min + (np.arange(height, dtype=np.float64) + 0.5) * (
            (y_max - y_min) / height
        )
        gx, gy = np.meshgrid(xs, ys)
        for leaf in leaves:
            mask = (
                (gx >= leaf.rect_x[0])
                & (gx <= leaf.rect_x[1])
                & (gy >= leaf.rect_y[0])
                & (gy <= leaf.rect_y[1])
            )
            if not np.any(mask):
                continue
            u = (gx[mask] - leaf.rect_x[0]) / leaf.width
            v = (gy[mask] - leaf.rect_y[0]) / leaf.height
            aperture[mask] = self.patterns.sample_aperture(
                leaf.layer, leaf.leaf, u, v
            ).astype(np.float32)
            thickness[mask] = np.float32(leaf.thickness)
        return LayerApertureField(
            layer=layer.layer,
            x_min=x_min,
            x_max=x_max,
            y_min=y_min,
            y_max=y_max,
            aperture=np.ascontiguousarray(aperture),
            thickness=np.ascontiguousarray(thickness),
        )

    def softness_width(
        self,
        light_to_leaf_distance: float | np.ndarray,
        leaf_to_receiver_distance: float | np.ndarray,
    ) -> np.ndarray:
        settings = self.scene.render
        return (
            settings.softness_base
            + settings.softness_per_leaf_receiver
            * np.asarray(leaf_to_receiver_distance, dtype=np.float64)
            + settings.softness_per_light_leaf
            * np.asarray(light_to_leaf_distance, dtype=np.float64)
        )

    def beam_at(
        self, light: Light, points: np.ndarray, normals: np.ndarray
    ) -> tuple[np.ndarray, np.ndarray, np.ndarray]:
        points = np.asarray(points, dtype=np.float64)
        normals = np.asarray(normals, dtype=np.float64)
        to_light = light.position[None, :] - points
        distance = np.linalg.norm(to_light, axis=1)
        direction = to_light / np.maximum(distance[:, None], 1e-12)
        cosine_axis = (-direction) @ light.axis
        spot = smoothstep(light.cos_outer, light.cos_inner, cosine_axis)
        distance_term = light.intensity / np.power(
            1.0 + distance * distance, float(light.falloff) * 0.5
        )
        cosine_surface = np.maximum(np.sum(direction * normals, axis=1), 0.0)
        beam = np.maximum(distance_term * spot * cosine_surface, 0.0)
        return beam, direction, distance

    def _single_light(
        self, light: Light, points: np.ndarray, normals: np.ndarray
    ) -> tuple[np.ndarray, np.ndarray, np.ndarray, np.ndarray]:
        points = np.asarray(points, dtype=np.float64)
        normals = np.asarray(normals, dtype=np.float64)
        count = points.shape[0]
        beam, _, _ = self.beam_at(light, points, normals)
        crossed = np.zeros((count, len(self.scene.layers)), dtype=np.float64)
        increments = np.zeros_like(crossed)
        passthrough = np.ones(count, dtype=np.float64)
        crossed_any = np.zeros(count, dtype=bool)
        for layer_index, layer in sorted(
            enumerate(self.scene.layers), key=lambda item: item[1].layer
        ):
            source_vector = light.position[None, :] - points
            denominator = light.position[2] - points[:, 2]
            valid = np.abs(denominator) > 1e-12
            parameter = np.zeros(count, dtype=np.float64)
            parameter[valid] = (
                layer.z_center - points[valid, 2]
            ) / denominator[valid]
            crosses = valid & (parameter > 1e-7) & (parameter < 1.0 - 1e-7)
            if not np.any(crosses):
                continue
            crossed_any |= crosses
            q = points + parameter[:, None] * source_vector
            light_to_leaf = np.linalg.norm(light.position[None, :] - q, axis=1)
            leaf_to_receiver = np.linalg.norm(q - points, axis=1)
            width = self.softness_width(light_to_leaf, leaf_to_receiver)
            field = self.fields[layer.layer]
            soft_aperture = field.sample_soft(
                q[:, 0],
                q[:, 1],
                width,
                self.scene.render.shadow_sample_density,
                self.kernel_offsets,
                self.kernel_weights,
            )
            thickness = field.sample_thickness(q[:, 0], q[:, 1])
            local_direction = light.position[None, :] - q
            local_distance = np.linalg.norm(local_direction, axis=1)
            local_direction = local_direction / np.maximum(local_distance[:, None], 1e-12)
            incidence = np.maximum(np.abs(local_direction[:, 2]), 0.20)
            attenuation = np.exp(-0.55 * thickness / incidence)
            weight = np.where(crosses, np.clip(soft_aperture * attenuation, 0.0, 1.0), 0.0)
            crossed[:, layer_index] = weight
            increment = weight * passthrough
            increments[:, layer_index] = increment
            passthrough = passthrough * (1.0 - weight)
        transmission = np.where(
            crossed_any,
            np.clip(1.0 - passthrough, 0.0, 1.0),
            1.0,
        )
        contribution = np.maximum(beam * transmission, 0.0)
        return contribution, crossed, increments, transmission

    def light_contributions(
        self,
        points: np.ndarray,
        normals: np.ndarray,
        light_order: Iterable[str] | None = None,
    ) -> LightContribution:
        points = np.asarray(points, dtype=np.float64)
        normals = np.asarray(normals, dtype=np.float64)
        if points.ndim == 1:
            points = points[None, :]
        if normals.ndim == 1:
            normals = np.broadcast_to(normals, points.shape).copy()
        if light_order is None:
            ordered_ids = [light.id for light in self.scene.lights]
        else:
            requested = list(light_order)
            known = {light.id for light in self.scene.lights}
            if set(requested) != known or len(requested) != len(known):
                raise ValueError("light_order must be a permutation of scene lights")
            # Canonicalise the accumulation order so traversal order cannot change bits.
            ordered_ids = sorted(requested, key=lambda item: item.encode("utf-8"))
        light_by_id = {light.id: light for light in self.scene.lights}
        by_light: dict[str, np.ndarray] = {}
        crossed_by_light: dict[str, np.ndarray] = {}
        increments_by_light: dict[str, np.ndarray] = {}
        for light_id in ordered_ids:
            contribution, crossed, increments, _ = self._single_light(
                light_by_id[light_id], points, normals
            )
            by_light[light_id] = contribution
            crossed_by_light[light_id] = crossed
            increments_by_light[light_id] = increments
        total = np.zeros(points.shape[0], dtype=np.float64)
        for light_id in ordered_ids:
            total = total + by_light[light_id]
        return LightContribution(
            total=np.maximum(total, 0.0),
            by_light=by_light,
            crossed_layer_weights=crossed_by_light,
            layer_increments=increments_by_light,
        )

    def receiver_radiance(
        self,
        points: np.ndarray,
        normals: np.ndarray,
        light_order: Iterable[str] | None = None,
    ) -> tuple[np.ndarray, LightContribution]:
        contribution = self.light_contributions(points, normals, light_order)
        points_arr = np.asarray(points, dtype=np.float64)
        if points_arr.ndim == 1:
            points_arr = points_arr[None, :]
        radiance = np.zeros((points_arr.shape[0], 3), dtype=np.float64)
        ordered_ids = sorted(
            contribution.by_light, key=lambda item: item.encode("utf-8")
        )
        for light_id in ordered_ids:
            light = next(light for light in self.scene.lights if light.id == light_id)
            radiance = radiance + contribution.by_light[light_id][:, None] * light.color
        return np.maximum(radiance, 0.0), contribution

    def landing_points(self) -> list[dict[str, Any]]:
        records: list[dict[str, Any]] = []
        receivers = self.scene.receivers
        layer_by_id = {layer.layer: layer for layer in self.scene.layers}
        for light in sorted(self.scene.lights, key=lambda item: item.id.encode("utf-8")):
            for leaf in sorted(self.scene.leaves, key=lambda item: item.key):
                pattern = self.patterns[leaf.key]
                aperture_sample = self.patterns.sample_aperture(
                    leaf.layer,
                    leaf.leaf,
                    pattern.hole_centers[:, 0],
                    pattern.hole_centers[:, 1],
                )
                for hole_index, (u, v) in enumerate(pattern.hole_centers):
                    source = np.asarray(
                        [
                            leaf.rect_x[0] + float(u) * leaf.width,
                            leaf.rect_y[0] + float(v) * leaf.height,
                            leaf.z_center,
                        ],
                        dtype=np.float64,
                    )
                    adjacent: dict[str, Any] | None = None
                    if leaf.layer < 3:
                        outer_layer = layer_by_id[leaf.layer + 1]
                        t = (outer_layer.z_center - light.position[2]) / (
                            source[2] - light.position[2]
                        )
                        if t > 0.0:
                            point = light.position + t * (source - light.position)
                            x_ok = (
                                self.fields[outer_layer.layer].x_min
                                <= point[0]
                                <= self.fields[outer_layer.layer].x_max
                            )
                            y_ok = (
                                self.fields[outer_layer.layer].y_min
                                <= point[1]
                                <= self.fields[outer_layer.layer].y_max
                            )
                            adjacent = {
                                "layer": outer_layer.layer,
                                "point": [round(float(c), 9) for c in point],
                                "in_bounds": bool(x_ok and y_ok),
                                "attenuation": round(
                                    float(
                                        math.exp(
                                            -0.55
                                            * leaf.thickness
                                            / max(
                                                abs(
                                                    (light.position[2] - source[2])
                                                    / max(
                                                        np.linalg.norm(
                                                            light.position - source
                                                        ),
                                                        1e-12,
                                                    )
                                                ),
                                                0.2,
                                            )
                                        )
                                    ),
                                    9,
                                ),
                            }
                    receiver_records: list[dict[str, Any]] = []
                    for receiver_id in ("wall", "floor"):
                        receiver = receivers[receiver_id]
                        axis_index = {"x": 0, "y": 1, "z": 2}[receiver.plane_axis]
                        denominator = (
                            light.position[axis_index] - source[axis_index]
                        )
                        if abs(denominator) <= 1e-12:
                            continue
                        t = (
                            receiver.plane_value - light.position[axis_index]
                        ) / (
                            source[axis_index] - light.position[axis_index]
                        )
                        point = light.position + t * (source - light.position)
                        in_bounds = all(
                            receiver.bounds[axis][0] <= point[{"x": 0, "y": 1, "z": 2}[axis]]
                            <= receiver.bounds[axis][1]
                            for axis in receiver.bounds
                        )
                        source_distance = float(np.linalg.norm(point - source))
                        direction = (source - light.position) / max(
                            np.linalg.norm(source - light.position), 1e-12
                        )
                        incident_cosine = max(
                            abs(float(direction[axis_index])), 0.2
                        )
                        attenuation = math.exp(
                            -0.55 * leaf.thickness / incident_cosine
                        )
                        contribution = (
                            float(aperture_sample[hole_index])
                            * attenuation
                            / (1.0 + source_distance * source_distance)
                            if in_bounds
                            else 0.0
                        )
                        receiver_records.append(
                            {
                                "receiver": receiver_id,
                                "point": [round(float(c), 9) for c in point],
                                "in_bounds": bool(in_bounds),
                                "contribution": round(max(contribution, 0.0), 9),
                                "out_of_bounds_policy": "zero_contribution_unoccluded_field",
                            }
                        )
                    records.append(
                        {
                            "light": light.id,
                            "layer": leaf.layer,
                            "leaf": leaf.leaf,
                            "hole": hole_index,
                            "source_uv": [round(float(u), 9), round(float(v), 9)],
                            "source_point": [round(float(c), 9) for c in source],
                            "adjacent_outer": adjacent,
                            "receivers": receiver_records,
                        }
                    )
        return records

    def landing_digest(self, records: list[dict[str, Any]]) -> str:
        from .util import object_digest

        return object_digest(records)
