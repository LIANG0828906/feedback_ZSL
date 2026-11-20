"""Local glyph-description parser and contour normalizer."""

from __future__ import annotations

import hashlib
import json
import math
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Iterable, Sequence

from .errors import E_GLYPH_INVALID, fail
from .geometry import (
    LineContour,
    Point,
    flatten_cubic,
    flatten_quadratic,
    signed_area,
    transform_contour,
)

FILL_RULES = {"nonzero", "evenodd"}
DIRECTION_ALIASES = {
    "ccw": "counterclockwise",
    "counterclockwise": "counterclockwise",
    "counter-clockwise": "counterclockwise",
    "cw": "clockwise",
    "clockwise": "clockwise",
}


@dataclass(frozen=True)
class SegmentSpec:
    kind: str
    control1: Point | None
    control2: Point | None
    to_index: int


@dataclass(frozen=True)
class ContourSpec:
    points: tuple[Point, ...]
    segments: tuple[SegmentSpec, ...]
    start_index: int
    declared_direction: str | None
    index: int


@dataclass(frozen=True)
class Component:
    glyph_name: str
    matrix: tuple[float, float, float, float, float, float]
    index: int


@dataclass(frozen=True)
class GlyphSpec:
    name: str
    unicode: tuple[int, ...]
    advance_width: float
    contours: tuple[ContourSpec, ...]
    components: tuple[Component, ...]
    fill_rule: str
    blank: bool
    index: int


@dataclass(frozen=True)
class ResolvedGlyph:
    name: str
    unicode: tuple[int, ...]
    advance_width: float
    contours: tuple[LineContour, ...]
    fill_rule: str
    blank: bool
    index: int


@dataclass(frozen=True)
class FontSpec:
    name: str
    units_per_em: float
    default_fill_rule: str
    glyphs: tuple[GlyphSpec, ...]
    curve_tolerance: float
    source_sha256: str

    def glyph(self, name_or_unicode: str | int) -> GlyphSpec:
        if isinstance(name_or_unicode, int):
            for glyph in self.glyphs:
                if name_or_unicode in glyph.unicode:
                    return glyph
            fail(E_GLYPH_INVALID, "Unicode mapping does not exist", unicode=name_or_unicode)
        for glyph in self.glyphs:
            if glyph.name == name_or_unicode:
                return glyph
        fail(E_GLYPH_INVALID, "Glyph reference does not exist", glyph=name_or_unicode, contour=-1)

    def resolve(self, name_or_unicode: str | int, fill_rule: str | None = None) -> ResolvedGlyph:
        return _GlyphResolver(self).resolve(name_or_unicode, fill_rule)

    def canonical_hash(self) -> str:
        return self.source_sha256


class _GlyphResolver:
    def __init__(self, font: FontSpec) -> None:
        self.font = font
        self.by_name = {glyph.name: glyph for glyph in font.glyphs}
        self.cache: dict[str, ResolvedGlyph] = {}
        self.stack: list[str] = []

    def resolve(self, name_or_unicode: str | int, fill_rule: str | None = None) -> ResolvedGlyph:
        glyph = self.font.glyph(name_or_unicode)
        resolved = self._resolve_spec(glyph)
        rule = fill_rule or resolved.fill_rule
        _validate_fill_rule(rule, glyph.name)
        if rule != resolved.fill_rule:
            return ResolvedGlyph(
                name=resolved.name,
                unicode=resolved.unicode,
                advance_width=resolved.advance_width,
                contours=resolved.contours,
                fill_rule=rule,
                blank=resolved.blank,
                index=resolved.index,
            )
        return resolved

    def _resolve_spec(self, glyph: GlyphSpec) -> ResolvedGlyph:
        cached = self.cache.get(glyph.name)
        if cached is not None:
            return cached
        if glyph.name in self.stack:
            fail(
                E_GLYPH_INVALID,
                "Composite glyph cycle or self reference",
                glyph=glyph.name,
                contour=-1,
                reference_chain=list(self.stack) + [glyph.name],
            )
        self.stack.append(glyph.name)
        try:
            contours: list[LineContour] = []
            for contour in glyph.contours:
                contours.append(_flatten_contour(glyph, contour, self.font.curve_tolerance))
            for component in glyph.components:
                if component.glyph_name == glyph.name:
                    fail(
                        E_GLYPH_INVALID,
                        "Self-referencing composite glyph",
                        glyph=glyph.name,
                        contour=component.index,
                        component=component.glyph_name,
                    )
                referenced = self.by_name.get(component.glyph_name)
                if referenced is None:
                    fail(
                        E_GLYPH_INVALID,
                        "Composite glyph reference does not exist",
                        glyph=glyph.name,
                        contour=component.index,
                        component=component.glyph_name,
                    )
                referenced_resolved = self._resolve_spec(referenced)
                for contour in referenced_resolved.contours:
                    contours.append(transform_contour(contour, component.matrix))
            if not contours and not glyph.blank:
                fail(
                    E_GLYPH_INVALID,
                    "Glyph has neither contours nor components",
                    glyph=glyph.name,
                    contour=-1,
                )
            resolved = ResolvedGlyph(
                name=glyph.name,
                unicode=glyph.unicode,
                advance_width=glyph.advance_width,
                contours=tuple(contours),
                fill_rule=glyph.fill_rule,
                blank=glyph.blank,
                index=glyph.index,
            )
            self.cache[glyph.name] = resolved
            return resolved
        finally:
            self.stack.pop()


