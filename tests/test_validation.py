import unittest

from kiln_glass.scene import build_default_scene
from kiln_glass.validate import (
    ERR_DEGENERATE_TRIANGLE,
    ERR_GROUT_NEGATIVE,
    ERR_INDEX_OUT_OF_RANGE,
    ERR_LIGHT_INSIDE_BRICK,
    ERR_THICKNESS_OUT_OF_RANGE,
    ERR_TRANSMITTANCE_OUT_OF_RANGE,
    ERR_VIEW_NOT_STRICT,
    validate_scene,
)


class ValidationTests(unittest.TestCase):
    def setUp(self):
        self.scene = build_default_scene()

    def assert_code(self, expected_code):
        issues = validate_scene(self.scene)
        codes = {issue.code for issue in issues}
        self.assertIn(expected_code, codes)
        for issue in issues:
            self.assertTrue(issue.object_id)

    def test_default_scene_is_valid(self):
        self.assertEqual(validate_scene(self.scene), [])

    def test_index_out_of_range(self):
        self.scene["wall"]["bricks"][0]["triangles"][0][0] = 999
        self.assert_code(ERR_INDEX_OUT_OF_RANGE)

    def test_degenerate_triangle(self):
        brick = self.scene["wall"]["bricks"][0]
        brick["triangles"][0] = [0, 0, 1]
        self.assert_code(ERR_DEGENERATE_TRIANGLE)

    def test_thickness_out_of_range(self):
        self.scene["wall"]["bricks"][1]["thickness"] = -0.1
        self.assert_code(ERR_THICKNESS_OUT_OF_RANGE)

    def test_transmittance_out_of_range(self):
        self.scene["wall"]["bricks"][1]["transmittance"] = 1.5
        self.assert_code(ERR_TRANSMITTANCE_OUT_OF_RANGE)

    def test_duplicate_brick_id(self):
        self.scene["wall"]["bricks"][2]["brick_id"] = (
            self.scene["wall"]["bricks"][1]["brick_id"]
        )
        issues = validate_scene(self.scene)
        self.assertTrue(any("E1005" in issue.code for issue in issues))

    def test_negative_grout(self):
        self.scene["wall"]["layers"][0]["grout_width"] = -0.02
        self.assert_code(ERR_GROUT_NEGATIVE)

    def test_view_sequence_must_be_strict(self):
        self.scene["view_angles"] = [0.0, 0.5, 0.5]
        self.assert_code(ERR_VIEW_NOT_STRICT)

    def test_light_inside_brick(self):
        brick = self.scene["wall"]["bricks"][0]
        origin = brick["origin"]
        size = brick["size"]
        self.scene["lights"][0]["position"] = [
            origin[0] + size[0] / 2,
            origin[1] + size[1] / 2,
            origin[2] + size[2] / 2,
        ]
        self.assert_code(ERR_LIGHT_INSIDE_BRICK)

    def test_multiple_issues_are_collected(self):
        self.scene["wall"]["bricks"][0]["thickness"] = -1
        self.scene["wall"]["layers"][1]["grout_width"] = -2
        self.scene["view_angles"] = [1.0, 0.0]
        issues = validate_scene(self.scene)
        self.assertGreaterEqual(len(issues), 3)
        self.assertTrue(all(issue.message for issue in issues))


if __name__ == "__main__":
    unittest.main()
