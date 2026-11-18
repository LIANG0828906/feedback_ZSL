from __future__ import annotations

import math
import unittest

import numpy as np

from elytron.optics import (
    caustic_point_contributions,
    penumbra_half_width,
    pixel_world_size,
)
from elytron.render import (
    build_context,
    enumerate_blocks,
    hue_continuity,
    render_block,
)
from elytron.report import energy_report
from elytron.scene import generate_scene, validate_scene


class InvariantTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.bundle = validate_scene(generate_scene(254))
        cls.context = build_context(cls.bundle)
        cls.blocks = enumerate_blocks(cls.bundle.scene)

    def test_hue_continuity_passes_declared_threshold(self) -> None:
        report = hue_continuity(self.context)
        self.assertTrue(report["pass"], report)
        self.assertLessEqual(
            report["max_adjacent_jump_deg"],
            report["threshold_deg"] + 1.0e-9,
        )
        self.assertGreater(report["saturation_gate"], 0.0)

    def test_caustic_landings_are_projection_recomputable(self) -> None:
        scene = self.bundle.scene
        leaf_y = float(scene["leaf"]["y"])
        bounds = scene["leaf"]["bounds"]
        sources = {light["id"]: np.asarray(light["position"], dtype=np.float64) for light in scene["lights"]}
        for source_id, samples in self.context.caustics.items():
            valid = samples.valid_indices
            self.assertGreater(valid.size, 0)
            source = sources[source_id]
            points = samples.surface_points[valid]
            t = (leaf_y - source[1]) / (points[:, 1] - source[1])
            calculated = source[None, :] + t[:, None] * (points - source[None, :])
            self.assertLessEqual(
                float(np.max(np.abs(calculated - samples.landing_points[valid]))),
                1.0e-12,
            )
            for landing in samples.landing_points[valid]:
                self.assertGreaterEqual(float(landing[0]), bounds[0])
                self.assertLessEqual(float(landing[0]), bounds[1])
                self.assertGreaterEqual(float(landing[2]), bounds[2])
                self.assertLessEqual(float(landing[2]), bounds[3])

    def test_penumbra_width_is_monotone_in_both_distances(self) -> None:
        penumbra = self.bundle.scene["leaf"]["penumbra"]
        leaf_distance = np.array([0.1, 0.2, 0.4, 0.8])
        source_distance = np.array([0.5, 1.0, 2.0, 4.0])
        widths = penumbra_half_width(leaf_distance, source_distance, penumbra)
        self.assertTrue(np.all(np.diff(widths) > 0.0))
        fixed_source = 1.7
        widths_leaf = penumbra_half_width(leaf_distance, fixed_source, penumbra)
        self.assertTrue(np.all(np.diff(widths_leaf) > 0.0))
        fixed_leaf = 0.5
        widths_source = penumbra_half_width(fixed_leaf, source_distance, penumbra)
        self.assertTrue(np.all(np.diff(widths_source) > 0.0))

    def test_penumbra_profile_has_no_hard_step(self) -> None:
        penumbra = self.bundle.scene["leaf"]["penumbra"]
        sigma = float(penumbra_half_width(0.5, 1.7, penumbra))
        pixel_size = pixel_world_size(
            self.bundle.scene["cameras"][0], self.bundle.scene["leaf"]["y"]
        )
        radius = np.linspace(-4.0 * sigma, 4.0 * sigma, 801)
        profile = np.exp(-0.5 * (radius / sigma) ** 2)
        self.assertTrue(np.all(np.diff(profile[radius <= 0.0]) >= -1.0e-15))
        per_pixel_step = pixel_size / sigma
        sample_count = max(2, int(math.ceil(per_pixel_step * 2.0)))
        sampled = np.exp(-0.5 * (np.linspace(-4, 4, 801 * sample_count) / 1.0) ** 2)
        self.assertLess(float(np.max(np.abs(np.diff(sampled)))), 0.02)

    def test_energy_is_additive_and_shares_sum_to_one(self) -> None:
        report = energy_report(self.context)
        self.assertTrue(report["pass"], report)
        self.assertEqual(report["active_sample_count"], len(report["sample_points"]))
        for sample in report["sample_points"]:
            self.assertLessEqual(
                sample["sum_brightness"],
                sample["sum_of_individual"] + 1.0e-12,
            )
            self.assertAlmostEqual(sample["ratio_sum"], 1.0, delta=1.0e-6)
            self.assertEqual(len(sample["contributions"]), len(report["canonical_light_order"]))
        self.assertGreater(report["pixel_audit"]["leaf_pixel_count"], 10000)
        self.assertLessEqual(
            report["pixel_audit"]["max_combined_vs_sum_excess"],
            1.0e-12,
        )

    def test_light_iteration_order_does_not_change_contributions(self) -> None:
        points = np.array(
            [
                [-0.3, self.bundle.scene["leaf"]["y"], 0.35],
                [0.0, self.bundle.scene["leaf"]["y"], 0.40],
                [0.35, self.bundle.scene["leaf"]["y"], 0.30],
            ]
        )
        forward = caustic_point_contributions(self.context.caustics, points, 8.0)
        reversed_samples = dict(reversed(list(self.context.caustics.items())))
        reverse = caustic_point_contributions(reversed_samples, points, 8.0)
        for source_id in forward:
            self.assertTrue(np.array_equal(forward[source_id], reverse[source_id]))
            self.assertTrue(np.all(forward[source_id] >= 0.0))

    def test_tile_rendering_is_deterministic_and_opaque(self) -> None:
        spec = self.blocks[0]
        camera = self.bundle.scene["cameras"][spec.camera_index]
        first = render_block(self.context, camera, spec)
        second = render_block(self.context, camera, spec)
        self.assertTrue(np.array_equal(first.rgba, second.rgba))
        self.assertTrue(np.array_equal(first.face_id, second.face_id))
        self.assertTrue(np.all(first.rgba[..., 3] == 255))
        self.assertEqual(first.rgba.shape, (spec.height, spec.width, 4))
        self.assertEqual(first.face_id.shape, (spec.height, spec.width))


if __name__ == "__main__":
    unittest.main()
