"""Deterministic geometric primitives: camera rays, mesh/AABB intersection,
perspective projection of grout gaps onto outer walls and the receiver.
"""

from __future__ import annotations

import numpy as np

from .model import Brick, Camera, PointLight, ReceiverPlane, Scene

EPS = 1e-9


def camera_frame(camera: Camera, yaw: float) -> tuple[np.ndarray, np.ndarray, np.ndarray]:
    """Return (origin, forward, right, up) for a yawed camera.

    The camera orbits around its declared target by ``yaw`` radians while
    keeping the same distance; the wall geometry itself never moves.
    """
    forward0 = camera.target - camera.position
    distance = np.linalg.norm(forward0)
    forward0 = forward0 / distance
    right0 = np.cross(forward0, camera.up)
    right0 /= np.linalg.norm(right0)
    up0 = np.cross(right0, forward0)

    cos_y, sin_y = np.cos(yaw), np.sin(yaw)
    forward = cos_y * forward0 + sin_y * right0
    right = -sin_y * forward0 + cos_y * right0
    right /= np.linalg.norm(right)
    up = up0
    origin = camera.target - forward * distance
    return origin, forward, right, up


def pixel_rays(
    camera: Camera, yaw: float, width: int, height: int
) -> tuple[np.ndarray, np.ndarray]:
    """Generate normalised ray origins/directions for every image pixel."""
    origin, forward, right, up = camera_frame(camera, yaw)
    xs = (np.arange(width, dtype=np.float64) + 0.5) / width - 0.5
    ys = 0.5 - (np.arange(height, dtype=np.float64) + 0.5) / height
    grid_x, grid_y = np.meshgrid(xs, ys)
    directions = (
        forward[None, None, :] * camera.focal_length
        + right[None, None, :] * (grid_x[..., None] * camera.sensor_width)
        + up[None, None, :] * (grid_y[..., None] * camera.sensor_height)
    )
    directions /= np.linalg.norm(directions, axis=-1, keepdims=True)
    origins = np.broadcast_to(origin, directions.shape).copy()
    return origins, directions


def intersect_triangles(
    origins: np.ndarray,
    directions: np.ndarray,
    vertices: np.ndarray,
    triangles: np.ndarray,
) -> np.ndarray:
    """Moller-Trumbore intersection.

    Returns an (N, T) array of positive ray parameters, ``inf`` on miss.
    """
    p0 = vertices[triangles[:, 0]]
    p1 = vertices[triangles[:, 1]]
    p2 = vertices[triangles[:, 2]]
    edge1 = p1 - p0
    edge2 = p2 - p0
    hits = np.full((origins.shape[0], triangles.shape[0]), np.inf, dtype=np.float64)
    for tri in range(triangles.shape[0]):
        h = np.cross(directions, edge2[tri])
        det_per_ray = np.sum(edge1[tri] * h, axis=1)
        parallel = np.abs(det_per_ray) < EPS
        inv_det = np.where(parallel, 0.0, 1.0 / det_per_ray)
        s = origins - p0[tri]
        u = np.sum(s * h, axis=1) * inv_det
        q = np.cross(s, edge1[tri])
        v = np.sum(directions * q, axis=1) * inv_det
        t = np.sum(edge2[tri] * q, axis=1) * inv_det
        valid = (~parallel) & (u >= 0.0) & (v >= 0.0) & (u + v <= 1.0) & (t > EPS)
        hits[valid, tri] = t[valid]
    return hits


def intersect_aabbs(
    origins: np.ndarray,
    directions: np.ndarray,
    boxes_min: np.ndarray,
    boxes_max: np.ndarray,
) -> np.ndarray:
    """Vectorised slab test. Returns (N, B) nearest positive t, inf on miss."""
    inv_dir = np.where(np.abs(directions) < EPS, np.where(directions >= 0, 1e30, -1e30),
                       1.0 / directions)
    t1 = (boxes_min[None, :, :] - origins[:, None, :]) * inv_dir[:, None, :]
    t2 = (boxes_max[None, :, :] - origins[:, None, :]) * inv_dir[:, None, :]
    tmin = np.minimum(t1, t2).max(axis=2)
    tmax = np.maximum(t1, t2).min(axis=2)
    valid = (tmax >= np.maximum(tmin, 0.0)) & (tmax > EPS)
    nearest = np.where(valid, np.maximum(tmin, 0.0), np.inf)
    return nearest


def primary_visibility(
    origins: np.ndarray,
    directions: np.ndarray,
    bricks: tuple[Brick, ...],
) -> tuple[np.ndarray, np.ndarray, np.ndarray]:
    """Nearest-brick hit per ray using brick AABBs.

    Returns (brick_index per ray or -1, hit t, hit point).
    """
    boxes_min = np.stack([brick.aabb_min for brick in bricks])
    boxes_max = np.stack([brick.aabb_max for brick in bricks])
    t_table = intersect_aabbs(origins, directions, boxes_min, boxes_max)
    nearest_index = np.argmin(t_table, axis=1)
    nearest_t = t_table[np.arange(t_table.shape[0]), nearest_index]
    no_hit = ~np.isfinite(nearest_t)
    hit_index = np.where(no_hit, -1, nearest_index).astype(np.int64)
    hit_points = origins + directions * nearest_t[:, None]
    return hit_index, nearest_t, hit_points


