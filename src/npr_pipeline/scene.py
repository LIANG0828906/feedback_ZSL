from __future__ import annotations

import json
import math
from dataclasses import dataclass
from pathlib import Path
from typing import Any

from .canonical import canonical_json_bytes, qfloat, sha256_json
from .errors import E_SCENE_INVALID, PipelineError, fail
from .geometry import (
    Point,
    bbox,
    cumulative_lengths,
    distance,
    polygons_overlap,
    quadratic_point,
    signed_area,
)

CURVE_SUBDIVISIONS = 32
CLOSE_TOLERANCE = 1e-7
RGBA = tuple[int, int, int, int]


@dataclass(frozen=True)
class PathSegment:
    kind: str
    to: Point
    control: Point | None = None

    def to_dict(self) -> dict[str, Any]:
        result: dict[str, Any] = {"type": self.kind, "to": list(self.to)}
        if self.control is not None:
            result["control"] = list(self.control)
        return result


@dataclass(frozen=True)
class PathGeometry:
    id: str
    start: Point
    segments: tuple[PathSegment, ...]
    closed: bool
    points: tuple[Point, ...]
    lengths: tuple[float, ...]
    bbox: tuple[float, float, float, float]
    area: float

    @property
    def length(self) -> float:
        return self.lengths[-1] if self.lengths else 0.0

    def to_geometry_dict(self) -> dict[str, Any]:
        return {
            "id": self.id,
            "closed": self.closed,
            "points": [[value[0], value[1]] for value in self.points],
            "length": self.length,
            "bbox": list(self.bbox),
            "area": self.area,
        }


@dataclass(frozen=True)
class ZoneGeometry:
    id: str
    label: str
    fill: RGBA
    path_id: str
    draw_order: int
    polygon: tuple[Point, ...]
    bbox: tuple[float, float, float, float]

    def to_geometry_dict(self) -> dict[str, Any]:
        return {
            "id": self.id,
            "label": self.label,
            "fill": list(self.fill),
            "path_id": self.path_id,
            "draw_order": self.draw_order,
            "polygon": [[value[0], value[1]] for value in self.polygon],
            "bbox": list(self.bbox),
        }


@dataclass(frozen=True)
class Scene:
    schema: str
    width: float
    height: float
    paths: tuple[PathGeometry, ...]
    zones: tuple[ZoneGeometry, ...]
    draw_order: tuple[str, ...]
    geometry_hash: str

    @property
    def scene_id(self) -> str:
        return self.geometry_hash

    @property
    def paths_by_id(self) -> dict[str, PathGeometry]:
        return {path.id: path for path in self.paths}

    @property
    def zones_by_id(self) -> dict[str, ZoneGeometry]:
        return {zone.id: zone for zone in self.zones}

    @property
    def ordered_zones(self) -> tuple[ZoneGeometry, ...]:
        by_id = self.zones_by_id
        return tuple(by_id[zone_id] for zone_id in self.draw_order)

    def geometry_document(self) -> dict[str, Any]:
        body = {
            "schema": "npr-geometry-v1",
            "source_schema": self.schema,
            "canvas": {"width": self.width, "height": self.height},
            "draw_order": list(self.draw_order),
            "zone_index": {zone.id: zone.draw_order for zone in self.zones},
            "paths": [path.to_geometry_dict() for path in self.paths],
            "zones": [zone.to_geometry_dict() for zone in self.zones],
        }
        body["geometry_hash"] = self.geometry_hash
        return body

    def zone_factor(self, zone_id: str) -> str:
        zone = self.zones_by_id[zone_id]
        path = self.paths_by_id[zone.path_id]
        return sha256_json(
            {
                "zone": zone.to_geometry_dict(),
                "path": path.to_geometry_dict(),
            }
        )


def _require(mapping: dict[str, Any], key: str, location: str) -> Any:
    if key not in mapping:
        fail(E_SCENE_INVALID, location, "missing_field", field=key)
    return mapping[key]


