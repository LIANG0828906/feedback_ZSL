from __future__ import annotations

from dataclasses import dataclass

import numpy as np

from .bake_types import BakeConfig, StrategyAudit, StrategyPlan
from .directions import DirectionSet, make_directions
from .grid import ProbeGrid
from .sampling import (
    direct_truth_irradiance,
    sample_probe,
)
from .scene import Scene
from .sh import coefficient_count, evaluate_irradiance, radiance_to_irradiance
from .util import sha256_json


@dataclass(frozen=True)
class InterpolationIndex:
    source_indices: tuple[int, ...]
    target_weights: dict[int, tuple[tuple[int, float], ...]]

    @property
    def source_count(self) -> int:
        return len(self.source_indices)


def _source_indices(grid: ProbeGrid) -> tuple[int, ...]:
    nx, ny, nz = grid.counts
    selected: list[int] = []
    for z in range(nz):
        for y in range(ny):
            for x in range(nx):
                if x % 2 == 0 and y % 2 == 0 and z % 2 == 0:
                    selected.append((z * ny + y) * nx + x)
    if len(selected) < 2:
        selected = list(range(0, grid.probe_count, 2))
    if not selected:
        selected = [0]
    return tuple(selected)


def build_interpolation_index(
    grid: ProbeGrid, config: BakeConfig
) -> InterpolationIndex:
    source_indices = _source_indices(grid)
    source_set = set(source_indices)
    source_coordinates = grid.coordinates[np.asarray(source_indices)]
    weights: dict[int, tuple[tuple[int, float], ...]] = {}
    neighbor_count = min(config.interpolation_neighbors, len(source_indices))
    for target_index in range(grid.probe_count):
        if target_index in source_set:
            weights[target_index] = ((target_index, 1.0),)
            continue
        coordinate = grid.coordinates[target_index]
        distances = np.linalg.norm(source_coordinates - coordinate[None, :], axis=1)
        if neighbor_count == len(distances):
            nearest = np.arange(len(distances))
        else:
            nearest = np.argpartition(distances, neighbor_count - 1)[:neighbor_count]
        nearest = nearest[np.argsort(distances[nearest], kind="stable")]
        local = 1.0 / np.maximum(distances[nearest], grid.spacing * 0.25) ** 2
        local /= np.sum(local)
        weights[target_index] = tuple(
            (source_indices[int(source)], float(weight))
            for source, weight in zip(nearest, local)
        )
    return InterpolationIndex(source_indices=source_indices, target_weights=weights)


def interpolate_coefficients(
    index: InterpolationIndex,
    target_index: int,
    values: dict[int, np.ndarray],
) -> np.ndarray:
    weighted = index.target_weights[target_index]
    result: np.ndarray | None = None
    for source_index, weight in weighted:
        value = values[source_index]
        if result is None:
            result = value * weight
        else:
            result = result + value * weight
    if result is None:
        raise RuntimeError("interpolation index has no source weights")
    return result


