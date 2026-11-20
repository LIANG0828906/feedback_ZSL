from __future__ import annotations

import math
from collections.abc import Iterable
from pathlib import Path
from typing import Any

import numpy as np

from .errors import ErrorCode, GlyphForgeError
from .model import Affine, Font, Glyph, GlyphContour
from .util import read_json, sha256_json

_POINT_EPSILON = 1.0e-6
_AREA_EPSILON = 1.0e-8


def _invalid(message: str, **details: Any) -> GlyphForgeError:
    return GlyphForgeError(ErrorCode.E_GLYPH_INVALID, message, details)


def _number(value: Any, path: str) -> float:
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        raise _invalid("expected a finite number", path=path)
    result = float(value)
    if not math.isfinite(result):
        raise _invalid("expected a finite number", path=path)
    return result


def _point(value: Any, path: str) -> tuple[float, float]:
    if not isinstance(value, dict):
        raise _invalid("point must be an object with x and y", path=path)
    if "x" not in value or "y" not in value:
        raise _invalid("point is missing x or y", path=path)
    return _number(value["x"], f"{path}.x"), _number(value["y"], f"{path}.y")


def _distance_to_segment_sq(
    px: float,
    py: float,
    ax: float,
    ay: float,
    bx: float,
    by: float,
) -> float:
    dx = bx - ax
    dy = by - ay
    length_sq = dx * dx + dy * dy
    if length_sq <= _AREA_EPSILON:
        return (px - ax) ** 2 + (py - ay) ** 2
    t = ((px - ax) * dx + (py - ay) * dy) / length_sq
    t = min(1.0, max(0.0, t))
    qx = ax + t * dx
    qy = ay + t * dy
    return (px - qx) ** 2 + (py - qy) ** 2


def _flatten_quadratic(
    output: list[tuple[float, float]],
    p0: tuple[float, float],
    control: tuple[float, float],
    p1: tuple[float, float],
    tolerance: float,
    depth: int = 0,
) -> None:
    tolerance_sq = tolerance * tolerance
    if depth >= 18 or _distance_to_segment_sq(
        control[0], control[1], p0[0], p0[1], p1[0], p1[1]
    ) <= tolerance_sq:
        output.append(p1)
        return
    p01 = ((p0[0] + control[0]) * 0.5, (p0[1] + control[1]) * 0.5)
    p12 = ((control[0] + p1[0]) * 0.5, (control[1] + p1[1]) * 0.5)
    midpoint = ((p01[0] + p12[0]) * 0.5, (p01[1] + p12[1]) * 0.5)
    _flatten_quadratic(output, p0, p01, midpoint, tolerance, depth + 1)
    _flatten_quadratic(output, midpoint, p12, p1, tolerance, depth + 1)


def _flatten_cubic(
    output: list[tuple[float, float]],
    p0: tuple[float, float],
    c1: tuple[float, float],
    c2: tuple[float, float],
    p1: tuple[float, float],
    tolerance: float,
    depth: int = 0,
) -> None:
    tolerance_sq = tolerance * tolerance
    flat = max(
        _distance_to_segment_sq(c1[0], c1[1], p0[0], p0[1], p1[0], p1[1]),
        _distance_to_segment_sq(c2[0], c2[1], p0[0], p0[1], p1[0], p1[1]),
    )
    if depth >= 18 or flat <= tolerance_sq:
        output.append(p1)
        return
    p01 = ((p0[0] + c1[0]) * 0.5, (p0[1] + c1[1]) * 0.5)
    p12 = ((c1[0] + c2[0]) * 0.5, (c1[1] + c2[1]) * 0.5)
    p23 = ((c2[0] + p1[0]) * 0.5, (c2[1] + p1[1]) * 0.5)
    p012 = ((p01[0] + p12[0]) * 0.5, (p01[1] + p12[1]) * 0.5)
    p123 = ((p12[0] + p23[0]) * 0.5, (p12[1] + p23[1]) * 0.5)
    midpoint = ((p012[0] + p123[0]) * 0.5, (p012[1] + p123[1]) * 0.5)
    _flatten_cubic(output, p0, p01, p012, midpoint, tolerance, depth + 1)
    _flatten_cubic(output, midpoint, p123, p23, p1, tolerance, depth + 1)


