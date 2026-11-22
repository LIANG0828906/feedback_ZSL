from __future__ import annotations

from dataclasses import dataclass

import numpy as np

from .errors import WorkbenchError
from .geometry import compute_truth, segment_point
from .models import Contour, Glyph, Segment
from .params import RenderParams

_EPSILON = 1e-10
_MAX_SUBDIVISION_DEPTH = 24


@dataclass
class DeviationStats:
    mean: float
    rms: float
    maximum: float
    p95: float
    sample_count: int

    def to_dict(self) -> dict[str, float | int]:
        return {
            "mean": self.mean,
            "rms": self.rms,
            "maximum": self.maximum,
            "p95": self.p95,
            "sample_count": self.sample_count,
        }


@dataclass
class FlatContour:
    contour_index: int
    points: np.ndarray
    curve_segment_count: int
    tolerance: float
    closed: bool = True

    @property
    def polyline_point_count(self) -> int:
        return int(self.points.shape[0])

    @property
    def polyline_segment_count(self) -> int:
        return int(self.points.shape[0])

    def to_dict(self, include_points: bool = False) -> dict:
        result = {
            "contour_index": self.contour_index,
            "curve_segment_count": self.curve_segment_count,
            "polyline_point_count": self.polyline_point_count,
            "polyline_segment_count": self.polyline_segment_count,
            "closed": self.closed,
            "tolerance": self.tolerance,
        }
        if include_points:
            result["points"] = self.points.tolist()
        return result


@dataclass
class FlattenResult:
    glyph_id: str
    params: RenderParams
    contours: list[FlatContour]
    deviation: DeviationStats

    @property
    def total_polyline_segments(self) -> int:
        return sum(contour.polyline_segment_count for contour in self.contours)

    def to_dict(self) -> dict:
        return {
            "glyph_id": self.glyph_id,
            "tolerance": self.params.tolerance,
            "pixels_per_unit": self.params.pixels_per_unit,
            "total_polyline_segments": self.total_polyline_segments,
            "deviation": self.deviation.to_dict(),
            "contours": [contour.to_dict() for contour in self.contours],
        }


def _scaled_controls(segment: Segment, scale: float) -> tuple[np.ndarray, ...]:
    points = [np.asarray(segment.p0, dtype=np.float64) * scale]
    points.append(np.asarray(segment.p1, dtype=np.float64) * scale)
    if segment.kind in ("quadratic", "cubic"):
        points.append(np.asarray(segment.p2, dtype=np.float64) * scale)
    if segment.kind == "cubic":
        points.append(np.asarray(segment.p3, dtype=np.float64) * scale)
    return tuple(points)


def _point_line_distance(point: np.ndarray, start: np.ndarray, end: np.ndarray) -> float:
    chord = end - start
    length = float(np.linalg.norm(chord))
    if length <= _EPSILON:
        return float(np.linalg.norm(point - start))
    return abs(float(chord[0] * (point[1] - start[1]) - chord[1] * (point[0] - start[0]))) / length


def _flat_enough(segment: Segment, scale: float, tolerance: float) -> bool:
    controls = _scaled_controls(segment, scale)
    start = controls[0]
    end = controls[-1]
    if segment.kind == "line":
        return True
    if segment.kind == "quadratic":
        return _point_line_distance(controls[1], start, end) <= tolerance
    return max(
        _point_line_distance(controls[1], start, end),
        _point_line_distance(controls[2], start, end),
    ) <= tolerance


def _lerp(
    point_a: np.ndarray, point_b: np.ndarray, t: float = 0.5
) -> np.ndarray:
    return point_a * (1.0 - t) + point_b * t


def _split_segment(segment: Segment, scale: float) -> tuple[Segment, Segment]:
    if segment.kind == "line":
        midpoint = _lerp(
            np.asarray(segment.p0, dtype=np.float64),
            np.asarray(segment.p1, dtype=np.float64),
        )
        midpoint_int = (
            round(midpoint[0]),
            round(midpoint[1]),
        )
        return (
            Segment("line", segment.p0, midpoint_int),
            Segment("line", midpoint_int, segment.p1),
        )
    if segment.kind == "quadratic":
        p0, p1, p2 = (
            np.asarray(segment.p0, dtype=np.float64),
            np.asarray(segment.p1, dtype=np.float64),
            np.asarray(segment.p2, dtype=np.float64),
        )
        p01 = _lerp(p0, p1)
        p12 = _lerp(p1, p2)
        p012 = _lerp(p01, p12)

        def qpoint(point: np.ndarray) -> tuple[int, int]:
            return round(point[0]), round(point[1])

        return (
            Segment("quadratic", segment.p0, qpoint(p01), qpoint(p012)),
            Segment("quadratic", qpoint(p012), qpoint(p12), segment.p2),
        )
    p0, p1, p2, p3 = (
        np.asarray(segment.p0, dtype=np.float64),
        np.asarray(segment.p1, dtype=np.float64),
        np.asarray(segment.p2, dtype=np.float64),
        np.asarray(segment.p3, dtype=np.float64),
    )
    p01 = _lerp(p0, p1)
    p12 = _lerp(p1, p2)
    p23 = _lerp(p2, p3)
    p012 = _lerp(p01, p12)
    p123 = _lerp(p12, p23)
    p0123 = _lerp(p012, p123)

    def cpoint(point: np.ndarray) -> tuple[int, int]:
        return round(point[0]), round(point[1])

    return (
        Segment("cubic", segment.p0, cpoint(p01), cpoint(p012), cpoint(p0123)),
        Segment("cubic", cpoint(p0123), cpoint(p123), cpoint(p23), segment.p3),
    )


