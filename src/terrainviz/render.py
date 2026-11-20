from __future__ import annotations

import hashlib
import math
from dataclasses import dataclass
from typing import Any

import numpy as np

from .camera import Camera
from .cancel import CancellationToken
from .errors import E_INVALID_ARGUMENT, TerrainError
from .lod import LodDecision, LodSelection, choose_lod
from .material import MaterialMap, PALETTE, sample_material_color
from .normals import DEFAULT_NORMAL, NormalManager, NormalUpdate
from .seams import SeamRepairResult, repair_seams
from .terrain import Block, Terrain


@dataclass(frozen=True)
class RenderConfig:
    precision: str = "float64"
    background_rgb: tuple[int, int, int] = (12, 17, 25)
    light_direction: tuple[float, float, float] = (-0.45, 0.82, -0.35)
    minimum_ambient: float = 0.28

    def __post_init__(self) -> None:
        if self.precision not in ("float32", "float64"):
            raise TerrainError(
                E_INVALID_ARGUMENT,
                "precision must be 'float32' or 'float64'",
                precision=self.precision,
            )
        if len(self.background_rgb) != 3 or not all(
            0 <= int(value) <= 255 for value in self.background_rgb
        ):
            raise TerrainError(E_INVALID_ARGUMENT, "background_rgb must contain 0..255 values")
        light = np.asarray(self.light_direction, dtype=np.float64)
        if light.shape != (3,) or not np.isfinite(light).all() or np.linalg.norm(light) < 1e-12:
            raise TerrainError(E_INVALID_ARGUMENT, "light direction must be a finite non-zero vector")
        if not (0.0 <= self.minimum_ambient <= 1.0):
            raise TerrainError(E_INVALID_ARGUMENT, "minimum ambient must be in [0, 1]")

    @property
    def depth_dtype(self) -> np.dtype:
        return np.dtype(self.precision)

    def to_dict(self) -> dict[str, Any]:
        return {
            "precision": self.precision,
            "background_rgb": [int(value) for value in self.background_rgb],
            "light_direction": [float(value) for value in self.light_direction],
            "minimum_ambient": float(self.minimum_ambient),
        }


@dataclass
class RenderResult:
    color: np.ndarray
    depth: np.ndarray
    error_heatmap: np.ndarray
    report: dict[str, Any]
    decision: LodDecision
    seam_result: SeamRepairResult
    normal_update: NormalUpdate

    @property
    def color_digest(self) -> str:
        return hashlib.sha256(self.color.tobytes(order="C")).hexdigest()

    @property
    def depth_digest(self) -> str:
        return hashlib.sha256(self.depth.tobytes(order="C")).hexdigest()

    @property
    def heatmap_digest(self) -> str:
        return hashlib.sha256(self.error_heatmap.tobytes(order="C")).hexdigest()

    @property
    def combined_digest(self) -> str:
        digest = hashlib.sha256()
        digest.update(self.color.tobytes(order="C"))
        digest.update(self.depth.tobytes(order="C"))
        digest.update(self.error_heatmap.tobytes(order="C"))
        return digest.hexdigest()


@dataclass
class _Mesh:
    positions: list[tuple[float, float, float]]
    vertex_normals: list[np.ndarray]
    material_samples: list[tuple[float, float]]
    triangles: list[tuple[int, int, int]]
    triangle_centers: list[tuple[float, float]]
    block: Block
    selection: LodSelection


def _between(values: np.ndarray, low: int, high: int) -> np.ndarray:
    return values[(values > low) & (values < high)]


def _sample_ground_color(
    terrain: Terrain,
    material: MaterialMap | None,
    row: float,
    col: float,
) -> np.ndarray:
    if material is not None:
        sampled = sample_material_color(
            material, terrain.width, terrain.height, np.asarray(row), np.asarray(col)
        )
        return np.asarray(sampled, dtype=np.float64).reshape(3)
    normalized = float(
        np.clip(
            (terrain.heightfield.height_at(row, col) - terrain.heightfield.valid_min)
            / terrain.heightfield.elevation_range,
            0.0,
            1.0,
        )
    )
    index = min(int(normalized * len(PALETTE)), len(PALETTE) - 1)
    return PALETTE[index].copy()