def load_font(path: str | Path) -> FontSpec:
    source_path = Path(path)
    raw_bytes = source_path.read_bytes()
    try:
        document = json.loads(raw_bytes.decode("utf-8"))
    except (UnicodeDecodeError, json.JSONDecodeError) as exc:
        fail(E_GLYPH_INVALID, "Font description is not valid UTF-8 JSON", path=str(source_path), error=str(exc))
    return parse_font(document, canonical_bytes=raw_bytes)


def parse_font(document: dict[str, Any], canonical_bytes: bytes | None = None) -> FontSpec:
    if not isinstance(document, dict):
        fail(E_GLYPH_INVALID, "Font description root must be an object", contour=-1)
    name = str(document.get("name", "local-font"))
    units = _finite_number(document.get("units_per_em", 1000), "units_per_em", -1, -1)
    if units <= 0:
        fail(E_GLYPH_INVALID, "units_per_em must be positive", glyph=name, contour=-1)
    default_rule = str(document.get("default_fill_rule", "nonzero"))
    if default_rule not in FILL_RULES:
        fail(E_GLYPH_INVALID, "Unknown default fill rule", glyph=name, contour=-1, fill_rule=default_rule)
    curve_tolerance = _finite_number(
        document.get("curve_tolerance", max(0.05, units / 2000.0)),
        "curve_tolerance",
        -1,
        -1,
    )
    if curve_tolerance <= 0:
        fail(E_GLYPH_INVALID, "curve_tolerance must be positive", glyph=name, contour=-1)

    raw_glyphs = document.get("glyphs")
    if not isinstance(raw_glyphs, list):
        fail(E_GLYPH_INVALID, "Font description must contain a glyphs array", contour=-1)
    raw_composites = document.get("composites")
    if raw_composites is not None:
        if not isinstance(raw_composites, list):
            fail(E_GLYPH_INVALID, "Font composites must be an array", contour=-1)
        raw_glyphs = list(raw_glyphs) + list(raw_composites)

    glyphs: list[GlyphSpec] = []
    names: set[str] = set()
    unicode_owner: dict[int, str] = {}
    for glyph_index, raw_glyph in enumerate(raw_glyphs):
        glyph = _parse_glyph(raw_glyph, glyph_index, default_rule)
        if glyph.name in names:
            fail(E_GLYPH_INVALID, "Duplicate glyph name", glyph=glyph.name, contour=-1)
        names.add(glyph.name)
        for codepoint in glyph.unicode:
            previous = unicode_owner.get(codepoint)
            if previous is not None:
                fail(
                    E_GLYPH_INVALID,
                    "Unicode mapping is assigned to multiple glyphs",
                    glyph=glyph.name,
                    contour=-1,
                    unicode=codepoint,
                    other_glyph=previous,
                )
            unicode_owner[codepoint] = glyph.name
        glyphs.append(glyph)

    canonical = canonical_bytes
    if canonical is None:
        canonical = json.dumps(document, sort_keys=True, separators=(",", ":"), ensure_ascii=False).encode("utf-8")
    font_spec = FontSpec(
        name=name,
        units_per_em=units,
        default_fill_rule=default_rule,
        glyphs=tuple(glyphs),
        curve_tolerance=curve_tolerance,
        source_sha256=hashlib.sha256(canonical).hexdigest(),
    )
    # Validate references and normalize every contour during loading rather
    # than discovering malformed composites later during rasterization.
    resolver = _GlyphResolver(font_spec)
    for glyph in font_spec.glyphs:
        resolver.resolve(glyph.name)
    return font_spec


