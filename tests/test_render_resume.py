import json
import os
import shutil
import tempfile
import unittest

import numpy as np
from PIL import Image

from paperlightbox.cli import main
from paperlightbox.pipeline import (render_scene, shadow_probes)
from paperlightbox.raster import render_tile
from paperlightbox.report import build_report, compare_outputs
from paperlightbox.sample_gen import build_gap_variant, build_sample_scene
from paperlightbox.scene import load_scene, ordered_layers, validate_scene
from paperlightbox.snapshot import SnapshotStore


class RenderResumeTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.mkdtemp(prefix="plb_")
        self.scene_path = os.path.join(self.tmp, "scene.json")
        self.scene = build_sample_scene(width=128, height=128, tile=32,
                                        sample_density=9)
        with open(self.scene_path, "w", encoding="utf-8") as handle:
            json.dump(self.scene, handle)
        validate_scene(self.scene)

    def tearDown(self):
        shutil.rmtree(self.tmp, ignore_errors=True)

    def _paths(self, name):
        return (os.path.join(self.tmp, name, "out"),
                os.path.join(self.tmp, name, "snap"))

    def test_full_render_is_deterministic_and_in_range(self):
        out_a, snap_a = self._paths("a")
        summary_a = render_scene(self.scene, out_a, snap_a)
        png_a = np.asarray(Image.open(os.path.join(out_a, "lightbox.png"))
                           .convert("RGBA"), dtype=np.uint8)

        scene_b = build_sample_scene(width=128, height=128, tile=32,
                                     sample_density=9)
        out_b, snap_b = self._paths("b")
        summary_b = render_scene(scene_b, out_b, snap_b)
        png_b = np.asarray(Image.open(os.path.join(out_b, "lightbox.png"))
                           .convert("RGBA"), dtype=np.uint8)

        self.assertTrue(np.array_equal(png_a, png_b))
        self.assertTrue(np.array_equal(
            np.load(os.path.join(out_a, "optical_depth.npy")),
            np.load(os.path.join(out_b, "optical_depth.npy"))))
        self.assertEqual(summary_a["recovery_count"], 0)
        self.assertLess(int(np.max(png_a[..., :3])), 255)

    def test_resume_matches_uninterrupted_pixel_for_pixel(self):
        ref_out, ref_snap = self._paths("ref")
        render_scene(self.scene, ref_out, ref_snap)
        ref_png = os.path.join(ref_out, "lightbox.png")
        ref_depth = os.path.join(ref_out, "optical_depth.npy")

        stop_file = os.path.join(self.tmp, "stop")
        with open(stop_file, "w", encoding="utf-8") as handle:
            handle.write("stop\n")
        res_out, res_snap = self._paths("res")
        interrupted = render_scene(self.scene, res_out, res_snap,
                                   stop_file=stop_file)
        self.assertTrue(interrupted["stopped_gracefully"])
        self.assertLess(len(interrupted["completed"]),
                         interrupted["total_tiles"])
        self.assertTrue(os.path.exists(
            os.path.join(res_out, "recovery_report.json")))

        os.remove(stop_file)
        resumed = render_scene(self.scene, res_out, res_snap)
        self.assertEqual(resumed["recovery_count"], 1)

        result = compare_outputs(
            ref_png, os.path.join(res_out, "lightbox.png"),
            ref_depth, os.path.join(res_out, "optical_depth.npy"),
            32, 32)
        self.assertEqual(result["status"], "identical")
        self.assertEqual(result["pixel_mismatch_count"], 0)
        self.assertEqual(result["depth_mismatch_count"], 0)

    def test_corrupted_snapshot_is_discarded_and_reruns_clean(self):
        out, snap = self._paths("corrupt")
        store = SnapshotStore(snap, self.scene)
        store.load()
        tile = render_tile(self.scene, 0, 0, 32, 32)
        store.commit_tile(0, tile["rgba"], tile["optical_depth"],
                          tile["layer_stats"])
        # Corrupt the manifest after a valid commit.
        manifest = os.path.join(snap, "manifest.json")
        with open(manifest, "w", encoding="utf-8") as handle:
            handle.write("{not-json")
        fresh_store = SnapshotStore(snap, self.scene)
        self.assertEqual(fresh_store.load(), set())
        self.assertFalse(os.path.exists(manifest))

        summary = render_scene(self.scene, out, snap)
        self.assertEqual(summary["recovery_count"], 0)
        self.assertEqual(len(summary["completed"]), summary["total_tiles"])

    def test_scene_digest_mismatch_discards_old_snapshot(self):
        out, snap = self._paths("digest")
        render_scene(self.scene, out, snap)
        changed = build_gap_variant(self.scene, 1.5)
        new_store = SnapshotStore(snap, changed)
        self.assertEqual(new_store.load(), set())

    def test_render_is_independent_of_input_layer_order(self):
        reordered = {**self.scene,
                     "layers": list(reversed(self.scene["layers"]))}
        out_a, snap_a = self._paths("order_a")
        out_b, snap_b = self._paths("order_b")
        render_scene(self.scene, out_a, snap_a)
        render_scene(reordered, out_b, snap_b)
        png_a = np.asarray(Image.open(os.path.join(out_a, "lightbox.png"))
                           .convert("RGBA"), dtype=np.uint8)
        png_b = np.asarray(Image.open(os.path.join(out_b, "lightbox.png"))
                           .convert("RGBA"), dtype=np.uint8)
        self.assertTrue(np.array_equal(png_a, png_b))

    def test_air_gap_shifts_projection_and_widens_penumbra(self):
        probes_base = shadow_probes(self.scene)
        wide = build_gap_variant(self.scene, 2.0)
        probes_wide = shadow_probes(wide)
        by_layer_base = {p["layer"]: p for p in probes_base}
        by_layer_wide = {p["layer"]: p for p in probes_wide}
        measurable = 0
        for layer in by_layer_base:
            base = by_layer_base[layer]
            wide_probe = by_layer_wide[layer]
            if base["penumbra_width_world"] > 0:
                measurable += 1
                self.assertGreaterEqual(
                    wide_probe["penumbra_width_world"],
                    base["penumbra_width_world"])
            # Projection center moves when the geometric spacing changes.
            self.assertNotEqual(base["projected_center"],
                                wide_probe["projected_center"])
        self.assertGreaterEqual(measurable, 3)

    def test_paper_edges_have_no_hard_black_rim(self):
        out, snap = self._paths("rim")
        render_scene(self.scene, out, snap)
        png = np.asarray(Image.open(os.path.join(out, "lightbox.png"))
                         .convert("RGBA"), dtype=np.float64) / 255.0
        # Background is near-black; paper pixels must not collapse to pure
        # black at cutout edges (ambient floor keeps them above zero).
        paper = png[png[..., 3] > 0.99]
        self.assertGreater(float(paper[..., :3].min()), 0.0)

    def test_report_fields_are_stable_and_recomputable(self):
        out, snap = self._paths("report")
        summary = render_scene(self.scene, out, snap)
        report = build_report(self.scene, summary)
        again = build_report(self.scene, summary)
        self.assertEqual(
            json.dumps(report, sort_keys=True),
            json.dumps(again, sort_keys=True))
        self.assertNotIn("time", json.dumps(report).lower())
        for layer in report["layers"]:
            self.assertGreater(layer["visible_fragments"], 0)
            self.assertGreater(layer["mean_transmittance"], 0.0)
        self.assertEqual(len(report["tiles"]["completed"]),
                         report["tiles"]["total"])

    def test_cli_nonzero_exit_on_invalid_scene(self):
        bad = os.path.join(self.tmp, "bad.json")
        invalid = build_sample_scene()
        invalid["layers"][0]["transmittance"] = 2.0
        with open(bad, "w", encoding="utf-8") as handle:
            json.dump(invalid, handle)
        error_path = os.path.join(self.tmp, "error.json")
        code = main(["render", "--scene", bad,
                     "--output", os.path.join(self.tmp, "noout"),
                     "--snapshot", os.path.join(self.tmp, "nosnap"),
                     "--error-json", error_path])
        self.assertNotEqual(code, 0)
        with open(error_path, encoding="utf-8") as handle:
            payload = json.load(handle)
        self.assertEqual(payload["error_code"], 4)
        self.assertEqual(payload["layer"], 0)
        self.assertFalse(os.path.exists(
            os.path.join(self.tmp, "noout", "lightbox.png")))


if __name__ == "__main__":
    unittest.main()
