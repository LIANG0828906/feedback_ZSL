"""Exact/bitwise comparison and first-divergence localization."""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any

import numpy as np

from .errors import E_DIVERGE, PipelineError
from .renderer import BlockWindow
from .util import canonical_json


@dataclass
class ComparisonReport:
    equal: bool
    checks: dict[str, bool]
    first_divergence: dict[str, Any] | None = None
    actual: Any = None
    expected: Any = None

    def to_dict(self) -> dict[str, Any]:
        result: dict[str, Any] = {
            "equal": self.equal,
            "checks": self.checks,
        }
        if not self.equal:
            result["error"] = {
                "code": E_DIVERGE,
                "field": (self.first_divergence or {}).get("field"),
                "message": "rendered outputs diverge",
                "details": self.first_divergence or {},
            }
            result["actual"] = self.actual
            result["expected"] = self.expected
        return result


def _block_for_pixel(windows: list[BlockWindow], x: int, y: int) -> int | None:
    for window in windows:
        if window.x0 <= x < window.x1 and window.y0 <= y < window.y1:
            return window.index
    return None


def _first_array_mismatch(
    actual: np.ndarray,
    expected: np.ndarray,
    field_name: str,
) -> tuple[int, int, int] | None:
    mismatch = np.not_equal(actual, expected)
    if not np.any(mismatch):
        return None
    coordinates = np.argwhere(mismatch)
    y, x, band = (int(value) for value in coordinates[0])
    return y, x, band


def _first_stat_mismatch(actual: dict[str, Any], expected: dict[str, Any]) -> tuple[str, Any, Any] | None:
    actual_bands = actual.get("wavelength_bands", [])
    expected_bands = expected.get("wavelength_bands", [])
    if len(actual_bands) != len(expected_bands):
        return "stats.wavelength_bands.length", len(actual_bands), len(expected_bands)
    for band_index, (actual_band, expected_band) in enumerate(zip(actual_bands, expected_bands, strict=False)):
        for key in expected_band:
            if actual_band.get(key) != expected_band.get(key):
                return (
                    f"stats.wavelength_bands[{band_index}].{key}",
                    actual_band.get(key),
                    expected_band.get(key),
                )
    for key in ("hit_pixels", "pixel_count"):
        if actual.get(key) != expected.get(key):
            return f"stats.{key}", actual.get(key), expected.get(key)
    return None


def compare_results(actual: Any, expected: Any) -> ComparisonReport:
    """Compare two RenderOutcome/ReferenceResult objects exactly."""

    checks = {
        "transmission": np.array_equal(actual.transmission, expected.transmission),
        "scattering": np.array_equal(actual.scattering, expected.scattering),
        "path_units": np.array_equal(actual.path_units, expected.path_units),
        "optical_units": np.array_equal(actual.optical_units, expected.optical_units),
        "stats": canonical_json(actual.stats) == canonical_json(expected.stats),
        "commit_order": list(actual.commit_order) == list(expected.commit_order),
    }
    if all(checks.values()):
        return ComparisonReport(equal=True, checks=checks)

    windows = getattr(actual, "block_windows", [])
    pixel_candidates: list[tuple[int, int, int, str, int]] = []
    for priority, field_name in enumerate(("transmission", "scattering", "path_units", "optical_units")):
        mismatch = _first_array_mismatch(
            getattr(actual, field_name),
            getattr(expected, field_name),
            field_name,
        )
        if mismatch is not None:
            y, x, band = mismatch
            pixel_candidates.append((y, x, priority, field_name, band))
    if pixel_candidates:
        y, x, _, field_name, band = min(pixel_candidates, key=lambda item: (item[0], item[1], item[2]))
        divergence = {
            "pixel": [x, y],
            "block_id": _block_for_pixel(windows, x, y),
            "field": field_name,
            "wavelength_index": band,
            "actual": float(np.asarray(getattr(actual, field_name))[y, x, band]),
            "expected": float(np.asarray(getattr(expected, field_name))[y, x, band]),
        }
        return ComparisonReport(
            equal=False,
            checks=checks,
            first_divergence=divergence,
            actual=divergence["actual"],
            expected=divergence["expected"],
        )

    if not checks["stats"]:
        stat_name, actual_stat, expected_stat = _first_stat_mismatch(actual.stats, expected.stats) or (
            "stats",
            actual.stats,
            expected.stats,
        )
        divergence = {
            "pixel": None,
            "block_id": None,
            "field": stat_name,
            "actual": actual_stat,
            "expected": expected_stat,
        }
        return ComparisonReport(False, checks, divergence, actual_stat, expected_stat)

    actual_order = list(actual.commit_order)
    expected_order = list(expected.commit_order)
    first_index = next(
        (index for index, pair in enumerate(zip(actual_order, expected_order, strict=False)) if pair[0] != pair[1]),
        min(len(actual_order), len(expected_order)),
    )
    divergence = {
        "pixel": None,
        "block_id": actual_order[first_index] if first_index < len(actual_order) else None,
        "field": "commit_order",
        "index": first_index,
        "actual": actual_order[first_index] if first_index < len(actual_order) else None,
        "expected": expected_order[first_index] if first_index < len(expected_order) else None,
    }
    return ComparisonReport(False, checks, divergence, divergence["actual"], divergence["expected"])


def diff_heatmap(actual: np.ndarray, expected: np.ndarray) -> list[list[float]]:
    if actual.shape != expected.shape:
        raise ValueError("diff heatmap inputs must have the same shape")
    difference = np.abs(np.asarray(actual, dtype=np.float64) - np.asarray(expected, dtype=np.float64))
    if difference.ndim == 3:
        difference = np.max(difference, axis=2)
    maximum = float(np.max(difference)) if difference.size else 0.0
    if maximum > 0.0:
        difference = difference / maximum
    return difference.tolist()


def assert_equal_results(actual: Any, expected: Any) -> None:
    report = compare_results(actual, expected)
    if report.equal:
        return
    divergence = report.first_divergence or {}
    raise PipelineError(
        E_DIVERGE,
        str(divergence.get("field", "render")),
        "rendered outputs diverge",
        {
            **divergence,
            "actual": report.actual,
            "expected": report.expected,
        },
    )


__all__ = ["ComparisonReport", "assert_equal_results", "compare_results", "diff_heatmap"]