def _as_number(value: Any, location: str, field_name: str) -> float:
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        fail(E_SCENE_INVALID, location, "coordinate_not_finite", field=field_name)
    number = float(value)
    if not math.isfinite(number):
        fail(E_SCENE_INVALID, location, "coordinate_not_finite", field=field_name)
    return qfloat(number)


def _as_point(value: Any, location: str, field_name: str) -> Point:
    if not isinstance(value, (list, tuple)) or len(value) != 2:
        fail(E_SCENE_INVALID, location, "invalid_point", field=field_name)
    return (
        _as_number(value[0], location, f"{field_name}[0]"),
        _as_number(value[1], location, f"{field_name}[1]"),
    )


def parse_color(value: Any, location: str, field_name: str = "fill") -> RGBA:
    if not isinstance(value, str):
        fail(E_SCENE_INVALID, location, "invalid_color", field=field_name)
    text = value.strip()
    if not text.startswith("#"):
        fail(E_SCENE_INVALID, location, "invalid_color", field=field_name)
    digits = text[1:]
    try:
        if len(digits) == 3:
            return (
                int(digits[0] * 2, 16),
                int(digits[1] * 2, 16),
                int(digits[2] * 2, 16),
                255,
            )
        if len(digits) == 6:
            return (
                int(digits[0:2], 16),
                int(digits[2:4], 16),
                int(digits[4:6], 16),
                255,
            )
        if len(digits) == 8:
            return (
                int(digits[0:2], 16),
                int(digits[2:4], 16),
                int(digits[4:6], 16),
                int(digits[6:8], 16),
            )
    except ValueError:
        pass
    fail(E_SCENE_INVALID, location, "invalid_color", field=field_name)


def _flatten_path(
    path_id: str,
    start: Point,
    segments: tuple[PathSegment, ...],
) -> tuple[tuple[Point, ...], tuple[float, ...]]:
    points: list[Point] = [start]
    current = start
    for segment in segments:
        if segment.kind == "line":
            points.append(segment.to)
        elif segment.kind == "quadratic":
            assert segment.control is not None
            for step in range(1, CURVE_SUBDIVISIONS + 1):
                t = step / CURVE_SUBDIVISIONS
                value = quadratic_point(current, segment.control, segment.to, t)
                points.append((qfloat(value[0]), qfloat(value[1])))
        else:
            fail(E_SCENE_INVALID, f"path:{path_id}", "unsupported_segment", kind=segment.kind)
        current = segment.to
    clean: list[Point] = [points[0]]
    for value in points[1:]:
        if distance(value, clean[-1]) > 1e-10:
            clean.append(value)
    if distance(clean[-1], clean[0]) <= 1e-10:
        clean[-1] = clean[0]
    if len(clean) < 3:
        fail(E_SCENE_INVALID, f"path:{path_id}", "degenerate_path")
    lengths = tuple(cumulative_lengths(clean))
    return tuple(clean), lengths


def _parse_path(raw: Any, index: int) -> PathGeometry:
    if not isinstance(raw, dict):
        fail(E_SCENE_INVALID, f"paths[{index}]", "invalid_path")
    path_id = _require(raw, "id", f"paths[{index}].id")
    if not isinstance(path_id, str) or not path_id:
        fail(E_SCENE_INVALID, f"paths[{index}].id", "invalid_path_id")
    location = f"path:{path_id}"
    start = _as_point(_require(raw, "start", location), location, "start")
    closed = _require(raw, "closed", location)
    if closed is not True:
        fail(E_SCENE_INVALID, location, "path_not_closed")
    raw_segments = _require(raw, "segments", location)
    if not isinstance(raw_segments, list) or not raw_segments:
        fail(E_SCENE_INVALID, location, "missing_segments")
    segments: list[PathSegment] = []
    for segment_index, raw_segment in enumerate(raw_segments):
        segment_location = f"{location}.segments[{segment_index}]"
        if not isinstance(raw_segment, dict):
            fail(E_SCENE_INVALID, segment_location, "invalid_segment")
        kind = _require(raw_segment, "type", segment_location)
        if kind in ("line", "L"):
            kind = "line"
            control = None
        elif kind in ("quadratic", "quad", "Q"):
            kind = "quadratic"
            control = _as_point(
                _require(raw_segment, "control", segment_location),
                segment_location,
                "control",
            )
        else:
            fail(E_SCENE_INVALID, location, "unsupported_segment", kind=str(kind))
        endpoint = _as_point(
            _require(raw_segment, "to", segment_location),
            segment_location,
            "to",
        )
        segments.append(PathSegment(kind=kind, to=endpoint, control=control))
    if distance(segments[-1].to, start) > CLOSE_TOLERANCE:
        fail(E_SCENE_INVALID, location, "path_not_closed")
    points, lengths = _flatten_path(path_id, start, tuple(segments))
    area = signed_area(points)
    if abs(area) <= 1e-7:
        fail(E_SCENE_INVALID, location, "degenerate_path")
    return PathGeometry(
        id=path_id,
        start=start,
        segments=tuple(segments),
        closed=True,
        points=points,
        lengths=lengths,
        bbox=bbox(points),
        area=area,
    )