def _validation_indices(
    grid: ProbeGrid,
    source_indices: tuple[int, ...],
    limit: int,
    seed: int,
) -> tuple[int, ...]:
    rng = np.random.default_rng(seed + 4441)
    source_set = set(source_indices)
    source_pool = np.asarray(source_indices, dtype=np.int64)
    target_pool = np.asarray(
        [index for index in range(grid.probe_count) if index not in source_set],
        dtype=np.int64,
    )
    rng.shuffle(source_pool)
    rng.shuffle(target_pool)
    count = min(limit, grid.probe_count)
    source_take = min(max(1, count // 2), len(source_pool))
    target_take = min(count - source_take, len(target_pool))
    source_take = min(count - target_take, len(source_pool))
    chosen = list(source_pool[:source_take]) + list(target_pool[:target_take])
    return tuple(sorted(int(item) for item in chosen))


def _error_stats(estimates: list[np.ndarray], truths: list[np.ndarray]) -> tuple[float, float]:
    if not estimates:
        return 0.0, 0.0
    per_probe = []
    for estimate, truth in zip(estimates, truths):
        difference = estimate - truth
        per_probe.append(float(np.sqrt(np.mean(difference * difference))))
    return float(np.sqrt(np.mean(np.square(per_probe)))), float(np.max(per_probe))


def choose_strategy(
    scene: Scene,
    grid: ProbeGrid,
    directions: DirectionSet,
    config: BakeConfig,
) -> tuple[StrategyPlan, InterpolationIndex]:
    index = build_interpolation_index(grid, config)
    coefficient_bytes = (
        grid.probe_count
        * coefficient_count(config.sh_order)
        * 3
        * np.dtype(np.float64).itemsize
        * 2
    )
    dense_ray_samples = grid.probe_count * (
        config.direction_count + config.audit_direction_count
    )
    sparse_ray_samples = index.source_count * config.direction_count + (
        grid.probe_count * config.audit_direction_count
    )
    dense_sh_projections = grid.probe_count
    sparse_sh_projections = index.source_count

    if config.strategy == "dense":
        chosen = "dense"
        reason = "forced_by_configuration"
    elif config.strategy == "sparse":
        chosen = "sparse"
        reason = "forced_by_configuration"
    elif dense_ray_samples > config.max_dense_ray_samples:
        chosen = "sparse"
        reason = (
            f"dense_ray_samples={dense_ray_samples} exceeds "
            f"budget={config.max_dense_ray_samples}"
        )
    elif coefficient_bytes > config.max_dense_coefficient_bytes:
        chosen = "sparse"
        reason = (
            f"coefficient_bytes={coefficient_bytes} exceeds "
            f"budget={config.max_dense_coefficient_bytes}"
        )
    else:
        chosen = "dense"
        reason = "dense_budget_fit"

    validation_indices = _validation_indices(
        grid,
        index.source_indices,
        config.validation_probe_limit,
        config.direction_seed,
    )
    truth_directions = make_directions(
        config.validation_direction_count, config.direction_seed + 7919
    ).vectors
    normals = np.asarray(
        [
            [0.0, 1.0, 0.0],
            [0.0, -1.0, 0.0],
            [1.0, 0.0, 0.0],
            [-1.0, 0.0, 0.0],
            [0.0, 0.0, 1.0],
            [0.0, 0.0, -1.0],
            [0.5773502691896258, 0.5773502691896258, 0.5773502691896258],
            [-0.5773502691896258, 0.5773502691896258, 0.5773502691896258],
        ],
        dtype=np.float64,
    )

    source_values: dict[int, np.ndarray] = {}
    for source_index in index.source_indices:
        sampled = sample_probe(
            scene, grid.coordinates[source_index], directions.vectors, config.sh_order
        )
        source_values[source_index] = sampled.radiance_coefficients

    dense_estimates: list[np.ndarray] = []
    sparse_estimates: list[np.ndarray] = []
    truths: list[np.ndarray] = []
    for probe_index in validation_indices:
        origin = grid.coordinates[probe_index]
        truth = direct_truth_irradiance(
            scene, origin, normals, truth_directions
        )
        dense_sampled = sample_probe(
            scene, origin, directions.vectors, config.sh_order
        )
        dense_estimates.append(
            evaluate_irradiance(
                dense_sampled.irradiance_coefficients, normals, config.sh_order
            )
        )
        residual_coefficients = interpolate_coefficients(
            index, probe_index, source_values
        )
        sparse_estimates.append(
            evaluate_irradiance(
                radiance_to_irradiance(residual_coefficients, config.sh_order),
                normals,
                config.sh_order,
            )
        )
        truths.append(truth)

    dense_rms, dense_max = _error_stats(dense_estimates, truths)
    sparse_rms, sparse_max = _error_stats(sparse_estimates, truths)
    if chosen == "dense":
        selected_rms, selected_max = dense_rms, dense_max
        sampled_count = grid.probe_count
        interpolated_count = 0
        ray_samples = dense_ray_samples
        projections = dense_sh_projections
    else:
        selected_rms, selected_max = sparse_rms, sparse_max
        sampled_count = index.source_count
        interpolated_count = grid.probe_count - index.source_count
        ray_samples = sparse_ray_samples
        projections = sparse_sh_projections

    audit = StrategyAudit(
        chosen=chosen,
        reason=reason,
        probe_count=grid.probe_count,
        sh_order=config.sh_order,
        direction_count=config.direction_count,
        audit_direction_count=config.audit_direction_count,
        sampled_probe_count=sampled_count,
        interpolated_probe_count=interpolated_count,
        estimated_ray_samples=ray_samples,
        estimated_sh_projections=projections,
        coefficient_bytes=coefficient_bytes,
        sampling_method=(
            "deterministic Fibonacci directions; validation probes sampled with "
            f"{config.validation_direction_count} independent truth directions"
        ),
        truth_source=(
            "Monte-Carlo diffuse irradiance truth on 8 normals (deterministic "
            "Fibonacci seed offset), same truth for both strategies"
        ),
        validation_probe_ids=tuple(
            grid.probe_ids[index_value] for index_value in validation_indices
        ),
        dense_rms_error=dense_rms,
        dense_max_error=dense_max,
        sparse_rms_error=sparse_rms,
        sparse_max_error=sparse_max,
        selected_rms_error=selected_rms,
        selected_max_error=selected_max,
        error_upper_bound=selected_max + 2.0 * selected_rms,
    )
    plan = StrategyPlan(
        name=chosen,
        source_indices=index.source_indices,
        stride=2,
        audit=audit,
    )
    return plan, index


def strategy_fingerprint(plan: StrategyPlan, config: BakeConfig) -> str:
    return sha256_json(
        {
            "name": plan.name,
            "stride": plan.stride,
            "sources": list(plan.source_indices),
            "config": config.fingerprint(),
        }
    )

