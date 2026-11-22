from __future__ import annotations

from dataclasses import asdict, dataclass
from typing import Any

import numpy as np

from .errors import DiffError
from .pipeline import Pipeline


@dataclass
class DiffResult:
    equal: bool
    first_pixel: tuple[int, int] | None = None
    path_id: str | None = None
    stage: str | None = None
    reason: str | None = None
    channel: int | None = None

    def to_dict(self) -> dict[str, Any]:
        value = asdict(self)
        if self.first_pixel is not None:
            value["first_pixel"] = list(self.first_pixel)
        return value

    def raise_if_different(self) -> None:
        if not self.equal:
            raise DiffError(self)


def _first_difference(reference: np.ndarray, candidate: np.ndarray) -> tuple[int, int, int] | None:
    if reference.shape != candidate.shape:
        return 0, 0, 0
    if reference.dtype == np.bool_ or candidate.dtype == np.bool_:
        different = reference != candidate
    else:
        different = reference != candidate
    if not bool(different.any()):
        return None
    locations = np.argwhere(different)
    location = locations[0]
    if len(location) >= 3:
        return int(location[1]), int(location[0]), int(location[2])
    if len(location) == 2:
        return int(location[1]), int(location[0]), 0
    return int(location[0]), 0, 0


def compare_pipelines(
    reference: Pipeline,
    candidate: Pipeline,
    *,
    raise_on_error: bool = False,
) -> DiffResult:
    if reference.image_size != candidate.image_size:
        result = DiffResult(
            equal=False,
            first_pixel=(0, 0),
            path_id=None,
            stage="compose",
            reason=(
                f"bitmap dimensions differ: {reference.image_size} != "
                f"{candidate.image_size}"
            ),
        )
        if raise_on_error:
            raise DiffError(result)
        return result

    if reference.commit_sequence != candidate.commit_sequence:
        limit = min(len(reference.commit_sequence), len(candidate.commit_sequence))
        mismatch_index = limit
        for index in range(limit):
            if reference.commit_sequence[index] != candidate.commit_sequence[index]:
                mismatch_index = index
                break
        if mismatch_index < limit:
            path_id, stage = reference.commit_sequence[mismatch_index]
        elif len(reference.commit_sequence) > limit:
            path_id, stage = reference.commit_sequence[limit]
        elif len(candidate.commit_sequence) > limit:
            path_id, stage = candidate.commit_sequence[limit]
        else:
            path_id, stage = (None, "compose")
        result = DiffResult(
            equal=False,
            first_pixel=(0, 0),
            path_id=path_id,
            stage=stage,
            reason="path stage commit sequence differs",
        )
        if raise_on_error:
            raise DiffError(result)
        return result

    path_order = reference.scene.paths
    for path in path_order:
        reference_points = reference.flatten_cache.get(path.id)
        candidate_points = candidate.flatten_cache.get(path.id)
        if (reference_points is None) != (candidate_points is None):
            result = DiffResult(
                equal=False,
                first_pixel=(0, 0),
                path_id=path.id,
                stage="flatten",
                reason="one pipeline is missing a committed flatten artifact",
            )
            if raise_on_error:
                raise DiffError(result)
            return result
        if reference_points is not None and candidate_points is not None:
            if (
                reference_points.shape != candidate_points.shape
                or not np.array_equal(reference_points, candidate_points)
            ):
                if reference_points.shape == candidate_points.shape:
                    location = np.argwhere(reference_points != candidate_points)[0]
                    point_index = int(location[0])
                    channel = int(location[1]) if len(location) > 1 else 0
                    pixel = (
                        int(round(float(reference_points[point_index, 0]))),
                        int(round(float(reference_points[point_index, 1]))),
                    )
                else:
                    point_index = 0
                    channel = 0
                    pixel = (
                        int(round(float(reference_points[0, 0]))),
                        int(round(float(reference_points[0, 1]))),
                    )
                result = DiffResult(
                    equal=False,
                    first_pixel=pixel,
                    path_id=path.id,
                    stage="flatten",
                    reason="flattened polyline values differ",
                    channel=channel,
                )
                if raise_on_error:
                    raise DiffError(result)
                return result

    for path in path_order:
        reference_coverage = reference.coverage_cache.get(path.id)
        candidate_coverage = candidate.coverage_cache.get(path.id)
        if (reference_coverage is None) != (candidate_coverage is None):
            result = DiffResult(
                equal=False,
                first_pixel=(0, 0),
                path_id=path.id,
                stage="coverage",
                reason="one pipeline is missing a committed coverage artifact",
            )
            if raise_on_error:
                raise DiffError(result)
            return result
        if reference_coverage is not None and candidate_coverage is not None:
            difference = _first_difference(reference_coverage, candidate_coverage)
            if difference is not None:
                x, y, channel = difference
                result = DiffResult(
                    equal=False,
                    first_pixel=(x, y),
                    path_id=path.id,
                    stage="coverage",
                    reason="coverage values differ",
                    channel=channel,
                )
                if raise_on_error:
                    raise DiffError(result)
                return result

    reference_stages = reference.materialize_compose_stages()
    candidate_stages = candidate.materialize_compose_stages()
    for path in path_order:
        reference_image = reference_stages.get(path.id)
        candidate_image = candidate_stages.get(path.id)
        if (reference_image is None) != (candidate_image is None):
            result = DiffResult(
                equal=False,
                first_pixel=(0, 0),
                path_id=path.id,
                stage="compose",
                reason="one pipeline is missing a compose artifact",
            )
            if raise_on_error:
                raise DiffError(result)
            return result
        if reference_image is not None and candidate_image is not None:
            difference = _first_difference(reference_image, candidate_image)
            if difference is not None:
                x, y, channel = difference
                result = DiffResult(
                    equal=False,
                    first_pixel=(x, y),
                    path_id=path.id,
                    stage="compose",
                    reason="composed pixel values differ",
                    channel=channel,
                )
                if raise_on_error:
                    raise DiffError(result)
                return result

    difference = _first_difference(reference.canvas, candidate.canvas)
    if difference is not None:
        x, y, channel = difference
        result = DiffResult(
            equal=False,
            first_pixel=(x, y),
            path_id=path_order[-1].id if path_order else None,
            stage="compose",
            reason="final bitmap values differ",
            channel=channel,
        )
        if raise_on_error:
            raise DiffError(result)
        return result
    return DiffResult(equal=True)


def difference_heatmap(reference: np.ndarray, candidate: np.ndarray) -> np.ndarray:
    if reference.shape != candidate.shape:
        raise ValueError(f"shape mismatch: {reference.shape} != {candidate.shape}")
    delta = np.abs(reference.astype(np.float64) - candidate.astype(np.float64))
    if delta.ndim == 3:
        delta = delta.mean(axis=2)
    maximum = float(delta.max())
    if maximum <= 0.0:
        return np.zeros(delta.shape, dtype=np.float32)
    return (delta / maximum).astype(np.float32)