def _subdivide_scaled(
    segment: Segment,
    scale: float,
    tolerance: float,
    depth: int,
    output: list[np.ndarray],
) -> None:
    if segment.kind == "line" or depth >= _MAX_SUBDIVISION_DEPTH:
        output.append(np.asarray(segment.endpoint, dtype=np.float64) * scale)
        return
    if _flat_enough(segment, scale, tolerance):
        output.append(np.asarray(segment.endpoint, dtype=np.float64) * scale)
        return
    left, right = _split_segment(segment, scale)
    _subdivide_scaled(left, scale, tolerance, depth + 1, output)
    _subdivide_scaled(right, scale, tolerance, depth + 1, output)


def flatten_contour(
    contour: Contour, params: RenderParams, *, validate_params: bool = True
) -> FlatContour:
    if validate_params:
        params.validate()
    scale = params.pixels_per_unit
    first = np.asarray(contour.start, dtype=np.float64) * scale
    points: list[np.ndarray] = [first]
    for segment in contour.segments:
        _subdivide_scaled(segment, scale, params.tolerance, 0, points)
    deduplicated: list[np.ndarray] = []
    for point in points:
        if not deduplicated or float(np.linalg.norm(point - deduplicated[-1])) > _EPSILON:
            deduplicated.append(point)
    # The closing edge is implicit. Storing the first point twice would create a
    # zero-length edge and violate the no-duplicate-endpoint invariant.
    if len(deduplicated) > 1 and float(
        np.linalg.norm(deduplicated[-1] - deduplicated[0])
    ) <= _EPSILON:
        deduplicated.pop()
    if len(deduplicated) < 3:
        raise WorkbenchError(
            "E_PARA",
            "flattened contour has fewer than three distinct vertices",
            glyph_id=contour.index,
            contour_index=contour.index,
        )
    return FlatContour(
        contour_index=contour.index,
        points=np.vstack(deduplicated),
        curve_segment_count=len(contour.segments),
        tolerance=params.tolerance,
        closed=True,
    )


def _distance_to_closed_polyline(points: np.ndarray, polyline: np.ndarray) -> np.ndarray:
    starts = polyline
    ends = np.roll(polyline, -1, axis=0)
    edges = ends - starts
    edge_length_sq = np.einsum("ij,ij->i", edges, edges)
    relative = points[:, None, :] - starts[None, :, :]
    projection = np.einsum("pni,ni->pn", relative, edges)
    safe = np.maximum(edge_length_sq, _EPSILON)
    t = np.clip(projection / safe, 0.0, 1.0)
    nearest = starts[None, :, :] + t[:, :, None] * edges[None, :, :]
    distances = np.linalg.norm(points[:, None, :] - nearest, axis=2)
    return distances.min(axis=1)


def deviation_statistics(
    glyph: Glyph, contours: list[FlatContour], scale: float
) -> DeviationStats:
    truth = glyph.truth or compute_truth(glyph)
    by_index = {contour.contour_index: contour for contour in contours}
    distances: list[np.ndarray] = []
    for contour_index, contour in by_index.items():
        samples = np.asarray(
            [
                sample.point
                for sample in truth.sampled_points
                if sample.contour_index == contour_index
            ],
            dtype=np.float64,
        )
        if samples.size == 0:
            continue
        samples = samples * scale
        distances.append(_distance_to_closed_polyline(samples, contour.points))
    if not distances:
        return DeviationStats(0.0, 0.0, 0.0, 0.0, 0)
    values = np.concatenate(distances)
    return DeviationStats(
        mean=float(np.mean(values)),
        rms=float(np.sqrt(np.mean(values * values))),
        maximum=float(np.max(values)),
        p95=float(np.percentile(values, 95.0)),
        sample_count=int(values.size),
    )


def flatten_glyph(
    glyph: Glyph,
    params: RenderParams,
) -> FlattenResult:
    params.validate()
    contours = [
        flatten_contour(contour, params, validate_params=False)
        for contour in glyph.contours
    ]
    deviation = deviation_statistics(
        glyph, contours, scale=params.pixels_per_unit
    )
    return FlattenResult(
        glyph_id=glyph.glyph_id,
        params=params,
        contours=contours,
        deviation=deviation,
    )


def flatten_segment_samples(segment: Segment, count: int) -> np.ndarray:
    if count < 2:
        raise WorkbenchError("E_PARA", "sample count must be at least two")
    return np.vstack([segment_point(segment, index / (count - 1)) for index in range(count)])

