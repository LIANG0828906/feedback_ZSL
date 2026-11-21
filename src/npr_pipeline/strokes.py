from __future__ import annotations

import bisect
import math
from dataclasses import dataclass
from typing import Any

from .canonical import DeterministicRNG, qfloat, stable_int
from .geometry import (
    Point,
    add,
    bbox,
    bbox_expand,
    bbox_intersects,
    distance,
    dot,
    lerp,
    line_polygon_intervals,
    mul,
    normalize,
    point_in_polygon,
    point_segment_distance,
    segment_segment_distance,
    sub,
)
from .params import RenderParams
from .scene import RGBA, Scene, ZoneGeometry, parse_color

LAYER_NAMES = {0: "outline", 1: "shadow", 2: "smear"}
LAYER_DRAW_ORDER = (2, 1, 0)


@dataclass(frozen=True)
class Stroke:
    points: tuple[Point, ...]
    width: float
    color: RGBA
    opacity: float
    kind: str
    zone_id: str
    path_id: str | None

    def to_dict(self, index: int) -> dict[str, Any]:
        return {
            "index": index,
            "kind": self.kind,
            "points": [[value[0], value[1]] for value in self.points],
            "width": self.width,
            "color": list(self.color),
            "opacity": self.opacity,
            "zone_id": self.zone_id,
            "path_id": self.path_id,
        }

    @property
    def bbox(self) -> tuple[float, float, float, float]:
        return bbox(self.points)

    @property
    def midpoint(self) -> Point:
        first, last = self.points[0], self.points[-1]
        return ((first[0] + last[0]) * 0.5, (first[1] + last[1]) * 0.5)

    @property
    def length(self) -> float:
        return sum(distance(self.points[i - 1], self.points[i]) for i in range(1, len(self.points)))


@dataclass(frozen=True)
class BatchData:
    layer: int
    zone_id: str
    row: int
    column: int
    batch_id: str
    chunk_id: str
    strokes: tuple[Stroke, ...]
    bbox: tuple[float, float, float, float]

    @property
    def layer_name(self) -> str:
        return LAYER_NAMES[self.layer]


@dataclass(frozen=True)
class LayerData:
    layer: int
    batches: tuple[BatchData, ...]
    strokes_by_zone: dict[str, tuple[Stroke, ...]]
    excluded_by_zone: dict[str, int]
    stroke_count: int
    excluded_count: int


def _rgba(value: str, alpha: float) -> RGBA:
    red, green, blue, color_alpha = parse_color(value, "$.params", "color")
    return (red, green, blue, int(round(color_alpha * alpha)))


def _sample_polyline(
    points: tuple[Point, ...],
    lengths: tuple[float, ...],
    spacing: float,
) -> list[tuple[Point, Point, float]]:
    total = lengths[-1]
    if total <= 0:
        return []
    samples: list[tuple[Point, Point, float]] = []
    position = spacing * 0.5
    segment = 1
    while position < total:
        while segment < len(lengths) - 1 and lengths[segment] < position:
            segment += 1
        start_length = lengths[segment - 1]
        end_length = lengths[segment]
        span = max(1e-12, end_length - start_length)
        t = (position - start_length) / span
        start = points[segment - 1]
        end = points[segment]
        sample = lerp(start, end, t)
        tangent = normalize(sub(end, start))
        samples.append((sample, tangent, position))
        position += spacing
    if not samples and total > 0:
        midpoint_length = total * 0.5
        segment = 1
        while segment < len(lengths) - 1 and lengths[segment] < midpoint_length:
            segment += 1
        start_length = lengths[segment - 1]
        end_length = lengths[segment]
        span = max(1e-12, end_length - start_length)
        t = (midpoint_length - start_length) / span
        start = points[segment - 1]
        end = points[segment]
        samples.append(
            (
                lerp(start, end, t),
                normalize(sub(end, start)),
                midpoint_length,
            )
        )
    return samples


