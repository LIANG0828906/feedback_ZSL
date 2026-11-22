from __future__ import annotations

import math
from functools import lru_cache

import numpy as np

from .models import Contour, Glyph, GlyphTruth, Segment, TruthSample


@lru_cache(maxsize=1)
def _gauss_legendre() -> tuple[np.ndarray, np.ndarray]:
    return np.polynomial.legendre.leggauss(12)


def segment_point(segment: Segment, t: float) -> np.ndarray:
    t = float(t)
    p0 = np.asarray(segment.p0, dtype=np.float64)
    p1 = np.asarray(segment.p1, dtype=np.float64)
    if segment.kind == "line":
        return p0 + (p1 - p0) * t
    p2 = np.asarray(segment.p2, dtype=np.float64)
    one_minus = 1.0 - t
    if segment.kind == "quadratic":
        return one_minus * one_minus * p0 + 2.0 * one_minus * t * p1 + t * t * p2
    p3 = np.asarray(segment.p3, dtype=np.float64)
    return (
        one_minus**3 * p0
        + 3.0 * one_minus * one_minus * t * p1
        + 3.0 * one_minus * t * t * p2
        + t**3 * p3
    )


def segment_derivative(segment: Segment, t: float) -> np.ndarray:
    t = float(t)
    p0 = np.asarray(segment.p0, dtype=np.float64)
    p1 = np.asarray(segment.p1, dtype=np.float64)
    if segment.kind == "line":
        return p1 - p0
    p2 = np.asarray(segment.p2, dtype=np.float64)
    one_minus = 1.0 - t
    if segment.kind == "quadratic":
        return 2.0 * (one_minus * (p1 - p0) + t * (p2 - p1))
    p3 = np.asarray(segment.p3, dtype=np.float64)
    return 3.0 * (
        one_minus * one_minus * (p1 - p0)
        + 2.0 * one_minus * t * (p2 - p1)
        + t * t * (p3 - p2)
    )


def segment_bbox(segment: Segment) -> tuple[float, float, float, float]:
    points = [np.asarray(segment.p0, dtype=np.float64)]
    if segment.kind == "line":
        points.append(np.asarray(segment.p1, dtype=np.float64))
    elif segment.kind == "quadratic":
        p0, p1, p2 = (
            np.asarray(segment.p0, dtype=np.float64),
            np.asarray(segment.p1, dtype=np.float64),
            np.asarray(segment.p2, dtype=np.float64),
        )
        denominator = p0 - 2.0 * p1 + p2
        for axis in range(2):
            if abs(denominator[axis]) > 1e-15:
                t = (p0[axis] - p1[axis]) / denominator[axis]
                if 0.0 < t < 1.0:
                    points.append(segment_point(segment, t))
        points.append(p2)
    else:
        p0, p1, p2, p3 = (
            np.asarray(segment.p0, dtype=np.float64),
            np.asarray(segment.p1, dtype=np.float64),
            np.asarray(segment.p2, dtype=np.float64),
            np.asarray(segment.p3, dtype=np.float64),
        )
        a = -p0 + 3.0 * p1 - 3.0 * p2 + p3
        b = 2.0 * (p0 - 2.0 * p1 + p2)
        c = p1 - p0
        for axis in range(2):
            aa, bb, cc = a[axis], b[axis], c[axis]
            roots: list[float] = []
            if abs(aa) < 1e-15:
                if abs(bb) > 1e-15:
                    roots.append(-cc / bb)
            else:
                discriminant = bb * bb - 4.0 * aa * cc
                if discriminant >= 0.0:
                    root = math.sqrt(discriminant)
                    roots.extend(((-bb + root) / (2.0 * aa), (-bb - root) / (2.0 * aa)))
            for t in roots:
                if 0.0 < t < 1.0:
                    points.append(segment_point(segment, t))
        points.append(p3)
    stacked = np.vstack(points)
    return (
        float(stacked[:, 0].min()),
        float(stacked[:, 1].min()),
        float(stacked[:, 0].max()),
        float(stacked[:, 1].max()),
    )


def contour_bbox(contour: Contour) -> tuple[float, float, float, float]:
    boxes = [segment_bbox(segment) for segment in contour.segments]
    return (
        min(box[0] for box in boxes),
        min(box[1] for box in boxes),
        max(box[2] for box in boxes),
        max(box[3] for box in boxes),
    )


def glyph_bbox(glyph: Glyph) -> tuple[float, float, float, float]:
    boxes = [contour_bbox(contour) for contour in glyph.contours]
    return (
        min(box[0] for box in boxes),
        min(box[1] for box in boxes),
        max(box[2] for box in boxes),
        max(box[3] for box in boxes),
    )


def segment_signed_area(segment: Segment) -> float:
    nodes, weights = _gauss_legendre()
    total = 0.0
    for node, weight in zip(nodes, weights):
        t = 0.5 * (float(node) + 1.0)
        jacobian = 0.5 * float(weight)
        point = segment_point(segment, t)
        derivative = segment_derivative(segment, t)
        total += jacobian * (
            point[0] * derivative[1] - point[1] * derivative[0]
        )
    return 0.5 * total


def contour_signed_area(contour: Contour) -> float:
    return float(sum(segment_signed_area(segment) for segment in contour.segments))


def glyph_signed_area(glyph: Glyph) -> float:
    return float(sum(contour_signed_area(contour) for contour in glyph.contours))


def reverse_segment(segment: Segment) -> Segment:
    if segment.kind == "line":
        return Segment("line", segment.endpoint, segment.p0)
    if segment.kind == "quadratic":
        return Segment("quadratic", segment.endpoint, segment.p1, segment.p0)
    return Segment("cubic", segment.endpoint, segment.p2, segment.p1, segment.p0)


def reverse_contour(contour: Contour) -> Contour:
    start = contour.start
    reversed_segments = [
        reverse_segment(segment) for segment in reversed(contour.segments)
    ]
    return Contour(index=contour.index, start=start, segments=reversed_segments)


def contour_as_polygon(contour: Contour, samples_per_segment: int = 32) -> np.ndarray:
    points: list[np.ndarray] = []
    for segment in contour.segments:
        for index in range(samples_per_segment):
            points.append(segment_point(segment, index / samples_per_segment))
    return np.vstack(points)


def compute_truth(glyph: Glyph, samples_per_curve: int = 8) -> GlyphTruth:
    sampled: list[TruthSample] = []
    contour_areas: list[float] = []
    for contour in glyph.contours:
        contour_areas.append(contour_signed_area(contour))
        for segment_index, segment in enumerate(contour.segments):
            for sample_index in range(samples_per_curve):
                t = sample_index / samples_per_curve
                point = segment_point(segment, t)
                sampled.append(
                    TruthSample(
                        contour_index=contour.index,
                        segment_index=segment_index,
                        t=t,
                        point=(float(point[0]), float(point[1])),
                    )
                )
    bbox = glyph_bbox(glyph)
    return GlyphTruth(
        bbox=bbox,
        area=float(sum(contour_areas)),
        sampled_points=sampled,
        contour_areas=contour_areas,
    )


def refresh_truth(glyph: Glyph) -> None:
    glyph.truth = compute_truth(glyph)