def _parse_zone(raw: Any, index: int, draw_order: dict[str, int]) -> ZoneGeometry:
    if not isinstance(raw, dict):
        fail(E_SCENE_INVALID, f"zones[{index}]", "invalid_zone")
    zone_id = _require(raw, "id", f"zones[{index}].id")
    if not isinstance(zone_id, str) or not zone_id:
        fail(E_SCENE_INVALID, f"zones[{index}].id", "invalid_zone_id")
    location = f"zone:{zone_id}"
    label = _require(raw, "label", location)
    if not isinstance(label, str) or not label:
        fail(E_SCENE_INVALID, location, "missing_label")
    fill = parse_color(_require(raw, "fill", location), location)
    path_id = _require(raw, "path", location)
    if not isinstance(path_id, str) or not path_id:
        fail(E_SCENE_INVALID, location, "invalid_path_reference")
    if zone_id not in draw_order:
        fail(E_SCENE_INVALID, location, "zone_missing_from_draw_order")
    return ZoneGeometry(
        id=zone_id,
        label=label,
        fill=fill,
        path_id=path_id,
        draw_order=draw_order[zone_id],
        polygon=(),
        bbox=(0.0, 0.0, 0.0, 0.0),
    )


def _attach_zone_geometry(zone: ZoneGeometry, path: PathGeometry) -> ZoneGeometry:
    return ZoneGeometry(
        id=zone.id,
        label=zone.label,
        fill=zone.fill,
        path_id=zone.path_id,
        draw_order=zone.draw_order,
        polygon=path.points,
        bbox=path.bbox,
    )