def generate_outline(
    scene: Scene,
    params: RenderParams,
    *,
    zones: set[str] | None = None,
) -> LayerData:
    selected = set(scene.draw_order) if zones is None else set(zones)
    color = _rgba(params.outline.color, params.outline.alpha)
    strokes_by_zone: dict[str, tuple[Stroke, ...]] = {}
    all_strokes: list[Stroke] = []
    for zone in scene.ordered_zones:
        if zone.id not in selected:
            continue
        path = scene.paths_by_id[zone.path_id]
        samples = _sample_polyline(path.points, path.lengths, params.outline.spacing)
        zone_strokes: list[Stroke] = []
        for sample_index, (center, tangent, _) in enumerate(samples):
            rng = DeterministicRNG(
                stable_int(params.outline.seed, "outline", zone.id, sample_index)
            )
            normal = (-tangent[1], tangent[0])
            offset = rng.uniform(-params.outline.jitter, params.outline.jitter)
            center = add(center, mul(normal, offset))
            along = rng.uniform(-params.outline.jitter, params.outline.jitter) * 0.35
            half_length = max(
                0.05,
                params.outline.length * 0.5
                + rng.uniform(-params.outline.jitter, params.outline.jitter) * 0.25,
            )
            start = add(center, mul(tangent, along - half_length))
            end = add(center, mul(tangent, along + half_length))
            width = max(
                0.1,
                params.outline.width
                + rng.uniform(-params.outline.jitter, params.outline.jitter) * 0.18,
            )
            stroke = Stroke(
                points=((qfloat(start[0]), qfloat(start[1])), (qfloat(end[0]), qfloat(end[1]))),
                width=qfloat(width),
                color=color,
                opacity=1.0,
                kind="outline",
                zone_id=zone.id,
                path_id=path.id,
            )
            zone_strokes.append(stroke)
            all_strokes.append(stroke)
        strokes_by_zone[zone.id] = tuple(zone_strokes)
    for zone in scene.ordered_zones:
        strokes_by_zone.setdefault(zone.id, ())
    batches = route_strokes(0, all_strokes, params.grid_size)
    return LayerData(
        layer=0,
        batches=batches,
        strokes_by_zone=strokes_by_zone,
        excluded_by_zone={zone.id: 0 for zone in scene.ordered_zones},
        stroke_count=len(all_strokes),
        excluded_count=0,
    )


def _outline_obstacles(
    outline_strokes: tuple[Stroke, ...],
) -> list[tuple[Point, Point, float, tuple[float, float, float, float]]]:
    obstacles = []
    for stroke in outline_strokes:
        if len(stroke.points) < 2:
            continue
        start = stroke.points[0]
        end = stroke.points[-1]
        obstacles.append((start, end, stroke.width * 0.5, bbox_expand(stroke.bbox, stroke.width)))
    return obstacles


def _intersects_outline(stroke: Stroke, obstacles: list[Any], margin: float) -> bool:
    stroke_box = bbox_expand(stroke.bbox, margin)
    half_shadow = stroke.width * 0.5
    for start, end, half_outline, outline_box in obstacles:
        if not bbox_intersects(stroke_box, outline_box):
            continue
        if len(stroke.points) == 2:
            distance_value = segment_segment_distance(
                stroke.points[0], stroke.points[1], start, end
            )
        else:
            distance_value = min(
                point_segment_distance(value, start, end) for value in stroke.points
            )
        if distance_value <= half_shadow + half_outline + margin:
            return True
    return False


def _outline_clearance(target: Point, obstacles: list[Any]) -> float:
    best = float("inf")
    for start, end, half_outline, outline_box in obstacles:
        if not (
            outline_box[0] <= target[0] <= outline_box[2]
            and outline_box[1] <= target[1] <= outline_box[3]
        ):
            continue
        best = min(best, point_segment_distance(target, start, end) - half_outline)
    return best


def _clear_hatch_endpoints(
    line_origin: Point,
    direction: Point,
    start_t: float,
    end_t: float,
    obstacles: list[Any],
    required_clearance: float,
) -> tuple[Point, Point] | None:
    midpoint = (start_t + end_t) * 0.5
    left_t = start_t
    right_t = end_t
    step = 0.5
    while left_t < midpoint and _outline_clearance(
        add(line_origin, mul(direction, left_t)), obstacles
    ) < required_clearance:
        left_t += step
    while right_t > midpoint and _outline_clearance(
        add(line_origin, mul(direction, right_t)), obstacles
    ) < required_clearance:
        right_t -= step
    if right_t - left_t <= 0.1:
        return None
    return (
        add(line_origin, mul(direction, left_t)),
        add(line_origin, mul(direction, right_t)),
    )


