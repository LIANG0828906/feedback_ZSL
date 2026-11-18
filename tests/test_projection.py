from __future__ import annotations

import unittest

import numpy as np

from shadowbox.procedural import generate_textures
from shadowbox.render import build_penumbra_probes, prepare_scene
from shadowbox.scene import validate_scene

from tests.helpers import small_scene


def _visibility_centroid(projection) -> tuple[float, float]:
    visibility = projection.visibility.astype(np.float64)
    ys, xs = np.indices(visibility.shape)
    total = visibility.sum()
    return (
        float((xs * visibility).sum() / total),
        float((ys * visibility).sum() / total),
    )


class ProjectionTests(unittest.TestCase):
    def test_projection_and_half_penumbra_are_deterministic(self) -> None:
        scene = validate_scene(small_scene())
        textures = generate_textures(scene)
        first = prepare_scene(scene, textures)
        second = prepare_scene(scene, textures)
        for layer_index in range(1, 5):
            left = first.projections[layer_index]
            right = second.projections[layer_index]
            self.assertTrue(np.array_equal(left.visibility, right.visibility))
            self.assertEqual(left.sample_count, right.sample_count)

    def test_air_gap_moves_projection_and_widens_penumbra(self) -> None:
        base_scene = validate_scene(small_scene())
        base_prepared = prepare_scene(base_scene, generate_textures(base_scene))
        wider = small_scene()
        wider["layers"][1]["air_gap_to_prev_mm"] = 12.0
        wider_scene = validate_scene(wider)
        wider_prepared = prepare_scene(wider_scene, generate_textures(wider_scene))

        base_projection = base_prepared.projections[1]
        wider_projection = wider_prepared.projections[1]
        self.assertNotEqual(
            base_projection.visibility.tobytes(),
            wider_projection.visibility.tobytes(),
        )
        base_centroid = _visibility_centroid(base_projection)
        wider_centroid = _visibility_centroid(wider_projection)
        self.assertGreater(
            abs(base_centroid[0] - wider_centroid[0])
            + abs(base_centroid[1] - wider_centroid[1]),
            0.05,
        )
        self.assertGreater(
            wider_projection.penumbra_target_mm,
            base_projection.penumbra_target_mm,
        )

        probes = build_penumbra_probes(
            base_scene,
            base_prepared.textures,
            point_count=241,
            span_mm=60.0,
        )
        widths = [probe["half_penumbra_width_mm"] for probe in probes]
        self.assertEqual(len(widths), 4)
        self.assertTrue(all(a < b for a, b in zip(widths, widths[1:])), widths)
        for probe in probes:
            self.assertEqual(len(probe["hole_visibility"]), 9)
            self.assertLessEqual(min(probe["hole_visibility"]), 0.1)
            self.assertGreaterEqual(max(probe["hole_visibility"]), 0.9)

    def test_projection_outside_paper_is_unoccluded(self) -> None:
        scene = validate_scene(small_scene())
        prepared = prepare_scene(scene, generate_textures(scene))
        projection = prepared.projections[1]
        values = projection.lookup(
            np.asarray([-0.2, 1.2]),
            np.asarray([0.5, 0.5]),
        )
        self.assertTrue(np.array_equal(values, np.asarray([1.0, 1.0])))


if __name__ == "__main__":
    unittest.main()
