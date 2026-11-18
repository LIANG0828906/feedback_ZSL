"""Direction-light projection, soft shadow maps, and landing reports."""

from __future__ import annotations

import hashlib
from dataclasses import dataclass
from typing import Any

import numpy as np

from .math3d import EPS
from .scene import Board, Layer, Light, Scene, Wall


@dataclass
class PlaneReceiver:
    key: tuple[str, int, int]
    kind: str
    layer_index: int | None
    object_id: int | None
    origin: np.ndarray
    axis_u: np.ndarray
    axis_v: np.ndarray
    normal: np.ndarray
    resolution: tuple[int, int]
    uv_matrix: np.ndarray
    uv_residual: float

    def uv_from_world(self, points: np.ndarray) -> np.ndarray:
        relative = np.asarray(points, dtype=np.float64) - self.origin
        return relative @ self.uv_matrix.T

    def world_from_uv(self, u: np.ndarray | float, v: np.ndarray | float) -> np.ndarray:
        u_arr = np.asarray(u, dtype=np.float64)
        v_arr = np.asarray(v, dtype=np.float64)
        return self.origin + u_arr[..., None] * self.axis_u + v_arr[..., None] * self.axis_v


@dataclass
class ShadowMap:
    receiver: PlaneReceiver
    light_id: int
    coverage: np.ndarray
    penumbra_width: float
    layer_gap: float
    samples: int
    digest: str
    mean_gray: float


def fit_affine_plane(vertices: np.ndarray, uvs: np.ndarray, normal: np.ndarray) -> tuple[np.ndarray, np.ndarray, np.ndarray, float, np.ndarray]:
    """Fit world = origin + u*axis_u + v*axis_v for a planar board."""
    design = np.column_stack((np.asarray(uvs, dtype=np.float64), np.ones(len(uvs), dtype=np.float64)))
    coefficients, _, rank, _ = np.linalg.lstsq(design, np.asarray(vertices, dtype=np.float64), rcond=None)
    if rank < 3:
        raise ValueError("UV mapping is rank deficient")
    axis_u = coefficients[0].copy()
    axis_v = coefficients[1].copy()
    origin = coefficients[2].copy()
    residual = float(np.max(np.linalg.norm(design @ coefficients - np.asarray(vertices, dtype=np.float64), axis=1)))
    basis = np.column_stack((axis_u, axis_v))
    uv_matrix, _, _, _ = np.linalg.lstsq(basis, np.eye(3), rcond=None)
    return origin, axis_u, axis_v, residual, uv_matrix


def board_receiver(board: Board, layer: Layer) -> PlaneReceiver:
    if board.mesh.uvs is None:
        raise ValueError("board has no UVs")
    origin, axis_u, axis_v, residual, uv_matrix = fit_affine_plane(board.mesh.vertices, board.mesh.uvs, layer.normal)
    return PlaneReceiver(
        key=("star_board", layer.index, board.board_id),
        kind="star_board",
        layer_index=layer.index,
        object_id=board.board_id,
        origin=origin,
        axis_u=axis_u,
        axis_v=axis_v,
        normal=layer.normal.copy(),
        resolution=board.resolution,
        uv_matrix=uv_matrix,
        uv_residual=residual,
    )


def wall_receiver(wall: Wall) -> PlaneReceiver:
    origin = wall.point - wall.u_axis * wall.half_size[0] - wall.v_axis * wall.half_size[1]
    axis_u = wall.u_axis * (2.0 * wall.half_size[0])
    axis_v = wall.v_axis * (2.0 * wall.half_size[1])
    basis = np.column_stack((axis_u, axis_v))
    uv_matrix, _, _, _ = np.linalg.lstsq(basis, np.eye(3), rcond=None)
    return PlaneReceiver(
        key=("wall", -1, -1),
        kind="wall",
        layer_index=None,
        object_id=None,
        origin=origin,
        axis_u=axis_u,
        axis_v=axis_v,
        normal=wall.normal.copy(),
        resolution=wall.resolution,
        uv_matrix=uv_matrix,
        uv_residual=0.0,
    )


def project_point_to_plane(point: np.ndarray, direction: np.ndarray, receiver: PlaneReceiver) -> tuple[np.ndarray, np.ndarray]:
    denom = float(np.dot(direction, receiver.normal))
    if abs(denom) <= EPS:
        raise ValueError("projection direction is parallel to receiver plane")
    t = float(np.dot(receiver.origin - point, receiver.normal)) / denom
    world = np.asarray(point, dtype=np.float64) + direction * t
    uv = receiver.uv_from_world(world[None, :])[0]
    return world, uv


