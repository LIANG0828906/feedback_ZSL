"""Deterministic brush plan generation and rasterization."""

from __future__ import annotations

import hashlib
import math
from dataclasses import dataclass
from typing import Callable

import numpy as np

from .errors import SeedDivergenceError
from .model import Camera, RenderParams, Scene
from .npr import palette, posterize
from .raster import shade_scene
from .seeds import stroke_channels, stroke_fingerprint


@dataclass(frozen=True)
class StrokeDescriptor:
    stroke_id: int
    u: float
    v: float
    angle: float
    length_norm: float
    width_norm: float
    bend_norm: float
    color: tuple[int, int, int]
    level: int
    reverse: bool
    fingerprint: str


@dataclass(frozen=True)
class StrokePlan:
    descriptors: tuple[StrokeDescriptor, ...]
    fingerprint_root: str
    guide_width: int
    guide_height: int
    guide_levels: np.ndarray
    guide_rgb: np.ndarray
    ids: np.ndarray
    u: np.ndarray
    v: np.ndarray
    angle: np.ndarray
    length_norm: np.ndarray
    width_norm: np.ndarray
    bend_norm: np.ndarray
    colors: np.ndarray
    reverse: np.ndarray

    @property
    def count(self) -> int:
        return len(self.descriptors)


def _root_fingerprint(descriptors: tuple[StrokeDescriptor, ...]) -> str:
    digest = hashlib.sha256()
    for descriptor in descriptors:
        digest.update(descriptor.fingerprint.encode("ascii"))
        digest.update(b"|")
    return digest.hexdigest()


def build_stroke_plan(
    scene: Scene,
    camera: Camera,
    params: RenderParams,
    content_hash: str,
) -> StrokePlan:
    guide = shade_scene(
        scene,
        camera,
        params.guide_width,
        params.guide_height,
        quality="preview",
    )
    guide_levels, guide_rgb = posterize(guide.rgb, params.levels)
    colors = palette(params.levels)
    descriptors: list[StrokeDescriptor] = []
    for gy in range(params.guide_height):
        for gx in range(params.guide_width):
            stroke_id = gy * params.guide_width + gx
            channels = stroke_channels(content_hash, stroke_id)
            if channels.presence >= params.density:
                continue
            u = (gx + 0.5 + (channels.anchor_jitter_x - 0.5) * 0.9) / params.guide_width
            v = (gy + 0.5 + (channels.anchor_jitter_y - 0.5) * 0.9) / params.guide_height
            if not (0.0 <= u <= 1.0 and 0.0 <= v <= 1.0):
                continue
            level = int(guide_levels[gy, gx])
            # Anchor color follows the guide's posterized color, with a tiny
            # deterministic per-stroke tint.  It never consults global RNG.
            anchor_rgb = guide_rgb[gy, gx].astype(np.float64) / 255.0
            tinted = anchor_rgb * 0.84 + colors[level] * 0.16
            tinted += (channels.tint_perturbation - 0.5) * 0.08
            color = tuple(
                int(np.clip(round(channel * 255.0), 0, 255)) for channel in tinted
            )
            length_ref = params.length * (0.72 + 0.56 * channels.length_perturbation)
            length_norm = length_ref / float(params.stroke_reference_width)
            width_ref = 0.55 + 0.45 * channels.width_perturbation
            bend_sign = -1.0 if channels.bend_sign < 0.5 else 1.0
            bend_norm = (
                (channels.bend_amount - 0.5)
                * 0.58
                * length_norm
                * bend_sign
            )
            descriptors.append(
                StrokeDescriptor(
                    stroke_id=stroke_id,
                    u=float(np.clip(u, 0.0, 1.0)),
                    v=float(np.clip(v, 0.0, 1.0)),
                    angle=float(channels.angle * math.tau),
                    length_norm=float(length_norm),
                    width_norm=width_ref / float(params.stroke_reference_width),
                    bend_norm=float(bend_norm),
                    color=color,
                    level=level,
                    reverse=channels.order >= 0.5,
                    fingerprint=stroke_fingerprint(content_hash, stroke_id),
                )
            )
    descriptors.sort(key=lambda item: item.stroke_id)
    frozen = tuple(descriptors)
    ids = np.asarray([item.stroke_id for item in frozen], dtype=np.int64)
    u = np.asarray([item.u for item in frozen], dtype=np.float64)
    v = np.asarray([item.v for item in frozen], dtype=np.float64)
    angle = np.asarray([item.angle for item in frozen], dtype=np.float64)
    length_norm = np.asarray([item.length_norm for item in frozen], dtype=np.float64)
    width_norm = np.asarray([item.width_norm for item in frozen], dtype=np.float64)
    bend_norm = np.asarray([item.bend_norm for item in frozen], dtype=np.float64)
    colors = np.asarray([item.color for item in frozen], dtype=np.float64)
    reverse = np.asarray([item.reverse for item in frozen], dtype=bool)
    return StrokePlan(
        descriptors=frozen,
        fingerprint_root=_root_fingerprint(frozen),
        guide_width=params.guide_width,
        guide_height=params.guide_height,
        guide_levels=guide_levels,
        guide_rgb=guide_rgb,
        ids=ids,
        u=u,
        v=v,
        angle=angle,
        length_norm=length_norm,
        width_norm=width_norm,
        bend_norm=bend_norm,
        colors=colors,
        reverse=reverse,
    )


