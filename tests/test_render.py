from __future__ import annotations

import copy
import json
import tempfile
import unittest
from pathlib import Path

import numpy as np

from bamboo_render.engine import run_render
from bamboo_render.sample_scene import make_sample_scene
from bamboo_render.scene import load_scene_data


def small_scene() -> dict:
    return make_sample_scene(
        image_width=128,
        image_height=96,
        chunk_width=64,
        chunk_height=48,
    )


class RenderTests(unittest.TestCase):
    def test_render_outputs_are_stable_and_physically_plausible(self) -> None:
        loaded = load_scene_data(small_scene())
        with tempfile.TemporaryDirectory() as first_dir, tempfile.TemporaryDirectory() as second_dir:
            first = run_render(loaded, first_dir)
            second = run_render(loaded, second_dir)
            self.assertEqual(first.status, "complete")
            self.assertEqual(second.status, "complete")
            first_path = Path(first_dir)
            second_path = Path(second_dir)
            first_rgba = np.load(first_path / "rgba.npy")
            second_rgba = np.load(second_path / "rgba.npy")
            np.testing.assert_array_equal(first_rgba, second_rgba)
            np.testing.assert_array_equal(
                np.load(first_path / "ownership.npy"),
                np.load(second_path / "ownership.npy"),
            )
            np.testing.assert_array_equal(
                np.load(first_path / "layer_premultiplied.npy"),
                np.load(second_path / "layer_premultiplied.npy"),
            )
            self.assertEqual(
                (first_path / "projection_points.json").read_bytes(),
                (second_path / "projection_points.json").read_bytes(),
            )
            self.assertEqual(
                (first_path / "run_report.json").read_bytes(),
                (second_path / "run_report.json").read_bytes(),
            )
            self.assertEqual(
                (first_path / "snapshot.json").read_bytes(),
                (second_path / "snapshot.json").read_bytes(),
            )
            report = json.loads((first_path / "run_report.json").read_text(encoding="utf-8"))
            self.assertEqual(report["layer_order_inner_to_outer"], [1, 2, 3])
            self.assertEqual(report["image_checks"]["pure_white_fraction"], 0.0)
            self.assertLess(report["image_checks"]["max_luma"], 0.9)
            self.assertGreater(report["image_checks"]["mean_luma"], 0.05)
            layer_means = [
                np.asarray(layer["mean_premultiplied_luma"]) for layer in report["layers"]
            ]
            self.assertEqual(len(layer_means), 3)
            for layer in report["layers"]:
                self.assertGreater(layer["visible_fragments"], 0)
                self.assertGreater(layer["transmission_share"], 0.0)
                self.assertGreaterEqual(layer["reflection_share"], 0.0)
                self.assertLess(layer["reflection_share"], 0.05)

    def test_layer_and_triangle_input_order_do_not_change_pixels(self) -> None:
        raw = small_scene()
        loaded = load_scene_data(copy.deepcopy(raw))
        reordered = copy.deepcopy(raw)
        reordered["layers"].reverse()
        for layer in reordered["layers"]:
            pairs = list(zip(layer["mesh"]["triangles"], layer["mesh"]["strand_ids"]))
            pairs.reverse()
            layer["mesh"]["triangles"] = [pair[0] for pair in pairs]
            layer["mesh"]["strand_ids"] = [pair[1] for pair in pairs]
        reordered_loaded = load_scene_data(reordered)
        with tempfile.TemporaryDirectory() as first_dir, tempfile.TemporaryDirectory() as second_dir:
            run_render(loaded, first_dir)
            run_render(reordered_loaded, second_dir)
            np.testing.assert_array_equal(
                np.load(Path(first_dir) / "rgba.npy"),
                np.load(Path(second_dir) / "rgba.npy"),
            )
            np.testing.assert_array_equal(
                np.load(Path(first_dir) / "ownership.npy"),
                np.load(Path(second_dir) / "ownership.npy"),
            )

    def test_projection_edge_widths_follow_air_gaps_in_report(self) -> None:
        loaded = load_scene_data(small_scene())
        with tempfile.TemporaryDirectory() as output_dir:
            run_render(loaded, output_dir)
            report = json.loads(
                (Path(output_dir) / "projection_report.json").read_text(encoding="utf-8")
            )
            pairs = {pair["pair"]: pair for pair in report["pairs"]}
            self.assertGreater(
                pairs["2->3"]["edge_width_world"],
                pairs["1->2"]["edge_width_world"],
            )
            self.assertGreater(
                pairs["1->wall"]["edge_width_world"],
                pairs["2->wall"]["edge_width_world"],
            )
            self.assertGreater(
                pairs["2->wall"]["edge_width_world"],
                pairs["3->wall"]["edge_width_world"],
            )
            for pair in report["pairs"]:
                self.assertEqual(len(pair["profile_samples"]), 5)


if __name__ == "__main__":
    unittest.main()
