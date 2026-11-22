from __future__ import annotations

import copy
import hashlib
import json
from collections.abc import Iterable
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

from .errors import WorkbenchError

Point = tuple[int, int]


@dataclass
class Segment:
    kind: str
    p0: Point
    p1: Point
    p2: Point | None = None
    p3: Point | None = None

    @property
    def endpoint(self) -> Point:
        if self.kind == "line":
            return self.p1
        if self.kind == "quadratic":
            assert self.p2 is not None
            return self.p2
        assert self.p3 is not None
        return self.p3

    def control_points(self) -> list[tuple[str, Point]]:
        points = [("p0", self.p0), ("p1", self.p1)]
        if self.kind in ("quadratic", "cubic") and self.p2 is not None:
            points.append(("p2", self.p2))
        if self.kind == "cubic" and self.p3 is not None:
            points.append(("p3", self.p3))
        return points

    def set_control(self, name: str, point: Point) -> None:
        if name not in {item[0] for item in self.control_points()}:
            raise WorkbenchError(
                "E_STATE",
                f"segment type {self.kind} has no control point {name}",
            )
        setattr(self, name, point)

    def copy(self) -> Segment:
        return copy.deepcopy(self)

    def to_dict(self) -> dict[str, Any]:
        result: dict[str, Any] = {
            "type": self.kind,
            "p0": list(self.p0),
            "p1": list(self.p1),
        }
        if self.kind in ("quadratic", "cubic"):
            result["p2"] = list(self.p2)  # type: ignore[arg-type]
        if self.kind == "cubic":
            result["p3"] = list(self.p3)  # type: ignore[arg-type]
        return result


@dataclass
class Contour:
    index: int
    start: Point
    segments: list[Segment]

    def copy(self) -> Contour:
        return copy.deepcopy(self)

    def to_dict(self) -> dict[str, Any]:
        return {
            "index": self.index,
            "closed": True,
            "start": list(self.start),
            "segments": [segment.to_dict() for segment in self.segments],
        }

    def iter_points(self) -> Iterable[Point]:
        for segment in self.segments:
            for _, point in segment.control_points():
                yield point


@dataclass
class TruthSample:
    contour_index: int
    segment_index: int
    t: float
    point: tuple[float, float]

    def to_dict(self) -> dict[str, Any]:
        return {
            "contour_index": self.contour_index,
            "segment_index": self.segment_index,
            "t": self.t,
            "point": list(self.point),
        }


@dataclass
class GlyphTruth:
    bbox: tuple[float, float, float, float]
    area: float
    sampled_points: list[TruthSample]
    contour_areas: list[float] = field(default_factory=list)

    @classmethod
    def from_dict(cls, raw: dict[str, Any]) -> GlyphTruth:
        samples = [
            TruthSample(
                contour_index=int(item["contour_index"]),
                segment_index=int(item["segment_index"]),
                t=float(item["t"]),
                point=(float(item["point"][0]), float(item["point"][1])),
            )
            for item in raw.get("sampled_points", [])
        ]
        return cls(
            bbox=tuple(float(value) for value in raw["bbox"]),  # type: ignore[arg-type]
            area=float(raw["area"]),
            sampled_points=samples,
            contour_areas=[float(value) for value in raw.get("contour_areas", [])],
        )

    def to_dict(self) -> dict[str, Any]:
        return {
            "bbox": list(self.bbox),
            "area": self.area,
            "contour_areas": self.contour_areas,
            "sampled_points": [sample.to_dict() for sample in self.sampled_points],
        }


