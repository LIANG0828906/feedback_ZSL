"""Vector scene parsing, validation and zone index construction."""

import math

import numpy as np

from .canonical import canonical_hash
from .errors import PipelineError

QUAD_SUBDIV = 16
CLOSE_EPS = 1e-9


class PathGeom:
    def __init__(self, index, zone, fill, order, points, segments):
        self.index = index
        self.zone = zone
        self.fill = fill
        self.order = order
        self.points = np.asarray(points, dtype=float)
        self.segments = segments

    def to_dict(self):
        return {
            "index": self.index,
            "zone": self.zone,
            "fill": self.fill,
            "order": self.order,
            "points": [[float(x), float(y)] for x, y in self.points],
            "segments": self.segments,
        }


class Scene:
    def __init__(self, doc, paths, zone_index, bbox, scene_hash):
        self.doc = doc
        self.paths = paths
        self.zone_index = zone_index
        self.bbox = bbox
        self.scene_hash = scene_hash

    def factor_dict(self):
        """Stable geometry factor representation (no wall-clock data)."""
        return {
            "paths": [
                {
                    "index": p.index,
                    "zone": p.zone,
                    "fill": p.fill,
                    "order": p.order,
                    "points": p.points.tolist(),
                }
                for p in sorted(self.paths, key=lambda p: p.index)
            ]
        }


def load_scene(path):
    from .canonical import read_json

    return parse_scene(read_json(path))


def parse_scene(doc):
    if not isinstance(doc, dict):
        raise PipelineError.scene_invalid("scene root must be an object")
    paths_raw = doc.get("paths")
    if not isinstance(paths_raw, list) or not paths_raw:
        raise PipelineError.scene_invalid("field 'paths' missing or empty")

    paths = []
    seen_zones = {}
    for idx, raw in enumerate(paths_raw):
        path = _parse_path(idx, raw)
        paths.append(path)
        seen_zones.setdefault(path.zone, []).append(idx)

    _validate_zones(paths)

    all_points = np.vstack([p.points for p in paths])
    bbox = [
        float(all_points[:, 0].min()),
        float(all_points[:, 1].min()),
        float(all_points[:, 0].max()),
        float(all_points[:, 1].max()),
    ]
    scene_hash = canonical_hash(
        {
            "paths": [
                {
                    "index": p.index,
                    "zone": p.zone,
                    "fill": p.fill,
                    "order": p.order,
                    "points": np.round(p.points, 6).tolist(),
                }
                for p in paths
            ]
        }
    )
    zone_index = {zone: list(idxs) for zone, idxs in sorted(seen_zones.items())}
    return Scene(doc, paths, zone_index, bbox, scene_hash)


def _parse_path(idx, raw):
    if not isinstance(raw, dict):
        raise PipelineError.scene_invalid("path entry must be an object", idx)
    for field in ("zone", "segments"):
        if field not in raw:
            raise PipelineError.scene_invalid(f"missing field '{field}'", idx)
    zone = raw["zone"]
    if not isinstance(zone, str) or not zone:
        raise PipelineError.scene_invalid("field 'zone' must be a non-empty string", idx)
    segments = raw["segments"]
    if not isinstance(segments, list) or len(segments) < 1:
        raise PipelineError.scene_invalid("field 'segments' missing or empty", idx)

    fill = raw.get("fill")
    if fill is not None:
        if (
            not isinstance(fill, list)
            or len(fill) != 3
            or any(not isinstance(c, (int, float)) for c in fill)
        ):
            raise PipelineError.scene_invalid("fill must be null or [r,g,b]", idx)
        fill = [int(c) for c in fill]
        if any(c < 0 or c > 255 for c in fill):
            raise PipelineError.scene_invalid("fill channels must be within 0..255", idx)

    order = raw.get("order", idx)
    if not isinstance(order, int):
        raise PipelineError.scene_invalid("field 'order' must be an integer", idx)

    ring = []
    cursor = None
    start = None
    for seg_idx, seg in enumerate(segments):
        kind, pts = _parse_segment(seg, idx, seg_idx)
        if seg_idx == 0:
            start = pts[0]
            cursor = pts[0]
            ring.append(pts[0])
        elif not _close(cursor, pts[0]):
            raise PipelineError.scene_invalid(
                f"segment {seg_idx} does not start at previous end", idx
            )
        ring.extend(pts[1:])
        cursor = pts[-1]

    closed_flag = bool(raw.get("closed", False))
    endpoints_match = _close(cursor, start)
    if not closed_flag and not endpoints_match:
        raise PipelineError.scene_invalid("path is not closed", idx)
    if not endpoints_match:
        raise PipelineError.scene_invalid(
            "closed flag set but end point does not match start", idx
        )

    points = np.asarray(ring, dtype=float)
    if not np.all(np.isfinite(points)):
        raise PipelineError.scene_invalid("coordinates must be finite", idx)
    if len(points) < 3:
        raise PipelineError.scene_invalid("closed path needs >= 3 vertices", idx)

    return PathGeom(idx, zone, fill, order, points, segments)