def _rasterize_uv_triangle(grid: np.ndarray, uv_triangle: np.ndarray) -> None:
    height, width = grid.shape
    x = uv_triangle[:, 0] * width
    y = uv_triangle[:, 1] * height
    min_x = max(0, int(np.floor(np.min(x) - 0.5)))
    max_x = min(width - 1, int(np.ceil(np.max(x) + 0.5)))
    min_y = max(0, int(np.floor(np.min(y) - 0.5)))
    max_y = min(height - 1, int(np.ceil(np.max(y) + 0.5)))
    if min_x > max_x or min_y > max_y:
        return
    xs = np.arange(min_x, max_x + 1, dtype=np.float64) + 0.5
    ys = np.arange(min_y, max_y + 1, dtype=np.float64) + 0.5
    xx, yy = np.meshgrid(xs, ys)
    x0, y0 = x[0], y[0]
    x1, y1 = x[1], y[1]
    x2, y2 = x[2], y[2]
    denominator = (y1 - y2) * (x0 - x2) + (x2 - x1) * (y0 - y2)
    if abs(denominator) <= 1.0e-14:
        return
    b0 = ((y1 - y2) * (xx - x2) + (x2 - x1) * (yy - y2)) / denominator
    b1 = ((y2 - y0) * (xx - x2) + (x0 - x2) * (yy - y2)) / denominator
    b2 = 1.0 - b0 - b1
    inside = (b0 >= -1.0e-10) & (b1 >= -1.0e-10) & (b2 >= -1.0e-10)
    subgrid = grid[min_y : max_y + 1, min_x : max_x + 1]
    subgrid[inside] = 1.0


def _disk_offsets(count: int) -> np.ndarray:
    if count <= 1:
        return np.zeros((1, 2), dtype=np.float64)
    indices = np.arange(count, dtype=np.float64)
    radius = np.sqrt((indices + 0.5) / count)
    angle = indices * (np.pi * (3.0 - np.sqrt(5.0)))
    return np.column_stack((radius * np.cos(angle), radius * np.sin(angle)))


def _shift_bilinear_zero(image: np.ndarray, dx: float, dy: float) -> np.ndarray:
    height, width = image.shape
    yy, xx = np.mgrid[0:height, 0:width]
    sample_x = xx - dx
    sample_y = yy - dy
    x0 = np.floor(sample_x).astype(np.int64)
    y0 = np.floor(sample_y).astype(np.int64)
    x1 = x0 + 1
    y1 = y0 + 1
    fx = sample_x - x0
    fy = sample_y - y0

    def gather(ix: np.ndarray, iy: np.ndarray) -> np.ndarray:
        valid = (ix >= 0) & (ix < width) & (iy >= 0) & (iy < height)
        out = np.zeros((height, width), dtype=np.float64)
        out[valid] = image[iy[valid], ix[valid]]
        return out

    c00 = gather(x0, y0)
    c10 = gather(x1, y0)
    c01 = gather(x0, y1)
    c11 = gather(x1, y1)
    return (c00 * (1.0 - fx) + c10 * fx) * (1.0 - fy) + (c01 * (1.0 - fx) + c11 * fx) * fy


def _soften_coverage(coverage: np.ndarray, radius_x: float, radius_y: float, samples: int) -> np.ndarray:
    if radius_x <= 1.0e-9 and radius_y <= 1.0e-9:
        return coverage.copy()
    offsets = _disk_offsets(samples)
    result = np.zeros_like(coverage, dtype=np.float64)
    for offset in offsets:
        result += _shift_bilinear_zero(coverage, float(offset[0] * radius_x), float(offset[1] * radius_y))
    result /= float(len(offsets))
    return result


def _coverage_digest(coverage: np.ndarray) -> str:
    packed = np.rint(np.clip(coverage, 0.0, 1.0) * 65535.0).astype("<u2")
    return hashlib.sha256(packed.tobytes()).hexdigest()


def _receiver_metric(receiver: PlaneReceiver) -> tuple[float, float]:
    width, height = receiver.resolution
    world_u = max(float(np.linalg.norm(receiver.axis_u)), 1.0e-12)
    world_v = max(float(np.linalg.norm(receiver.axis_v)), 1.0e-12)
    return width / world_u, height / world_v


def _adjacent_blocker_layer(scene: Scene, receiver: PlaneReceiver) -> Layer | None:
    if receiver.kind == "star_board":
        target = int(receiver.layer_index or 0) - 1
        if target < 0:
            return None
        return next((layer for layer in scene.layers if layer.index == target), None)
    if scene.layers:
        return scene.layers[-1]
    return None


