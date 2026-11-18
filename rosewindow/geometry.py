"""Triangle meshes, camera projection, rasterization, and lead geometry."""

from __future__ import annotations

import math
from dataclasses import dataclass
from typing import Iterable

import numpy as np

from .scene import GlassPiece, Scene


KIND_NONE = 0
KIND_WALL = 1
KIND_STONE = 2
KIND_GLASS = 3
KIND_LEAD = 4


@dataclass
class HitBuffers:
    width: int
    height: int
    depth: np.ndarray
    kind: np.ndarray
    owner: np.ndarray
    world: np.ndarray


def create_hit_buffers(width: int, height: int) -> HitBuffers:
    return HitBuffers(
        width=width,
        height=height,
        depth=np.full((height, width), np.inf, dtype=np.float64),
        kind=np.zeros((height, width), dtype=np.uint8),
        owner=np.zeros((height, width), dtype=np.int32),
        world=np.zeros((height, width, 3), dtype=np.float64),
    )


def camera_project(
    points: np.ndarray, scene: Scene
) -> tuple[np.ndarray, np.ndarray]:
    camera = scene.camera
    relative = np.asarray(points, dtype=np.float64).reshape(-1, 3) - camera.position
    camera_x = relative @ camera.right
    camera_y = relative @ camera.camera_up
    camera_z = relative @ camera.forward
    focal = (scene.height * 0.5) / math.tan(
        math.radians(camera.vertical_fov_degrees) * 0.5
    )
    safe_z = np.where(camera_z > 1e-12, camera_z, 1e-12)
    screen_x = scene.width * 0.5 + camera_x * focal / safe_z
    screen_y = scene.height * 0.5 - camera_y * focal / safe_z
    return np.stack([screen_x, screen_y], axis=1), camera_z


def _edge_function(a: np.ndarray, b: np.ndarray, px: np.ndarray, py: np.ndarray):
    return (b[0] - a[0]) * (py - a[1]) - (b[1] - a[1]) * (px - a[0])


def rasterize_mesh(
    buffers: HitBuffers,
    scene: Scene,
    vertices: np.ndarray,
    triangles: np.ndarray,
    *,
    kind: int,
    owners: np.ndarray | None = None,
) -> None:
    """Rasterize a triangle mesh into a z-buffer with perspective-correct world."""
    if len(triangles) == 0:
        return
    screen, camera_z = camera_project(vertices, scene)
    width = buffers.width
    height = buffers.height
    owner_values = (
        np.zeros(len(triangles), dtype=np.int32)
        if owners is None
        else np.asarray(owners, dtype=np.int32)
    )
    for triangle_index, triangle in enumerate(triangles):
        index0, index1, index2 = (int(triangle[0]), int(triangle[1]), int(triangle[2]))
        z0, z1, z2 = camera_z[index0], camera_z[index1], camera_z[index2]
        if z0 <= 1e-5 or z1 <= 1e-5 or z2 <= 1e-5:
            # Scene construction keeps the camera away from the near plane.
            continue
        p0, p1, p2 = screen[index0], screen[index1], screen[index2]
        area = (p1[0] - p0[0]) * (p2[1] - p0[1]) - (p1[1] - p0[1]) * (
            p2[0] - p0[0]
        )
        if abs(float(area)) <= 1e-12:
            continue
        x_min = max(0, int(math.floor(float(min(p0[0], p1[0], p2[0])))))
        x_max = min(width - 1, int(math.ceil(float(max(p0[0], p1[0], p2[0])))))
        y_min = max(0, int(math.floor(float(min(p0[1], p1[1], p2[1])))))
        y_max = min(height - 1, int(math.ceil(float(max(p0[1], p1[1], p2[1])))))
        if x_min > x_max or y_min > y_max:
            continue

        pixel_y, pixel_x = np.mgrid[y_min : y_max + 1, x_min : x_max + 1]
        px = pixel_x.astype(np.float64) + 0.5
        py = pixel_y.astype(np.float64) + 0.5
        w0 = _edge_function(p1, p2, px, py) / area
        w1 = _edge_function(p2, p0, px, py) / area
        w2 = _edge_function(p0, p1, px, py) / area
        epsilon = -1e-9
        inside = (w0 >= epsilon) & (w1 >= epsilon) & (w2 >= epsilon)
        if not np.any(inside):
            continue

        inv_z = w0 / z0 + w1 / z1 + w2 / z2
        valid = inside & (inv_z > 1e-12)
        if not np.any(valid):
            continue
        depth = np.full_like(inv_z, np.inf)
        depth[valid] = 1.0 / inv_z[valid]

        sub_y = pixel_y[valid]
        sub_x = pixel_x[valid]
        current_depth = buffers.depth[sub_y, sub_x]
        tie = np.abs(depth[valid] - current_depth) <= 1e-10
        write = depth[valid] < current_depth - 1e-10
        if np.any(tie):
            current_kind = buffers.kind[sub_y, sub_x]
            current_owner = buffers.owner[sub_y, sub_x]
            priority = {
                KIND_WALL: 0,
                KIND_STONE: 1,
                KIND_GLASS: 2,
                KIND_LEAD: 3,
            }
            new_priority = priority.get(kind, 0)
            new_owner = int(owner_values[triangle_index])
            for local_index in np.flatnonzero(tie):
                old_priority = priority.get(int(current_kind[local_index]), 0)
                old_owner = int(current_owner[local_index])
                if new_priority > old_priority or (
                    new_priority == old_priority and new_owner < old_owner
                ):
                    write[local_index] = True
        if not np.any(write):
            continue

        local_write = np.flatnonzero(valid)[write]
        w0v, w1v, w2v = w0[valid][write], w1[valid][write], w2[valid][write]
        invzv = inv_z[valid][write]
        world = np.empty((local_write.size, 3), dtype=np.float64)
        world[:, 0] = (
            w0v * vertices[index0][0]
            + w1v * vertices[index1][0]
            + w2v * vertices[index2][0]
        ) / invzv
        world[:, 1] = (
            w0v * vertices[index0][1]
            + w1v * vertices[index1][1]
            + w2v * vertices[index2][1]
        ) / invzv
        world[:, 2] = (
            w0v * vertices[index0][2]
            + w1v * vertices[index1][2]
            + w2v * vertices[index2][2]
        ) / invzv

        flat_y = pixel_y.ravel()[local_write]
        flat_x = pixel_x.ravel()[local_write]
        buffers.depth[flat_y, flat_x] = depth[valid][write]
        buffers.kind[flat_y, flat_x] = kind
        buffers.owner[flat_y, flat_x] = (
            int(owner_values[triangle_index]) if kind == KIND_GLASS else 0
        )
        buffers.world[flat_y, flat_x] = world


