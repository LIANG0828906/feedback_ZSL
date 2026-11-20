from __future__ import annotations

import unittest

import numpy as np

from volrender import (
    E_TF_INVALID,
    ControlPoint,
    TransferFunction,
    VolumeRenderer,
    VolRenderError,
)
from volrender.transfer import sample_lut

from tests.helpers import synthetic_transfer, synthetic_volume


class TransferFunctionTests(unittest.TestCase):
    def test_valid_lut_modes_and_interval_bounds(self) -> None:
        transfer = synthetic_transfer()
        for mode in ("float", "fixed"):
            bundle = transfer.build_lut(mode)
            self.assertEqual(bundle.mode, mode)
            self.assertEqual(len(bundle.interval_bounds), len(transfer.points) - 1)
            self.assertTrue(np.all(np.diff(bundle.opacity) >= -1.0e-12))
            for bound in bundle.interval_bounds:
                self.assertGreaterEqual(bound.upper_bound, 0.0)
                self.assertLessEqual(bound.upper_bound, 1.0)

        float_bundle = transfer.build_lut("float")
        fixed_bundle = transfer.build_lut("fixed", fixed_bits=12, fixed_color_bits=12)
        self.assertLessEqual(
            float(np.max(np.abs(float_bundle.opacity - fixed_bundle.opacity))),
            fixed_bundle.opacity_quant_step,
        )
        self.assertLessEqual(
            float(np.max(np.abs(float_bundle.color - fixed_bundle.color))),
            fixed_bundle.color_quant_step,
        )

    def test_adjacent_color_interpolation_is_continuous(self) -> None:
        transfer = synthetic_transfer()
        bundle = transfer.build_lut("float")
        for point in transfer.points[1:-1]:
            epsilon = 1.0e-6
            left = np.array([point.x - epsilon])
            right = np.array([point.x + epsilon])
            _, left_color = sample_lut(bundle, left)
            _, right_color = sample_lut(bundle, right)
            self.assertTrue(np.allclose(left_color, right_color, atol=1.0e-4))

    def test_invalid_edit_preserves_previous_lut(self) -> None:
        renderer = VolumeRenderer(
            volume=synthetic_volume(16),
            transfer=synthetic_transfer(),
            image_size=(24, 24),
        )
        old_transfer = renderer.transfer
        old_lut = renderer.lut
        old_opacity = old_lut.opacity.copy()
        invalid_cases = [
            [(0.0, 0.0, 0.0, 0.0, 0.0), (0.8, 0.1, 0.5, 0.5, 0.5), (0.5, 0.2, 1, 1, 1)],
            [(0.0, 0.0, 0.0, 0.0, 0.0), (0.5, 0.1, 0.5, 0.5, 0.5), (0.5, 0.2, 1, 1, 1)],
            [(0.0, 0.0, 0.0, 0.0, 0.0), (1.0, 1.5, 1, 1, 1)],
            [(0.0, 0.0, 0.0, 0.0, 0.0), (1.0, float("nan"), 1, 1, 1)],
            [(0.0, 0.0)],
            [{"x": 0.0, "opacity": 0.0, "r": 0, "g": 0}],
        ]
        for points in invalid_cases:
            with self.subTest(points=points):
                with self.assertRaises(VolRenderError) as raised:
                    renderer.set_transfer_points(points)
                self.assertEqual(raised.exception.code, E_TF_INVALID)
                self.assertIs(renderer.transfer, old_transfer)
                self.assertIs(renderer.lut, old_lut)
                np.testing.assert_array_equal(renderer.lut.opacity, old_opacity)

    def test_control_point_coercion(self) -> None:
        point = ControlPoint.coerce({"x": 0.2, "opacity": 0.3, "r": 0.4, "g": 0.5, "b": 0.6})
        self.assertEqual(point.x, 0.2)
        self.assertEqual(point.b, 0.6)


if __name__ == "__main__":
    unittest.main()
