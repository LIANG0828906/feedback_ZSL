from __future__ import annotations

import tempfile
import unittest
from pathlib import Path

import numpy as np

from glass_verify.geometry import build_geometry_context
from glass_verify.sample import make_sample_scene
from glass_verify.scene import validate_scene
from glass_verify.texture import (
    build_texture_tables,
    generate_texture_table,
    save_texture_tables,
)


class TextureTests(unittest.TestCase):
    def test_same_parameters_are_byte_identical(self) -> None:
        scene = validate_scene(make_sample_scene())
        pane = scene.panes[0]
        first = generate_texture_table(scene.texture, pane.sector_id, pane.pane_id)
        second = generate_texture_table(scene.texture, pane.sector_id, pane.pane_id)
        self.assertEqual(first.tobytes(), second.tobytes())
        self.assertEqual(first.dtype, np.float32)

    def test_adjacent_same_colour_tables_differ(self) -> None:
        scene = validate_scene(make_sample_scene())
        blue = [pane for pane in scene.panes if pane.sector_id == 0]
        self.assertGreaterEqual(len(blue), 2)
        first = generate_texture_table(scene.texture, blue[0].sector_id, blue[0].pane_id)
        second = generate_texture_table(scene.texture, blue[1].sector_id, blue[1].pane_id)
        self.assertFalse(np.array_equal(first, second))
        self.assertGreater(float(np.mean(np.abs(first - second))), 0.01)

    def test_tables_do_not_depend_on_input_order(self) -> None:
        raw = make_sample_scene()
        forward = validate_scene(raw)
        reversed_raw = dict(raw)
        reversed_raw["panes"] = list(reversed(raw["panes"]))
        reverse = validate_scene(reversed_raw)
        forward_tables = build_texture_tables(forward)
        reverse_tables = build_texture_tables(reverse)
        self.assertEqual(forward_tables.tobytes(), reverse_tables.tobytes())

    def test_saved_texture_tables_are_stable(self) -> None:
        scene = validate_scene(make_sample_scene())
        context = build_geometry_context(scene)
        with tempfile.TemporaryDirectory() as temporary:
            first = Path(temporary) / "a.npy"
            second = Path(temporary) / "b.npy"
            save_texture_tables(first, context.textures, scene)
            save_texture_tables(second, context.textures, scene)
            self.assertEqual(first.read_bytes(), second.read_bytes())
            loaded = np.load(first, allow_pickle=False)
            self.assertEqual(loaded.dtype, np.float32)
            self.assertEqual(loaded.tobytes(), context.textures.astype(np.float32).tobytes())


if __name__ == "__main__":
    unittest.main()