def _parse_glyph(raw_glyph: Any, glyph_index: int, default_rule: str) -> GlyphSpec:
    if not isinstance(raw_glyph, dict):
        fail(E_GLYPH_INVALID, "Glyph entry must be an object", contour=-1, glyph_index=glyph_index)
    name = raw_glyph.get("name")
    if not isinstance(name, str) or not name:
        fail(E_GLYPH_INVALID, "Glyph name is missing", glyph_index=glyph_index, contour=-1)
    raw_unicode = raw_glyph.get("unicode", raw_glyph.get("codepoints", ()))
    codepoints = _parse_unicode(raw_unicode, name)
    rule = str(raw_glyph.get("fill_rule", default_rule))
    _validate_fill_rule(rule, name)
    advance = _finite_number(raw_glyph.get("advance_width", 0), "advance_width", name, -1)
    if advance < 0:
        fail(E_GLYPH_INVALID, "advance_width cannot be negative", glyph=name, contour=-1)
    blank = bool(raw_glyph.get("blank", False))

    raw_contours = raw_glyph.get("contours")
    if raw_contours is None:
        raw_contours = []
    if not isinstance(raw_contours, list):
        fail(E_GLYPH_INVALID, "Glyph contours must be an array", glyph=name, contour=-1)
    contours = tuple(_parse_contour(raw_contour, name, contour_index) for contour_index, raw_contour in enumerate(raw_contours))

    raw_components = raw_glyph.get("components")
    if raw_components is None:
        raw_components = []
    if not isinstance(raw_components, list):
        fail(E_GLYPH_INVALID, "Glyph components must be an array", glyph=name, contour=-1)
    components = tuple(_parse_component(raw_component, name, index) for index, raw_component in enumerate(raw_components))

    if not contours and not components and not blank:
        fail(
            E_GLYPH_INVALID,
            "Glyph has neither contours nor components and is not explicitly blank",
            glyph=name,
            contour=-1,
        )
    return GlyphSpec(
        name=name,
        unicode=codepoints,
        advance_width=advance,
        contours=contours,
        components=components,
        fill_rule=rule,
        blank=blank,
        index=glyph_index,
    )


def _parse_contour(raw_contour: Any, glyph_name: str, contour_index: int) -> ContourSpec:
    if not isinstance(raw_contour, dict):
        fail(
            E_GLYPH_INVALID,
            "Contour entry must be an object",
            glyph=glyph_name,
            contour=contour_index,
        )
    raw_points = raw_contour.get("points")
    if not isinstance(raw_points, list) or not raw_points:
        fail(
            E_GLYPH_INVALID,
            "Contour points are missing or empty",
            glyph=glyph_name,
            contour=contour_index,
        )
    points = tuple(_parse_point(raw_point, glyph_name, contour_index, index) for index, raw_point in enumerate(raw_points))

    raw_segments = raw_contour.get("segments")
    if not isinstance(raw_segments, list) or not raw_segments:
        fail(
            E_GLYPH_INVALID,
            "Contour segments are missing or empty",
            glyph=glyph_name,
            contour=contour_index,
        )

    start_index = raw_contour.get("start", raw_contour.get("start_index", 0))
    if isinstance(start_index, bool) or not isinstance(start_index, int):
        fail(E_GLYPH_INVALID, "Contour start index must be an integer", glyph=glyph_name, contour=contour_index)
    if start_index < 0 or start_index >= len(points):
        fail(
            E_GLYPH_INVALID,
            "Contour start index is out of range",
            glyph=glyph_name,
            contour=contour_index,
            start=start_index,
        )

    declared = raw_contour.get("direction", raw_contour.get("orientation"))
    if declared is not None:
        declared_key = str(declared).strip().lower()
        if declared_key not in DIRECTION_ALIASES:
            fail(
                E_GLYPH_INVALID,
                "Unknown contour direction",
                glyph=glyph_name,
                contour=contour_index,
                direction=declared,
            )
        declared = DIRECTION_ALIASES[declared_key]

    segments = tuple(
        _parse_segment(raw_segment, glyph_name, contour_index, index, len(points))
        for index, raw_segment in enumerate(raw_segments)
    )
    used = {start_index}
    for segment in segments:
        used.add(segment.to_index)
        if segment.control1 is not None:
            used.add(segment.control1)
        if segment.control2 is not None:
            used.add(segment.control2)
    if len(used) != len(points):
        missing = sorted(set(range(len(points))) - used)
        fail(
            E_GLYPH_INVALID,
            "Contour contains points not referenced by any segment",
            glyph=glyph_name,
            contour=contour_index,
            unreachable_points=missing,
        )
    return ContourSpec(
        points=points,
        segments=segments,
        start_index=start_index,
        declared_direction=declared,
        index=contour_index,
    )


