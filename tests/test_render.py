from __future__ import annotations

import copy
import json
import tempfile
import unittest
from pathlib import Path

import numpy as np
from PIL import Image

from canopy_render.camera import Camera
from canopy_render.renderer import render_scene
from canopy_render.scene import generate_scene, scene_digest
from canopy_render.shadows import (
    projection_catalogue,
    shadow_measurements,
)
from canopy_render.validation import validate_scene


class RenderTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.temporary = tempfile.TemporaryDirectory()
        cls.root = Path(cls.temporary.name)
        cls.scene = generate_scene(96, 72, 32, 24)
        cls.out_dir = cls.root / "out"
        cls.result = render_scene(cls.scene, cls.out_dir)
        with open(
            cls.out_dir / "render_report.json", "r", encoding="utf-8"
        ) as handle:
            cls.report = json.load(handle)
        cls.rgba = np.asarray(
            Image.open(cls.out_dir / "render.png").convert("RGBA")
        )
        cls.owner = np.asarray(Image.open(cls.out_dir / "ownership.png"))

    @classmethod
    def tearDownClass(cls) -> None:
        cls.temporary.cleanup()

    def test_report_fields_are_stable_and_path_free(self) -> None:
        expected_prefix = [
            "schema_version",
            "status",
            "business_code",
            "scene_name",
            "scene_digest",
            "canvas",
            "tile_layout",
            "recovery",
            "block_summaries",
            "image",
            "ownership",
            "fabric_composite",
            "premultiplied_layers",
            "transmission_model",
            "image_summary",
            "layers",
            "soft_shadows",
            "projection",
        ]
        self.assertEqual(list(self.report), expected_prefix)
        serialized = json.dumps(self.report)
        self.assertNotIn(str(self.root), serialized)
        self.assertNotIn("/tmp/", serialized)
        self.assertNotIn("timestamp", serialized.lower())
        self.assertEqual(self.report["status"], "complete")
        self.assertEqual(self.report["business_code"], "OK")

    def test_image_ownership_and_premultiplied_layers_are_consistent(self) -> None:
        self.assertEqual(self.rgba.shape, (72, 96, 4))
        self.assertEqual(self.owner.dtype, np.uint16)
        self.assertEqual(
            set(np.unique(self.owner).tolist()),
            {0, 1, 2, 3},
        )
        summary = self.report["image_summary"]
        self.assertGreater(summary["fabric_pixel_count"], 1000)
        self.assertLessEqual(summary["fabric_rgb_max"][0], 246)
        self.assertLessEqual(summary["fabric_rgb_max"][1], 246)
        self.assertLessEqual(summary["fabric_rgb_max"][2], 246)
        self.assertGreater(summary["fabric_luminance"]["stddev"], 0.015)
        self.assertGreater(
            summary["fabric_luminance"]["dark_quartile_stddev"],
            0.01,
        )
        self.assertEqual(
            self.report["premultiplied_layers"]["composition_order"],
            [1, 2, 3],
        )
        for descriptor in self.report["premultiplied_layers"]["files"]:
            layer = np.asarray(
                Image.open(self.out_dir / descriptor["file"]).convert("RGBA")
            )
            self.assertTrue(
                np.all(layer[:, :, :3] <= layer[:, :, 3:4]),
                descriptor["file"],
            )

    def test_layer_contributions_are_reported(self) -> None:
        self.assertEqual(len(self.report["layers"]), 3)
        for layer in self.report["layers"]:
            self.assertGreater(layer["visible_fragments"], 0)
            self.assertGreaterEqual(layer["mean_effective_transmittance"], 0.0)
            self.assertAlmostEqual(
                layer["transmission_ratio"] + layer["reflection_ratio"],
                1.0,
                places=6,
            )
            self.assertEqual(len(layer["pattern_table_sha256"]), 64)
            self.assertEqual(len(layer["fold_normal_table_sha256"]), 64)

    def test_layer_thickness_changes_deterministic_transmission(self) -> None:
        thicker = copy.deepcopy(self.scene)
        for layer in thicker["layers"]:
            layer["thickness"] *= 2.0
        second_dir = self.root / "thicker"
        result = render_scene(thicker, second_dir)
        self.assertEqual(result.status, "complete")
        with open(
            second_dir / "render_report.json", "r", encoding="utf-8"
        ) as handle:
            report = json.load(handle)
        self.assertEqual(
            report["transmission_model"]["model"],
            "beer_lambert_path_length",
        )
        self.assertNotEqual(
            (second_dir / "render.png").read_bytes(),
            (self.out_dir / "render.png").read_bytes(),
        )
        self.assertLess(
            report["layers"][0]["mean_effective_transmittance"],
            self.report["layers"][0]["mean_effective_transmittance"],
        )

    def test_seeded_fold_normal_perturbation_changes_pixels(self) -> None:
        flat = copy.deepcopy(self.scene)
        flat["fabric_shading"]["fold_normal_strength"] = 0.0
        flat_dir = self.root / "flat_normals"
        result = render_scene(flat, flat_dir)
        self.assertEqual(result.status, "complete")
        self.assertNotEqual(
            (flat_dir / "render.png").read_bytes(),
            (self.out_dir / "render.png").read_bytes(),
        )

    def test_layer_list_reordering_does_not_change_pixels(self) -> None:
        reordered = copy.deepcopy(self.scene)
        reordered["layers"].reverse()
        for layer in reordered["layers"]:
            layer["folds"].reverse()
            for fold in layer["folds"]:
                fold["triangles"].reverse()
        reordered["tassels"].reverse()
        reordered["lights"].reverse()
        self.assertEqual(scene_digest(reordered), scene_digest(self.scene))
        second_dir = self.root / "reordered"
        second = render_scene(reordered, second_dir)
        self.assertEqual(second.status, "complete")
        self.assertEqual(
            (second_dir / "render.png").read_bytes(),
            (self.out_dir / "render.png").read_bytes(),
        )
        self.assertEqual(
            (second_dir / "ownership.png").read_bytes(),
            (self.out_dir / "ownership.png").read_bytes(),
        )
        self.assertEqual(
            (second_dir / "fabric_composite.png").read_bytes(),
            (self.out_dir / "fabric_composite.png").read_bytes(),
        )

    def test_projection_recomputes_from_declared_light_and_wall(self) -> None:
        catalogue = self.report["projection"]["points"]
        self.assertGreater(len(catalogue), 0)
        point = next(
            item
            for item in catalogue
            if item["tassel_id"] == "tassel_mid_left"
            and item["light_id"] == "lamp_center"
        )
        self.assertEqual(point["status"], "projected")
        light = next(
            light
            for light in self.scene["lights"]
            if light["light_id"] == "lamp_center"
        )
        wall = self.scene["wall"]
        light_position = np.asarray(light["position"], dtype=np.float64)
        source = np.asarray(point["source_point"], dtype=np.float64)
        normal = np.asarray(wall["normal"], dtype=np.float64)
        origin = np.asarray(wall["origin"], dtype=np.float64)
        direction = source - light_position
        t = np.dot(origin - light_position, normal) / np.dot(direction, normal)
        reconstructed = light_position + t * direction
        self.assertTrue(
            np.allclose(
                reconstructed,
                np.asarray(point["wall_point"], dtype=np.float64),
                atol=1e-6,
            )
        )
        self.assertTrue(point["inside_wall"])

    def test_projection_moves_with_light_and_hanging_height(self) -> None:
        baseline = self.report["projection"]["points"]
        baseline_point = next(
            item
            for item in baseline
            if item["tassel_id"] == "tassel_far_left"
            and item["light_id"] == "lamp_center"
        )

        changed = copy.deepcopy(self.scene)
        for tassel in changed["tassels"]:
            for segment in tassel["segments"]:
                segment["a"][1] -= 0.12
                segment["b"][1] -= 0.12
        validation = validate_scene(changed)
        self.assertTrue(validation.ok, [i.as_dict() for i in validation.issues])
        moved_height = projection_catalogue(validation.data)
        moved_point = next(
            item
            for item in moved_height
            if item["tassel_id"] == "tassel_far_left"
            and item["light_id"] == "lamp_center"
        )
        self.assertNotEqual(
            baseline_point["wall_point"],
            moved_point["wall_point"],
        )
        self.assertLess(
            moved_point["wall_point"][1],
            baseline_point["wall_point"][1],
        )

        moved_light_scene = copy.deepcopy(self.scene)
        for light in moved_light_scene["lights"]:
            if light["light_id"] == "lamp_center":
                light["position"][0] += 0.15
        validation = validate_scene(moved_light_scene)
        self.assertTrue(validation.ok)
        moved_light = projection_catalogue(validation.data)
        moved_light_point = next(
            item
            for item in moved_light
            if item["tassel_id"] == "tassel_far_left"
            and item["light_id"] == "lamp_center"
        )
        self.assertNotEqual(
            baseline_point["wall_point"][0],
            moved_light_point["wall_point"][0],
        )

    def test_soft_shadow_width_grows_with_wall_distance(self) -> None:
        full_scene = generate_scene()
        full_dir = self.root / "full"
        result = render_scene(full_scene, full_dir)
        with open(
            full_dir / "render_report.json", "r", encoding="utf-8"
        ) as handle:
            report = json.load(handle)
        measurements = {
            item["tassel_id"]: item
            for item in report["soft_shadows"]["measurements"]
        }
        ordered = [
            measurements["tassel_near_left"],
            measurements["tassel_mid_left"],
            measurements["tassel_far_left"],
        ]
        self.assertTrue(
            all(item["status"] == "projected" for item in ordered)
        )
        self.assertLess(
            ordered[0]["distance_to_wall"],
            ordered[1]["distance_to_wall"],
        )
        self.assertLess(
            ordered[1]["distance_to_wall"],
            ordered[2]["distance_to_wall"],
        )
        self.assertGreater(
            ordered[0]["transition_width_px"],
            0.0,
        )
        self.assertLess(
            ordered[0]["transition_width_px"],
            ordered[1]["transition_width_px"],
        )
        self.assertLess(
            ordered[1]["transition_width_px"],
            ordered[2]["transition_width_px"],
        )
        for item in ordered:
            self.assertLess(
                item["shadowed_brightness"],
                item["unshadowed_brightness"],
            )
            self.assertGreater(len(item["samples"]), 50)
        self.assertGreater(
            report["soft_shadows"]["soft_shadow_sample_count"],
            0,
        )

    def test_shadow_measurements_are_byte_stable(self) -> None:
        validation = validate_scene(self.scene)
        self.assertTrue(validation.ok)
        camera = Camera.from_data(
            validation.data.camera,
            validation.data.width,
            validation.data.height,
        )
        first = shadow_measurements(validation.data, camera)
        second = shadow_measurements(validation.data, camera)
        self.assertEqual(first, second)
        self.assertGreater(
            validation.data.shadows["samples_per_light"],
            1,
        )
        for measurement in first:
            for sample in measurement["samples"]:
                self.assertTrue(0.0 <= sample["visibility"] <= 1.0)


if __name__ == "__main__":
    unittest.main()

