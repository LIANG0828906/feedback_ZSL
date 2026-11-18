"""Chunked deterministic software rasterizer with separable layers."""

from __future__ import annotations

from dataclasses import dataclass

import numpy as np

from .camera import Camera
from .geometry import FacetMaterial, fresnel_schlick, intersect_facet_rays, lead_distance
from .model import SceneModel


def _normalize(vector: np.ndarray) -> np.ndarray:
    return vector / np.linalg.norm(vector)


@dataclass
class ChunkResult:
    row_start: int
    row_end: int
    rgb: np.ndarray
    transmission: np.ndarray
    reflection: np.ndarray
    owner: np.ndarray
    lead: np.ndarray
    owner_lanes: np.ndarray


class Rasterizer:
    def __init__(self, model: SceneModel):
        self.model = model
        self.camera = Camera.from_scene(model)
        table_size = int(model.texture["table_size"])
        scale = float(model.texture["scale"])
        self.materials = {
            facet.stable_id: FacetMaterial(facet, model.seed, table_size, scale)
            for facet in model.facets
        }
        self.sun_direction = _normalize(np.asarray(model.directional_light["direction"], dtype=np.float64))
        self.sun_color = np.asarray(model.directional_light["color"], dtype=np.float64)
        self.sun_energy = float(model.directional_light["irradiance"])
        self.fill_direction = _normalize(np.asarray(model.interior_light["direction"], dtype=np.float64))
        self.fill_color = np.asarray(model.interior_light["color"], dtype=np.float64)
        self.fill_energy = float(model.interior_light["irradiance"])
        self.background = np.asarray(model.glass["background_color"], dtype=np.float64)
        self.lead_color = np.asarray(model.glass["lead_color"], dtype=np.float64)
        self.reflection_tint = np.asarray(model.glass["reflection_tint"], dtype=np.float64)
        self.reflection_gain = float(model.glass["reflection_gain"])
        self.width = int(model.render["width"])
        self.height = int(model.render["height"])
        self.chunk_size = int(model.render["chunk_size"])
        self.samples = int(model.render["primary_samples"])

    @property
    def chunk_count(self) -> int:
        return (self.height + self.chunk_size - 1) // self.chunk_size

    def chunk_bounds(self, chunk_index: int) -> tuple[int, int]:
        row_start = chunk_index * self.chunk_size
        return row_start, min(self.height, row_start + self.chunk_size)

    def render_chunk(self, chunk_index: int) -> ChunkResult:
        row_start, row_end = self.chunk_bounds(chunk_index)
        rows = row_end - row_start
        origins, directions, _pixels = self.camera.pixel_rays(row_start, row_end)

        ray_count = origins.shape[0]
        hit_id = np.zeros(ray_count, dtype=np.uint16)
        hit_t = np.full(ray_count, np.inf, dtype=np.float64)

        # Traversal order cannot affect the result: nearest t wins, equal t
        # ties resolve to the lowest stable id (ascending facet ordering).
        for facet in self.model.facets:
            t, hit = intersect_facet_rays(origins, directions, facet)
            closer = hit & (t < hit_t - 1e-12)
            finite_prior = np.isfinite(hit_t)
            finite_t = np.where(np.isfinite(t), t, 1e30)
            safe_delta = np.abs(finite_t - np.where(finite_prior, hit_t, 1e30))
            equal = hit & finite_prior & (safe_delta <= 1e-12)
            winner = closer | (equal & (facet.stable_id < np.maximum(hit_id, 1)))
            hit_t = np.where(winner, t, hit_t)
            hit_id = np.where(winner, np.uint16(facet.stable_id), hit_id)

        points = origins + directions * hit_t[:, None]
        transmission_ray = np.zeros((ray_count, 3), dtype=np.float64)
        reflection_ray = np.zeros((ray_count, 3), dtype=np.float64)
        lead_ray = np.zeros(ray_count, dtype=np.float64)

        for facet in self.model.facets:
            mask = hit_id == facet.stable_id
            if not np.any(mask):
                continue
            transmitted, reflected, lead_weight = self._shade_facet(
                facet, points[mask], directions[mask]
            )
            transmission_ray[mask] = transmitted
            reflection_ray[mask] = reflected
            lead_ray[mask] = lead_weight

        lane_shape = (rows, self.width, self.samples)
        transmission_layer = transmission_ray.reshape(lane_shape + (3,)).mean(axis=2)
        reflection_layer = reflection_ray.reshape(lane_shape + (3,)).mean(axis=2)
        lead = lead_ray.reshape(lane_shape).mean(axis=2)

        lane_ids = hit_id.reshape(lane_shape)
        fill_id = np.where(lane_ids == 0, np.iinfo(np.uint16).max, lane_ids)
        owner = np.where(np.any(lane_ids > 0, axis=2), fill_id.min(axis=2), 0).astype(np.uint16)
        # Full per-pixel hit list sorted by stable id, independent of input order.
        owner_lanes = np.sort(np.where(lane_ids == 0, 0, lane_ids), axis=2).astype(np.uint16)

        rgb_layer = np.broadcast_to(self.background, (rows, self.width, 3)).copy()
        composite = rgb_layer + transmission_layer + reflection_layer
        composite = (1.0 - lead[:, :, None]) * composite + lead[:, :, None] * self.lead_color
        rgb_layer = np.clip(composite, 0.0, 1.0)

        return ChunkResult(
            row_start=row_start,
            row_end=row_end,
            rgb=rgb_layer,
            transmission=transmission_layer,
            reflection=reflection_layer,
            owner=owner,
            lead=lead,
            owner_lanes=owner_lanes,
        )

    def _shade_facet(self, facet, points: np.ndarray, view_directions: np.ndarray):
        material = self.materials[facet.stable_id]
        mottle = material.mottle(points)

        # All facets sit in the z=0 plane. The front normal faces the interior
        # camera (-z); the exterior sun arrives from the +z side.
        normal = np.array([0.0, 0.0, -1.0], dtype=np.float64)
        cos_sun = float(np.clip(np.dot(self.sun_direction, -normal), 0.0, 1.0))
        cos_view = np.clip(view_directions @ normal, 0.0, 1.0)

        # Per-channel Schlick Fresnel keeps front-lit cobalt glass below
        # clipping while side-facing ruby glass retains red/blue separation.
        r0 = np.clip(facet.glaze_reflectance * self.reflection_tint, 0.0, 1.0)
        fresnel = fresnel_schlick(cos_view, r0)
        transmittance = facet.transmittance * mottle
        glass_tint = facet.transmission_rgb[None, :] * transmittance[:, None]
        direct = self.sun_color * (self.sun_energy * max(cos_sun, 0.18))
        incident = direct + self.fill_color * self.fill_energy
        transmitted = np.clip(glass_tint[None, :] * (1.0 - fresnel) * incident[None, :], 0.0, 1.0)

        half_vectors = view_directions + (-self.sun_direction)[None, :]
        half_vectors /= np.linalg.norm(half_vectors, axis=1, keepdims=True)
        specular = np.clip(half_vectors @ normal, 0.0, 1.0) ** 32
        reflected = (
            self.reflection_tint[None, :]
            * facet.glaze_reflectance
            * mottle[:, None]
            * self.reflection_gain
            * (0.65 * fresnel + 0.35 * specular[:, None] + 0.25)
            * self.sun_energy
        )
        reflected = np.clip(reflected, 0.0, 1.0)

        distance = lead_distance(points, facet)
        half_width = facet.lead_width * 0.5
        lead_weight = np.clip((half_width - distance) / max(half_width, 1e-9) + 0.5, 0.0, 1.0)
        return transmitted, reflected, lead_weight


def encode_sqrt_gamma(linear: np.ndarray) -> np.ndarray:
    """Deterministic sqrt gamma encode to uint8 without overflow."""
    clipped = np.clip(linear, 0.0, 1.0)
    return np.rint(np.sqrt(clipped) * 255.0).astype(np.uint8)
