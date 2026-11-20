"""Ray and transparent-medium intersection helpers."""

from __future__ import annotations

import math
from dataclasses import dataclass
from typing import Iterable

import numpy as np

EPSILON = 1e-9


def ray_sphere_interval(
    origin: np.ndarray, direction: np.ndarray, center: np.ndarray, radius: float
) -> tuple[float, float] | None:
    oc = origin - center
    a = float(np.dot(direction, direction))
    b = 2.0 * float(np.dot(oc, direction))
    c = float(np.dot(oc, oc) - radius * radius)
    discriminant = b * b - 4.0 * a * c
    if discriminant < 0.0:
        return None
    root = math.sqrt(max(0.0, discriminant))
    return ((-b - root) / (2.0 * a), (-b + root) / (2.0 * a))


def ray_aabb_interval(
    origin: np.ndarray, direction: np.ndarray, minimum: np.ndarray, maximum: np.ndarray
) -> tuple[float, float] | None:
    low = -math.inf
    high = math.inf
    for axis in range(3):
        component = float(direction[axis])
        if abs(component) < EPSILON:
            if origin[axis] < minimum[axis] or origin[axis] > maximum[axis]:
                return None
            continue
        first = float((minimum[axis] - origin[axis]) / component)
        second = float((maximum[axis] - origin[axis]) / component)
        if first > second:
            first, second = second, first
        low = max(low, first)
        high = min(high, second)
        if low > high:
            return None
    return low, high


@dataclass(frozen=True)
class Segment:
    start: float
    end: float
    media: tuple[object, ...]


def segment_media(
    media: Iterable[object],
    origin: np.ndarray,
    direction: np.ndarray,
    near: float,
    far: float,
) -> list[Segment]:
    """Return a partition of [near, far] by all medium boundaries."""

    entries: list[tuple[float, float, object]] = []
    for medium in media:
        interval = medium.intersect(origin, direction)
        if interval is None:
            continue
        start = max(near, interval[0])
        end = min(far, interval[1])
        if end > start + EPSILON:
            entries.append((start, end, medium))
    if not entries:
        return []

    boundaries = {near, far}
    for start, end, _ in entries:
        boundaries.add(start)
        boundaries.add(end)
    ordered = sorted(boundaries)
    result: list[Segment] = []
    for left, right in zip(ordered, ordered[1:]):
        if right <= left + EPSILON:
            continue
        midpoint = 0.5 * (left + right)
        active = tuple(
            medium for start, end, medium in entries if start <= midpoint <= end
        )
        if not active:
            continue
        if result and abs(result[-1].end - left) <= EPSILON and result[-1].media == active:
            previous = result.pop()
            result.append(Segment(previous.start, right, active))
        else:
            result.append(Segment(left, right, active))
    return result
