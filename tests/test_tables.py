from __future__ import annotations

import unittest

import numpy as np

from canopy_render.scene import canonical_scene, generate_scene, scene_digest
from canopy_render.tables import (
    brocade_pattern,
    material_tables,
    sample_tables,
)
from canopy_render.util import canonical_json_bytes


class TableTests(unittest.TestCase):
    def test_scene_generation_is_byte_deterministic(self) -> None:
        first = generate_scene(80, 60, 32, 20)
        second = generate_scene(80, 60, 32, 20)
        self.assertEqual(
            canonical_json_bytes(first),
            canonical_json_bytes(second),
        )
        self.assertEqual(scene_digest(first), scene_digest(second))

    def test_material_tables_are_seeded_and_reproducible(self) -> None:
        first = material_tables(1103)
        second = material_tables(1103)
        other = material_tables(1104)
        self.assertTrue(np.array_equal(first.pattern, second.pattern))
        self.assertTrue(np.array_equal(first.normal_u, second.normal_u))
        self.assertTrue(np.array_equal(first.normal_v, second.normal_v))
        self.assertFalse(np.array_equal(first.pattern, other.pattern))

        u = np.linspace(0.0, 1.0, 33)
        v = np.linspace(0.0, 1.0, 33)
        p1, du1, dv1 = sample_tables(first, u, v)
        p2, du2, dv2 = sample_tables(second, u, v)
        self.assertTrue(np.array_equal(p1, p2))
        self.assertTrue(np.array_equal(du1, du2))
        self.assertTrue(np.array_equal(dv1, dv2))
        weave1 = brocade_pattern(p1, u, v, 1103)
        weave2 = brocade_pattern(p2, u, v, 1103)
        self.assertTrue(np.array_equal(weave1, weave2))
        self.assertGreater(float(np.std(weave1)), 0.05)

    def test_digest_is_independent_of_semantically_reordered_lists(self) -> None:
        scene = generate_scene(64, 48, 32, 24)
        digest = scene_digest(scene)
        shuffled = canonical_scene(scene)
        shuffled["layers"].reverse()
        for layer in shuffled["layers"]:
            layer["folds"].reverse()
            for fold in layer["folds"]:
                fold["triangles"].reverse()
        shuffled["tassels"].reverse()
        shuffled["lights"].reverse()
        self.assertEqual(scene_digest(shuffled), digest)


if __name__ == "__main__":
    unittest.main()