def _parse_segment(
    raw_segment: Any,
    glyph_name: str,
    contour_index: int,
    segment_index: int,
    point_count: int,
) -> SegmentSpec:
    if not isinstance(raw_segment, dict):
        fail(
            E_GLYPH_INVALID,
            "Segment entry must be an object",
            glyph=glyph_name,
            contour=contour_index,
            segment=segment_index,
        )
    raw_kind = str(raw_segment.get("type", "")).strip().lower()
    aliases = {
        "line": "line",
        "l": "line",
        "quadratic": "quadratic",
        "quad": "quadratic",
        "q": "quadratic",
        "quadratic_bezier": "quadratic",
        "cubic": "cubic",
        "c": "cubic",
        "cubic_bezier": "cubic",
    }
    kind = aliases.get(raw_kind)
    if kind is None:
        fail(
            E_GLYPH_INVALID,
            "Segment type is missing or unsupported",
            glyph=glyph_name,
            contour=contour_index,
            segment=segment_index,
            type=raw_kind,
        )
    if "to" not in raw_segment:
        fail(
            E_GLYPH_INVALID,
            "Segment end point reference is missing",
            glyph=glyph_name,
            contour=contour_index,
            segment=segment_index,
        )
    to_index = _index(raw_segment["to"], glyph_name, contour_index, segment_index, point_count, "to")
    control1 = None
    control2 = None
    if kind in {"quadratic", "cubic"}:
        key1 = "control" if kind == "quadratic" else "control1"
        if key1 not in raw_segment:
            fail(
                E_GLYPH_INVALID,
                "Bezier control point reference is missing",
                glyph=glyph_name,
                contour=contour_index,
                segment=segment_index,
                field=key1,
            )
        control1 = _index(raw_segment[key1], glyph_name, contour_index, segment_index, point_count, key1)
    if kind == "cubic":
        if "control2" not in raw_segment:
            fail(
                E_GLYPH_INVALID,
                "Cubic Bezier second control point reference is missing",
                glyph=glyph_name,
                contour=contour_index,
                segment=segment_index,
                field="control2",
            )
        control2 = _index(raw_segment["control2"], glyph_name, contour_index, segment_index, point_count, "control2")
    return SegmentSpec(kind=kind, control1=control1, control2=control2, to_index=to_index)


def _parse_component(raw_component: Any, glyph_name: str, component_index: int) -> Component:
    if not isinstance(raw_component, dict):
        fail(
            E_GLYPH_INVALID,
            "Composite component must be an object",
            glyph=glyph_name,
            contour=component_index,
        )
    referenced = raw_component.get("glyph", raw_component.get("name"))
    if not isinstance(referenced, str) or not referenced:
        fail(
            E_GLYPH_INVALID,
            "Composite glyph reference is missing",
            glyph=glyph_name,
            contour=component_index,
        )
    if "transform" in raw_component:
        raw_matrix = raw_component["transform"]
        if isinstance(raw_matrix, dict):
            matrix = tuple(
                _finite_number(raw_matrix.get(key, default), f"transform.{key}", glyph_name, component_index)
                for key, default in (("a", 1), ("b", 0), ("c", 0), ("d", 1), ("e", 0), ("f", 0))
            )
        elif isinstance(raw_matrix, list) and len(raw_matrix) == 6:
            matrix = tuple(
                _finite_number(value, f"transform[{index}]", glyph_name, component_index)
                for index, value in enumerate(raw_matrix)
            )
        else:
            fail(
                E_GLYPH_INVALID,
                "Composite transform must be a six-value array or object",
                glyph=glyph_name,
                contour=component_index,
            )
    else:
        scale = _finite_number(raw_component.get("scale", 1), "scale", glyph_name, component_index)
        dx = _finite_number(raw_component.get("dx", 0), "dx", glyph_name, component_index)
        dy = _finite_number(raw_component.get("dy", 0), "dy", glyph_name, component_index)
        matrix = (scale, 0.0, 0.0, scale, dx, dy)
    return Component(glyph_name=referenced, matrix=matrix, index=component_index)


