from __future__ import annotations

from dataclasses import dataclass
from typing import Any

import numpy as np

from .engine import EngineResult
from .errors import WorkbenchError


@dataclass
class DiffReport:
    equal: bool
    reason: str | None = None
    pixel: tuple[int, int] | None = None
    flat_index: int | None = None
    segment_index: int | None = None
    state: str | None = None
    left: float | None = None
    right: float | None = None
    left_size: tuple[int, int] | None = None
    right_size: tuple[int, int] | None = None
    left_sequence: list[int] | None = None
    right_sequence: list[int] | None = None

    def to_dict(self) -> dict[str, Any]:
        return {
            "equal": self.equal,
            "reason": self.reason,
            "pixel": list(self.pixel) if self.pixel is not None else None,
            "x": self.pixel[1] if self.pixel is not None else None,
            "y": self.pixel[0] if self.pixel is not None else None,
            "flat_index": self.flat_index,
            "segment_index": self.segment_index,
            "state": self.state,
            "left": self.left,
            "right": self.right,
            "left_size": list(self.left_size) if self.left_size else None,
            "right_size": list(self.right_size) if self.right_size else None,
            "left_sequence": self.left_sequence,
            "right_sequence": self.right_sequence,
        }


def _component_pixel_counts(
    result: EngineResult, row: int, col: int
) -> dict[int, int]:
    counts: dict[int, int] = {}
    for component in result.raster.components:
        if (
            0 <= row < component.coverage_counts.shape[0]
            and 0 <= col < component.coverage_counts.shape[1]
        ):
            counts[component.contour_index] = int(
                component.coverage_counts[row, col]
            )
    return counts


def _owner_segment(
    left: EngineResult, right: EngineResult, row: int, col: int
) -> int | None:
    left_counts = _component_pixel_counts(left, row, col)
    right_counts = _component_pixel_counts(right, row, col)
    for contour_index in sorted(set(left_counts) | set(right_counts)):
        if left_counts.get(contour_index, 0) != right_counts.get(
            contour_index, 0
        ):
            return contour_index
    for result in (right, left):
        for component in result.raster.components:
            if (
                0 <= row < component.coverage_counts.shape[0]
                and 0 <= col < component.coverage_counts.shape[1]
                and int(component.coverage_counts[row, col]) > 0
            ):
                return component.contour_index
    return None


def _segment_position(result: EngineResult, contour_index: int | None) -> int:
    if contour_index is None:
        return 0
    for position, component in enumerate(result.raster.components):
        if component.contour_index == contour_index:
            return position
    return 0


def diff_results(left: EngineResult, right: EngineResult) -> DiffReport:
    left_size = (left.raster.height, left.raster.width)
    right_size = (right.raster.height, right.raster.width)
    if left_size != right_size:
        return DiffReport(
            equal=False,
            reason="bitmap dimensions differ",
            pixel=(0, 0),
            segment_index=0,
            state=right.states["segments"][0]["state"] if right.states["segments"] else None,
            left_size=left_size,
            right_size=right_size,
            left_sequence=left.commit_sequence,
            right_sequence=right.commit_sequence,
        )
    mismatch = np.argwhere(left.raster.coverage_counts != right.raster.coverage_counts)
    if mismatch.size:
        row, col = (int(mismatch[0][0]), int(mismatch[0][1]))
        owner_contour = _owner_segment(left, right, row, col)
        segment_index = _segment_position(right, owner_contour)
        state = None
        if segment_index < len(right.states["segments"]):
            state = right.states["segments"][segment_index]["state"]
        flat_index = row * right.raster.width + col
        return DiffReport(
            equal=False,
            reason="pixel coverage differs",
            pixel=(row, col),
            flat_index=flat_index,
            segment_index=segment_index,
            state=state,
            left=float(left.raster.coverage_counts[row, col]),
            right=float(right.raster.coverage_counts[row, col]),
            left_size=left_size,
            right_size=right_size,
            left_sequence=left.commit_sequence,
            right_sequence=right.commit_sequence,
        )
    if left.commit_sequence != right.commit_sequence:
        return DiffReport(
            equal=False,
            reason="segment commit sequence differs",
            pixel=(0, 0),
            segment_index=0,
            state=right.states["segments"][0]["state"] if right.states["segments"] else None,
            left_size=left_size,
            right_size=right_size,
            left_sequence=left.commit_sequence,
            right_sequence=right.commit_sequence,
        )
    return DiffReport(
        equal=True,
        left_size=left_size,
        right_size=right_size,
        left_sequence=left.commit_sequence,
        right_sequence=right.commit_sequence,
    )


def assert_results_equal(left: EngineResult, right: EngineResult) -> DiffReport:
    report = diff_results(left, right)
    if not report.equal:
        raise WorkbenchError(
            "E_DIFF",
            report.reason or "raster outputs differ",
            context=report.to_dict(),
        )
    return report