def _build_block_mesh(
    terrain: Terrain,
    material: MaterialMap | None,
    normals: NormalManager,
    selection: LodSelection,
    block: Block,
    boundary_samples: dict[str, np.ndarray],
) -> _Mesh:
    stride = selection.stride
    rows = np.arange(block.row0, block.row1 + 1, stride, dtype=np.int64)
    cols = np.arange(block.col0, block.col1 + 1, stride, dtype=np.int64)
    if rows[-1] != block.row1:
        rows = np.append(rows, block.row1)
    if cols[-1] != block.col1:
        cols = np.append(cols, block.col1)

    positions: list[tuple[float, float, float]] = []
    vertex_normals: list[np.ndarray] = []
    material_samples: list[tuple[float, float]] = []
    triangles: list[tuple[int, int, int]] = []
    triangle_centers: list[tuple[float, float]] = []

    def add_vertex(row: float, col: float) -> int:
        height = terrain.heightfield.height_at(row, col)
        positions.append(
            (float(col) * terrain.spacing, float(height), float(row) * terrain.spacing)
        )
        vertex_normals.append(normals.sample(block.id, row, col, block))
        material_samples.append((float(row), float(col)))
        return len(positions) - 1

    def add_triangle(indices: tuple[int, int, int]) -> None:
        i0, i1, i2 = indices
        if len(set(indices)) < 3:
            return
        p0 = np.asarray(positions[i0])
        p1 = np.asarray(positions[i1])
        p2 = np.asarray(positions[i2])
        if np.linalg.norm(np.cross(p1 - p0, p2 - p0)) <= 1e-12:
            return
        triangles.append(indices)
        center_rows = (
            material_samples[i0][0] + material_samples[i1][0] + material_samples[i2][0]
        ) / 3.0
        center_cols = (
            material_samples[i0][1] + material_samples[i1][1] + material_samples[i2][1]
        ) / 3.0
        triangle_centers.append((center_rows, center_cols))

    top_extra = boundary_samples.get("N", cols)
    bottom_extra = boundary_samples.get("S", cols)
    left_extra = boundary_samples.get("W", rows)
    right_extra = boundary_samples.get("E", rows)

    for row_index in range(rows.size - 1):
        r_top = int(rows[row_index])
        r_bottom = int(rows[row_index + 1])
        for col_index in range(cols.size - 1):
            c_left = int(cols[col_index])
            c_right = int(cols[col_index + 1])
            touches_boundary = (
                row_index == 0
                or row_index == rows.size - 2
                or col_index == 0
                or col_index == cols.size - 2
            )
            if not touches_boundary:
                v00 = add_vertex(r_top, c_left)
                v01 = add_vertex(r_top, c_right)
                v10 = add_vertex(r_bottom, c_left)
                v11 = add_vertex(r_bottom, c_right)
                add_triangle((v00, v10, v11))
                add_triangle((v00, v11, v01))
                continue

            ring_keys: list[tuple[float, float]] = []
            ring_keys.append((r_top, c_left))
            if row_index == 0:
                for value in _between(top_extra, c_left, c_right):
                    ring_keys.append((r_top, float(value)))
            ring_keys.append((r_top, c_right))
            if col_index == cols.size - 2:
                for value in _between(right_extra, r_top, r_bottom):
                    ring_keys.append((float(value), c_right))
            ring_keys.append((r_bottom, c_right))
            if row_index == rows.size - 2:
                for value in _between(bottom_extra, c_left, c_right)[::-1]:
                    ring_keys.append((r_bottom, float(value)))
            ring_keys.append((r_bottom, c_left))
            if col_index == 0:
                for value in _between(left_extra, r_top, r_bottom)[::-1]:
                    ring_keys.append((float(value), c_left))

            deduped: list[tuple[float, float]] = []
            for key in ring_keys:
                if not deduped or deduped[-1] != key:
                    deduped.append(key)
            if len(deduped) > 1 and deduped[0] == deduped[-1]:
                deduped.pop()
            center_row = (r_top + r_bottom) * 0.5
            center_col = (c_left + c_right) * 0.5
            center_index = add_vertex(center_row, center_col)
            ring_indices = [add_vertex(row, col) for row, col in deduped]
            for index in range(len(ring_indices)):
                add_triangle(
                    (
                        center_index,
                        ring_indices[index],
                        ring_indices[(index + 1) % len(ring_indices)],
                    )
                )

    return _Mesh(
        positions=positions,
        vertex_normals=vertex_normals,
        material_samples=material_samples,
        triangles=triangles,
        triangle_centers=triangle_centers,
        block=block,
        selection=selection,
    )


def _triangle_colors(
    terrain: Terrain,
    material: MaterialMap | None,
    config: RenderConfig,
    mesh: _Mesh,
) -> np.ndarray:
    light = np.asarray(config.light_direction, dtype=np.float64)
    light /= np.linalg.norm(light)
    colors = np.empty((len(mesh.triangles), 3), dtype=np.float64)
    for index, (i0, i1, i2) in enumerate(mesh.triangles):
        normal = (
            mesh.vertex_normals[i0] + mesh.vertex_normals[i1] + mesh.vertex_normals[i2]
        )
        length = float(np.linalg.norm(normal))
        normal = normal / length if length > 1e-15 else DEFAULT_NORMAL
        diffuse = max(0.0, float(np.dot(normal, light)))
        intensity = config.minimum_ambient + (1.0 - config.minimum_ambient) * diffuse
        center_row, center_col = mesh.triangle_centers[index]
        base = _sample_ground_color(terrain, material, center_row, center_col)
        colors[index] = base * intensity
    return np.clip(np.floor(colors + 0.5), 0.0, 255.0)