def _build_one_shadow_map(scene: Scene, receiver: PlaneReceiver, light: Light, blocker: Layer | None) -> ShadowMap:
    width, height = receiver.resolution
    density = scene.renderer.projection_density
    high_width = width * density
    high_height = height * density
    high_grid = np.zeros((high_height, high_width), dtype=np.float64)
    gap = 0.0
    if blocker is not None:
        gap = abs(float(np.dot(receiver.origin - blocker.plane_point, blocker.normal)))
        for rib in blocker.ribs:
            for triangle in rib.mesh.triangles:
                vertices = rib.mesh.vertices[triangle]
                projected_uv: list[np.ndarray] = []
                valid = True
                for vertex in vertices:
                    try:
                        _, uv = project_point_to_plane(vertex, light.direction, receiver)
                    except ValueError:
                        valid = False
                        break
                    projected_uv.append(uv)
                if not valid:
                    continue
                triangle_uv = np.asarray(projected_uv)
                centroid = np.mean(triangle_uv, axis=0)
                if centroid[0] < 0.0 or centroid[0] > 1.0 or centroid[1] < 0.0 or centroid[1] > 1.0:
                    continue
                _rasterize_uv_triangle(high_grid, triangle_uv)
    coverage = high_grid.reshape(height, density, width, density).mean(axis=(1, 3))
    penumbra_width = gap * float(np.tan(light.angular_radius)) + light.source_distance * light.distance_softness
    px_per_world_u, px_per_world_v = _receiver_metric(receiver)
    radius_x = 0.5 * penumbra_width * px_per_world_u
    radius_y = 0.5 * penumbra_width * px_per_world_v
    coverage = np.clip(_soften_coverage(coverage, radius_x, radius_y, scene.renderer.shadow_samples), 0.0, 1.0)
    return ShadowMap(
        receiver=receiver,
        light_id=light.light_id,
        coverage=coverage,
        penumbra_width=penumbra_width,
        layer_gap=gap,
        samples=scene.renderer.shadow_samples,
        digest=_coverage_digest(coverage),
        mean_gray=float(np.mean(coverage)),
    )


def build_shadow_maps(scene: Scene) -> dict[tuple[str, int, int, int], ShadowMap]:
    maps: dict[tuple[str, int, int, int], ShadowMap] = {}
    for layer in scene.layers:
        blocker = _adjacent_blocker_layer(scene, board_receiver_stub(layer, layer.boards[0]))
        for board in layer.boards:
            receiver = board_receiver(board, layer)
            for light in scene.lights:
                maps[(receiver.key[0], receiver.key[1], receiver.key[2], light.light_id)] = _build_one_shadow_map(scene, receiver, light, blocker)
    wall = wall_receiver(scene.wall)
    blocker = _adjacent_blocker_layer(scene, wall)
    for light in scene.lights:
        maps[(wall.key[0], wall.key[1], wall.key[2], light.light_id)] = _build_one_shadow_map(scene, wall, light, blocker)
    return maps


def board_receiver_stub(layer: Layer, board: Board) -> PlaneReceiver:
    """Return a lightweight key-only receiver for blocker lookup."""
    return PlaneReceiver(
        key=("star_board", layer.index, board.board_id),
        kind="star_board",
        layer_index=layer.index,
        object_id=board.board_id,
        origin=layer.plane_point.copy(),
        axis_u=np.zeros(3),
        axis_v=np.zeros(3),
        normal=layer.normal.copy(),
        resolution=board.resolution,
        uv_matrix=np.zeros((2, 3)),
        uv_residual=0.0,
    )


def sample_shadow_map(shadow: ShadowMap, u: np.ndarray, v: np.ndarray) -> np.ndarray:
    coverage = shadow.coverage
    height, width = coverage.shape
    u_arr = np.clip(np.asarray(u, dtype=np.float64), 0.0, 1.0)
    v_arr = np.clip(np.asarray(v, dtype=np.float64), 0.0, 1.0)
    x = u_arr * (width - 1)
    y = v_arr * (height - 1)
    x0 = np.floor(x).astype(np.int64)
    y0 = np.floor(y).astype(np.int64)
    x1 = np.minimum(x0 + 1, width - 1)
    y1 = np.minimum(y0 + 1, height - 1)
    fx = np.asarray(x - x0)[..., None]
    fy = np.asarray(y - y0)[..., None]
    c00 = coverage[y0, x0][..., None]
    c10 = coverage[y0, x1][..., None]
    c01 = coverage[y1, x0][..., None]
    c11 = coverage[y1, x1][..., None]
    top = c00 * (1.0 - fx) + c10 * fx
    bottom = c01 * (1.0 - fx) + c11 * fx
    return (top * (1.0 - fy) + bottom * fy)[..., 0]


