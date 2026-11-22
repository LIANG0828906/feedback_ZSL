"""Mask geometry, closed contours, optimal matching and quality reports."""

from __future__ import annotations

import math
from dataclasses import replace
from typing import Any

import numpy as np

from .config import QualityTolerance, ScaleTolerance
from .errors import BusinessError
from .models import (
    ComparisonReport,
    RegionComparison,
    RegionStat,
    ScaleReport,
    ScaleViolation,
)

_MOORE = (
    (-1, 0),
    (-1, 1),
    (0, 1),
    (1, 1),
    (1, 0),
    (1, -1),
    (0, -1),
    (-1, -1),
)


def _trace_contour(mask: np.ndarray) -> list[list[int]]:
    ys, xs = np.nonzero(mask)
    if len(xs) == 0:
        return []
    start_index = int(np.lexsort((xs, ys))[0])
    start = (int(ys[start_index]), int(xs[start_index]))
    height, width = mask.shape
    current = start
    previous = (start[0], start[1] - 1)
    contour: list[tuple[int, int]] = [start]
    limit = max(32, int(mask.sum()) * 20 + 16)
    for _ in range(limit):
        direction = (previous[0] - current[0], previous[1] - current[1])
        try:
            direction_index = _MOORE.index(direction)
        except ValueError:
            direction_index = 4
        found = None
        for offset in range(1, 9):
            d_row, d_col = _MOORE[(direction_index + offset) % 8]
            candidate = (current[0] + d_row, current[1] + d_col)
            if (
                0 <= candidate[0] < height
                and 0 <= candidate[1] < width
                and mask[candidate]
            ):
                found = candidate
                break
        if found is None:
            break
        previous = current
        current = found
        contour.append(current)
        if current == start and len(contour) >= 4:
            break
    if contour[0] != contour[-1]:
        contour.append(contour[0])
    return [[int(col), int(row)] for row, col in contour]


def extract_regions(
    labels: np.ndarray, *, include_contours: bool = True
) -> list[RegionStat]:
    regions: list[RegionStat] = []
    for label in sorted(int(value) for value in np.unique(labels) if value != 0):
        mask = labels == label
        ys, xs = np.nonzero(mask)
        if len(xs) == 0:
            continue
        region_id = label
        regions.append(
            RegionStat(
                region_id=region_id,
                label=label,
                area=int(len(xs)),
                centroid_x=float(xs.mean()),
                centroid_y=float(ys.mean()),
                bbox_x=int(xs.min()),
                bbox_y=int(ys.min()),
                bbox_w=int(xs.max() - xs.min() + 1),
                bbox_h=int(ys.max() - ys.min() + 1),
                contour=_trace_contour(mask) if include_contours else [],
            )
        )
    return regions


def _bbox_iou(a: RegionStat, b: RegionStat) -> float:
    ax1, ay1 = a.bbox_x, a.bbox_y
    ax2, ay2 = a.bbox_x + a.bbox_w, a.bbox_y + a.bbox_h
    bx1, by1 = b.bbox_x, b.bbox_y
    bx2, by2 = b.bbox_x + b.bbox_w, b.bbox_y + b.bbox_h
    inter_w = max(0, min(ax2, bx2) - max(ax1, bx1))
    inter_h = max(0, min(ay2, by2) - max(ay1, by1))
    intersection = inter_w * inter_h
    union = a.bbox_w * a.bbox_h + b.bbox_w * b.bbox_h - intersection
    return float(intersection / union) if union > 0 else 0.0


def _mask_iou(first: np.ndarray, second: np.ndarray) -> float:
    intersection = int(np.count_nonzero(first & second))
    union = int(np.count_nonzero(first | second))
    return float(intersection / union) if union else 0.0


