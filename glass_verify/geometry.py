from __future__ import annotations

from dataclasses import dataclass

import numpy as np

from .scene import EPS, Scene, Wall
from .texture import build_texture_tables, pane_variation


@dataclass(frozen=True)
class LeadBar:
    pane_index: int
    sector_id: int
    pane_id: int
    edge_index: int
    center: np.ndarray
    axis_e: np.ndarray
    axis_m: np.ndarray
    axis_n: np.ndarray
    half_length: float
    half_width: float
    half_depth: float


@dataclass(frozen=True)
class ShadowEdge:
    pane_index: int
    sector_id: int
    pane_id: int
    edge_index: int
    a_uv: np.ndarray
    b_uv: np.ndarray
    width: float
    depth: float


@dataclass
class GeometryContext:
    scene: Scene
    labels_by_key: dict[tuple[int, int], int]
    label_info: list[dict[str, int]]
    pane_index_by_label: dict[int, int]
    textures: np.ndarray
    pane_variations: np.ndarray
    lead_bars: tuple[LeadBar, ...]
    shadow_edges: tuple[ShadowEdge, ...]
    aperture_min: np.ndarray
    aperture_max: np.ndarray
    shadow_offsets: np.ndarray


def _norm(value: np.ndarray) -> np.ndarray:
    length = float(np.linalg.norm(value))
    if length <= EPS:
        return np.zeros_like(value)
    return value / length


def _build_lead_bars(scene: Scene) -> tuple[LeadBar, ...]:
    bars: list[LeadBar] = []
    normal = scene.window.normal
    for pane_index, pane in enumerate(scene.panes):
        edge_index = 0
        for first, second in pane.boundary_edges:
            a = pane.vertices[first]
            b = pane.vertices[second]
            edge = b - a
            length = float(np.linalg.norm(edge))
            if length <= EPS:
                continue
            axis_e = edge / length
            axis_m = _norm(np.cross(axis_e, normal))
            if np.linalg.norm(axis_m) <= EPS:
                continue
            bars.append(
                LeadBar(
                    pane_index=pane_index,
                    sector_id=pane.sector_id,
                    pane_id=pane.pane_id,
                    edge_index=edge_index,
                    center=0.5 * (a + b),
                    axis_e=axis_e,
                    axis_m=axis_m,
                    axis_n=normal,
                    half_length=0.5 * length,
                    half_width=0.5 * pane.lead_width,
                    half_depth=max(0.5 * pane.lead_depth, 1.0e-6),
                )
            )
            edge_index += 1
    bars.sort(key=lambda bar: (bar.sector_id, bar.pane_id, bar.edge_index))
    return tuple(bars)


def _build_shadow_edges(scene: Scene) -> tuple[ShadowEdge, ...]:
    edges: list[ShadowEdge] = []
    window = scene.window
    for pane_index, pane in enumerate(scene.panes):
        uv = np.column_stack(
            (
                (pane.vertices - window.origin) @ window.axis_u,
                (pane.vertices - window.origin) @ window.axis_v,
            )
        )
        edge_index = 0
        for first, second in pane.boundary_edges:
            edges.append(
                ShadowEdge(
                    pane_index=pane_index,
                    sector_id=pane.sector_id,
                    pane_id=pane.pane_id,
                    edge_index=edge_index,
                    a_uv=uv[first].copy(),
                    b_uv=uv[second].copy(),
                    width=pane.lead_width,
                    depth=pane.lead_depth,
                )
            )
            edge_index += 1
    edges.sort(key=lambda edge: (edge.sector_id, edge.pane_id, edge.edge_index))
    return tuple(edges)


def _shadow_offsets(samples_per_axis: int) -> np.ndarray:
    values = np.linspace(-1.0, 1.0, samples_per_axis, dtype=np.float64)
    offsets = np.asarray([(x, y) for y in values for x in values], dtype=np.float64)
    maximum = np.max(np.linalg.norm(offsets, axis=1))
    if maximum > 0.0:
        offsets = offsets / maximum
    return offsets


def build_geometry_context(scene: Scene) -> GeometryContext:
    labels_by_key: dict[tuple[int, int], int] = {}
    label_info: list[dict[str, int]] = []
    pane_index_by_label: dict[int, int] = {}
    for pane_index, pane in enumerate(scene.panes):
        label = pane_index + 2
        labels_by_key[(pane.sector_id, pane.pane_id)] = label
        label_info.append(
            {"value": label, "sector_id": pane.sector_id, "pane_id": pane.pane_id}
        )
        pane_index_by_label[label] = pane_index
    textures = build_texture_tables(scene)
    variations = np.asarray(
        [
            pane_variation(scene.texture.seed, pane.sector_id, pane.pane_id)
            for pane in scene.panes
        ],
        dtype=np.float64,
    )
    all_uv_min = np.min(np.stack([pane.uv_min for pane in scene.panes]), axis=0)
    all_uv_max = np.max(np.stack([pane.uv_max for pane in scene.panes]), axis=0)
    return GeometryContext(
        scene=scene,
        labels_by_key=labels_by_key,
        label_info=label_info,
        pane_index_by_label=pane_index_by_label,
        textures=textures,
        pane_variations=variations,
        lead_bars=_build_lead_bars(scene),
        shadow_edges=_build_shadow_edges(scene),
        aperture_min=all_uv_min,
        aperture_max=all_uv_max,
        shadow_offsets=_shadow_offsets(scene.projection.samples_per_axis),
    )


def plane_intersection(
    points: np.ndarray,
    directions: np.ndarray,
    plane_point: np.ndarray,
    plane_normal: np.ndarray,
) -> tuple[np.ndarray, np.ndarray]:
    denom = directions @ plane_normal
    numerator = (plane_point - points) @ plane_normal
    with np.errstate(divide="ignore", invalid="ignore"):
        distance = numerator / denom
    valid = np.abs(denom) > 1.0e-10
    distance = np.where(valid, distance, np.inf)
    hit = points + distance[:, None] * directions
    return distance, hit


def wall_uv(wall: Wall, points: np.ndarray) -> np.ndarray:
    relative = points - wall.point
    return np.column_stack((relative @ wall.axis_u, relative @ wall.axis_v))


def point_segment_distance(points: np.ndarray, first: np.ndarray, second: np.ndarray) -> np.ndarray:
    segment = second - first
    denominator = float(segment @ segment)
    if denominator <= 1.0e-20:
        return np.linalg.norm(points - first, axis=-1)
    relative = points - first
    t = np.clip((relative @ segment) / denominator, 0.0, 1.0)
    closest = first + t[..., None] * segment
    return np.linalg.norm(points - closest, axis=-1)
