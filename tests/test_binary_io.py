import os
import tempfile
import unittest

import numpy as np

from terrain_tool.binary_io import (
    KIND_HEIGHT,
    KIND_MATERIAL,
    HeightField,
    MaterialMap,
    Patch,
    load_heightfield,
    load_material,
    load_patch,
    save_heightfield,
    save_material,
    save_patch,
)
from terrain_tool.errors import ErrorCode, TerrainError


class BinaryRoundTripTest(unittest.TestCase):
    def setUp(self) -> None:
        self.tmp = tempfile.mkdtemp()
        self.elev = np.arange(100, dtype=np.float32).reshape(10, 10) + 0.5
        self.mat = (np.arange(100) % 8).astype(np.uint8).reshape(10, 10)

    def test_heightfield_round_trip(self) -> None:
        path = os.path.join(self.tmp, "a.hgt")
        hf = HeightField(self.elev, 2.0, (1, 1, 8, 8), 3)
        save_heightfield(path, hf)
        loaded = load_heightfield(path)
        self.assertTrue(np.array_equal(loaded.elevations, self.elev))
        self.assertEqual(loaded.cell_size, 2.0)
        self.assertEqual(loaded.valid_rect, (1, 1, 8, 8))
        self.assertEqual(loaded.base_version, 3)

    def test_material_round_trip(self) -> None:
        path = os.path.join(self.tmp, "a.mat")
        save_material(path, MaterialMap(self.mat, 2))
        loaded = load_material(path)
        self.assertTrue(np.array_equal(loaded.indices, self.mat))
        self.assertEqual(loaded.base_version, 2)

    def test_patch_round_trip(self) -> None:
        path = os.path.join(self.tmp, "a.ptc")
        patch = Patch(
            KIND_HEIGHT, (2, 3, 4, 5), 2.0, 7, 1, np.ones((5, 4), np.float32)
        )
        save_patch(path, patch)
        loaded = load_patch(path)
        self.assertEqual(loaded.rect, (2, 3, 4, 5))
        self.assertEqual(loaded.base_version, 7)
        self.assertEqual(loaded.seq, 1)
        self.assertTrue(np.array_equal(loaded.data, np.ones((5, 4), np.float32)))

    def test_non_finite_heightfield_rejected(self) -> None:
        path = os.path.join(self.tmp, "bad.hgt")
        bad = self.elev.copy()
        bad[0, 0] = np.nan
        save_heightfield(path, HeightField(bad, 1.0, (0, 0, 10, 10), 0))
        with self.assertRaises(TerrainError) as ctx:
            load_heightfield(path)
        self.assertEqual(ctx.exception.code, ErrorCode.E_TERRAIN_INVALID)

    def test_truncated_body_rejected(self) -> None:
        path = os.path.join(self.tmp, "trunc.hgt")
        save_heightfield(
            path, HeightField(self.elev, 1.0, (0, 0, 10, 10), 0)
        )
        with open(path, "rb") as fh:
            raw = fh.read()
        with open(path, "wb") as fh:
            fh.write(raw[:-20])
        with self.assertRaises(TerrainError) as ctx:
            load_heightfield(path)
        self.assertEqual(ctx.exception.code, ErrorCode.E_TERRAIN_INVALID)

    def test_bad_magic_rejected(self) -> None:
        path = os.path.join(self.tmp, "magic.hgt")
        save_heightfield(
            path, HeightField(self.elev, 1.0, (0, 0, 10, 10), 0)
        )
        with open(path, "r+b") as fh:
            fh.seek(0)
            fh.write(b"XXXX")
        with self.assertRaises(TerrainError) as ctx:
            load_heightfield(path)
        self.assertEqual(ctx.exception.code, ErrorCode.E_TERRAIN_INVALID)

    def test_non_finite_patch_rejected(self) -> None:
        path = os.path.join(self.tmp, "bad.ptc")
        bad = np.full((3, 3), np.inf, np.float32)
        save_patch(path, Patch(KIND_HEIGHT, (0, 0, 3, 3), 1.0, 0, 1, bad))
        with self.assertRaises(TerrainError) as ctx:
            load_patch(path)
        self.assertEqual(ctx.exception.code, ErrorCode.E_PATCH_INVALID)


if __name__ == "__main__":  # pragma: no cover
    unittest.main()