@dataclass
class Glyph:
    glyph_id: str
    baseline: int
    advance_width: int
    units_per_em: int
    coordinate_quantum: int
    coordinate_min: int
    coordinate_max: int
    contours: list[Contour]
    truth: GlyphTruth | None = None

    def copy(self) -> Glyph:
        return copy.deepcopy(self)

    def to_dict(self, include_truth: bool = True) -> dict[str, Any]:
        result: dict[str, Any] = {
            "id": self.glyph_id,
            "baseline": self.baseline,
            "advance_width": self.advance_width,
            "units_per_em": self.units_per_em,
            "coordinate_quantum": self.coordinate_quantum,
            "coordinate_min": self.coordinate_min,
            "coordinate_max": self.coordinate_max,
            "contours": [contour.to_dict() for contour in self.contours],
        }
        if include_truth and self.truth is not None:
            result["truth"] = self.truth.to_dict()
        return result

    def content_dict(self) -> dict[str, Any]:
        return self.to_dict(include_truth=False)

    def content_hash(self) -> str:
        payload = json.dumps(
            self.content_dict(),
            ensure_ascii=True,
            sort_keys=True,
            separators=(",", ":"),
        ).encode("utf-8")
        return hashlib.sha256(payload).hexdigest()


@dataclass
class GlyphDocument:
    schema: str
    pixels_per_unit: float
    glyphs: list[Glyph]
    source_path: Path | None = None

    def find_glyph(self, glyph_id: str) -> Glyph:
        for glyph in self.glyphs:
            if glyph.glyph_id == glyph_id:
                return glyph
        raise WorkbenchError(
            "E_SCHEMA",
            f"glyph {glyph_id!r} does not exist",
            glyph_id=glyph_id,
        )

    def copy(self) -> GlyphDocument:
        return copy.deepcopy(self)

    def to_dict(self, include_truth: bool = True) -> dict[str, Any]:
        return {
            "schema": self.schema,
            "pixels_per_unit": self.pixels_per_unit,
            "glyphs": [
                glyph.to_dict(include_truth=include_truth) for glyph in self.glyphs
            ],
        }


def _require_mapping(value: Any, message: str, **context: Any) -> dict[str, Any]:
    if not isinstance(value, dict):
        raise WorkbenchError("E_SCHEMA", message, context)
    return value


def _require_field(
    mapping: dict[str, Any], key: str, message: str, **context: Any
) -> Any:
    if key not in mapping:
        raise WorkbenchError("E_SCHEMA", message, context)
    return mapping[key]


def _parse_point(
    raw: Any,
    *,
    quantum: int,
    coordinate_min: int,
    coordinate_max: int,
    context: dict[str, Any],
) -> Point:
    if not isinstance(raw, list) or len(raw) != 2:
        raise WorkbenchError("E_SCHEMA", "point must be a two-element array", context)
    for value in raw:
        if isinstance(value, bool) or not isinstance(value, int):
            raise WorkbenchError(
                "E_SCHEMA", "coordinates must be quantized integers", context
            )
        if value < coordinate_min or value > coordinate_max:
            raise WorkbenchError(
                "E_SCHEMA",
                f"coordinate {value} outside [{coordinate_min}, {coordinate_max}]",
                **context,
            )
        if quantum <= 0 or value % quantum != 0:
            raise WorkbenchError(
                "E_SCHEMA",
                f"coordinate {value} is not aligned to quantum {quantum}",
                **context,
            )
    return int(raw[0]), int(raw[1])


def _parse_segment(
    raw: Any,
    *,
    quantum: int,
    coordinate_min: int,
    coordinate_max: int,
    context: dict[str, Any],
) -> Segment:
    data = _require_mapping(raw, "curve segment must be an object", **context)
    kind = _require_field(data, "type", "curve segment is missing type", **context)
    if kind not in ("line", "quadratic", "cubic"):
        raise WorkbenchError(
            "E_CURVE", f"unrecognized curve segment type {kind!r}", context
        )
    p0 = _parse_point(
        _require_field(data, "p0", "curve segment is missing p0", **context),
        quantum=quantum,
        coordinate_min=coordinate_min,
        coordinate_max=coordinate_max,
        context=context,
    )
    p1 = _parse_point(
        _require_field(data, "p1", "curve segment is missing p1", **context),
        quantum=quantum,
        coordinate_min=coordinate_min,
        coordinate_max=coordinate_max,
        context=context,
    )
    p2 = p3 = None
    if kind in ("quadratic", "cubic"):
        p2 = _parse_point(
            _require_field(data, "p2", "curve segment is missing p2", **context),
            quantum=quantum,
            coordinate_min=coordinate_min,
            coordinate_max=coordinate_max,
            context=context,
        )
    if kind == "cubic":
        p3 = _parse_point(
            _require_field(data, "p3", "curve segment is missing p3", **context),
            quantum=quantum,
            coordinate_min=coordinate_min,
            coordinate_max=coordinate_max,
            context=context,
        )
    return Segment(kind=kind, p0=p0, p1=p1, p2=p2, p3=p3)


