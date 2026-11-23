"""Explicit pair and panorama state machines."""

from __future__ import annotations

from dataclasses import dataclass, field
from enum import Enum
from typing import Any

import numpy as np

from .errors import ErrorCode, WorkbenchError


class PairState(str, Enum):
    PENDING = "pending"
    REGISTERING = "registering"
    COMMITTED = "committed"
    FAILED = "failed"


class PanoramaState(str, Enum):
    PENDING = "pending"
    COMPOSITING = "compositing"
    COMMITTED = "committed"
    FAILED = "failed"


PAIR_TRANSITIONS: dict[PairState, frozenset[PairState]] = {
    PairState.PENDING: frozenset({PairState.REGISTERING}),
    PairState.REGISTERING: frozenset({PairState.COMMITTED, PairState.FAILED}),
    PairState.FAILED: frozenset({PairState.REGISTERING}),
    PairState.COMMITTED: frozenset(),
}

PANORAMA_TRANSITIONS: dict[PanoramaState, frozenset[PanoramaState]] = {
    PanoramaState.PENDING: frozenset({PanoramaState.COMPOSITING}),
    PanoramaState.COMPOSITING: frozenset(
        {PanoramaState.COMMITTED, PanoramaState.FAILED}
    ),
    PanoramaState.FAILED: frozenset({PanoramaState.COMPOSITING}),
    PanoramaState.COMMITTED: frozenset(),
}


def _illegal_state(kind: str, source: Enum, target: Enum) -> WorkbenchError:
    return WorkbenchError(
        ErrorCode.STATE,
        f"illegal {kind} transition {source.value} -> {target.value}",
        field=f"{kind}.state",
        details={"from": source.value, "to": target.value},
    )


@dataclass(frozen=True)
class PairResult:
    """Immutable committed registration payload."""

    pair_id: str
    image_a: int
    image_b: int
    homography_a_to_b: tuple[float, ...]
    matches: int
    inliers: int
    inlier_ratio: float
    residual_median: float
    residual_p95: float
    covariance_condition: float
    content_hash: str
    parameter_version: str
    algorithm_version: str
    truth_corner_error: float | None = None
    truth_ok: bool | None = None
    inlier_points_a: tuple[tuple[float, float], ...] = ()
    inlier_points_b: tuple[tuple[float, float], ...] = ()
    inlier_residuals: tuple[float, ...] = ()

    @property
    def homography(self) -> np.ndarray:
        result = np.asarray(self.homography_a_to_b, dtype=np.float64).reshape(3, 3)
        result.setflags(write=False)
        return result

    def to_dict(self, *, include_points: bool = True) -> dict[str, Any]:
        payload: dict[str, Any] = {
            "pair_id": self.pair_id,
            "image_a": self.image_a,
            "image_b": self.image_b,
            "homography_a_to_b": list(self.homography_a_to_b),
            "matches": self.matches,
            "inliers": self.inliers,
            "inlier_ratio": self.inlier_ratio,
            "residual_median": self.residual_median,
            "residual_p95": self.residual_p95,
            "covariance_condition": self.covariance_condition,
            "content_hash": self.content_hash,
            "parameter_version": self.parameter_version,
            "algorithm_version": self.algorithm_version,
            "truth_corner_error": self.truth_corner_error,
            "truth_ok": self.truth_ok,
        }
        if include_points:
            payload["inlier_points_a"] = [list(point) for point in self.inlier_points_a]
            payload["inlier_points_b"] = [list(point) for point in self.inlier_points_b]
            payload["inlier_residuals"] = list(self.inlier_residuals)
        return payload


@dataclass
class PairRecord:
    pair_id: str
    image_a: int
    image_b: int
    state: PairState = PairState.PENDING
    result: PairResult | None = None
    error: WorkbenchError | None = None
    matches: int = 0

    def transition(self, target: PairState) -> None:
        if target not in PAIR_TRANSITIONS[self.state]:
            raise _illegal_state("pair", self.state, target)
        self.state = target

    def begin(self) -> None:
        self.error = None
        self.transition(PairState.REGISTERING)

    def commit(self, result: PairResult) -> None:
        if result.pair_id != self.pair_id:
            raise WorkbenchError(
                ErrorCode.STATE, "pair result id does not match pair record"
            )
        self.transition(PairState.COMMITTED)
        self.result = result

    def fail(self, error: WorkbenchError) -> None:
        self.transition(PairState.FAILED)
        self.error = error

    def to_dict(self) -> dict[str, Any]:
        payload: dict[str, Any] = {
            "pair_id": self.pair_id,
            "image_a": self.image_a,
            "image_b": self.image_b,
            "state": self.state.value,
            "matches": self.result.matches if self.result else self.matches,
            "result": self.result.to_dict() if self.result else None,
            "error": self.error.to_dict() if self.error else None,
        }
        return payload


@dataclass
class PanoramaRecord:
    state: PanoramaState = PanoramaState.PENDING
    error: WorkbenchError | None = None
    result: dict[str, Any] | None = None

    def transition(self, target: PanoramaState) -> None:
        if target not in PANORAMA_TRANSITIONS[self.state]:
            raise _illegal_state("panorama", self.state, target)
        self.state = target

    def begin(self) -> None:
        self.error = None
        self.transition(PanoramaState.COMPOSITING)

    def commit(self, result: dict[str, Any]) -> None:
        self.transition(PanoramaState.COMMITTED)
        self.result = result

    def fail(self, error: WorkbenchError) -> None:
        self.transition(PanoramaState.FAILED)
        self.error = error

    def to_dict(self) -> dict[str, Any]:
        return {
            "state": self.state.value,
            "error": self.error.to_dict() if self.error else None,
            "result": self.result,
        }


@dataclass
class PairRegistry:
    records: dict[str, PairRecord] = field(default_factory=dict)

    def add(self, pair_id: str, image_a: int, image_b: int) -> PairRecord:
        if pair_id in self.records:
            raise WorkbenchError(ErrorCode.STATE, f"duplicate pair id {pair_id}")
        record = PairRecord(pair_id=pair_id, image_a=image_a, image_b=image_b)
        self.records[pair_id] = record
        return record

    def ordered(self) -> list[PairRecord]:
        return [self.records[key] for key in sorted(self.records)]

    @property
    def committed_count(self) -> int:
        return sum(
            record.state is PairState.COMMITTED for record in self.records.values()
        )

    @property
    def total_count(self) -> int:
        return len(self.records)

    def to_dict(self) -> dict[str, Any]:
        return {
            "committed": self.committed_count,
            "total": self.total_count,
            "pairs": [record.to_dict() for record in self.ordered()],
        }
