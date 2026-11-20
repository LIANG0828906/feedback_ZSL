"""Adaptive dense-versus-sparse sampling policy."""

from __future__ import annotations

import math
from dataclasses import dataclass
from typing import Any

import numpy as np

from .canonical import hash_arrays, hash_object
from .errors import ErrorCode, ProbeError
from .scene import Scene
from .sh import fibonacci_directions, normalize_directions, sh_coefficient_count, validate_order


STRATEGY_AUTO = "auto"
STRATEGY_DENSE = "dense_per_probe"
STRATEGY_SPARSE = "sparse_interpolated"


@dataclass(frozen=True)
class BakeConfig:
    spacing: float | None = None
    sh_order: int = 3
    direction_count: int = 96
    directions: tuple[tuple[float, float, float], ...] | None = None
    compensation_strength: float = 0.35
    strategy: str = STRATEGY_AUTO
    chunk_size: int = 64
    sparse_anchor_stride: int = 2

    def resolved_spacing(self, scene: Scene) -> float:
        spacing = scene.spacing if self.spacing is None else float(self.spacing)
        if not math.isfinite(spacing) or spacing <= 0.0:
            raise ProbeError(ErrorCode.SCENE_INVALID, "spacing must be finite and positive")
        return spacing

    def resolved_directions(self) -> np.ndarray:
        if self.directions is None:
            directions = fibonacci_directions(self.direction_count)
        else:
            directions = normalize_directions(
                np.asarray(self.directions, dtype=np.float64)
            )
        count = sh_coefficient_count(validate_order(self.sh_order))
        if directions.shape[0] < count:
            raise ProbeError(
                ErrorCode.SH_INVALID,
                "direction set is too small for the requested SH order",
                details={"direction_count": int(directions.shape[0]), "required": count},
            )
        return directions

    def validate(self) -> None:
        _ = validate_order(self.sh_order)
        if not math.isfinite(self.compensation_strength) or not 0.0 <= self.compensation_strength <= 1.0:
            raise ProbeError(ErrorCode.INTERP_FAILED, "compensation_strength must be in [0, 1]")
        if self.strategy not in (STRATEGY_AUTO, STRATEGY_DENSE, STRATEGY_SPARSE):
            raise ProbeError(
                ErrorCode.SCENE_INVALID,
                f"strategy must be one of {STRATEGY_AUTO}, {STRATEGY_DENSE}, {STRATEGY_SPARSE}",
            )
        if isinstance(self.chunk_size, bool) or not isinstance(self.chunk_size, int) or self.chunk_size <= 0:
            raise ProbeError(ErrorCode.SCENE_INVALID, "chunk_size must be a positive integer")
        if (
            isinstance(self.sparse_anchor_stride, bool)
            or not isinstance(self.sparse_anchor_stride, int)
            or self.sparse_anchor_stride < 2
        ):
            raise ProbeError(ErrorCode.SCENE_INVALID, "sparse_anchor_stride must be an integer >= 2")
        _ = self.resolved_directions()

    def factor_hashes(self) -> dict[str, str]:
        directions = self.resolved_directions()
        return {
            "sh_order": hash_object(self.sh_order),
            "directions": hash_arrays([("directions", directions)]),
            "compensation": hash_object(self.compensation_strength),
        }

    def to_dict(self) -> dict[str, Any]:
        return {
            "spacing": self.spacing,
            "sh_order": self.sh_order,
            "direction_count": self.direction_count,
            "compensation_strength": self.compensation_strength,
            "strategy": self.strategy,
            "chunk_size": self.chunk_size,
            "sparse_anchor_stride": self.sparse_anchor_stride,
        }

    def resolved_spacing_hash(self, scene: Scene) -> str:
        return hash_object(self.resolved_spacing(scene))


@dataclass(frozen=True)
class StrategyDecision:
    strategy: str
    requested: str
    reason: str
    probe_count: int
    sh_order: int
    direction_count: int
    estimated_sampled_probes: int
    estimated_direction_samples: int
    estimated_seconds: float
    estimated_error_upper_bound: float
    anchor_stride: int

    def to_dict(self) -> dict[str, Any]:
        return {
            "strategy": self.strategy,
            "requested": self.requested,
            "reason": self.reason,
            "probe_count": self.probe_count,
            "sh_order": self.sh_order,
            "direction_count": self.direction_count,
            "estimated_sampled_probes": self.estimated_sampled_probes,
            "estimated_direction_samples": self.estimated_direction_samples,
            "estimated_seconds": self.estimated_seconds,
            "estimated_error_upper_bound": self.estimated_error_upper_bound,
            "anchor_stride": self.anchor_stride,
            "cost_basis": (
                "direction samples * (1 + 0.025 * SH coefficient count); "
                "sparse also counts interpolation of every unsampled probe"
            ),
        }


def choose_strategy(
    probe_count: int,
    sh_order: int,
    direction_count: int,
    requested: str,
    anchor_stride: int = 2,
) -> StrategyDecision:
    order = validate_order(sh_order)
    if probe_count <= 0:
        raise ProbeError(ErrorCode.SCENE_INVALID, "probe count must be positive")
    coefficient_count = sh_coefficient_count(order)
    anchor_fraction = 1.0 / float(anchor_stride**3)
    sampled_probes_dense = int(probe_count)
    sampled_probes_sparse = max(1, int(math.ceil(probe_count * anchor_fraction)))
    dense_samples = sampled_probes_dense * direction_count
    sparse_samples = sampled_probes_sparse * direction_count + int(
        round((probe_count - sampled_probes_sparse) * coefficient_count * 2.0)
    )
    dense_seconds = float(dense_samples) * (1.0 + 0.025 * coefficient_count) / 180_000.0
    sparse_seconds = float(sparse_samples) * (1.0 + 0.015 * coefficient_count) / 180_000.0

    base_error = 1.0 / float(order * order + 1)
    dense_error = base_error
    sparse_error = min(
        4.0,
        base_error * 1.9
        + 0.09 * float(anchor_stride - 1)
        + 0.045 * coefficient_count / 32.0,
    )
    if requested == STRATEGY_DENSE:
        selected = STRATEGY_DENSE
        reason = "explicit dense request"
        sampled = sampled_probes_dense
        seconds = dense_seconds
        error_bound = dense_error
    elif requested == STRATEGY_SPARSE:
        selected = STRATEGY_SPARSE
        reason = "explicit sparse request"
        sampled = sampled_probes_sparse
        seconds = sparse_seconds
        error_bound = sparse_error
    elif probe_count <= 1024 or dense_seconds <= sparse_seconds * 1.2:
        selected = STRATEGY_DENSE
        reason = (
            "small lattice or cost model keeps coverage-per-probe within 20% "
            "of the sparse estimate"
        )
        sampled = sampled_probes_dense
        seconds = dense_seconds
        error_bound = dense_error
    else:
        selected = STRATEGY_SPARSE
        reason = "large lattice: full direction coverage on anchors amortizes interpolation"
        sampled = sampled_probes_sparse
        seconds = sparse_seconds
        error_bound = sparse_error
    return StrategyDecision(
        strategy=selected,
        requested=requested,
        reason=reason,
        probe_count=probe_count,
        sh_order=order,
        direction_count=direction_count,
        estimated_sampled_probes=sampled,
        estimated_direction_samples=sampled * direction_count,
        estimated_seconds=seconds,
        estimated_error_upper_bound=error_bound,
        anchor_stride=anchor_stride,
    )

