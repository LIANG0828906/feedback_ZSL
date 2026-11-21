import unittest

from npr.canonical import canonical_bytes
from npr.params import OutlineParams, ShadowParams, SmearParams
from npr.scene import parse_scene
from npr.strokes import (
    generate_outline,
    generate_shadow,
    generate_smear,
    remove_culled,
)
from tests.test_scene import square_scene


def two_zone_scene():
    doc = square_scene("a", 0.0, 0.0, 5.0)
    doc["paths"].append(
        square_scene("b", 5.0, 0.0, 5.0)["paths"][0]
    )
    return parse_scene(doc)


class StrokeTests(unittest.TestCase):
    def setUp(self):
        self.scene = two_zone_scene()
        self.outline_p = OutlineParams()
        self.shadow_p = ShadowParams()
        self.smear_p = SmearParams()

    def test_outline_strokes_are_sequential_and_on_path(self):
        strokes = generate_outline(self.scene, self.outline_p)
        self.assertTrue(strokes)
        self.assertEqual([s["id"] for s in strokes], list(range(len(strokes))))
        for stroke in strokes:
            self.assertGreaterEqual(stroke["x"], -0.5)
            self.assertLessEqual(stroke["x"], 10.5)
            self.assertIn(stroke["zone"], ("a", "b"))

    def test_shadow_culling_reports_ids_and_removes_strokes(self):
        outline = generate_outline(self.scene, self.outline_p)
        shadow_all, culled = generate_shadow(self.scene, self.shadow_p, outline)
        self.assertTrue(culled)
        self.assertTrue(all(0 <= cid < len(shadow_all) for cid in culled))
        kept = remove_culled(shadow_all, culled)
        self.assertEqual(len(kept), len(shadow_all) - len(culled))
        kept_ids = {s["id"] for s in kept}
        self.assertFalse(kept_ids.intersection(culled))

    def test_smear_alpha_follows_shadow_density(self):
        outline = generate_outline(self.scene, self.outline_p)
        shadow_all, culled = generate_shadow(self.scene, self.shadow_p, outline)
        shadow = remove_culled(shadow_all, culled)
        dense_p = ShadowParams(spacing=0.8, stroke_len=1.2, cull_margin=0.05)
        dense_all, dense_culled = generate_shadow(self.scene, dense_p, outline)
        dense_shadow = remove_culled(dense_all, dense_culled)
        sparse = generate_smear(self.scene, self.smear_p, shadow)
        dense = generate_smear(self.scene, self.smear_p, dense_shadow)
        self.assertTrue(sparse and dense)
        self.assertGreaterEqual(
            sum(s["alpha"] for s in dense) / len(dense),
            sum(s["alpha"] for s in sparse) / len(sparse),
        )

    def test_stroke_bytes_are_deterministic_under_same_params(self):
        out1 = generate_outline(self.scene, self.outline_p)
        out2 = generate_outline(self.scene, OutlineParams(**self.outline_p.__dict__))
        self.assertEqual(canonical_bytes(out1), canonical_bytes(out2))
        shadow1, cull1 = generate_shadow(self.scene, self.shadow_p, out1)
        shadow2, cull2 = generate_shadow(self.scene, self.shadow_p, out2)
        self.assertEqual(canonical_bytes(shadow1), canonical_bytes(shadow2))
        self.assertEqual(cull1, cull2)
        smear1 = generate_smear(self.scene, self.smear_p, remove_culled(shadow1, cull1))
        smear2 = generate_smear(self.scene, self.smear_p, remove_culled(shadow2, cull2))
        self.assertEqual(canonical_bytes(smear1), canonical_bytes(smear2))

    def test_parameter_change_changes_stroke_bytes(self):
        base = generate_outline(self.scene, self.outline_p)
        changed = generate_outline(
            self.scene, OutlineParams(**{**self.outline_p.__dict__, "jitter": 0.9})
        )
        self.assertNotEqual(canonical_bytes(base), canonical_bytes(changed))


if __name__ == "__main__":
    unittest.main()