def _screen_coordinates(scene: Scene, world: np.ndarray) -> tuple[list[float], bool]:
    v = world - scene.camera.position
    depth = float(np.dot(v, scene.camera.forward))
    if depth <= scene.camera.near or depth >= scene.camera.far:
        return [0.0, 0.0], False
    aspect = scene.renderer.width / scene.renderer.height
    ndc_x = float(np.dot(v, scene.camera.right)) / (depth * scene.camera.tan_half_fov * aspect)
    ndc_y = float(np.dot(v, scene.camera.true_up)) / (depth * scene.camera.tan_half_fov)
    px = (ndc_x + 1.0) * 0.5 * scene.renderer.width
    py = (1.0 - ndc_y) * 0.5 * scene.renderer.height
    return [round(px, 6), round(py, 6)], 0.0 <= px < scene.renderer.width and 0.0 <= py < scene.renderer.height


def build_projection_report(scene: Scene, shadow_maps: dict[tuple[str, int, int, int], ShadowMap]) -> dict[str, Any]:
    lights_out: list[dict[str, Any]] = []
    for light in sorted(scene.lights, key=lambda item: item.light_id):
        entries: list[dict[str, Any]] = []
        for layer in sorted(scene.layers, key=lambda item: item.index):
            outer_layer = next((candidate for candidate in scene.layers if candidate.index == layer.index + 1), None)
            receivers: list[PlaneReceiver] = []
            if outer_layer is not None:
                receivers.extend(board_receiver(board, outer_layer) for board in outer_layer.boards)
            receivers.append(wall_receiver(scene.wall))
            for rib in sorted(layer.ribs, key=lambda item: item.rib_id):
                for triangle in rib.mesh.triangles:
                    centroid = np.mean(rib.mesh.vertices[triangle], axis=0)
                    for receiver in receivers:
                        try:
                            world, uv = project_point_to_plane(centroid, light.direction, receiver)
                        except ValueError:
                            continue
                        accepted = bool(-1.0e-9 <= float(uv[0]) <= 1.0 + 1.0e-9 and -1.0e-9 <= float(uv[1]) <= 1.0 + 1.0e-9)
                        screen, screen_inside = _screen_coordinates(scene, world)
                        shadow_key = (receiver.key[0], receiver.key[1], receiver.key[2], light.light_id)
                        shadow_gray = None
                        if accepted and shadow_key in shadow_maps:
                            shadow_gray = round(float(sample_shadow_map(shadow_maps[shadow_key], np.array([uv[0]]), np.array([uv[1]]))[0]), 8)
                        entries.append(
                            {
                                "source": {
                                    "layer": layer.index,
                                    "rib_id": rib.rib_id,
                                    "point": "triangle_centroid",
                                    "origin": [round(float(value), 8) for value in centroid],
                                },
                                "receiver": {
                                    "kind": receiver.kind,
                                    "layer": receiver.layer_index,
                                    "id": receiver.object_id,
                                    "resolution": [receiver.resolution[0], receiver.resolution[1]],
                                },
                                "world": [round(float(value), 8) for value in world],
                                "uv": [round(float(uv[0]), 8), round(float(uv[1]), 8)],
                                "screen": screen,
                                "screen_inside": screen_inside,
                                "accepted": accepted,
                                "shadow_gray": shadow_gray,
                            }
                        )
        entries.sort(
            key=lambda item: (
                item["source"]["layer"],
                item["source"]["rib_id"],
                item["receiver"]["kind"],
                item["receiver"]["layer"] if item["receiver"]["layer"] is not None else -1,
                item["receiver"]["id"] if item["receiver"]["id"] is not None else -1,
                tuple(item["source"]["origin"]),
                tuple(item["world"]),
            )
        )
        accepted_count = sum(1 for entry in entries if entry["accepted"])
        lights_out.append(
            {
                "light_id": light.light_id,
                "direction": [round(float(value), 9) for value in light.direction],
                "irradiance": [round(float(value), 9) for value in light.irradiance],
                "entry_count": len(entries),
                "accepted_count": accepted_count,
                "outside_count": len(entries) - accepted_count,
                "entries": entries,
            }
        )
    return {
        "schema_version": 1,
        "projection": "directional_parallel",
        "coordinate_spaces": ["world", "receiver_uv", "screen_pixel_center"],
        "lights": lights_out,
    }