def mesh_owner_consistency(
    origins: np.ndarray,
    directions: np.ndarray,
    bricks: tuple[Brick, ...],
    limit: int = 256,
) -> bool:
    """Cross-check AABB ownership against actual triangle meshes."""
    sample_n = min(limit, origins.shape[0])
    if sample_n == 0:
        return True
    pick = np.linspace(0, origins.shape[0] - 1, sample_n).astype(np.int64)
    o = origins[pick]
    d = directions[pick]
    aabb_index, _, _ = primary_visibility(o, d, bricks)
    for ray in range(sample_n):
        best_t = np.inf
        mesh_brick = -1
        for brick_idx, brick in enumerate(bricks):
            table = intersect_triangles(
                o[ray : ray + 1], d[ray : ray + 1],
                brick.vertices, brick.triangles,
            )
            t = table.min()
            if np.isfinite(t) and t < best_t:
                best_t = t
                mesh_brick = brick_idx
        if mesh_brick != int(aabb_index[ray]):
            return False
    return True


def project_to_plane(
    point: np.ndarray, light: PointLight, plane_z: float
) -> np.ndarray | None:
    """Perspective-project ``point`` from ``light`` onto a constant-z plane."""
    delta = plane_z - light.position[2]
    denom = point[2] - light.position[2]
    if abs(denom) < EPS:
        return None
    factor = delta / denom
    if factor <= 0.0:
        return None
    return light.position + (point - light.position) * factor


def grout_gap_segments(scene: Scene) -> list[dict]:
    """Return the grout gap centre lines per layer as axis-aligned segments.

    Each segment stores its layer, constant coordinate and range; these are
    the geometric gaps whose perspective projections form light bands.
    """
    segments: list[dict] = []
    for layer in scene.layers:
        layer_bricks = [scene.brick_by_id(bid) for bid in layer.brick_ids]
        grout = layer.grout_width
        x_edges = sorted({float(b.origin[0]) for b in layer_bricks})
        y_edges = sorted({float(b.origin[1]) for b in layer_bricks})
        x_max = max(float(b.aabb_max[0]) for b in layer_bricks)
        y_max = max(float(b.aabb_max[1]) for b in layer_bricks)
        x_min = min(float(b.origin[0]) for b in layer_bricks)
        y_min = min(float(b.origin[1]) for b in layer_bricks)
        z_mid = layer.plane_z + (
            layer_bricks[0].size[2] if layer_bricks else 0.0
        ) / 2.0
        for x in x_edges[1:]:
            segments.append(
                {
                    "layer": layer.index,
                    "orientation": "vertical",
                    "constant": x,
                    "range": (y_min, y_max),
                    "width": grout,
                    "z": z_mid,
                }
            )
        for y in y_edges[1:]:
            segments.append(
                {
                    "layer": layer.index,
                    "orientation": "horizontal",
                    "constant": y,
                    "range": (x_min, x_max),
                    "width": grout,
                    "z": z_mid,
                }
            )
    return segments


def grout_landing_points(
    scene: Scene, receiver: ReceiverPlane
) -> list[dict]:
    """Perspective landing coordinates of every grout gap for every light.

    Points outside the receiver bounds are flagged ``in_bounds=False``; the
    renderer then treats them as unoccluded rather than drawing a hard edge.
    """
    segments = grout_gap_segments(scene)
    records: list[dict] = []
    sample_offsets = (0.25, 0.5, 0.75)
    for segment in segments:
        lo, hi = segment["range"]
        for light in scene.lights:
            for frac in sample_offsets:
                along = lo + (hi - lo) * frac
                if segment["orientation"] == "vertical":
                    point = np.array([segment["constant"], along, segment["z"]])
                else:
                    point = np.array([along, segment["constant"], segment["z"]])
                landing = project_to_plane(point, light, receiver.z)
                record = {
                    "light_id": light.light_id,
                    "layer": segment["layer"],
                    "orientation": segment["orientation"],
                    "source_point": point.tolist(),
                    "sample_fraction": frac,
                    "landing": None if landing is None else landing.tolist(),
                    "in_bounds": False
                    if landing is None
                    else receiver.contains(landing),
                }
                records.append(record)
    return records


def penumbra_width(
    light: PointLight, gap_z: float, receiver_z: float, gap_width: float
) -> float:
    """Artist-facing effective-aperture penumbra model.

    The width grows monotonically with the gap-to-receiver distance and with
    the light-to-gap distance, matching a diverging light cone:
    ``w = gap_width * d_lr / d_lg * (1 + k * d_lg)``.
    """
    d_lg = max(abs(gap_z - light.position[2]), EPS)
    d_lr = max(abs(receiver_z - light.position[2]), EPS)
    k = 0.35
    return float(gap_width * (d_lr / d_lg) * (1.0 + k * d_lg))
