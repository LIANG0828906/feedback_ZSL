from __future__ import annotations

import unittest

import numpy as np

from volrender import (
    E_LIGHT_DRIFT,
    E_MODE_INVALID,
    VolumeData,
    VolumeRenderer,
    VolRenderError,
)
from volrender.transfer import TransferFunction

from tests.helpers import synthetic_transfer, synthetic_volume


def make_renderer(**overrides) -> VolumeRenderer:
    options = {
        "volume": synthetic_volume(24),
        "transfer": synthetic_transfer(),
        "mode": "float",
        "step": 1.0,
        "image_size": (32, 32),
        "error_threshold": 1.0e-3,
    }
    options.update(overrides)
    return VolumeRenderer(**options)


class RenderTests(unittest.TestCase):
    def test_render_is_byte_deterministic_and_observable(self) -> None:
        renderer = make_renderer()
        first = renderer.render()
        second = renderer.render()
        np.testing.assert_array_equal(first.rgba8, second.rgba8)
        self.assertEqual(first.content_hash, second.content_hash)
        self.assertEqual(first.state_hash, second.state_hash)
        self.assertEqual(first.error_map.shape, (32, 32))
        self.assertEqual(first.untrusted.shape, (32, 32))
        self.assertTrue(np.all(first.error_map >= 0.0))
        self.assertGreater(first.error_summary.total_pixels, 0)
        self.assertGreaterEqual(first.error_summary.trusted_fraction, 0.0)
        self.assertLessEqual(first.error_summary.trusted_fraction, 1.0)

    def test_threshold_marks_untrusted_pixels_without_color(self) -> None:
        renderer = make_renderer(error_threshold=1.0e-12)
        result = renderer.render()
        self.assertGreater(result.error_summary.untrusted_pixels, 0)
        self.assertTrue(np.all(result.rgba8[result.untrusted] == 0))
        self.assertTrue(np.all(result.error_map[result.untrusted] > 1.0e-12))
        self.assertEqual(result.error_summary.status, "low_confidence")
        self.assertFalse(result.error_summary.converged)

    def test_invalid_and_uninitialized_precision_modes(self) -> None:
        with self.assertRaises(VolRenderError) as raised:
            make_renderer(mode="int8")
        self.assertEqual(raised.exception.code, E_MODE_INVALID)

        with self.assertRaises(VolRenderError) as raised:
            VolumeRenderer(
                volume=None,
                transfer=synthetic_transfer(),
                mode="float",
            )
        self.assertEqual(raised.exception.code, E_MODE_INVALID)

        renderer = make_renderer()
        with self.assertRaises(VolRenderError) as raised:
            renderer.set_precision("uint16")
        self.assertEqual(raised.exception.code, E_MODE_INVALID)

    def test_precision_switch_compares_paths_without_changing_semantics(self) -> None:
        renderer = make_renderer(mode="float")
        report = renderer.set_precision("fixed")
        self.assertEqual(renderer.mode, "fixed")
        self.assertEqual(report.mode, "fixed")
        self.assertTrue(report.comparison.semantics_unchanged)
        self.assertGreaterEqual(
            report.comparison.fixed_max_error,
            report.comparison.float_max_error,
        )
        self.assertGreaterEqual(report.comparison.changed_pixel_fraction, 0.0)
        self.assertLessEqual(report.comparison.changed_pixel_fraction, 1.0)
        fixed_a = renderer.render()
        fixed_b = renderer.render()
        self.assertEqual(fixed_a.content_hash, fixed_b.content_hash)

    def test_lighting_drift_pass_and_error_code(self) -> None:
        renderer = make_renderer()
        report = renderer.audit_lighting_drift(tolerance=0.2)
        self.assertTrue(report.passed)
        self.assertLessEqual(report.max_abs_diff, 0.2)
        with self.assertRaises(VolRenderError) as raised:
            renderer.audit_lighting_drift(tolerance=1.0e-12)
        self.assertEqual(raised.exception.code, E_LIGHT_DRIFT)

    def test_boundary_and_zero_gradient_normals_are_finite(self) -> None:
        raw = np.zeros((10, 10, 10), dtype=np.int16)
        volume = VolumeData(
            raw,
            spacing=(1.0, 1.0, 1.0),
            window_level=0.0,
            window_width=1.0,
            truth_source="constant",
        )
        transfer = TransferFunction(
            [(0.0, 0.2, 0.4, 0.6, 0.8), (1.0, 0.8, 1.0, 1.0, 1.0)]
        )
        renderer = VolumeRenderer(
            volume=volume,
            transfer=transfer,
            image_size=(24, 24),
            error_threshold=0.01,
        )
        boundary = np.array(
            [
                [0.0, 0.0, 0.0],
                [-1.0, -1.0, -1.0],
                [9.0, 9.0, 9.0],
            ]
        )
        normals = renderer._normals_at(boundary)
        self.assertTrue(np.all(np.isfinite(normals)))
        np.testing.assert_allclose(normals[:, 0], 0.0)
        np.testing.assert_allclose(normals[:, 1], 0.0)
        np.testing.assert_allclose(normals[:, 2], 1.0)
        result = renderer.render()
        self.assertTrue(np.all(np.isfinite(result.linear_rgb)))


if __name__ == "__main__":
    unittest.main()
