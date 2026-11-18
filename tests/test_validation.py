import unittest

from rosewin.errors import SceneValidationError
from rosewin.model import parse_scene, validate_scene
from rosewin.scene import build_invalid_scene, build_scene


class ValidationTests(unittest.TestCase):
    def test_valid_scene_parses_with_sorted_stable_ids(self):
        model = parse_scene(build_scene())
        self.assertEqual(len(model.facets), 20)
        self.assertEqual([facet.stable_id for facet in model.facets], list(range(1, 21)))
        self.assertTrue(all(facet.quadrant in range(4) for facet in model.facets))

    def test_index_out_of_range(self):
        issues = validate_scene(build_invalid_scene("index"))
        self.assertEqual(issues[0].code, "E_INDEX_OUT_OF_RANGE")
        self.assertEqual((issues[0].quadrant, issues[0].facet), (0, 1))

    def test_degenerate_triangle(self):
        issues = validate_scene(build_invalid_scene("degenerate"))
        self.assertEqual(issues[0].code, "E_TRIANGLE_DEGENERATE")

    def test_transmittance_and_reflectance_ranges(self):
        self.assertEqual(validate_scene(build_invalid_scene("transmittance"))[0].code, "E_TRANSMITTANCE_RANGE")
        self.assertEqual(validate_scene(build_invalid_scene("reflectance"))[0].code, "E_REFLECTANCE_RANGE")

    def test_duplicate_quadrant_and_facet(self):
        codes = {issue.code for issue in validate_scene(build_invalid_scene("duplicate_quadrant"))}
        self.assertIn("E_QUADRANT_DUP", codes)
        codes = {issue.code for issue in validate_scene(build_invalid_scene("duplicate_facet"))}
        self.assertIn("E_FACET_DUP", codes)

    def test_parse_raises_structured_error(self):
        with self.assertRaises(SceneValidationError) as context:
            parse_scene(build_invalid_scene("index"))
        payload = context.exception.to_payload()
        self.assertEqual(payload["status"], "invalid_scene")
        self.assertEqual(payload["issues"][0]["quadrant"], 0)
        self.assertEqual(payload["issues"][0]["facet"], 1)


if __name__ == "__main__":
    unittest.main()