def generate_shadow(
    scene: Scene,
    params: RenderParams,
    outline_strokes: dict[str, tuple[Stroke, ...]] | tuple[Stroke, ...],
    *,
    zones: set[str] | None = None,
) -> LayerData:
    selected = set(scene.draw_order) if zones is None else set(zones)
    color = _rgba(params.shadow.color, params.shadow.alpha)
    if isinstance(outline_strokes, dict):
        flat_outline = tuple(
            stroke for zone_id in scene.draw_order for stroke in outline_strokes.get(zone_id, ())
        )
    else:
        flat_outline = outline_strokes
    obstacles = _outline_obstacles(flat_outline)
    angle = math.radians(params.shadow.angle_deg)
    direction = (math.cos(angle), math.sin(angle))
    normal = (-direction[1], direction[0])
    origin = (scene.width * 0.5, scene.height * 0.5)
    strokes_by_zone: dict[str, tuple[Stroke, ...]] = {}
    excluded_by_zone: dict[str, int] = {}
    all_strokes: list[Stroke] = []

    for zone in scene.ordered_zones:
        if zone.id not in selected:
            continue
        projections = [dot(sub(vertex, origin), normal) for vertex in zone.polygon]
        minimum = min(projections)
        maximum = max(projections)
        start_index = math.floor(minimum / params.shadow.spacing) - 1
        end_index = math.ceil(maximum / params.shadow.spacing) + 1
        zone_strokes: list[Stroke] = []
        excluded = 0
        for line_index in range(start_index, end_index + 1):
            rng = DeterministicRNG(
                stable_int(params.shadow.seed, "shadow", zone.id, line_index)
            )
            base_offset = line_index * params.shadow.spacing
            offset = base_offset + rng.uniform(
                -params.shadow.jitter, params.shadow.jitter
            ) * 0.55
            line_origin = add(origin, mul(normal, offset))
            intervals = line_polygon_intervals(line_origin, direction, zone.polygon)
            for interval_index, (start_t, end_t) in enumerate(intervals):
                span = end_t - start_t
                if span <= 1e-6:
                    continue
                adjusted_start = start_t + rng.uniform(0.0, params.shadow.jitter * 0.35)
                adjusted_end = end_t - rng.uniform(0.0, params.shadow.jitter * 0.35)
                cleared = _clear_hatch_endpoints(
                    line_origin,
                    direction,
                    adjusted_start,
                    adjusted_end,
                    obstacles,
                    params.shadow.width * 0.5 + params.shadow.reject_margin,
                )
                if cleared is None:
                    excluded += 1
                    continue
                start, end = cleared
                if distance(start, end) <= 0.1:
                    continue
                width = max(
                    0.1,
                    params.shadow.width
                    + rng.uniform(-params.shadow.jitter, params.shadow.jitter) * 0.12,
                )
                candidate = Stroke(
                    points=(
                        (qfloat(start[0]), qfloat(start[1])),
                        (qfloat(end[0]), qfloat(end[1])),
                    ),
                    width=qfloat(width),
                    color=color,
                    opacity=1.0,
                    kind="shadow",
                    zone_id=zone.id,
                    path_id=scene.zones_by_id[zone.id].path_id,
                )
                if _intersects_outline(
                    candidate, obstacles, params.shadow.reject_margin
                ):
                    excluded += 1
                    continue
                zone_strokes.append(candidate)
                all_strokes.append(candidate)
        strokes_by_zone[zone.id] = tuple(zone_strokes)
        excluded_by_zone[zone.id] = excluded

    for zone in scene.ordered_zones:
        strokes_by_zone.setdefault(zone.id, ())
        excluded_by_zone.setdefault(zone.id, 0)
    batches = route_strokes(1, all_strokes, params.grid_size)
    return LayerData(
        layer=1,
        batches=batches,
        strokes_by_zone=strokes_by_zone,
        excluded_by_zone=excluded_by_zone,
        stroke_count=len(all_strokes),
        excluded_count=sum(excluded_by_zone.values()),
    )


def _local_density(
    target: Point,
    shadow_strokes: tuple[Stroke, ...],
    radius: float,
) -> float:
    if radius <= 0:
        return 0.0
    total = 0.0
    for stroke in shadow_strokes:
        start, end = stroke.points[0], stroke.points[-1]
        separation = point_segment_distance(target, start, end)
        if separation >= radius:
            continue
        falloff = 1.0 - separation / radius
        total += stroke.length * falloff
    area = math.pi * radius * radius
    return max(0.0, min(1.0, total / (area * 0.42)))


