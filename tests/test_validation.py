import copy
import unittest

import numpy as np

from paperlightbox.errors import SceneError
from paperlightbox.sample_gen import build_sample_scene
from paperlightbox.scene import scene_digest, validate_scene


class ValidationTests(unittest.TestCase):
    def _expect(self, code, mutator, layer=None):
        scene = build_sample_scene(width=64, height=64, tile=32)
        mutator(scene)
        with self.assertRaises(SceneError) as ctx:
            validate_scene(scene)
        self.assertEqual(ctx.exception.code, code)
        if layer is not None:
            self.assertEqual(ctx.exception.layer, layer)

    def test_index_out_of_range_is_code_2(self):
        def mutate(scene):
            mesh = scene["layers"][0]["mesh"]
            mesh["vertices"] = [[0, 0, 0], [1, 0, 0], [0, 1, 0]]
            mesh["triangles"] = [[0, 1, 5]]
            del mesh["grid"]
            del mesh["holes"]

        self._expect(2, mutate, layer=0)

    def test_degenerate_triangle_is_code_3(self):
        def mutate(scene):
            mesh = scene["layers"][1]["mesh"]
            mesh["vertices"] = [[0, 0, 0], [1, 0, 0], [1, 0, 0]]
            mesh["triangles"] = [[0, 1, 2]]
            del mesh["grid"]
            del mesh["holes"]

        self._expect(3, mutate, layer=1)

    def test_transmittance_out_of_range_is_code_4(self):
        self._expect(
            4, lambda s: s["layers"][2].__setitem__("transmittance", 1.5),
            layer=2)

    def test_duplicate_layer_index_is_code_5(self):
        self._expect(
            5, lambda s: s["layers"][-1].__setitem__("index", 0))

    def test_light_on_paper_is_code_6(self):
        def mutate(scene):
            z = scene["layers"][2]["mesh"]["grid"]["z"]
            scene["light"]["position"] = [-0.9, -0.9, z]

        self._expect(6, mutate, layer=2)

    def test_invalid_camera_is_code_7(self):
        self._expect(
            7, lambda s: s["camera"].__setitem__(
                "target", s["camera"]["position"]))

    def test_invalid_light_intensity_is_code_8(self):
        self._expect(
            8, lambda s: s["light"].__setitem__("intensity", -1.0))

    def test_cutout_mask_tampering_is_code_10(self):
        def mutate(scene):
            holes = scene["layers"][0]["mesh"]["holes"]
            holes[0] = 0 if holes[0] else 1

        self._expect(10, mutate, layer=0)

    def test_valid_sample_scene_passes(self):
        validate_scene(build_sample_scene())

    def test_shuffled_layer_order_keeps_digest(self):
        scene = build_sample_scene()
        before = scene_digest(scene)
        shuffled = copy.deepcopy(scene)
        np.random.default_rng(3).shuffle(shuffled["layers"])
        validate_scene(shuffled)
        self.assertEqual(scene_digest(shuffled), before)


if __name__ == "__main__":
    unittest.main()
