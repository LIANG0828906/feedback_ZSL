from __future__ import annotations

import dataclasses

import numpy as np
import pytest

from probe_baker.errors import ErrorCode, ProbeError
from probe_baker.grid import build_probe_grid
from probe_baker.interpolation import (
    inverse_distance_neighbour_coefficients,
    trilinear_query,
)
from probe_baker.scene import parse_scene
from probe_baker.samples import room_scene_payload
from probe_baker.sh import (
    fibonacci_directions,
    project_radiance,
    real_sh_basis,
    validate_order,
)
from probe_baker.strategy import BakeConfig
from probe_baker.strategy import choose_strategy


def test_sh_basis_is_orthonormal_for_order_six() -> None:
    directions = fibonacci_directions(20000)
    basis = real_sh_basis(directions, 6)
    gram = (4.0 * np.pi / directions.shape[0]) * (basis.T @ basis)
    assert basis.shape == (20000, 49)
    assert np.max(np.abs(gram - np.eye(49))) < 2e-4


@pytest.mark.parametrize("code_order", [1, 7, -2, 2.5])
def test_invalid_sh_order(code_order) -> None:
    with pytest.raises(ProbeError) as exc:
        validate_order(code_order)
    assert exc.value.code == ErrorCode.SH_INVALID


def test_nonfinite_direction_aborts_projection_transactionally() -> None:
    directions = np.array(fibonacci_directions(32), copy=True)
    directions[3, 1] = np.nan
    radiance = np.ones((4, 32, 3), dtype=np.float64)
    with pytest.raises(ProbeError) as exc:
        project_radiance(radiance, directions, 2)
    assert exc.value.code == ErrorCode.SH_INVALID


def test_projection_errors_finite_and_order_matters(room_scene) -> None:
    directions = fibonacci_directions(128)
    radiance = np.repeat(
        np.linspace(0.2, 1.1, 128, dtype=np.float64)[None, :, None],
        7,
        axis=0,
    ) * np.asarray([0.7, 0.75, 0.8])
    low = project_radiance(radiance, directions, 2)
    high = project_radiance(radiance, directions, 6)
    assert low.coefficients.shape == (7, 9, 3)
    assert high.coefficients.shape == (7, 49, 3)
    assert np.isfinite(low.truncation_error).all()
    assert np.mean(high.truncation_error) != np.mean(low.truncation_error)


def test_trilinear_query_exact_at_probe_and_outside_failure(room_scene) -> None:
    grid = build_probe_grid(room_scene, 2.0).grid
    coefficients = np.arange(grid.count * 9 * 3, dtype=np.float64).reshape(
        grid.count, 9, 3
    )
    exact = trilinear_query(grid, coefficients, grid.centers[5])
    assert np.allclose(exact.coefficients, coefficients[5])
    with pytest.raises(ProbeError) as exc:
        trilinear_query(grid, coefficients, (-100.0, -100.0, -100.0))
    assert exc.value.code == ErrorCode.INTERP_FAILED
    assert exc.value.probe_id is not None


def test_broken_adjacency_returns_probe_id(room_scene) -> None:
    grid = build_probe_grid(room_scene, 2.0).grid
    broken_adjacency = tuple(
        np.asarray([], dtype=np.int64) for _ in range(grid.count)
    )
    broken = dataclasses.replace(grid, adjacency=broken_adjacency)
    coefficients = np.zeros((grid.count, 9, 3), dtype=np.float64)
    with pytest.raises(ProbeError) as exc:
        inverse_distance_neighbour_coefficients(coefficients, broken)
    assert exc.value.code == ErrorCode.INTERP_FAILED
    assert exc.value.probe_id == 0


def test_adaptive_strategy_scales_to_tens_of_thousands() -> None:
    decision = choose_strategy(
        probe_count=30_000,
        sh_order=5,
        direction_count=96,
        requested="auto",
    )
    assert decision.strategy == "sparse_interpolated"
    assert decision.estimated_sampled_probes < decision.probe_count
    assert decision.estimated_direction_samples > 0
    assert decision.estimated_error_upper_bound > 0.0
    assert decision.direction_count == 96