def parse_scene(raw: Any) -> Scene:
    if not isinstance(raw, dict):
        fail(E_SCENE_INVALID, "$", "scene_not_object")
    schema = _require(raw, "schema", "$")
    if schema != "npr-scene-v1":
        fail(E_SCENE_INVALID, "$.schema", "unsupported_schema", value=str(schema))
    canvas = _require(raw, "canvas", "$")
    if not isinstance(canvas, dict):
        fail(E_SCENE_INVALID, "$.canvas", "invalid_canvas")
    width = _as_number(_require(canvas, "width", "$.canvas"), "$.canvas", "width")
    height = _as_number(_require(canvas, "height", "$.canvas"), "$.canvas", "height")
    if width <= 0 or height <= 0:
        fail(E_SCENE_INVALID, "$.canvas", "invalid_canvas_size")

    raw_paths = _require(raw, "paths", "$")
    if not isinstance(raw_paths, list) or not raw_paths:
        fail(E_SCENE_INVALID, "$.paths", "missing_paths")
    paths = tuple(_parse_path(item, index) for index, item in enumerate(raw_paths))
    path_ids = [item.id for item in paths]
    if len(set(path_ids)) != len(path_ids):
        seen: set[str] = set()
        for value in path_ids:
            if value in seen:
                fail(E_SCENE_INVALID, f"path:{value}", "duplicate_path_id")
            seen.add(value)

    raw_order = _require(raw, "draw_order", "$")
    if not isinstance(raw_order, list) or not raw_order:
        fail(E_SCENE_INVALID, "$.draw_order", "missing_draw_order")
    if any(not isinstance(item, str) for item in raw_order):
        fail(E_SCENE_INVALID, "$.draw_order", "invalid_draw_order")
    if len(set(raw_order)) != len(raw_order):
        fail(E_SCENE_INVALID, "$.draw_order", "duplicate_draw_order")
    order_map = {zone_id: index for index, zone_id in enumerate(raw_order)}

    raw_zones = _require(raw, "zones", "$")
    if not isinstance(raw_zones, list) or not raw_zones:
        fail(E_SCENE_INVALID, "$.zones", "missing_zones")
    zones = tuple(
        _parse_zone(item, index, order_map) for index, item in enumerate(raw_zones)
    )
    zone_ids = [item.id for item in zones]
    if len(set(zone_ids)) != len(zone_ids):
        seen: set[str] = set()
        for value in zone_ids:
            if value in seen:
                fail(E_SCENE_INVALID, f"zone:{value}", "duplicate_zone_id")
            seen.add(value)
    if set(zone_ids) != set(raw_order):
        missing = sorted(set(zone_ids) - set(raw_order))
        extra = sorted(set(raw_order) - set(zone_ids))
        location = f"zone:{missing[0]}" if missing else f"zone:{extra[0]}"
        fail(
            E_SCENE_INVALID,
            location,
            "draw_order_zone_mismatch",
            missing=missing,
            extra=extra,
        )

    paths_by_id = {path.id: path for path in paths}
    referenced: list[str] = []
    attached: list[ZoneGeometry] = []
    for zone in zones:
        path = paths_by_id.get(zone.path_id)
        if path is None:
            fail(E_SCENE_INVALID, f"zone:{zone.id}", "missing_path_reference", path=zone.path_id)
        referenced.append(zone.path_id)
        attached.append(_attach_zone_geometry(zone, path))
    if len(set(referenced)) != len(referenced):
        seen: set[str] = set()
        for path_id in referenced:
            if path_id in seen:
                fail(E_SCENE_INVALID, f"path:{path_id}", "path_reused_by_multiple_zones")
            seen.add(path_id)
    unreferenced = sorted(set(path_ids) - set(referenced))
    if unreferenced:
        fail(E_SCENE_INVALID, f"path:{unreferenced[0]}", "path_without_zone")

    for left_index, left in enumerate(attached):
        for right in attached[left_index + 1 :]:
            if polygons_overlap(left.polygon, right.polygon):
                fail(
                    E_SCENE_INVALID,
                    f"zone:{left.id}/{right.id}",
                    "zone_overlap",
                )

    ordered = sorted(attached, key=lambda item: item.draw_order)
    body = {
        "schema": "npr-geometry-v1",
        "source_schema": schema,
        "canvas": {"width": width, "height": height},
        "draw_order": list(raw_order),
        "zone_index": {zone.id: zone.draw_order for zone in ordered},
        "paths": [path.to_geometry_dict() for path in paths],
        "zones": [zone.to_geometry_dict() for zone in ordered],
    }
    geometry_hash = sha256_json(body)
    return Scene(
        schema=schema,
        width=width,
        height=height,
        paths=paths,
        zones=tuple(ordered),
        draw_order=tuple(raw_order),
        geometry_hash=geometry_hash,
    )


def load_scene(path: str | Path) -> Scene:
    source = Path(path)
    try:
        raw = json.loads(source.read_text(encoding="utf-8"))
    except OSError as exc:
        fail(E_SCENE_INVALID, "scene_file", "read_failed", reason=str(exc))
    except json.JSONDecodeError as exc:
        fail(E_SCENE_INVALID, "scene_file", "invalid_json", line=exc.lineno, column=exc.colno)
    return parse_scene(raw)


def scene_bytes(scene: Scene) -> bytes:
    return canonical_json_bytes(scene.geometry_document())


__all__ = [
    "PathGeometry",
    "PathSegment",
    "PipelineError",
    "Scene",
    "ZoneGeometry",
    "load_scene",
    "parse_scene",
    "parse_color",
    "scene_bytes",
]