def _parse_contour(
    raw: Any,
    glyph_name: str,
    contour_index: int,
    tolerance: float,
) -> GlyphContour:
    details = {"glyph": glyph_name, "contour_index": contour_index}
    if not isinstance(raw, dict):
        raise _invalid("contour must be an object", **details)
    direction_raw = raw.get("direction")
    if direction_raw not in ("cw", "ccw", "clockwise", "counterclockwise"):
        raise _invalid("contour direction must be cw or ccw", **details)
    direction = "cw" if direction_raw in ("cw", "clockwise") else "ccw"
    start = _point(raw.get("start"), f"glyph[{glyph_name}].contours[{contour_index}].start")
    segments = raw.get("segments")
    if not isinstance(segments, list) or not segments:
        raise _invalid("contour must contain at least one segment", **details)

    points: list[tuple[float, float]] = [start]
    current = start
    for segment_index, segment in enumerate(segments):
        path = (
            f"glyph[{glyph_name}].contours[{contour_index}]."
            f"segments[{segment_index}]"
        )
        if not isinstance(segment, dict):
            raise _invalid("segment must be an object", path=path, **details)
        kind = segment.get("type")
        if kind == "line":
            target = _point(segment.get("to"), f"{path}.to")
            if target != current:
                points.append(target)
        elif kind == "quadratic":
            control = _point(segment.get("control"), f"{path}.control")
            target = _point(segment.get("to"), f"{path}.to")
            _flatten_quadratic(points, current, control, target, tolerance)
        elif kind == "cubic":
            control1 = _point(segment.get("control1"), f"{path}.control1")
            control2 = _point(segment.get("control2"), f"{path}.control2")
            target = _point(segment.get("to"), f"{path}.to")
            _flatten_cubic(points, current, control1, control2, target, tolerance)
        else:
            raise _invalid("unsupported segment type", path=path, **details)
        current = target

    if abs(current[0] - start[0]) > _POINT_EPSILON or abs(
        current[1] - start[1]
    ) > _POINT_EPSILON:
        raise _invalid("contour endpoint does not close to start point", **details)
    if points[-1] != start:
        points.append(start)

    unique: list[tuple[float, float]] = []
    for point in points:
        if not unique or (
            abs(point[0] - unique[-1][0]) > _POINT_EPSILON
            or abs(point[1] - unique[-1][1]) > _POINT_EPSILON
        ):
            unique.append(point)
    if len(unique) < 4 or unique[0] != unique[-1]:
        raise _invalid("contour must enclose a non-degenerate area", **details)
    array = np.asarray(unique, dtype=np.float64)
    contour = GlyphContour(
        index=contour_index,
        points=array,
        direction=direction,
        source="local",
    )
    area = contour.signed_area
    if not math.isfinite(area) or abs(area) <= _AREA_EPSILON:
        raise _invalid("contour has zero signed area", **details)
    wants_clockwise = direction == "cw"
    if (area < 0.0) != wants_clockwise:
        contour = GlyphContour(
            index=contour_index,
            points=array[::-1].copy(),
            direction=direction,
            source="local",
        )
    return contour


def _bbox(contours: Iterable[GlyphContour]) -> tuple[float, float, float, float]:
    arrays = [contour.points for contour in contours if len(contour.points)]
    if not arrays:
        return (0.0, 0.0, 0.0, 0.0)
    points = np.concatenate(arrays, axis=0)
    mins = points.min(axis=0)
    maxs = points.max(axis=0)
    return (float(mins[0]), float(mins[1]), float(maxs[0]), float(maxs[1]))


