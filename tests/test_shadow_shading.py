import unittest
import copy

import numpy as np

from brocade_embroidery.render import (
    build_shadow_fields,
    build_tables,
    make_camera,
)
from brocade_embroidery.rasterizer import (
    LayerTables,
    component_id_map,
    prepare_layer_geometry,
    render_tile,
    tile_layout,
)
from brocade_embroidery.scene_gen import generate_scene
from brocade_embroidery.shadow import (
    build_receiver_field,
    floor_receiver_spec,
    penumbra_width,
)


class PenumbraTests(unittest.TestCase):
    def test_penumbra_monotone_in_both_distances(self):
        base = penumbra_width(5.0, 2.0, 16)
        farther_light = penumbra_width(8.0, 2.0, 16)
        farther_receiver = penumbra_width(5.0, 4.0, 16)
        denser = penumbra_width(5.0, 2.0, 64)
        self.assertGreater(farther_light, base)
        self.assertGreater(farther_receiver, base)
        self.assertLess(denser, base)

    def test_landing_points_exist_for_both_lights_and_shift_with_geometry(self):
        scene = generate_scene(small=True)
        tables = build_tables(scene)
        _, landing = build_shadow_fields(scene, tables)
        lights = {point.light_id for point in landing}
        self.assertEqual(lights, {"L0", "L1"})
        self.assertTrue(any(point.receiver == "floor" for point in landing))
        # Same hole projected by distinct lights must land apart.
        grouped = {}
        for point in landing:
            key = (point.kesi_layer, point.receiver, point.hole_id)
            grouped.setdefault(key, {})[point.light_id] = point.world
        example = next(iter(grouped.values()))
        if "L0" in example and "L1" in example:
            self.assertNotEqual(example["L0"], example["L1"])

    def test_out_of_range_hit_is_unoccluded_without_hard_edge(self):
        scene = generate_scene(small=True)
        tables = build_tables(scene)
        kesi = next(layer for layer in scene["layers"]
                    if layer["kind"] == "kesi")
        spec = floor_receiver_spec(scene, 64)
        field, _ = build_receiver_field(
            scene, kesi, tables.holes[kesi["layer_index"]], spec,
            scene["lights"][0], sample_density=8)
        # Boundary cells must not collapse to zero (no hard black border).
        self.assertGreater(field.transmission[0, :].min(), 0.0)
        self.assertGreater(field.transmission[-1, :].min(), 0.0)
        self.assertLessEqual(field.transmission.max(), 1.0)

    def test_landing_shifts_consistently_when_receiver_distance_changes(self):
        scene = generate_scene(small=True)
        tables = build_tables(scene)
        _, baseline = build_shadow_fields(scene, tables)
        moved = copy.deepcopy(scene)
        # Move the outer satin receiver one world unit farther away.
        moved["layers"][2]["plane"]["point"][2] += 1.0
        for vertex in moved["layers"][2]["vertices"]:
            vertex[2] += 1.0
        moved_tables = build_tables(moved)
        _, shifted = build_shadow_fields(moved, moved_tables)
        base_map = {(p.kesi_layer, p.receiver, p.light_id, p.hole_id): p
                    for p in baseline if p.receiver == "2"}
        shift_map = {(p.kesi_layer, p.receiver, p.light_id, p.hole_id): p
                     for p in shifted if p.receiver == "2"}
        common = set(base_map) & set(shift_map)
        self.assertGreater(len(common), 10)
        moved_count = 0
        for key in list(common)[:25]:
            if base_map[key].world != shift_map[key].world:
                moved_count += 1
        self.assertGreater(moved_count, 0)

    def test_inside_receiver_penumbra_stays_bounded(self):
        scene = generate_scene(small=True)
        tables = build_tables(scene)
        _, landing = build_shadow_fields(scene, tables)
        inside_widths = [point.penumbra_width for point in landing
                         if point.inside_receiver]
        self.assertTrue(inside_widths)
        self.assertLess(max(inside_widths), 0.2)
        self.assertGreater(min(inside_widths), 0.0)


class ShadingContinuityTests(unittest.TestCase):
    def test_specular_changes_continuously_across_adjacent_pixels(self):
        scene = generate_scene(small=True)
        tables = build_tables(scene)
        fields, _ = build_shadow_fields(scene, tables)
        camera = make_camera(scene, 25.0)
        prepared = [prepare_layer_geometry(layer, camera)
                    for layer in scene["layers"]]
        layout = tile_layout(scene)
        tile = layout[0]
        result = render_tile(
            scene, camera, tile, tables, fields,
            component_id_map(scene), prepared)
        rgba = result.rgba
        horizontal_jump = np.abs(np.diff(rgba[:, :, :3], axis=1))
        vertical_jump = np.abs(np.diff(rgba[:, :, :3], axis=0))
        # A continuous highlight cannot produce full-channel per-pixel jumps.
        self.assertLess(float(horizontal_jump.max()), 0.95)
        self.assertLess(float(vertical_jump.max()), 0.95)

    def test_backlit_satin_retains_embroidery_contrast(self):
        scene = generate_scene(small=True)
        tables = build_tables(scene)
        fields, _ = build_shadow_fields(scene, tables)
        # The back of the stack is reached from a positive azimuth angle.
        camera = make_camera(scene, 35.0)
        prepared = [prepare_layer_geometry(layer, camera)
                    for layer in scene["layers"]]
        result = render_tile(
            scene, camera, tile_layout(scene)[0], tables, fields,
            component_id_map(scene), prepared)
        red = result.rgba[:, :, 0]
        visible = red[red > 0.02]
        self.assertGreater(visible.size, 100)
        self.assertGreater(float(visible.std()), 0.008)


if __name__ == "__main__":
    unittest.main()