def _parse_segment(seg, path_idx, seg_idx):
    if not isinstance(seg, dict) or "type" not in seg:
        raise PipelineError.scene_invalid(
            f"segment {seg_idx} must be an object with 'type'", path_idx
        )
    kind = seg["type"]
    if kind == "line":
        pts = [_point(seg, "p0", path_idx, seg_idx), _point(seg, "p1", path_idx, seg_idx)]
        return kind, pts
    if kind == "quad":
        p0 = _point(seg, "p0", path_idx, seg_idx)
        ctrl = _point(seg, "c", path_idx, seg_idx)
        p1 = _point(seg, "p1", path_idx, seg_idx)
        pts = [_quad_point(p0, ctrl, p1, t) for t in np.linspace(0, 1, QUAD_SUBDIV + 1)]
        return kind, pts
    raise PipelineError.scene_invalid(
        f"segment {seg_idx} has unsupported type {kind!r}", path_idx
    )


def _point(seg, key, path_idx, seg_idx):
    value = seg.get(key)
    if (
        not isinstance(value, list)
        or len(value) != 2
        or any(not isinstance(c, (int, float)) for c in value)
    ):
        raise PipelineError.scene_invalid(
            f"segment {seg_idx} field '{key}' must be [x,y]", path_idx
        )
    if not math.isfinite(value[0]) or not math.isfinite(value[1]):
        raise PipelineError.scene_invalid(
            f"segment {seg_idx} field '{key}' must be finite", path_idx
        )
    return [float(value[0]), float(value[1])]


def _quad_point(p0, ctrl, p1, t):
    u = 1.0 - t
    x = u * u * p0[0] + 2 * u * t * ctrl[0] + t * t * p1[0]
    y = u * u * p0[1] + 2 * u * t * ctrl[1] + t * t * p1[1]
    return [float(x), float(y)]


def _close(a, b, eps=CLOSE_EPS):
    return abs(a[0] - b[0]) <= eps and abs(a[1] - b[1]) <= eps


def _validate_zones(paths):
    """Zones must not overlap; shared boundary edges/points are allowed."""
    for i in range(len(paths)):
        for j in range(i + 1, len(paths)):
            if paths[i].zone == paths[j].zone:
                continue
            if _rings_overlap(paths[i].points, paths[j].points):
                raise PipelineError.scene_invalid(
                    f"zones {paths[i].zone!r} (path#{paths[i].index}) and "
                    f"{paths[j].zone!r} (path#{paths[j].index}) overlap",
                    paths[i].index,
                )


def _rings_overlap(ring_a, ring_b):
    if _segments_cross(ring_a, ring_b) or _segments_cross(ring_b, ring_a):
        return True
    if _point_in_ring(ring_a[0], ring_b) or _point_in_ring(ring_b[0], ring_a):
        return True
    return False


def _segments_cross(ring_a, ring_b):
    n = len(ring_a)
    for k in range(n):
        a0 = ring_a[k]
        a1 = ring_a[(k + 1) % n]
        for m in range(len(ring_b)):
            b0 = ring_b[m]
            b1 = ring_b[(m + 1) % len(ring_b)]
            if _segments_intersect_proper(a0, a1, b0, b1):
                return True
            if _collinear_overlap(a0, a1, b0, b1):
                return True
    return False


def _orient(a, b, c):
    return (b[0] - a[0]) * (c[1] - a[1]) - (b[1] - a[1]) * (c[0] - a[0])


def _segments_intersect_proper(a, b, c, d, eps=1e-10):
    o1 = _orient(a, b, c)
    o2 = _orient(a, b, d)
    o3 = _orient(c, d, a)
    o4 = _orient(c, d, b)
    return ((o1 > eps and o2 < -eps) or (o1 < -eps and o2 > eps)) and (
        (o3 > eps and o4 < -eps) or (o3 < -eps and o4 > eps)
    )


def _collinear_overlap(a, b, c, d, eps=1e-9):
    if abs(_orient(a, b, c)) > eps or abs(_orient(a, b, d)) > eps:
        return False
    if abs(b[0] - a[0]) > eps:
        lo, hi = sorted((a[0], b[0]))
        c_on = lo + eps < c[0] < hi - eps
        d_on = lo + eps < d[0] < hi - eps
    else:
        lo, hi = sorted((a[1], b[1]))
        c_on = lo + eps < c[1] < hi - eps
        d_on = lo + eps < d[1] < hi - eps
    return c_on or d_on


def _point_in_ring(point, ring):
    inside = False
    x, y = point
    n = len(ring)
    j = n - 1
    for i in range(n):
        xi, yi = ring[i]
        xj, yj = ring[j]
        if (yi > y) != (yj > y):
            x_cross = (xj - xi) * (y - yi) / (yj - yi) + xi
            if x_cross > x:
                inside = not inside
        j = i
    return inside
