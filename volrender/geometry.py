"""Ray/medium intersection and deterministic sampling phase helpers."""

from __future__ import annotations

import math
from dataclasses import dataclass
from typing import Iterable

import numpy as np

from .model import MEDIUM_BOX, MEDIUM_SPHERE, Medium, Scene

EPS = 1e-12


def intersect_aabb(
    origin: np.ndarray,
    direction: np.ndarray,
    bounds_lo: np.ndarray,
    bounds_hi: np.ndarray,
) -> tuple[float, float] | None:
    t0 = -math.inf
    t1 = math.inf
    for axis in range(3):
        d = float(direction[axis])
        o = float(origin[axis])
        lo = float(bounds_lo[axis])
        hi = float(bounds_hi[axis])
        if abs(d) <= EPS:
            if o < lo or o > hi:
                return None
            continue
        inv = 1.0 / d
        ta = (lo - o) * inv
        tb = (hi - o) * inv
        if ta > tb:
            ta, tb = tb, ta
        if ta > t0:
            t0 = ta
        if tb < t1:
            t1 = tb
        if t1 < t0:
            return None
    return t0, t1


def _medium_interval(
    medium: Medium, origin: np.ndarray, direction: np.ndarray
) -> tuple[float, float] | None:
    if medium.kind == MEDIUM_BOX:
        lo, hi = medium.bounds
        interval = intersect_aabb(origin, direction, lo, hi)
        if interval is None:
            return None
        t0, t1 = interval
        if t1 <= 0.0:
            return None
        return max(t0, 0.0), t1
    if medium.kind == MEDIUM_SPHERE:
        center = medium.shape_a
        radius = float(medium.shape_b[0])
        oc = origin - center
        b = float(np.dot(oc, direction))
        c = float(np.dot(oc, oc) - radius * radius)
        disc = b * b - c
        if disc < 0.0:
            return None
        root = math.sqrt(max(disc, 0.0))
        ta = -b - root
        tb = -b + root
        if tb <= 0.0:
            return None
        return max(ta, 0.0), tb
    return None


def ray_medium_intervals(
    scene: Scene, origin: np.ndarray, direction: np.ndarray
) -> dict[int, tuple[float, float]]:
    intervals: dict[int, tuple[float, float]] = {}
    for medium in scene.media:
        interval = _medium_interval(medium, origin, direction)
        if interval is not None and interval[1] > interval[0] + EPS:
            intervals[medium.id] = interval
    return intervals


@dataclass(frozen=True)
class MediaSegment:
    t0: float
    t1: float
    medium_ids: tuple[int, ...]


def media_segments(
    scene: Scene, origin: np.ndarray, direction: np.ndarray
) -> list[MediaSegment]:
    intervals = ray_medium_intervals(scene, origin, direction)
    if not intervals:
        return []
    endpoints = sorted({value for interval in intervals.values() for value in interval})
    if len(endpoints) < 2:
        return []
    segments: list[MediaSegment] = []
    for left, right in zip(endpoints[:-1], endpoints[1:]):
        if right <= left + EPS:
            continue
        midpoint = (left + right) * 0.5
        active = tuple(
            sorted(
                medium_id
                for medium_id, (t0, t1) in intervals.items()
                if t0 < midpoint < t1
            )
        )
        if active:
            segments.append(MediaSegment(float(left), float(right), active))
    return segments


def sample_phase(pixel_x: int, pixel_y: int) -> float:
    """Global-coordinate sampling phase shared by every tile partition.

    The phase depends only on the full-screen pixel coordinate, never on the
    tile origin or worker, which is what keeps boundary quadrature stable.
    """

    value = (
        (int(pixel_x) * 73856093)
        ^ (int(pixel_y) * 19349663)
        ^ 0x9E3779B9
    ) & 0xFFFFFF
    return 0.05 + 0.9 * (value / float(0x1000000))


def scene_shadow_tmax(
    scene: Scene,
    origin: np.ndarray,
    direction: np.ndarray,
    bounds: tuple[np.ndarray, np.ndarray] | None = None,
) -> float:
    bounds = scene.scene_bounds if bounds is None else bounds
    if bounds is None:
        return 0.0
    interval = intersect_aabb(origin, direction, bounds[0], bounds[1])
    if interval is None:
        return 0.0
    return max(0.0, float(interval[1]))


def aabb_corners(lo: np.ndarray, hi: np.ndarray) -> Iterable[np.ndarray]:
    for x in (float(lo[0]), float(hi[0])):
        for y in (float(lo[1]), float(hi[1])):
            for z in (float(lo[2]), float(hi[2])):
                yield np.array([x, y, z], dtype=np.float64)
