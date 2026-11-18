from __future__ import annotations

import unittest

import numpy as np

from paper_lantern.texture import generate_texture


class TextureTests(unittest.TestCase):
    def test_same_parameters_are_byte_identical(self) -> None:
        first = generate_texture(206, 3, 64)
        second = generate_texture(206, 3, 64)
        self.assertEqual(first.raw_bytes(), second.raw_bytes())
        self.assertEqual(first.digest, second.digest)
        self.assertEqual(first.table.dtype, np.dtype(np.float32))
        self.assertEqual(first.table.shape, (64, 64, 3))

    def test_layer_traversal_order_does_not_change_tables(self) -> None:
        layer_resolutions = [(0, 48), (1, 44), (2, 40), (3, 36), (4, 32)]
        forward = {
            layer_index: generate_texture(91, layer_index, resolution).raw_bytes()
            for layer_index, resolution in layer_resolutions
        }
        backward = {
            layer_index: generate_texture(91, layer_index, resolution).raw_bytes()
            for layer_index, resolution in reversed(layer_resolutions)
        }
        self.assertEqual(forward, backward)

    def test_seed_and_layer_change_texture(self) -> None:
        base = generate_texture(7, 0, 48).raw_bytes()
        self.assertNotEqual(base, generate_texture(8, 0, 48).raw_bytes())
        self.assertNotEqual(base, generate_texture(7, 1, 48).raw_bytes())

    def test_cutouts_and_fibers_modulate_paper(self) -> None:
        texture = generate_texture(206, 0, 96)
        paper_mask = texture.paper_mask
        self.assertTrue(np.any(paper_mask == 0.0))
        self.assertTrue(np.any(paper_mask == 1.0))
        self.assertTrue(np.all(texture.transmittance_modulation[paper_mask == 0.0] == 0.0))
        self.assertTrue(np.all(texture.scatter_modulation[paper_mask == 0.0] == 0.0))
        paper_transmission = texture.transmittance_modulation[paper_mask > 0.0]
        paper_scatter = texture.scatter_modulation[paper_mask > 0.0]
        self.assertGreater(float(np.std(paper_transmission)), 0.01)
        self.assertGreater(float(np.std(paper_scatter)), 0.01)


if __name__ == "__main__":
    unittest.main()