def _hungarian_min(cost: np.ndarray) -> list[tuple[int, int]]:
    """Rectangular Hungarian assignment; returns (row, column) pairs."""

    if cost.size == 0:
        return []
    rows, cols = cost.shape
    transposed = rows > cols
    matrix = cost.T if transposed else cost
    n, m = matrix.shape
    u = np.zeros(n + 1, dtype=np.float64)
    v = np.zeros(m + 1, dtype=np.float64)
    p = np.zeros(m + 1, dtype=np.int64)
    way = np.zeros(m + 1, dtype=np.int64)
    for i in range(1, n + 1):
        p[0] = i
        j0 = 0
        minv = np.full(m + 1, np.inf, dtype=np.float64)
        used = np.zeros(m + 1, dtype=bool)
        while True:
            used[j0] = True
            i0 = int(p[j0])
            delta = np.inf
            j1 = 0
            for j in range(1, m + 1):
                if used[j]:
                    continue
                current = matrix[i0 - 1, j - 1] - u[i0] - v[j]
                if current < minv[j]:
                    minv[j] = current
                    way[j] = j0
                if minv[j] < delta:
                    delta = minv[j]
                    j1 = j
            for j in range(m + 1):
                if used[j]:
                    u[p[j]] += delta
                    v[j] -= delta
                else:
                    minv[j] -= delta
            j0 = j1
            if p[j0] == 0:
                break
        while True:
            j1 = int(way[j0])
            p[j0] = p[j1]
            j0 = j1
            if j0 == 0:
                break
    assignment = []
    for j in range(1, m + 1):
        if p[j] != 0:
            row = int(p[j]) - 1
            col = j - 1
            assignment.append((col, row) if transposed else (row, col))
    return sorted(assignment)


def match_regions(
    truth: list[RegionStat],
    predicted: list[RegionStat],
    truth_mask: np.ndarray,
    predicted_mask: np.ndarray,
) -> list[tuple[RegionStat, RegionStat]]:
    if not truth or not predicted:
        return []
    cost = np.ones((len(truth), len(predicted)), dtype=np.float64)
    for i, truth_region in enumerate(truth):
        truth_pixels = truth_mask == truth_region.label
        for j, predicted_region in enumerate(predicted):
            predicted_pixels = predicted_mask == predicted_region.label
            cost[i, j] = 1.0 - _mask_iou(truth_pixels, predicted_pixels)
    assignment = _hungarian_min(cost)
    return [(truth[i], predicted[j]) for i, j in assignment]


def _region_lookup(regions: list[RegionStat]) -> dict[int, RegionStat]:
    return {region.region_id: region for region in regions}


def compare_masks(
    predicted_mask: np.ndarray,
    truth_mask: np.ndarray,
    *,
    tolerance: QualityTolerance | None = None,
    include_contours: bool = False,
) -> ComparisonReport:
    truth_regions = extract_regions(truth_mask, include_contours=False)
    predicted_regions = extract_regions(predicted_mask, include_contours=include_contours)
    if len(truth_regions) != len(predicted_regions):
        matched = match_regions(
            truth_regions, predicted_regions, truth_mask, predicted_mask
        )
        matched_truth = {item.region_id for item, _ in matched}
        matched_predicted = {item.region_id for _, item in matched}
        extra = [
            {"region_id": item.region_id, "area": item.area}
            for item in predicted_regions
            if item.region_id not in matched_predicted
        ]
        missing = [
            {"region_id": item.region_id, "area": item.area}
            for item in truth_regions
            if item.region_id not in matched_truth
        ]
        raise BusinessError(
            "E_REGION",
            "区域数量与真值不一致",
            details={
                "truth_count": len(truth_regions),
                "predicted_count": len(predicted_regions),
                "extra_regions": extra,
                "missing_regions": missing,
            },
        )
    matched = match_regions(
        truth_regions, predicted_regions, truth_mask, predicted_mask
    )
    comparisons: list[RegionComparison] = []
    for truth_region, predicted_region in matched:
        centroid_error = math.hypot(
            truth_region.centroid_x - predicted_region.centroid_x,
            truth_region.centroid_y - predicted_region.centroid_y,
        )
        area_error = abs(predicted_region.area - truth_region.area) / max(
            1, truth_region.area
        )
        comparisons.append(
            RegionComparison(
                truth_region_id=truth_region.region_id,
                predicted_region_id=predicted_region.region_id,
                centroid_error_px=float(centroid_error),
                area_relative_error=float(area_error),
                bbox_iou=_bbox_iou(truth_region, predicted_region),
                mask_iou=_mask_iou(
                    predicted_mask == predicted_region.label,
                    truth_mask == truth_region.label,
                ),
                truth_area=truth_region.area,
                predicted_area=predicted_region.area,
            )
        )
    comparisons.sort(key=lambda item: item.truth_region_id)
    matched_count = len(comparisons)
    report = ComparisonReport(
        region_count_truth=len(truth_regions),
        region_count_predicted=len(predicted_regions),
        matched_count=matched_count,
        mean_centroid_error_px=float(
            np.mean([item.centroid_error_px for item in comparisons])
        )
        if comparisons
        else 0.0,
        mean_area_relative_error=float(
            np.mean([item.area_relative_error for item in comparisons])
        )
        if comparisons
        else 0.0,
        mean_bbox_iou=float(np.mean([item.bbox_iou for item in comparisons]))
        if comparisons
        else 0.0,
        mean_mask_iou=float(np.mean([item.mask_iou for item in comparisons]))
        if comparisons
        else 0.0,
        regions=comparisons,
    )
    if tolerance is not None:
        failed = [
            item
            for item in comparisons
            if item.centroid_error_px > tolerance.centroid_px
            or item.area_relative_error > tolerance.area_relative
            or item.bbox_iou < tolerance.bbox_iou_min
            or item.mask_iou < tolerance.mask_iou_min
        ]
        report.tolerance = {
            "centroid_px": tolerance.centroid_px,
            "area_relative": tolerance.area_relative,
            "bbox_iou_min": tolerance.bbox_iou_min,
            "mask_iou_min": tolerance.mask_iou_min,
        }
        report.passed = not failed
    return report