def parse_font(
    raw: Any,
    *,
    path: str = "<memory>",
    flatten_tolerance: float = 0.05,
) -> Font:
    if not isinstance(raw, dict):
        raise _invalid("font description must be a JSON object")
    if flatten_tolerance <= 0 or not math.isfinite(flatten_tolerance):
        raise _invalid("flatten_tolerance must be positive", path="flatten_tolerance")
    font_format = raw.get("format")
    if not isinstance(font_format, str) or not font_format:
        raise _invalid("font format is missing", path="format")
    units_per_em = _number(raw.get("units_per_em"), "units_per_em")
    if units_per_em <= 0:
        raise _invalid("units_per_em must be positive", path="units_per_em")
    ascender = _number(raw.get("ascender", units_per_em * 0.8), "ascender")
    descender = _number(raw.get("descender", -units_per_em * 0.2), "descender")
    raw_glyphs = raw.get("glyphs")
    if not isinstance(raw_glyphs, dict) or not raw_glyphs:
        raise _invalid("glyphs must be a non-empty object", path="glyphs")

    local_contours: dict[str, tuple[GlyphContour, ...]] = {}
    local_stats: dict[str, tuple[float, tuple[int, ...], str]] = {}
    for name, glyph_raw in raw_glyphs.items():
        if not isinstance(name, str) or not name:
            raise _invalid("glyph name must be a non-empty string", path="glyphs")
        if not isinstance(glyph_raw, dict):
            raise _invalid("glyph must be an object", glyph=name)
        if "contours" in glyph_raw:
            contours_raw = glyph_raw["contours"]
            if not isinstance(contours_raw, list):
                raise _invalid("contours must be an array", glyph=name)
        else:
            contours_raw = []
        parsed_contours: list[GlyphContour] = []
        for index, contour in enumerate(contours_raw):
            try:
                parsed_contours.append(
                    _parse_contour(contour, name, index, flatten_tolerance)
                )
            except GlyphForgeError as exc:
                if exc.code == ErrorCode.E_GLYPH_INVALID:
                    exc.details.setdefault("glyph", name)
                    exc.details.setdefault("contour_index", index)
                raise
        contours = tuple(parsed_contours)
        advance = _number(glyph_raw.get("advance", units_per_em * 0.6), f"glyph[{name}].advance")
        unicode_raw = glyph_raw.get("unicode", [])
        if isinstance(unicode_raw, int):
            unicode_raw = [unicode_raw]
        if not isinstance(unicode_raw, list):
            raise _invalid("unicode must be an array of integers", glyph=name)
        unicode_values: list[int] = []
        for codepoint in unicode_raw:
            if (
                isinstance(codepoint, bool)
                or not isinstance(codepoint, int)
                or codepoint < 0
                or codepoint > 0x10FFFF
            ):
                raise _invalid("invalid Unicode codepoint", glyph=name, codepoint=codepoint)
            unicode_values.append(codepoint)
        kind = "composite" if glyph_raw.get("components") else "simple"
        local_contours[name] = contours
        local_stats[name] = (advance, tuple(unicode_values), kind)

    resolved: dict[str, Glyph] = {}
    resolving: set[str] = set()

    def resolve(name: str, stack: tuple[str, ...] = ()) -> Glyph:
        if name in resolved:
            return resolved[name]
        if name not in raw_glyphs:
            consumed_by = stack[-1] if stack else None
            raise _invalid(
                "dangling composite glyph reference",
                glyph=consumed_by,
                referenced_glyph=name,
                contour_index=None,
            )
        if name in resolving:
            raise _invalid(
                "cyclic composite glyph reference",
                glyph=name,
                reference_stack=list(stack + (name,)),
                contour_index=None,
            )
        resolving.add(name)
        glyph_raw = raw_glyphs[name]
        advance, unicode_values, kind = local_stats[name]
        contours = list(local_contours[name])
        components = glyph_raw.get("components", [])
        if components is not None:
            if not isinstance(components, list):
                raise _invalid("components must be an array", glyph=name)
            for component_index, component_raw in enumerate(components):
                if not isinstance(component_raw, dict):
                    raise _invalid(
                        "composite component must be an object",
                        glyph=name,
                        component_index=component_index,
                        contour_index=len(contours),
                    )
                reference = component_raw.get("glyph")
                if not isinstance(reference, str) or not reference:
                    raise _invalid(
                        "composite component has no glyph reference",
                        glyph=name,
                        component_index=component_index,
                        contour_index=len(contours),
                    )
                transform_raw = component_raw.get("transform", [1, 0, 0, 1, 0, 0])
                try:
                    transform = Affine.from_sequence(transform_raw)
                except (TypeError, ValueError):
                    raise _invalid(
                        "component transform must contain six finite numbers",
                        glyph=name,
                        component_index=component_index,
                        contour_index=len(contours),
                    ) from None
                if not all(
                    math.isfinite(value)
                    for value in (
                        transform.a,
                        transform.b,
                        transform.c,
                        transform.d,
                        transform.e,
                        transform.f,
                    )
                ):
                    raise _invalid(
                        "component transform must contain six finite numbers",
                        glyph=name,
                        component_index=component_index,
                        contour_index=len(contours),
                    )
                referenced = resolve(reference, stack + (name,))
                for source_contour in referenced.contours:
                    transformed = transform.apply(source_contour.points)
                    if len(transformed) < 4:
                        raise _invalid(
                            "transformed component contour is degenerate",
                            glyph=name,
                            component_index=component_index,
                            contour_index=len(contours),
                        )
                    contours.append(
                        GlyphContour(
                            index=len(contours),
                            points=transformed,
                            direction=source_contour.direction,
                            source=f"composite:{reference}",
                        )
                    )
                if component_index >= 0 and not unicode_values:
                    unicode_values = referenced.unicode
        if not contours and not unicode_values:
            raise _invalid("glyph has neither contours nor components", glyph=name)
        resolved_glyph = Glyph(
            name=name,
            unicode=tuple(unicode_values),
            advance=advance,
            contours=tuple(contours),
            bbox=_bbox(contours),
            source_kind=kind,
        )
        resolving.remove(name)
        resolved[name] = resolved_glyph
        return resolved_glyph

    for glyph_name in raw_glyphs:
        resolve(glyph_name)

    unicode_map: dict[int, str] = {}
    for glyph in resolved.values():
        for codepoint in glyph.unicode:
            previous = unicode_map.get(codepoint)
            if previous is not None and previous != glyph.name:
                raise _invalid(
                    "Unicode codepoint maps to multiple glyphs",
                    glyph=glyph.name,
                    codepoint=codepoint,
                    previous_glyph=previous,
                )
            unicode_map[codepoint] = glyph.name

    extra_mapping = raw.get("unicode_map", {})
    if extra_mapping is not None:
        if not isinstance(extra_mapping, dict):
            raise _invalid("unicode_map must be an object", path="unicode_map")
        for key, glyph_name in extra_mapping.items():
            try:
                codepoint = int(key)
            except (TypeError, ValueError):
                raise _invalid("unicode_map key must be an integer", key=key) from None
            if codepoint < 0 or codepoint > 0x10FFFF:
                raise _invalid("unicode_map key is out of range", codepoint=codepoint)
            if not isinstance(glyph_name, str) or glyph_name not in resolved:
                raise _invalid(
                    "unicode_map references an unknown glyph",
                    glyph=glyph_name,
                    codepoint=codepoint,
                    contour_index=None,
                )
            previous = unicode_map.get(codepoint)
            if previous is not None and previous != glyph_name:
                raise _invalid(
                    "unicode_map conflicts with glyph Unicode field",
                    glyph=glyph_name,
                    codepoint=codepoint,
                    previous_glyph=previous,
                )
            unicode_map[codepoint] = glyph_name

    return Font(
        path=path,
        format=font_format,
        units_per_em=units_per_em,
        ascender=ascender,
        descender=descender,
        glyphs=resolved,
        unicode_map=unicode_map,
        source_hash=sha256_json(raw),
        raw=raw,
    )


def load_font(
    path: str | Path,
    *,
    flatten_tolerance: float = 0.05,
) -> Font:
    source = Path(path)
    try:
        raw = read_json(source)
    except FileNotFoundError:
        raise _invalid("font description file does not exist", path=str(path)) from None
    except (OSError, UnicodeError, ValueError) as exc:
        raise _invalid("cannot read font description", path=str(path), error=str(exc)) from None
    return parse_font(raw, path=str(source), flatten_tolerance=flatten_tolerance)
