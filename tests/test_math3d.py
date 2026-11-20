from __future__ import annotations

import itertools

import numpy as np

from mocap_workbench.math3d import (
    euler_matrix,
    matrix_to_euler,
    record_to_transform,
    transform_matrix,
    transform_to_record,
)


def test_all_bvh_euler_orders_round_trip():
    angles = np.array([0.31, -0.47, 0.83])
    for order in itertools.permutations(("x", "y", "z")):
        matrix = euler_matrix(order, angles)
        decoded = matrix_to_euler(order, matrix)
        rebuilt = euler_matrix(order, decoded)
        assert np.max(np.abs(matrix - rebuilt)) < 1.0e-12


def test_transform_quaternion_record_round_trip():
    matrix = transform_matrix(
        euler_matrix(("z", "x", "y"), (0.2, -0.4, 0.7)),
        np.array([1.25, -2.5, 0.75]),
    )
    record = transform_to_record(matrix)
    restored = record_to_transform(record)
    assert np.max(np.abs(matrix - restored)) < 1.0e-12
    assert abs(np.linalg.det(restored[:3, :3]) - 1.0) < 1.0e-12
    assert record[6] >= 0.0