def scale_consistency(
    fine_mask: np.ndarray,
    downsampled_mask: np.ndarray,
    factor: int,
    tolerance: ScaleTolerance,
) -> ScaleReport:
    tolerance.validate()
    fine_regions = extract_regions(fine_mask, include_contours=False)
    coarse_regions = extract_regions(downsampled_mask, include_contours=False)
    if len(fine_regions) != len(coarse_regions):
        matched = match_regions(
            fine_regions, coarse_regions, fine_mask, downsampled_mask
        )
        matched_fine = {item.region_id for item, _ in matched}
        matched_coarse = {item.region_id for _, item in matched}
        raise BusinessError(
            "E_SCALE",
            "规模路径区域数量不一致",
            details={
                "factor": factor,
                "fine_count": len(fine_regions),
                "downsampled_count": len(coarse_regions),
                "fine_unmatched": [
                    {"region_id": item.region_id, "area": item.area}
                    for item in fine_regions
                    if item.region_id not in matched_fine
                ],
                "downsampled_unmatched": [
                    {"region_id": item.region_id, "area": item.area}
                    for item in coarse_regions
                    if item.region_id not in matched_coarse
                ],
            },
        )
    matched = match_regions(fine_regions, coarse_regions, fine_mask, downsampled_mask)
    records: list[dict[str, Any]] = []
    violations: list[ScaleViolation] = []
    for fine_region, coarse_region in matched:
        centroid_error = math.hypot(
            fine_region.centroid_x - coarse_region.centroid_x,
            fine_region.centroid_y - coarse_region.centroid_y,
        )
        area_error = abs(coarse_region.area - fine_region.area) / max(
            1, fine_region.area
        )
        bbox_iou = _bbox_iou(fine_region, coarse_region)
        record = {
            "region_id": fine_region.region_id,
            "downsampled_region_id": coarse_region.region_id,
            "centroid_error_px": float(centroid_error),
            "area_relative_error": float(area_error),
            "bbox_iou": float(bbox_iou),
            "fine_area": fine_region.area,
            "downsampled_area": coarse_region.area,
        }
        records.append(record)
        if (
            centroid_error > tolerance.centroid_px
            or area_error > tolerance.area_relative
            or bbox_iou < tolerance.bbox_iou_min
        ):
            violations.append(
                ScaleViolation(
                    region_id=fine_region.region_id,
                    centroid_error_px=float(centroid_error),
                    area_relative_error=float(area_error),
                    bbox_iou=float(bbox_iou),
                )
            )
    report = ScaleReport(
        factor=factor,
        tolerance=tolerance.to_dict(),
        regions=records,
        violations=violations,
        passed=not violations,
        max_centroid_error_px=float(
            max(record["centroid_error_px"] for record in records)
        )
        if records
        else 0.0,
        max_area_relative_error=float(
            max(record["area_relative_error"] for record in records)
        )
        if records
        else 0.0,
        min_bbox_iou=float(min(record["bbox_iou"] for record in records))
        if records
        else 0.0,
    )
    if violations:
        raise BusinessError(
            "E_SCALE",
            "降采样路径与精细路径的质量偏差超过声明容差",
            details={
                "factor": factor,
                "tolerance": tolerance.to_dict(),
                "violations": [
                    {
                        "region_id": item.region_id,
                        "centroid_error_px": item.centroid_error_px,
                        "area_relative_error": item.area_relative_error,
                        "bbox_iou": item.bbox_iou,
                    }
                    for item in violations
                ],
                "regions": records,
            },
        )
    return report