def verify_plan_seed_contract(
    plan: StrokePlan,
    content_hash: str,
    expected_root: str | None = None,
) -> None:
    seen: set[int] = set()
    for descriptor in plan.descriptors:
        if descriptor.stroke_id in seen:
            raise SeedDivergenceError(
                descriptor.stroke_id, "duplicate stroke id in plan"
            )
        seen.add(descriptor.stroke_id)
        expected = stroke_fingerprint(content_hash, descriptor.stroke_id)
        if descriptor.fingerprint != expected:
            raise SeedDivergenceError(
                descriptor.stroke_id,
                "descriptor fingerprint was not derived from scene hash and stroke index",
            )
    root = _root_fingerprint(plan.descriptors)
    if root != plan.fingerprint_root or (
        expected_root is not None and root != expected_root
    ):
        bad_id = plan.descriptors[0].stroke_id if plan.descriptors else 0
        raise SeedDivergenceError(bad_id, "plan fingerprint root differs")


def rasterize_strokes_tile(
    plan: StrokePlan,
    width: int,
    height: int,
    x0: int,
    y0: int,
    x1: int,
    y1: int,
    *,
    sample_factor: int,
    cache_budget: int = 4096,
    cache_lease: Callable[[int], "CacheLease"] | None = None,
) -> tuple[np.ndarray, np.ndarray, tuple[int, ...], int]:
    tw = x1 - x0
    th = y1 - y0
    pixel_count = tw * th
    counts = np.zeros(pixel_count, dtype=np.int32)
    color_sums = np.zeros((pixel_count, 3), dtype=np.float64)
    painted_ids: set[int] = set()
    total_points = 0
    if plan.count == 0:
        rgba = np.zeros((th, tw, 4), dtype=np.uint8)
        return rgba, np.zeros((th, tw), dtype=bool), (), 0
    weight_totals = np.zeros(pixel_count, dtype=np.float64)
    max_length_px = max(1.0, float(np.max(plan.length_norm)) * width)
    samples = max(2, int(math.ceil(max_length_px / max(1, sample_factor))) + 1)
    samples = min(samples, 257)
    sample_t = np.linspace(0.0, 1.0, samples, dtype=np.float64)
    sample_count = samples
    budget = max(1, int(cache_budget))
    for start in range(0, plan.count, budget):
        stop = min(start + budget, plan.count)
        lease = cache_lease(stop - start) if cache_lease is not None else None
        if lease is not None:
            lease.__enter__()
        try:
            ids = plan.ids[start:stop]
            u = plan.u[start:stop]
            v = plan.v[start:stop]
            angles = plan.angle[start:stop]
            lengths = np.maximum(1.0, plan.length_norm[start:stop] * width)
            bends = plan.bend_norm[start:stop] * width
            widths = np.clip(plan.width_norm[start:stop] * width, 0.05, 1.0)
            colors = plan.colors[start:stop]
            reverse = plan.reverse[start:stop]
            radius = np.maximum(lengths * 0.72, np.abs(bends) * 0.9) + 2.0
            centers_x = u * width
            centers_y = v * height
            select = (
                (centers_x + radius >= x0)
                & (centers_x - radius < x1)
                & (centers_y + radius >= y0)
                & (centers_y - radius < y1)
            )
            if not np.any(select):
                continue
            ids = ids[select]
            u = u[select]
            v = v[select]
            angles = angles[select]
            lengths = lengths[select]
            bends = bends[select]
            widths = widths[select]
            colors = colors[select]
            reverse = reverse[select]
            t = np.where(reverse[:, None], 1.0 - sample_t[None, :], sample_t[None, :])
            along = (t - 0.5) * lengths[:, None]
            perpendicular = bends[:, None] * 4.0 * t * (1.0 - t)
            cos_a = np.cos(angles)[:, None]
            sin_a = np.sin(angles)[:, None]
            x = u[:, None] * width + along * cos_a - perpendicular * sin_a
            y = v[:, None] * height + along * sin_a + perpendicular * cos_a
            pix_x = np.floor(x + 0.5).astype(np.int64)
            pix_y = np.floor(y + 0.5).astype(np.int64)
            inside = (
                (pix_x >= x0)
                & (pix_x < x1)
                & (pix_y >= y0)
                & (pix_y < y1)
            )
            if not np.any(inside):
                continue
            local_x = pix_x[inside] - x0
            local_y = pix_y[inside] - y0
            indices = local_y * tw + local_x
            point_colors = np.repeat(colors, sample_count, axis=0)[inside.ravel()]
            point_weights = np.repeat(widths, sample_count)[inside.ravel()]
            point_ids = np.repeat(ids, sample_count)[inside.ravel()]
            counts += np.bincount(indices, minlength=pixel_count).astype(np.int32)
            weight_sums = np.bincount(
                indices, weights=point_weights, minlength=pixel_count
            )
            weight_totals += weight_sums
            for channel in range(3):
                color_sums[:, channel] += np.bincount(
                    indices,
                    weights=point_colors[:, channel] * point_weights,
                    minlength=pixel_count,
                )
            painted_ids.update(int(value) for value in np.unique(point_ids).tolist())
            total_points += int(indices.size)
        finally:
            if lease is not None:
                lease.__exit__(None, None, None)
    # The accumulated sub-pixel width becomes alpha; a hairline at 2x
    # resolution therefore covers the same normalized image fraction.
    alpha = np.zeros(pixel_count, dtype=np.uint8)
    occupied = counts > 0
    alpha[occupied] = np.clip(weight_totals[occupied] * 225.0, 0, 225).astype(
        np.uint8
    )
    rgb = np.zeros((pixel_count, 3), dtype=np.uint8)
    safe_weights = np.maximum(weight_totals, 1e-9)
    rgb[occupied] = np.clip(
        color_sums[occupied] / safe_weights[occupied, None] + 0.5,
        0,
        255,
    ).astype(np.uint8)
    rgba = np.empty((th, tw, 4), dtype=np.uint8)
    rgba[..., :3] = rgb.reshape(th, tw, 3)
    rgba[..., 3] = alpha.reshape(th, tw)
    coverage = (alpha > 8).reshape(th, tw)
    return rgba, coverage, tuple(sorted(painted_ids)), total_points


class CacheLease:
    """Small context manager used by the resource governor."""

    def __init__(self, governor: object, count: int) -> None:
        self.governor = governor
        self.count = int(count)
        self.entered = False

    def __enter__(self) -> "CacheLease":
        enter = getattr(self.governor, "_acquire_stroke_slots")
        enter(self.count)
        self.entered = True
        return self

    def __exit__(self, exc_type: object, exc: object, tb: object) -> None:
        if self.entered:
            release = getattr(self.governor, "_release_stroke_slots")
            release(self.count)
            self.entered = False

