import json
import os
import shutil
import tempfile
import unittest

import numpy as np

from kiln_glass.band_report import band_trend_samples, verify_monotonic_trend
from kiln_glass.compare import compare_runs
from kiln_glass.geometry import grout_landing_points, mesh_owner_consistency, pixel_rays, primary_visibility
from kiln_glass.io_utils import read_ownership, read_pam
from kiln_glass.render import Renderer
from kiln_glass.scene import build_default_scene, parse_scene


def _small_scene():
    description = build_default_scene()
    cfg = description["render_config"]
    cfg.update(
        {
            "image_width": 48,
            "image_height": 36,
            "tile_width": 24,
            "tile_height": 18,
            "sample_density": 8,
            "bubble_count": 4,
        }
    )
    return parse_scene(description)


class RenderPipelineTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.mkdtemp(prefix="kiln_test_")
        self.scene = _small_scene()

    def tearDown(self):
        shutil.rmtree(self.tmp, ignore_errors=True)

    def output(self, name):
        path = os.path.join(self.tmp, name)
        return path

    def test_full_run_writes_all_views_and_maps(self):
        renderer = Renderer(self.scene, self.output("full"))
        report = renderer.run()
        self.assertEqual(len(report["views"]), len(self.scene.view_angles))
        for view in range(len(self.scene.view_angles)):
            image = read_pam(os.path.join(self.output("full"),
                                          f"view_{view:03d}", "image.pam"))
            ownership = read_ownership(
                os.path.join(self.output("full"),
                             f"view_{view:03d}", "ownership.kgown")
            )
            self.assertEqual(image.dtype, np.uint8)
            self.assertEqual(image.shape, (36, 48, 4))
            self.assertEqual(ownership.shape, (36, 48))
            self.assertTrue(np.all(image <= 255) and np.all(image >= 0))
        self.assertFalse(
            os.path.exists(os.path.join(self.output("full"), "progress_snapshot.json"))
        )

    def test_ownership_is_recomputable_from_scene(self):
        out = self.output("own")
        Renderer(self.scene, out).run()
        cfg = self.scene.config
        for view, yaw in enumerate(self.scene.view_angles):
            origins, directions = pixel_rays(
                self.scene.camera, yaw, cfg.image_width, cfg.image_height
            )
            owner_index, _, _ = primary_visibility(
                origins.reshape(-1, 3), directions.reshape(-1, 3),
                self.scene.bricks,
            )
            recomputed = np.full(owner_index.shape, -1, dtype=np.int32)
            valid = owner_index >= 0
            recomputed[valid] = np.array(
                [self.scene.bricks[i].brick_id for i in owner_index[valid]],
                dtype=np.int32,
            )
            stored = read_ownership(
                os.path.join(out, f"view_{view:03d}", "ownership.kgown")
            ).reshape(-1)
            np.testing.assert_array_equal(stored, recomputed)
            self.assertTrue(
                mesh_owner_consistency(
                    origins.reshape(-1, 3), directions.reshape(-1, 3),
                    self.scene.bricks, limit=64,
                )
            )

    def test_repeated_runs_byte_identical(self):
        first = self.output("first")
        second = self.output("second")
        Renderer(self.scene, first).run()
        Renderer(self.scene, second).run()
        differences = compare_runs(first, second)
        self.assertEqual(differences, [])
        for view in range(len(self.scene.view_angles)):
            a = read_pam(os.path.join(first, f"view_{view:03d}", "image.pam"))
            b = read_pam(os.path.join(second, f"view_{view:03d}", "image.pam"))
            np.testing.assert_array_equal(a, b)

    def test_views_share_geometry_but_change_with_camera(self):
        out = self.output("camera")
        Renderer(self.scene, out).run()
        images = [
            read_pam(os.path.join(out, f"view_{v:03d}", "image.pam"))
            for v in range(len(self.scene.view_angles))
        ]
        self.assertFalse(np.array_equal(images[0], images[-1]))
        # Wall material ids are identical across views.
        owners = [
            read_ownership(os.path.join(out, f"view_{v:03d}", "ownership.kgown"))
            for v in range(len(self.scene.view_angles))
        ]
        all_ids = set()
        for owner in owners:
            all_ids.update(owner[owner >= 0].tolist())
        self.assertTrue(all_ids.issubset({b.brick_id for b in self.scene.bricks}))

    def test_grout_landings_differ_per_light_and_clip_gracefully(self):
        landings = grout_landing_points(self.scene, self.scene.receiver)
        per_light = {}
        for record in landings:
            per_light.setdefault(record["light_id"], []).append(record)
        self.assertEqual(len(per_light), len(self.scene.lights))
        points_a = [r["landing"] for r in per_light[0]]
        points_b = [r["landing"] for r in per_light[1]]
        self.assertNotEqual(points_a, points_b)
        self.assertTrue(all(r["in_bounds"] in (True, False) for r in landings))

    def test_out_of_bounds_landing_is_flagged_without_hard_edge(self):
        narrow = parse_scene(
            {
                **self.scene.raw,
                "receiver_plane": {
                    "z": self.scene.receiver.z,
                    "x_min": -0.05,
                    "x_max": 0.05,
                    "y_min": -0.05,
                    "y_max": 0.05,
                },
            }
        )
        landings = grout_landing_points(narrow, narrow.receiver)
        self.assertTrue(any(not item["in_bounds"] for item in landings))
        out = self.output("narrow")
        Renderer(narrow, out).run()
        for view in range(len(narrow.view_angles)):
            image = read_pam(os.path.join(out, f"view_{view:03d}", "image.pam"))
            # No hard black edge pixels: minimum stays at the background floor.
            self.assertGreaterEqual(int(image[..., :3].min()), 2)
            self.assertNotIn(0, set(image[..., :3].reshape(-1).tolist()))

    def test_penumbra_monotonic_and_distribution_deterministic(self):
        records = band_trend_samples(self.scene)
        trend = verify_monotonic_trend(records)
        self.assertTrue(trend["monotonic"], trend["violations"])
        again = band_trend_samples(self.scene)
        self.assertEqual(
            json.dumps(records, sort_keys=True),
            json.dumps(again, sort_keys=True),
        )

    def test_channels_never_overflow_and_no_hard_white(self):
        out = self.output("levels")
        Renderer(self.scene, out).run()
        for view in range(len(self.scene.view_angles)):
            image = read_pam(os.path.join(out, f"view_{view:03d}", "image.pam"))
            self.assertEqual(image.dtype, np.uint8)
            # No blown-out pure white fragments on directly lit bricks.
            self.assertLess(image[..., :3].max(), 245)

    def test_no_flicker_speckles_between_repeated_renders(self):
        out_a = self.output("flicker_a")
        out_b = self.output("flicker_b")
        Renderer(self.scene, out_a).run()
        Renderer(self.scene, out_b).run()
        for view in range(len(self.scene.view_angles)):
            a = read_pam(os.path.join(out_a, f"view_{view:03d}", "image.pam"))
            b = read_pam(os.path.join(out_b, f"view_{view:03d}", "image.pam"))
            np.testing.assert_array_equal(a, b)
            # Isolated single-pixel extremes would indicate sub-pixel flicker.
            luminance = a[..., :3].mean(axis=2)
            central = luminance[1:-1, 1:-1]
            neighbour_max = np.maximum.reduce(
                [
                    luminance[:-2, 1:-1],
                    luminance[2:, 1:-1],
                    luminance[1:-1, :-2],
                    luminance[1:-1, 2:],
                ]
            )
            isolated_bright = np.count_nonzero(
                (central > neighbour_max + 60) & (central > 180)
            )
            self.assertEqual(isolated_bright, 0)

    def test_landing_shifts_with_thickness_and_layer_distance(self):
        baseline = grout_landing_points(self.scene, self.scene.receiver)
        shifted_raw = json.loads(json.dumps(self.scene.raw))
        for layer in shifted_raw["wall"]["layers"]:
            layer["plane_z"] += 0.08
        for brick in shifted_raw["wall"]["bricks"]:
            brick["origin"][2] += 0.08
            for vertex in brick["vertices"]:
                vertex[2] += 0.08
        shifted = parse_scene(shifted_raw)
        shifted_points = grout_landing_points(shifted, shifted.receiver)
        self.assertEqual(len(baseline), len(shifted_points))
        moved = 0
        for base, other in zip(baseline, shifted_points):
            self.assertEqual(base["light_id"], other["light_id"])
            if base["landing"] and other["landing"]:
                if any(
                    abs(x - y) > 1e-9
                    for x, y in zip(base["landing"], other["landing"])
                ):
                    moved += 1
        self.assertGreater(moved, 0)


if __name__ == "__main__":
    unittest.main()