def _wall_grid(scene: Scene, nx: int = 24, ny: int = 16) -> tuple[np.ndarray, np.ndarray]:
    x_values = np.linspace(
        scene.wall.center_x - scene.wall.width * 0.5,
        scene.wall.center_x + scene.wall.width * 0.5,
        nx + 1,
    )
    y_values = np.linspace(
        scene.wall.center_y - scene.wall.height * 0.5,
        scene.wall.center_y + scene.wall.height * 0.5,
        ny + 1,
    )
    vertices: list[list[float]] = []
    for y_value in y_values:
        for x_value in x_values:
            vertices.append([float(x_value), float(y_value), scene.wall.z])
    triangles: list[list[int]] = []
    stride = nx + 1
    for iy in range(ny):
        for ix in range(nx):
            a = iy * stride + ix
            b = a + 1
            c = a + stride
            d = c + 1
            triangles.append([a, b, d])
            triangles.append([a, d, c])
    return np.asarray(vertices, dtype=np.float64), np.asarray(triangles, dtype=np.int32)


def window_plane_basis(scene: Scene) -> tuple[np.ndarray, np.ndarray, np.ndarray]:
    normal = scene.window_normal
    helper = np.asarray([0.0, 0.0, 1.0], dtype=np.float64)
    if abs(float(np.dot(helper, normal))) > 0.95:
        helper = np.asarray([0.0, 1.0, 0.0], dtype=np.float64)
    basis_u = np.cross(helper, normal)
    basis_u /= np.linalg.norm(basis_u)
    basis_v = np.cross(normal, basis_u)
    basis_v /= np.linalg.norm(basis_v)
    return basis_u, basis_v, normal


def _ring_mesh(
    scene: Scene,
) -> tuple[np.ndarray, np.ndarray]:
    basis_u, basis_v, normal = window_plane_basis(scene)
    radii = []
    for piece in scene.pieces:
        relative = piece.vertices - scene.window_center
        u = relative @ basis_u
        v = relative @ basis_v
        radii.append(float(np.sqrt(u * u + v * v).max()))
    inner_radius = max(radii) if radii else 0.0
    outer_radius = max(scene.surround_outer_radius, inner_radius + 1e-4)
    segments = 72
    radial_steps = 3
    vertices: list[list[float]] = []
    for radial_index in range(radial_steps + 1):
        radius = inner_radius + (outer_radius - inner_radius) * (
            radial_index / radial_steps
        )
        for segment in range(segments):
            angle = math.tau * segment / segments
            point = (
                scene.window_center
                + basis_u * (radius * math.cos(angle))
                + basis_v * (radius * math.sin(angle))
                - normal * 0.035
            )
            vertices.append(point.tolist())
    triangles: list[list[int]] = []
    for radial_index in range(radial_steps):
        for segment in range(segments):
            next_segment = (segment + 1) % segments
            a = radial_index * segments + segment
            b = radial_index * segments + next_segment
            c = (radial_index + 1) * segments + segment
            d = (radial_index + 1) * segments + next_segment
            triangles.append([a, b, d])
            triangles.append([a, d, c])
    return np.asarray(vertices, dtype=np.float64), np.asarray(triangles, dtype=np.int32)


@dataclass(frozen=True)
class LeadEdge:
    key: tuple
    a: np.ndarray
    b: np.ndarray
    width: float
    depth: float


