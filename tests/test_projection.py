from __future__ import annotations

import unittest

import numpy as np

from bamboo_render.io_utils import canonical_json_bytes
from bamboo_render.procedural import generate_textures
from bamboo_render.projection import (
    build_projection_report,
    layer_transmission,
    projection_offsets,
)
from bamboo_render.sample_scene import make_sample_scene
from bamboo_render.scene import load_scene_data


class ProjectionTests(unittest.TestCase):
    def _scene(self, gaps: tuple[float, float, float]):
        raw = make_sample_scene(
            gaps=gaps,
            image_width=64,
            image_height=48,
            chunk_width=32,
            chunk_height=24,
        )
        return load_scene_data(raw).spec

    def test_gap_increases_penumbra_and_shifts_landing_point(self) -> None:
        small = self._scene((0.0, 0.12, 0.15))
        large = self._scene((0.0, 0.30, 0.38))
        small_report, _ = build_projection_report(
            small,
            generate_textures(small),
            small.camera,
        )
        large_report, _ = build_projection_report(
            large,
            generate_textures(large),
            large.camera,
        )
        small_pairs = {pair["pair"]: pair for pair in small_report["pairs"]}
        large_pairs = {pair["pair"]: pair for pair in large_report["pairs"]}
        for pair_id in ("1->2", "2->3"):
            self.assertGreater(
                large_pairs[pair_id]["edge_width_world"],
                small_pairs[pair_id]["edge_width_world"],
            )
            shift = np.linalg.norm(
                np.asarray(large_pairs[pair_id]["target_world"])
                - np.asarray(small_pairs[pair_id]["target_world"])
            )
            self.assertGreater(shift, 1e-3)

    def test_wall_penumbra_grows_as_source_is_farther(self) -> None:
        scene = self._scene((0.0, 0.22, 0.26))
        report, _ = build_projection_report(
            scene,
            generate_textures(scene),
            scene.camera,
        )
        widths = {
            pair["pair"]: pair["edge_width_world"]
            for pair in report["pairs"]
            if pair["target"] == "inner_wall"
        }
        self.assertGreater(widths["1->wall"], widths["2->wall"])
        self.assertGreater(widths["2->wall"], widths["3->wall"])

    def test_projection_outside_source_geometry_is_unoccluded(self) -> None:
        scene = self._scene((0.0, 0.22, 0.26))
        textures = generate_textures(scene)
        source = scene.layers[0]
        point = np.array([[10.0, 10.0, source.plane_z]], dtype=np.float64)
        transmission, uv = layer_transmission(
            source,
            textures.layers[source.layer],
            point,
            scene.light,
            scene.shadow,
            projection_offsets(scene.shadow.sampling_density),
        )
        self.assertEqual(float(transmission[0]), 1.0)
        self.assertTrue(np.any(uv < 0.0) or np.any(uv > 1.0))

    def test_same_sampling_density_is_byte_stable(self) -> None:
        scene = self._scene((0.0, 0.22, 0.26))
        textures = generate_textures(scene)
        first, _ = build_projection_report(scene, textures, scene.camera)
        second, _ = build_projection_report(scene, textures, scene.camera)
        self.assertEqual(canonical_json_bytes(first), canonical_json_bytes(second))


if __name__ == "__main__":
    unittest.main()
