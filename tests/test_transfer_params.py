from __future__ import annotations

import unittest

import numpy as np

from volrender.errors import ErrorCode, VolumeRenderError
from volrender.params import RenderParameters, make_parameters
from volrender.transfer import (
    ControlPoint,
    DEFAULT_CONTROL_POINTS,
    TransferFunction,
    build_transfer_lut,
)


class TransferFunctionTests(unittest.TestCase):
    def test_lut_is_continuous_piecewise_linear(self) -> None:
        points = (
            ControlPoint(0.0, 0.0, (0.0, 0.0, 0.0)),
            ControlPoint(0.5, 0.25, (0.2, 0.4, 0.6)),
            ControlPoint(1.0, 1.0, (1.0, 1.0, 1.0)),
        )
        lut, accepted = build_transfer_lut(points, size=101)
        self.assertEqual(accepted, points)
        np.testing.assert_allclose(lut[50, :3], [0.2, 0.4, 0.6], atol=1e-6)
        self.assertAlmostEqual(float(lut[50, 3]), 0.25, places=6)
        self.assertLess(float(np.max(np.abs(np.diff(lut[:, 3])))), 0.02)

    def test_invalid_update_preserves_previous_lut(self) -> None:
        editor = TransferFunction(DEFAULT_CONTROL_POINTS, size=128)
        before = editor.lut
        before_points = editor.control_points
        invalid_sets = (
            (
                (0.0, 0.0, (0, 0, 0)),
                (0.4, 0.2, (1, 1, 1)),
                (0.4, 0.8, (1, 0, 0)),
            ),
            (
                (0.0, 0.0, (0, 0, 0)),
                (1.3, 1.0, (1, 1, 1)),
            ),
            ((0.0, 0.0, (0, 0, 0)),),
        )
        for points in invalid_sets:
            with self.assertRaises(VolumeRenderError) as raised:
                editor.update(points)
            self.assertEqual(raised.exception.code, ErrorCode.E_TF_INVALID)
            self.assertTrue(np.array_equal(editor.lut, before))
            self.assertEqual(editor.control_points, before_points)


class ParameterHashTests(unittest.TestCase):
    def setUp(self) -> None:
        self.params = make_parameters(
            "a" * 64,
            120.0,
            300.0,
            DEFAULT_CONTROL_POINTS,
            step_size=0.01,
            viewport=(32, 24),
        )

    def test_hash_is_stable(self) -> None:
        duplicate = RenderParameters(
            volume_hash=self.params.volume_hash,
            window_center=self.params.window_center,
            window_width=self.params.window_width,
            control_points=self.params.control_points,
            step_size=self.params.step_size,
            view_matrix=self.params.view_matrix,
            viewport=self.params.viewport,
            mask_enabled=self.params.mask_enabled,
            mask_hash=self.params.mask_hash,
        )
        self.assertEqual(duplicate.parameter_hash, self.params.parameter_hash)
        self.assertEqual(len(self.params.parameter_hash), 64)

    def test_hash_covers_window_controls_and_step(self) -> None:
        changed_window = self.params.with_changes(window_width=301.0)
        changed_center = self.params.with_changes(window_center=121.0)
        changed_step = self.params.with_changes(step_size=0.011)
        changed_controls = self.params.with_changes(
            control_points=(
                ControlPoint(0.0, 0.0, (0, 0, 0)),
                ControlPoint(0.6, 0.7, (1, 0.5, 0.2)),
                ControlPoint(1.0, 1.0, (1, 1, 1)),
            )
        )
        hashes = {
            self.params.parameter_hash,
            changed_window.parameter_hash,
            changed_center.parameter_hash,
            changed_step.parameter_hash,
            changed_controls.parameter_hash,
        }
        self.assertEqual(len(hashes), 5)
        self.assertIn("window_width", self.params.describe_differences(changed_window))
        self.assertIn("control_points", self.params.describe_differences(changed_controls))
        self.assertIn("step_size", self.params.describe_differences(changed_step))

    def test_singular_view_matrix_is_rejected(self) -> None:
        singular = (0.0,) * 16
        with self.assertRaises(VolumeRenderError) as raised:
            self.params.with_changes(view_matrix=singular)
        self.assertEqual(raised.exception.code, ErrorCode.E_TF_INVALID)
        self.assertEqual(raised.exception.field, "view_matrix")


if __name__ == "__main__":
    unittest.main()
