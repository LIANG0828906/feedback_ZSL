"""Fixed-point geometry and checked integer arithmetic."""

from __future__ import annotations

import math

COORD_SCALE = 1000
ANGLE_SCALE = 1000
WEIGHT_SCALE = 1000
DIAGONAL_SCALE = 1414
MAX_CELL_COST = 1_000_000
MAX_PLANNER_COST = (1 << 63) - 1
MAX_WORLD_EXTENT = 1_000_000_000


def checked_add(left: int, right: int, *, label: str = "cost") -> int:
    if left < 0 or right < 0:
        raise OverflowError(f"{label} must be non-negative")
    if left > MAX_PLANNER_COST - right:
        raise OverflowError(f"{label} overflow: {left} + {right}")
    return left + right


def cell_center(coord: tuple[int, int]) -> tuple[int, int]:
    x, y = coord
    return x * COORD_SCALE + COORD_SCALE // 2, y * COORD_SCALE + COORD_SCALE // 2


def point_to_grid(point: tuple[int, int]) -> tuple[int, int]:
    return point[0] // COORD_SCALE, point[1] // COORD_SCALE


def isqrt(value: int) -> int:
    if value < 0:
        raise ValueError("square root of negative value")
    return math.isqrt(value)


def point_distance(left: tuple[int, int], right: tuple[int, int]) -> int:
    dx = right[0] - left[0]
    dy = right[1] - left[1]
    return isqrt(dx * dx + dy * dy)


def point_distance_squared(left: tuple[int, int], right: tuple[int, int]) -> int:
    dx = right[0] - left[0]
    dy = right[1] - left[1]
    return dx * dx + dy * dy


def _normalize_angle(angle: int) -> int:
    return angle % (360 * ANGLE_SCALE)


def angle_difference(target: int, current: int) -> int:
    """Signed shortest angular distance in [-180000, 180000]."""

    raw = _normalize_angle(target) - _normalize_angle(current)
    half = 180 * ANGLE_SCALE
    full = 360 * ANGLE_SCALE
    if raw > half:
        raw -= full
    elif raw < -half:
        raw += full
    return raw


def rotate_towards(current: int, target: int, maximum_step: int) -> tuple[int, int]:
    """Return (new_angle, applied_delta)."""

    if maximum_step < 0:
        raise ValueError("maximum turn step must be non-negative")
    delta = angle_difference(target, current)
    applied = max(-maximum_step, min(maximum_step, delta))
    return _normalize_angle(current + applied), applied


def segment_heading(start: tuple[int, int], end: tuple[int, int]) -> int:
    dx = end[0] - start[0]
    dy = end[1] - start[1]
    if dx == 0 and dy == 0:
        return 0
    degrees = math.atan2(dy, dx) * 180.0 / math.pi
    return int(round(degrees * ANGLE_SCALE)) % (360 * ANGLE_SCALE)


def interpolate_point(
    start: tuple[int, int],
    end: tuple[int, int],
    traveled: int,
    length: int,
) -> tuple[int, int]:
    if length <= 0:
        return end
    if traveled <= 0:
        return start
    if traveled >= length:
        return end
    x = start[0] + ((end[0] - start[0]) * traveled) // length
    y = start[1] + ((end[1] - start[1]) * traveled) // length
    return x, y


def point_segment_distance(
    point: tuple[int, int],
    start: tuple[int, int],
    end: tuple[int, int],
) -> int:
    """Distance from a point to a segment, returned as integer ceil-ish estimate."""

    vx = end[0] - start[0]
    vy = end[1] - start[1]
    wx = point[0] - start[0]
    wy = point[1] - start[1]
    length_sq = vx * vx + vy * vy
    if length_sq == 0:
        return point_distance(point, start)
    projection = wx * vx + wy * vy
    if projection <= 0:
        return point_distance(point, start)
    if projection >= length_sq:
        return point_distance(point, end)
    cross = wx * vy - wy * vx
    return (abs(cross) + isqrt(length_sq) - 1) // isqrt(length_sq)
