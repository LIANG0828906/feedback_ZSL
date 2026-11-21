"""Three-layer stroke generation with byte-deterministic stroke ids."""

import numpy as np

from . import qround
from .geometry import (
    clip_line_to_ring,
    distance_to_ring,
    perpendicular,
    point_at_arclength,
    point_in_ring,
    polyline_arclength,
    segment_intersect,
    unit_angle,
)


def _rng(seed, salt):
    # PCG64 is stable across numpy versions/platforms for a fixed seed.
    return np.random.Generator(np.random.PCG64(seed + salt))


def _stroke(stroke_id, point, angle, width, length, alpha=1.0, zone=None):
    return {
        "id": int(stroke_id),
        "x": float(qround(point[0])),
        "y": float(qround(point[1])),
        "angle": float(qround(angle)),
        "width": float(qround(width)),
        "length": float(qround(length)),
        "alpha": float(qround(alpha)),
        "zone": zone,
    }


def generate_outline(scene, params):
    rng = _rng(params.seed, 17)
    strokes = []
    stroke_id = 0
    for path in sorted(scene.paths, key=lambda p: (p.order, p.index)):
        total = polyline_arclength(path.points)
        if total <= params.spacing:
            continue
        count = max(1, int(total // params.spacing))
        for k in range(count):
            target = (k + 0.5) * params.spacing
            point, tangent = point_at_arclength(path.points, target)
            normal = perpendicular(tangent)
            offset = float(rng.uniform(-params.jitter, params.jitter))
            point = point + normal * offset
            angle = float(np.degrees(np.arctan2(tangent[1], tangent[0])))
            strokes.append(
                _stroke(stroke_id, point, angle, params.width, params.spacing, 1.0, path.zone)
            )
            stroke_id += 1
    return strokes


def generate_shadow(scene, params, outline_strokes):
    """Hatching inside zones; strokes crossing outline strokes are culled."""
    rng = _rng(params.seed, 29)
    direction = unit_angle(params.angle_deg)
    normal = perpendicular(direction)
    min_x, min_y, max_x, max_y = scene.bbox
    span = np.hypot(max_x - min_x, max_y - min_y) + 4.0
    center = np.array([0.5 * (min_x + max_x), 0.5 * (min_y + max_y)])
    level_count = int(span // params.spacing) + 1
    levels = (np.arange(level_count) - level_count // 2) * params.spacing

    strokes = []
    stroke_id = 0
    for path in sorted(scene.paths, key=lambda p: (p.order, p.index)):
        ring = path.points[:-1]
        for level in levels:
            origin = center + normal * level - direction * span * 0.5
            for t0, t1 in clip_line_to_ring(origin, direction, ring):
                seg_len = t1 - t0
                if seg_len < params.stroke_len:
                    continue
                n_strokes = int(seg_len // params.stroke_len)
                for k in range(n_strokes):
                    t = t0 + (k + 0.5) * params.stroke_len
                    jitter_t = float(rng.uniform(-params.jitter, params.jitter))
                    point = origin + direction * (t + jitter_t)
                    strokes.append(
                        _stroke(
                            stroke_id,
                            point,
                            params.angle_deg,
                            params.width,
                            params.stroke_len,
                            1.0,
                            path.zone,
                        )
                    )
                    stroke_id += 1

    culled = _cull_intersecting(strokes, scene, outline_strokes, params.cull_margin)
    return strokes, culled


def _cull_intersecting(strokes, scene, outline_strokes, margin):
    culled = []
    rings = [path.points for path in sorted(scene.paths, key=lambda p: p.index)]
    for stroke in strokes:
        direction = unit_angle(stroke["angle"])
        half = 0.5 * stroke["length"]
        p0 = np.array([stroke["x"], stroke["y"]]) - direction * half
        p1 = np.array([stroke["x"], stroke["y"]]) + direction * half
        hit = False
        for ring in rings:
            n = len(ring)
            for i in range(n - 1):
                if segment_intersect(p0, p1, ring[i], ring[i + 1]):
                    hit = True
                    break
            if hit:
                break
            if distance_to_ring(np.array([stroke["x"], stroke["y"]]), ring[:-1]) < margin:
                hit = True
                break
        if hit:
            culled.append(stroke["id"])
    return culled


def generate_smear(scene, params, shadow_strokes):
    rng = _rng(params.seed, 43)
    min_x, min_y, max_x, max_y = scene.bbox
    nx = int((max_x - min_x) // params.grid) + 2
    ny = int((max_y - min_y) // params.grid) + 2
    shadow_xy = (
        np.array([[s["x"], s["y"]] for s in shadow_strokes], dtype=float)
        if shadow_strokes
        else np.empty((0, 2))
    )
    r2 = params.density_radius * params.density_radius

    strokes = []
    stroke_id = 0
    rings = sorted(scene.paths, key=lambda p: (p.order, p.index))
    for ix in range(nx):
        for iy in range(ny):
            base = np.array(
                [min_x + (ix + 0.5) * params.grid, min_y + (iy + 0.5) * params.grid]
            )
            jitter = np.array(
                [
                    float(rng.uniform(-params.jitter, params.jitter)),
                    float(rng.uniform(-params.jitter, params.jitter)),
                ]
            )
            point = base + jitter
            zone = _zone_of(point, rings)
            if zone is None:
                continue
            density = _local_density(point, shadow_xy, r2, params.density_cap)
            alpha = params.min_alpha + density * (params.max_alpha - params.min_alpha)
            strokes.append(
                _stroke(
                    stroke_id,
                    point,
                    params.angle_deg,
                    params.width,
                    params.grid,
                    alpha,
                    zone,
                )
            )
            stroke_id += 1
    return strokes


def _zone_of(point, paths):
    for path in paths:
        if point_in_ring(point, path.points[:-1]):
            return path.zone
    return None


def _local_density(point, shadow_xy, r2, cap):
    if len(shadow_xy) == 0:
        return 0.0
    d2 = ((shadow_xy - point) ** 2).sum(axis=1)
    count = int((d2 <= r2).sum())
    return min(1.0, count / float(cap))


def remove_culled(strokes, culled_ids):
    culled_set = set(culled_ids)
    return [s for s in strokes if s["id"] not in culled_set]
