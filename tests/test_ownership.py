import unittest

import numpy as np

from brocade_embroidery.render import build_shadow_fields, build_tables, make_camera
from brocade_embroidery.rasterizer import (
    component_id_map,
    prepare_layer_geometry,
    render_tile,
    tile_layout,
)
from brocade_embroidery.scene_gen import generate_scene


class OwnershipTests(unittest.TestCase):
    def test_ownership_is_recomputable_from_scene_only(self):
        scene = generate_scene(small=True)
        tables = build_tables(scene)
        fields, _ = build_shadow_fields(scene, tables)
        camera = make_camera(scene, 15.0)
        prepared = [prepare_layer_geometry(layer, camera)
                    for layer in scene["layers"]]
        mapping = component_id_map(scene)
        reverse = {value: key for key, value in mapping.items()}
        owners = []
        for tile in tile_layout(scene):
            result = render_tile(scene, camera, tile, tables, fields,
                                 mapping, prepared)
            owners.append(result.ownership)
        ownership = np.concatenate(
            [np.concatenate(owners[:2], axis=1),
             np.concatenate(owners[2:], axis=1)], axis=0)
        unique = set(np.unique(ownership).tolist())
        self.assertTrue(unique.issubset(set(mapping.values())))
        self.assertGreater(len(unique & {value for key, value in mapping.items()
                                         if key.startswith(("S", "K"))}), 1)
        # Recompute from scratch with fresh objects.
        scene_b = generate_scene(small=True)
        tables_b = build_tables(scene_b)
        fields_b, _ = build_shadow_fields(scene_b, tables_b)
        camera_b = make_camera(scene_b, 15.0)
        prepared_b = [prepare_layer_geometry(layer, camera_b)
                      for layer in scene_b["layers"]]
        owners_b = [
            render_tile(scene_b, camera_b, tile, tables_b, fields_b,
                        component_id_map(scene_b), prepared_b).ownership
            for tile in tile_layout(scene_b)
        ]
        ownership_b = np.concatenate(
            [np.concatenate(owners_b[:2], axis=1),
             np.concatenate(owners_b[2:], axis=1)], axis=0)
        np.testing.assert_array_equal(ownership, ownership_b)
        self.assertIn(reverse[int(ownership[90, 160])], reverse.values())


if __name__ == "__main__":
    unittest.main()