def _parse_contour(
    raw: Any,
    *,
    fallback_index: int,
    quantum: int,
    coordinate_min: int,
    coordinate_max: int,
    glyph_id: str,
) -> Contour:
    base_context = {"glyph_id": glyph_id, "contour_index": fallback_index}
    data = _require_mapping(raw, "contour must be an object", **base_context)
    contour_index = data.get("index", fallback_index)
    if isinstance(contour_index, bool) or not isinstance(contour_index, int):
        raise WorkbenchError("E_SCHEMA", "contour index must be an integer", **base_context)
    base_context["contour_index"] = contour_index
    closed = _require_field(data, "closed", "contour is missing closed flag", **base_context)
    if closed is not True:
        raise WorkbenchError("E_SCHEMA", "contour must be closed", **base_context)
    start = _parse_point(
        _require_field(data, "start", "contour is missing start point", **base_context),
        quantum=quantum,
        coordinate_min=coordinate_min,
        coordinate_max=coordinate_max,
        context=base_context,
    )
    raw_segments = _require_field(
        data, "segments", "contour is missing segments", **base_context
    )
    if not isinstance(raw_segments, list) or not raw_segments:
        raise WorkbenchError(
            "E_SCHEMA", "contour segments must be a non-empty array", **base_context
        )
    segments: list[Segment] = []
    for segment_index, raw_segment in enumerate(raw_segments):
        segment_context = dict(base_context, segment_index=segment_index)
        segment = _parse_segment(
            raw_segment,
            quantum=quantum,
            coordinate_min=coordinate_min,
            coordinate_max=coordinate_max,
            context=segment_context,
        )
        segments.append(segment)
    if segments[0].p0 != start:
        raise WorkbenchError(
            "E_SCHEMA",
            "first curve segment does not start at contour start",
            **base_context,
            segment_index=0,
        )
    for segment_index in range(1, len(segments)):
        if segments[segment_index].p0 != segments[segment_index - 1].endpoint:
            raise WorkbenchError(
                "E_SCHEMA",
                "curve segments are not head-to-tail continuous",
                **base_context,
                segment_index=segment_index,
            )
    if segments[-1].endpoint != start:
        raise WorkbenchError(
            "E_SCHEMA",
            "contour is not closed: last endpoint differs from start",
            **base_context,
            segment_index=len(segments) - 1,
        )
    return Contour(index=contour_index, start=start, segments=segments)