def _rasterize_mesh(
    mesh: _Mesh,
    camera: Camera,
    config: RenderConfig,
    color_buffer: np.ndarray,
    depth_buffer: np.ndarray,
    heat_buffer: np.ndarray,
    triangle_colors: np.ndarray,
) -> tuple[int, int]:
    if not mesh.triangles:
        return 0, 0
    positions = np.asarray(mesh.positions, dtype=np.float64)
    screen_x, screen_y, view_z, visible = camera.project(positions)
    if config.precision == "float32":
        screen_x = screen_x.astype(np.float32).astype(np.float64)
        screen_y = screen_y.astype(np.float32).astype(np.float64)
        view_z = view_z.astype(np.float32).astype(np.float64)
    width = camera.width
    height = camera.height
    spans = 0
    for triangle_index, triangle in enumerate(mesh.triangles):
        i0, i1, i2 = triangle
        if not (visible[i0] and visible[i1] and visible[i2]):
            continue
        x0, y0, z0 = screen_x[i0], screen_y[i0], view_z[i0]
        x1, y1, z1 = screen_x[i1], screen_y[i1], view_z[i1]
        x2, y2, z2 = screen_x[i2], screen_y[i2], view_z[i2]
        denominator = (y1 - y2) * (x0 - x2) + (x2 - x1) * (y0 - y2)
        if abs(denominator) < 1e-12:
            continue
        min_x = max(0, int(math.floor(min(x0, x1, x2))))
        max_x = min(width - 1, int(math.ceil(max(x0, x1, x2))))
        min_y = max(0, int(math.floor(min(y0, y1, y2))))
        max_y = min(height - 1, int(math.ceil(max(y0, y1, y2))))
        if min_x > max_x or min_y > max_y:
            continue
        px = np.arange(min_x, max_x + 1, dtype=np.float64)[None, :]
        py = np.arange(min_y, max_y + 1, dtype=np.float64)[:, None]
        w0 = ((y1 - y2) * (px - x2) + (x2 - x1) * (py - y2)) / denominator
        w1 = ((y2 - y0) * (px - x2) + (x0 - x2) * (py - y2)) / denominator
        w2 = 1.0 - w0 - w1
        inside = (w0 >= -1e-9) & (w1 >= -1e-9) & (w2 >= -1e-9)
        if not np.any(inside):
            continue
        z_values = w0 * z0 + w1 * z1 + w2 * z2
        zbuf = depth_buffer[min_y : max_y + 1, min_x : max_x + 1]
        write = inside & (z_values < zbuf)
        if not np.any(write):
            continue
        cbuf = color_buffer[min_y : max_y + 1, min_x : max_x + 1]
        hbuf = heat_buffer[min_y : max_y + 1, min_x : max_x + 1]
        cbuf[write] = triangle_colors[triangle_index].astype(np.uint8)
        zbuf[write] = z_values[write]
        hbuf[write] = mesh.selection.budget_usage
        spans += int(np.count_nonzero(write))
    return len(mesh.triangles), spans


def _pixel_diff(
    previous: RenderResult | None,
    color: np.ndarray,
    depth: np.ndarray,
    heatmap: np.ndarray,
    precision_changed: bool,
) -> dict[str, Any] | None:
    if previous is None:
        return None
    if (
        previous.color.shape != color.shape
        or previous.depth.shape != depth.shape
        or previous.error_heatmap.shape != heatmap.shape
    ):
        return {
            "shape_changed": True,
            "precision_changed": precision_changed,
        }
    color_changed = int(np.count_nonzero(np.any(previous.color != color, axis=2)))
    depth_difference = np.abs(
        previous.depth.astype(np.float64) - depth.astype(np.float64)
    )
    heat_difference = np.abs(
        previous.error_heatmap.astype(np.float64)
        - heatmap.astype(np.float64)
    )
    return {
        "shape_changed": False,
        "precision_changed": precision_changed,
        "changed_color_pixels": color_changed,
        "changed_color_fraction": round(
            color_changed / max(1, color.shape[0] * color.shape[1]), 12
        ),
        "mean_abs_depth_difference": float(np.mean(depth_difference)),
        "max_abs_depth_difference": float(np.max(depth_difference)),
        "mean_abs_heatmap_difference": float(np.mean(heat_difference)),
        "max_abs_heatmap_difference": float(np.max(heat_difference)),
    }


