import copy
import unittest

from brocade_embroidery.scene_gen import generate_scene
from brocade_embroidery.validation import validate_scene


class ValidationTests(unittest.TestCase):
    def setUp(self):
        self.scene = generate_scene(small=True)

    def test_generated_scene_is_valid(self):
        self.assertEqual(validate_scene(self.scene), [])

    def test_index_out_of_bounds(self):
        layer = self.scene["layers"][0]
        layer["triangles"][0][0] = len(layer["vertices"]) + 5
        warnings = validate_scene(self.scene)
        codes = {item["error_code"] for item in warnings}
        self.assertIn("E-MESH-INDEX-OUT-OF-BOUNDS", codes)
        self.assertTrue(any(item["component_id"] for item in warnings))

    def test_degenerate_triangle(self):
        layer = self.scene["layers"][0]
        layer["triangles"][1] = [0, 0, 1]
        warnings = validate_scene(self.scene)
        self.assertIn("E-MESH-DEGENERATE",
                      {item["error_code"] for item in warnings})

    def test_reflectivity_out_of_range(self):
        self.scene["layers"][0]["material"]["anisotropic_reflectivity"] = 1.5
        warnings = validate_scene(self.scene)
        self.assertIn("E-MATERIAL-REFLECTIVITY",
                      {item["error_code"] for item in warnings})

    def test_transmittance_out_of_range(self):
        self.scene["layers"][1]["material"]["transmittance"] = 0.0
        warnings = validate_scene(self.scene)
        self.assertIn("E-MATERIAL-TRANSMITTANCE",
                      {item["error_code"] for item in warnings})

    def test_duplicate_layer_index_and_component(self):
        self.scene["layers"][2]["layer_index"] = 0
        warnings = validate_scene(self.scene)
        codes = {item["error_code"] for item in warnings}
        self.assertIn("E-LAYER-DUPLICATE", codes)
        self.assertIn("E-LAYER-INDEX-MISSING", codes)

        scene = generate_scene(small=True)
        duplicate = scene["layers"][0]["component_ids"][0]
        scene["layers"][1]["component_ids"][0] = duplicate
        scene["layers"][1]["triangle_components"][0] = duplicate
        warnings = validate_scene(scene)
        self.assertIn("E-COMPONENT-DUPLICATE",
                      {item["error_code"] for item in warnings})

    def test_light_inside_layer_plane(self):
        layer = self.scene["layers"][0]
        z_plane = layer["plane"]["point"][2]
        self.scene["lights"][0]["position"] = [0.1, 0.2, z_plane]
        warnings = validate_scene(self.scene)
        self.assertIn("E-LIGHT-IN-LAYER-PLANE",
                      {item["error_code"] for item in warnings})

    def test_bad_camera_and_frame(self):
        self.scene["camera"]["radius"] = -1
        self.scene["frame"]["tile_height"] = 9999
        warnings = validate_scene(self.scene)
        codes = {item["error_code"] for item in warnings}
        self.assertIn("E-CAMERA-PARAM", codes)
        self.assertIn("E-FRAME-TILE", codes)


if __name__ == "__main__":
    unittest.main()
