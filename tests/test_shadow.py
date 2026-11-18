from __future__ import annotations

import unittest

from glass_verify.geometry import build_geometry_context
from glass_verify.projection import build_shadow_profiles
from glass_verify.report import stable_json_bytes
from glass_verify.sample import make_sample_scene
from glass_verify.scene import validate_scene


class ShadowTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.scene = validate_scene(make_sample_scene())
        cls.context = build_geometry_context(cls.scene)
        cls.profiles = build_shadow_profiles(cls.context)

    def test_transition_width_grows_with_lead_depth(self) -> None:
        trend = self.profiles["depth_trend"]
        self.assertGreater(len(trend), 3)
        previous_depth = -1.0
        previous_width = -1.0
        for item in trend:
            self.assertGreater(item["lead_depth"], previous_depth)
            self.assertGreater(item["transition_width_world"], previous_width)
            previous_depth = item["lead_depth"]
            previous_width = item["transition_width_world"]

    def test_profiles_contain_soft_samples(self) -> None:
        self.assertGreater(self.profiles["total_profile_samples"], 0)
        for profile in self.profiles["profiles"]:
            luminances = [
                sample["linear_luminance"] for sample in profile["samples"]
            ]
            visibilities = [sample["visibility"] for sample in profile["samples"]]
            self.assertGreater(max(luminances) - min(luminances), 0.05)
            self.assertTrue(any(0.0 < value < 1.0 for value in visibilities))

    def test_profiles_are_byte_stable(self) -> None:
        second_context = build_geometry_context(validate_scene(make_sample_scene()))
        second = build_shadow_profiles(second_context)
        self.assertEqual(stable_json_bytes(self.profiles), stable_json_bytes(second))

    def test_sample_density_is_reported(self) -> None:
        count = self.scene.projection.samples_per_axis ** 2
        self.assertEqual(self.profiles["samples_per_light_probe"], count)
        self.assertIn("transition_width_definition", self.profiles)


if __name__ == "__main__":
    unittest.main()