class Renderer:
    def __init__(self) -> None:
        self.normal_manager = NormalManager()
        self._previous: RenderResult | None = None
        self.render_count = 0

    def render(
        self,
        terrain: Terrain,
        camera: Camera,
        screen_error_threshold: float,
        seam_tolerance: float,
        max_level: int = 4,
        config: RenderConfig | None = None,
        cancel_token: CancellationToken | None = None,
    ) -> RenderResult:
        config = config or RenderConfig()
        decision = choose_lod(terrain, camera, screen_error_threshold, max_level)
        if cancel_token is not None:
            cancel_token.check("after-lod-selection")
        seam_result = repair_seams(terrain, decision, seam_tolerance)
        if cancel_token is not None:
            cancel_token.check("after-seam-repair")
        selected_ids = sorted(seam_result.decision.selections)
        normal_update = self.normal_manager.ensure(
            terrain, selected_ids, material=terrain.material
        )
        if cancel_token is not None:
            cancel_token.check("after-normal-update")

        width = camera.width
        height = camera.height
        color = np.empty((height, width, 3), dtype=np.uint8)
        color[:] = np.asarray(config.background_rgb, dtype=np.uint8)
        depth = np.full(
            (height, width), float(camera.far), dtype=config.depth_dtype
        )
        heatmap = np.zeros((height, width), dtype=np.float32)
        triangle_count = 0
        fragment_spans = 0
        block_heat: list[dict[str, Any]] = []

        for block_id in selected_ids:
            if cancel_token is not None:
                cancel_token.check(f"block-boundary:{block_id}")
            block = terrain.block_by_id[block_id]
            selection = seam_result.decision.selections[block_id]
            mesh = _build_block_mesh(
                terrain,
                terrain.material,
                self.normal_manager,
                selection,
                block,
                seam_result.boundary_samples[block_id],
            )
            triangle_colors = _triangle_colors(terrain, terrain.material, config, mesh)
            triangles, spans = _rasterize_mesh(
                mesh,
                camera,
                config,
                color,
                depth,
                heatmap,
                triangle_colors,
            )
            triangle_count += triangles
            fragment_spans += spans
            block_heat.append(
                {
                    "block_id": block_id,
                    "level": selection.level,
                    "budget_usage": round(selection.budget_usage, 9),
                    "triangles": triangles,
                    "rasterized_pixels": spans,
                }
            )

        if cancel_token is not None:
            cancel_token.check("before-buffer-commit")
        previous_precision = (
            self._previous.report.get("render_config", {}).get("precision")
            if self._previous is not None
            else None
        )
        precision_changed = (
            previous_precision is not None and previous_precision != config.precision
        )
        pixel_diff = _pixel_diff(
            self._previous, color, depth, heatmap, precision_changed
        )
        finite_heat = heatmap[np.isfinite(heatmap)]
        error_summary = seam_result.decision.error_summary()
        error_summary.update(
            {
                "max_raster_heat": round(float(np.max(finite_heat)), 9)
                if finite_heat.size
                else 0.0,
                "mean_raster_heat": round(float(np.mean(finite_heat)), 9)
                if finite_heat.size
                else 0.0,
            }
        )
        report = {
            "format": "terrainviz-render-report",
            "version": 1,
            "heightfield_digest": terrain.heightfield.source_digest,
            "height_digest": terrain.heightfield.height_digest,
            "material_version": terrain.material.material_version
            if terrain.material is not None
            else None,
            "tree_build": terrain.last_build_report.to_dict(),
            "camera": camera.to_dict(),
            "render_config": config.to_dict(),
            "screen_error_threshold": float(screen_error_threshold),
            "seam_tolerance": float(seam_tolerance),
            "levels": seam_result.decision.level_table,
            "lod_summary": seam_result.decision.error_summary(),
            "seams": seam_result.to_dict(),
            "normals": normal_update.to_dict(),
            "blocks": block_heat,
            "triangle_count": triangle_count,
            "fragment_spans": fragment_spans,
            "error_summary": error_summary,
            "precision_changed": precision_changed,
            "pixel_diff": pixel_diff,
            "buffers": {
                "color_sha256": hashlib.sha256(color.tobytes(order="C")).hexdigest(),
                "depth_sha256": hashlib.sha256(depth.tobytes(order="C")).hexdigest(),
                "error_heatmap_sha256": hashlib.sha256(
                    heatmap.tobytes(order="C")
                ).hexdigest(),
            },
        }
        result = RenderResult(
            color=color,
            depth=depth,
            error_heatmap=heatmap,
            report=report,
            decision=seam_result.decision,
            seam_result=seam_result,
            normal_update=normal_update,
        )
        self._previous = result
        self.render_count += 1
        return result