def _edge_key(a: np.ndarray, b: np.ndarray) -> tuple:
    rounded_a = tuple(float(round(value, 8)) for value in a)
    rounded_b = tuple(float(round(value, 8)) for value in b)
    return tuple(sorted((rounded_a, rounded_b)))


def lead_edges(scene: Scene) -> tuple[LeadEdge, ...]:
    accumulated: dict[tuple, dict] = {}
    for piece in scene.pieces:
        boundary_counts: dict[tuple[int, int], int] = {}
        for triangle in piece.triangles:
            a, b, c = int(triangle[0]), int(triangle[1]), int(triangle[2])
            for left, right in ((a, b), (b, c), (c, a)):
                pair = tuple(sorted((left, right)))
                boundary_counts[pair] = boundary_counts.get(pair, 0) + 1
        for (left, right), count in boundary_counts.items():
            if count != 1:
                continue
            a = piece.vertices[left]
            b = piece.vertices[right]
            key = _edge_key(a, b)
            record = accumulated.get(key)
            width = max(piece.lead_width, 0.0)
            depth = max(piece.lead_depth, 0.0)
            if record is None:
                accumulated[key] = {
                    "a": a.copy(),
                    "b": b.copy(),
                    "width": width,
                    "depth": depth,
                }
            else:
                record["width"] = max(record["width"], width)
                record["depth"] = max(record["depth"], depth)
    result = [
        LeadEdge(
            key=key,
            a=record["a"],
            b=record["b"],
            width=float(record["width"]),
            depth=float(record["depth"]),
        )
        for key, record in accumulated.items()
    ]
    result.sort(key=lambda edge: edge.key)
    return tuple(result)


def lead_mesh(edges: Iterable[LeadEdge]) -> tuple[np.ndarray, np.ndarray]:
    vertices: list[list[float]] = []
    triangles: list[list[int]] = []
    for edge in edges:
        direction = edge.b - edge.a
        plan_length = float(np.linalg.norm(direction[:2]))
        if plan_length <= 1e-12:
            continue
        perpendicular = np.asarray(
            [-direction[1] / plan_length, direction[0] / plan_length, 0.0],
            dtype=np.float64,
        )
        half = edge.width * 0.5
        base = len(vertices)
        for point in (edge.a, edge.b):
            for sign in (-1.0, 1.0):
                bottom = point + perpendicular * (half * sign)
                top = bottom.copy()
                top[2] += edge.depth
                vertices.append(bottom.tolist())
                vertices.append(top.tolist())
        # Vertex order per endpoint: a-bottom-, a-top-, a-bottom+, a-top+,
        # b-bottom-, b-top-, b-bottom+, b-top+.
        a0, a1, a2, a3, b0, b1, b2, b3 = (
            base + index for index in range(8)
        )
        # Top surface.
        triangles.extend([[a1, b1, b3], [a1, b3, a3]])
        # Bottom surface.
        triangles.extend([[a0, a2, b2], [a0, b2, b0]])
        # Side walls.
        triangles.extend([[a0, b0, b1], [a0, b1, a1]])
        triangles.extend([[a2, a3, b3], [a2, b3, b2]])
        triangles.extend([[a0, a1, a3], [a0, a3, a2]])
        triangles.extend([[b0, b2, b3], [b0, b3, b1]])
    return np.asarray(vertices, dtype=np.float64), np.asarray(triangles, dtype=np.int32)


@dataclass(frozen=True)
class Geometry:
    edges: tuple[LeadEdge, ...]
    lead_vertices: np.ndarray
    lead_triangles: np.ndarray
    wall_vertices: np.ndarray
    wall_triangles: np.ndarray
    ring_vertices: np.ndarray
    ring_triangles: np.ndarray


def build_geometry(scene: Scene) -> Geometry:
    edges = lead_edges(scene)
    lead_vertices, lead_triangles = lead_mesh(edges)
    wall_vertices, wall_triangles = _wall_grid(scene)
    ring_vertices, ring_triangles = _ring_mesh(scene)
    return Geometry(
        edges=edges,
        lead_vertices=lead_vertices,
        lead_triangles=lead_triangles,
        wall_vertices=wall_vertices,
        wall_triangles=wall_triangles,
        ring_vertices=ring_vertices,
        ring_triangles=ring_triangles,
    )


def rasterize_scene(scene: Scene, geometry: Geometry) -> HitBuffers:
    buffers = create_hit_buffers(scene.width, scene.height)
    rasterize_mesh(
        buffers,
        scene,
        geometry.wall_vertices,
        geometry.wall_triangles,
        kind=KIND_WALL,
    )
    rasterize_mesh(
        buffers,
        scene,
        geometry.ring_vertices,
        geometry.ring_triangles,
        kind=KIND_STONE,
    )
    for piece in scene.pieces:
        rasterize_mesh(
            buffers,
            scene,
            piece.vertices,
            piece.triangles,
            kind=KIND_GLASS,
            owners=np.full(len(piece.triangles), piece.serial, dtype=np.int32),
        )
    rasterize_mesh(
        buffers,
        scene,
        geometry.lead_vertices,
        geometry.lead_triangles,
        kind=KIND_LEAD,
    )
    return buffers