def generate_smear(
    scene: Scene,
    params: RenderParams,
    shadow_strokes: dict[str, tuple[Stroke, ...]] | tuple[Stroke, ...],
    *,
    zones: set[str] | None = None,
) -> LayerData:
    selected = set(scene.draw_order) if zones is None else set(zones)
    angle = math.radians(params.smear.angle_deg)
    direction = (math.cos(angle), math.sin(angle))
    all_strokes: list[Stroke] = []
    strokes_by_zone: dict[str, tuple[Stroke, ...]] = {}
    for zone in scene.ordered_zones:
        if zone.id not in selected:
            continue
        if isinstance(shadow_strokes, dict):
            zone_shadow = shadow_strokes.get(zone.id, ())
        else:
            zone_shadow = tuple(
                stroke for stroke in shadow_strokes if stroke.zone_id == zone.id
            )
        min_x, min_y, max_x, max_y = zone.bbox
        start_x = min_x + params.smear.spacing * 0.5
        start_y = min_y + params.smear.spacing * 0.5
        rows = max(1, math.ceil((max_y - min_y) / params.smear.spacing))
        columns = max(1, math.ceil((max_x - min_x) / params.smear.spacing))
        zone_strokes: list[Stroke] = []
        for row in range(rows):
            for column in range(columns):
                rng = DeterministicRNG(
                    stable_int(params.smear.seed, "smear", zone.id, row, column)
                )
                center = (
                    start_x + column * params.smear.spacing
                    + rng.uniform(-params.smear.jitter, params.smear.jitter),
                    start_y + row * params.smear.spacing
                    + rng.uniform(-params.smear.jitter, params.smear.jitter),
                )
                if not point_in_polygon(center, zone.polygon):
                    continue
                density = _local_density(
                    center, zone_shadow, params.smear.density_radius
                )
                opacity = params.smear.opacity * (
                    1.0 - params.smear.density_weight * density
                )
                opacity = max(params.smear.min_opacity, min(1.0, opacity))
                jitter_angle = rng.uniform(-params.smear.jitter, params.smear.jitter) * 0.002
                local_direction = (
                    direction[0] * math.cos(jitter_angle) - direction[1] * math.sin(jitter_angle),
                    direction[0] * math.sin(jitter_angle) + direction[1] * math.cos(jitter_angle),
                )
                half_length = max(
                    0.1,
                    (
                        params.smear.length
                        + rng.uniform(-params.smear.jitter, params.smear.jitter) * 0.4
                    )
                    * 0.5,
                )
                start = add(center, mul(local_direction, -half_length))
                end = add(center, mul(local_direction, half_length))
                width = max(
                    0.1,
                    params.smear.width
                    + rng.uniform(-params.smear.jitter, params.smear.jitter) * 0.2,
                )
                color = (
                    zone.fill[0],
                    zone.fill[1],
                    zone.fill[2],
                    int(round(zone.fill[3] * opacity)),
                )
                stroke = Stroke(
                    points=(
                        (qfloat(start[0]), qfloat(start[1])),
                        (qfloat(end[0]), qfloat(end[1])),
                    ),
                    width=qfloat(width),
                    color=color,
                    opacity=qfloat(opacity),
                    kind="smear",
                    zone_id=zone.id,
                    path_id=zone.path_id,
                )
                zone_strokes.append(stroke)
                all_strokes.append(stroke)
        strokes_by_zone[zone.id] = tuple(zone_strokes)
    for zone in scene.ordered_zones:
        strokes_by_zone.setdefault(zone.id, ())
    batches = route_strokes(2, all_strokes, params.grid_size)
    return LayerData(
        layer=2,
        batches=batches,
        strokes_by_zone=strokes_by_zone,
        excluded_by_zone={zone.id: 0 for zone in scene.ordered_zones},
        stroke_count=len(all_strokes),
        excluded_count=0,
    )


def route_strokes(
    layer: int,
    strokes: list[Stroke],
    grid_size: float,
) -> tuple[BatchData, ...]:
    grouped: dict[tuple[int, int, str], list[Stroke]] = {}
    for stroke in strokes:
        midpoint = stroke.midpoint
        column = math.floor(midpoint[0] / grid_size)
        row = math.floor(midpoint[1] / grid_size)
        grouped.setdefault((row, column, stroke.zone_id), []).append(stroke)
    result: list[BatchData] = []
    layer_name = LAYER_NAMES[layer]
    for (row, column, zone_id), batch_strokes in sorted(
        grouped.items(), key=lambda item: (item[0][2], item[0][0], item[0][1])
    ):
        chunk_id = f"r{row}c{column}"
        batch_id = f"{layer_name}:{zone_id}:{chunk_id}"
        boxes = [stroke.bbox for stroke in batch_strokes]
        batch_box = (
            min(item[0] for item in boxes),
            min(item[1] for item in boxes),
            max(item[2] for item in boxes),
            max(item[3] for item in boxes),
        )
        result.append(
            BatchData(
                layer=layer,
                zone_id=zone_id,
                row=row,
                column=column,
                batch_id=batch_id,
                chunk_id=chunk_id,
                strokes=tuple(batch_strokes),
                bbox=batch_box,
            )
        )
    return tuple(result)


def generator_digest(
    batches: tuple[BatchData, ...],
    grid_size: float,
) -> str:
    from .canonical import sha256_json

    payload = []
    for batch in sorted(batches, key=lambda item: (item.layer, item.batch_id)):
        payload.append(
            {
                "layer": batch.layer,
                "batch": batch.batch_id,
                "chunk": batch.chunk_id,
                "strokes": [
                    stroke.to_dict(index) for index, stroke in enumerate(batch.strokes)
                ],
            }
        )
    return sha256_json({"grid_size": grid_size, "batches": payload})


__all__ = [
    "BatchData",
    "LAYER_DRAW_ORDER",
    "LAYER_NAMES",
    "LayerData",
    "Stroke",
    "generate_outline",
    "generate_shadow",
    "generate_smear",
    "generator_digest",
    "route_strokes",
]