def _flatten_contour(glyph: GlyphSpec, contour: ContourSpec, tolerance: float) -> LineContour:
    cursor = contour.points[contour.start_index]
    vertices = [cursor]
    for segment_index, segment in enumerate(contour.segments):
        end = contour.points[segment.to_index]
        if segment.kind == "line":
            vertices.append(end)
        elif segment.kind == "quadratic":
            assert segment.control1 is not None
            control = contour.points[segment.control1]
            vertices.extend(flatten_quadratic(cursor, control, end, tolerance))
        else:
            assert segment.control1 is not None and segment.control2 is not None
            control1 = contour.points[segment.control1]
            control2 = contour.points[segment.control2]
            vertices.extend(flatten_cubic(cursor, control1, control2, end, tolerance))
        cursor = end
    if len(vertices) > 1 and vertices[-1].almost_equals(vertices[0]):
        vertices.pop()
    if len(vertices) < 2:
        fail(
            E_GLYPH_INVALID,
            "Contour has fewer than two distinct vertices after normalization",
            glyph=glyph.name,
            contour=contour.index,
            segment=len(contour.segments) - 1,
        )
    area = signed_area(vertices)
    return LineContour(
        points=tuple(vertices),
        origin_glyph=glyph.name,
        origin_contour=contour.index,
        declared_direction=contour.declared_direction,
        signed_area=area,
    )


def _parse_point(raw_point: Any, glyph_name: str, contour_index: int, point_index: int) -> Point:
    if isinstance(raw_point, dict):
        if "x" not in raw_point or "y" not in raw_point:
            fail(
                E_GLYPH_INVALID,
                "Point is missing x or y",
                glyph=glyph_name,
                contour=contour_index,
                point=point_index,
            )
        x = _finite_number(raw_point["x"], "point.x", glyph_name, contour_index, point_index)
        y = _finite_number(raw_point["y"], "point.y", glyph_name, contour_index, point_index)
        return Point(x, y)
    if isinstance(raw_point, (list, tuple)) and len(raw_point) == 2:
        x = _finite_number(raw_point[0], "point.x", glyph_name, contour_index, point_index)
        y = _finite_number(raw_point[1], "point.y", glyph_name, contour_index, point_index)
        return Point(x, y)
    fail(
        E_GLYPH_INVALID,
        "Point must be [x, y] or an object with x and y",
        glyph=glyph_name,
        contour=contour_index,
        point=point_index,
    )
    raise AssertionError("unreachable")


def _finite_number(value: Any, field: str, glyph: str, contour: int, point: int | None = None) -> float:
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        fail(E_GLYPH_INVALID, "Numeric field is missing or invalid", glyph=glyph, contour=contour, field=field)
    number = float(value)
    if not math.isfinite(number):
        fail(E_GLYPH_INVALID, "Numeric field must be finite", glyph=glyph, contour=contour, field=field)
    return number


def _index(value: Any, glyph: str, contour: int, segment: int, count: int, field: str) -> int:
    if isinstance(value, bool) or not isinstance(value, int):
        fail(
            E_GLYPH_INVALID,
            "Point index is missing or invalid",
            glyph=glyph,
            contour=contour,
            segment=segment,
            field=field,
        )
    if value < 0 or value >= count:
        fail(
            E_GLYPH_INVALID,
            "Point index is out of range",
            glyph=glyph,
            contour=contour,
            segment=segment,
            field=field,
            index=value,
            point_count=count,
        )
    return value


def _parse_unicode(value: Any, glyph_name: str) -> tuple[int, ...]:
    if value is None:
        return ()
    values: Iterable[Any]
    if isinstance(value, (list, tuple)):
        values = value
    else:
        values = (value,)
    result: list[int] = []
    for item in values:
        codepoint: int
        if isinstance(item, bool):
            fail(E_GLYPH_INVALID, "Unicode mapping is invalid", glyph=glyph_name, contour=-1, unicode=item)
        if isinstance(item, int):
            codepoint = item
        elif isinstance(item, str):
            text = item.strip()
            try:
                if text.lower().startswith("u+"):
                    codepoint = int(text[2:], 16)
                elif text.lower().startswith("0x"):
                    codepoint = int(text[2:], 16)
                elif len(text) == 4 and all(character in "0123456789abcdefABCDEF" for character in text):
                    codepoint = int(text, 16)
                else:
                    codepoint = int(text, 10)
            except ValueError:
                fail(E_GLYPH_INVALID, "Unicode mapping is invalid", glyph=glyph_name, contour=-1, unicode=item)
        else:
            fail(E_GLYPH_INVALID, "Unicode mapping is invalid", glyph=glyph_name, contour=-1, unicode=item)
        if codepoint < 0 or codepoint > 0x10FFFF:
            fail(E_GLYPH_INVALID, "Unicode code point is out of range", glyph=glyph_name, contour=-1, unicode=item)
        if codepoint not in result:
            result.append(codepoint)
    return tuple(result)


def _validate_fill_rule(rule: str, glyph_name: str) -> None:
    if rule not in FILL_RULES:
        fail(
            E_GLYPH_INVALID,
            "Unknown fill rule",
            glyph=glyph_name,
            contour=-1,
            fill_rule=rule,
        )