def shadow_summary(shadow_maps: dict[tuple[str, int, int, int], ShadowMap]) -> list[dict[str, Any]]:
    rows: list[dict[str, Any]] = []
    for key in sorted(shadow_maps):
        shadow = shadow_maps[key]
        rows.append(
            {
                "receiver_kind": key[0],
                "receiver_layer": key[1],
                "receiver_id": key[2],
                "light_id": key[3],
                "gap": round(shadow.layer_gap, 9),
                "penumbra_width": round(shadow.penumbra_width, 9),
                "samples": shadow.samples,
                "mean_gray": round(shadow.mean_gray, 9),
                "gray_digest": shadow.digest,
            }
        )
    return rows


def _measure_transition_width(line: np.ndarray, start: float, end: float) -> float:
    indices = np.arange(len(line), dtype=np.float64)
    high_cross = None
    low_cross = None
    for index in range(1, len(line)):
        a = float(line[index - 1])
        b = float(line[index])
        if high_cross is None and a >= start >= b:
            high_cross = float(indices[index - 1] + (a - start) / max(a - b, 1.0e-15))
        if low_cross is None and a >= end >= b:
            low_cross = float(indices[index - 1] + (a - end) / max(a - b, 1.0e-15))
    if high_cross is None or low_cross is None:
        return 0.0
    return max(0.0, low_cross - high_cross)


def penumbra_trend_report(scene: Scene) -> dict[str, Any]:
    """Compare deterministic edge profiles under the scene's sampling density."""
    light = scene.lights[0]
    receiver_layer = 1 if len(scene.layers) > 1 else 0
    layer = next(item for item in scene.layers if item.index == receiver_layer)
    receiver = board_receiver(layer.boards[0], layer)
    px_per_world = _receiver_metric(receiver)[0]
    base_gap = 0.0
    if len(scene.layers) > 1:
        inner = scene.layers[0]
        base_gap = abs(float(np.dot(receiver.origin - inner.plane_point, inner.normal)))
    scenarios = [
        ("base", base_gap, light.source_distance),
        ("gap_x2", base_gap * 2.0, light.source_distance),
        ("source_plus_2", base_gap, light.source_distance + 2.0),
        ("gap_x2_source_plus_2", base_gap * 2.0, light.source_distance + 2.0),
    ]
    rows: list[dict[str, Any]] = []
    line_width = 128
    for name, gap, source_distance in scenarios:
        world_width = gap * float(np.tan(light.angular_radius)) + source_distance * light.distance_softness
        radius_px = 0.5 * world_width * px_per_world
        line = np.ones((9, line_width), dtype=np.float64)
        line[:, line_width // 2 :] = 0.0
        softened = _soften_coverage(line, radius_px, 0.0, scene.renderer.shadow_samples)
        profile = softened[softened.shape[0] // 2]
        transition = _measure_transition_width(profile, 0.90, 0.10)
        offsets = (-2.0, -1.0, -0.5, 0.0, 0.5, 1.0, 2.0)
        center = line_width // 2
        sample_points = []
        for offset in offsets:
            coordinate = int(round(center + offset * max(radius_px, 1.0)))
            coordinate = min(line_width - 1, max(0, coordinate))
            sample_points.append({"offset_radius": offset, "x": coordinate, "gray": round(float(profile[coordinate]), 9)})
        quantized = np.rint(np.clip(softened, 0.0, 1.0) * 65535.0).astype("<u2")
        rows.append(
            {
                "scenario": name,
                "layer_gap": round(gap, 9),
                "source_distance": round(source_distance, 9),
                "penumbra_width_world": round(world_width, 9),
                "blur_radius_px": round(radius_px, 9),
                "edge_transition_10_90_px": round(transition, 9),
                "samples": sample_points,
                "gray_digest": hashlib.sha256(quantized.tobytes()).hexdigest(),
            }
        )
    return {
        "light_id": light.light_id,
        "receiver": {
            "kind": receiver.kind,
            "layer": receiver.layer_index,
            "id": receiver.object_id,
            "resolution": [receiver.resolution[0], receiver.resolution[1]],
        },
        "sampling_density": scene.renderer.shadow_samples,
        "projection_density": scene.renderer.projection_density,
        "model": "penumbra_width = gap*tan(angular_radius) + source_distance*distance_softness",
        "comparisons": rows,
    }