def _parse_glyph(raw: Any, fallback_index: int) -> Glyph:
    data = _require_mapping(raw, "glyph must be an object")
    glyph_id = data.get("id")
    if not isinstance(glyph_id, str) or not glyph_id:
        raise WorkbenchError(
            "E_SCHEMA", "glyph is missing a non-empty string id"
        )
    context = {"glyph_id": glyph_id}
    baseline = _require_field(data, "baseline", "glyph is missing baseline", **context)
    advance_width = _require_field(
        data, "advance_width", "glyph is missing advance_width", **context
    )
    units_per_em = _require_field(
        data, "units_per_em", "glyph is missing units_per_em", **context
    )
    quantum = _require_field(
        data,
        "coordinate_quantum",
        "glyph is missing coordinate_quantum",
        **context,
    )
    coordinate_min = _require_field(
        data, "coordinate_min", "glyph is missing coordinate_min", **context
    )
    coordinate_max = _require_field(
        data, "coordinate_max", "glyph is missing coordinate_max", **context
    )
    for name, value in (
        ("baseline", baseline),
        ("advance_width", advance_width),
        ("units_per_em", units_per_em),
        ("coordinate_quantum", quantum),
        ("coordinate_min", coordinate_min),
        ("coordinate_max", coordinate_max),
    ):
        if isinstance(value, bool) or not isinstance(value, int):
            raise WorkbenchError("E_SCHEMA", f"{name} must be an integer", **context)
    if units_per_em <= 0 or quantum <= 0:
        raise WorkbenchError(
            "E_SCHEMA", "units_per_em and coordinate_quantum must be positive", **context
        )
    if coordinate_min >= coordinate_max:
        raise WorkbenchError(
            "E_SCHEMA", "coordinate_min must be less than coordinate_max", **context
        )
    raw_contours = _require_field(
        data, "contours", "glyph is missing contours", **context
    )
    if not isinstance(raw_contours, list) or not raw_contours:
        raise WorkbenchError(
            "E_SCHEMA", "glyph contours must be a non-empty array", **context
        )
    contours = [
        _parse_contour(
            raw_contour,
            fallback_index=index,
            quantum=quantum,
            coordinate_min=coordinate_min,
            coordinate_max=coordinate_max,
            glyph_id=glyph_id,
        )
        for index, raw_contour in enumerate(raw_contours)
    ]
    truth = None
    if "truth" in data:
        try:
            truth = GlyphTruth.from_dict(data["truth"])
        except (KeyError, TypeError, ValueError, IndexError) as exc:
            raise WorkbenchError(
                "E_SCHEMA", f"invalid truth payload: {exc}", **context
            ) from exc
    return Glyph(
        glyph_id=glyph_id,
        baseline=baseline,
        advance_width=advance_width,
        units_per_em=units_per_em,
        coordinate_quantum=quantum,
        coordinate_min=coordinate_min,
        coordinate_max=coordinate_max,
        contours=contours,
        truth=truth,
    )


def parse_document(raw: Any, source_path: Path | None = None) -> GlyphDocument:
    data = _require_mapping(raw, "document root must be a JSON object")
    schema = data.get("schema")
    if not isinstance(schema, str):
        raise WorkbenchError("E_SCHEMA", "document is missing schema")
    pixels_per_unit = data.get("pixels_per_unit")
    if isinstance(pixels_per_unit, bool) or not isinstance(
        pixels_per_unit, (int, float)
    ):
        raise WorkbenchError("E_SCHEMA", "document is missing numeric pixels_per_unit")
    if float(pixels_per_unit) <= 0:
        raise WorkbenchError("E_SCHEMA", "pixels_per_unit must be positive")
    raw_glyphs = _require_field(data, "glyphs", "document is missing glyphs")
    if not isinstance(raw_glyphs, list) or not raw_glyphs:
        raise WorkbenchError("E_SCHEMA", "glyphs must be a non-empty array")
    glyphs = [_parse_glyph(raw_glyph, index) for index, raw_glyph in enumerate(raw_glyphs)]
    ids = [glyph.glyph_id for glyph in glyphs]
    if len(ids) != len(set(ids)):
        raise WorkbenchError("E_SCHEMA", "glyph ids must be unique")
    return GlyphDocument(
        schema=schema,
        pixels_per_unit=float(pixels_per_unit),
        glyphs=glyphs,
        source_path=source_path,
    )


def load_document(path: str | Path) -> GlyphDocument:
    source = Path(path)
    try:
        text = source.read_text(encoding="utf-8")
    except OSError as exc:
        raise WorkbenchError(
            "E_DECODE", f"cannot read glyph document: {exc}", path=str(source)
        ) from exc
    try:
        raw = json.loads(text)
    except json.JSONDecodeError as exc:
        raise WorkbenchError(
            "E_DECODE",
            f"invalid JSON at line {exc.lineno}, column {exc.colno}: {exc.msg}",
            path=str(source),
        ) from exc
    return parse_document(raw, source_path=source)


def validate_document(document: GlyphDocument) -> None:
    parse_document(document.to_dict(include_truth=True), document.source_path)

