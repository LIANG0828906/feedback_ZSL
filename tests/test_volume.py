from __future__ import annotations

import struct
import tempfile
import unittest
from pathlib import Path

import numpy as np

from volrender.errors import ErrorCode, VolumeRenderError
from volrender.volume import compare_window_mappings, load_volume
from tests.helpers import small_uint16_volume


class VolumeHeaderAndWindowTests(unittest.TestCase):
    def test_header_and_mask_round_trip(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            volume, density, mask = small_uint16_volume(Path(directory))
            self.assertEqual(volume.dims, (4, 3, 2))
            self.assertEqual(volume.spacing, (1.0, 1.25, 2.0))
            self.assertEqual(volume.header.window_center, 30.0)
            self.assertEqual(volume.header.window_width, 40.0)
            self.assertEqual(volume.header.scalar_type, "uint16")
            self.assertEqual(volume.header.mask_offset, 128 + density.nbytes)
            self.assertTrue(np.array_equal(volume.density, density))
            self.assertTrue(np.array_equal(volume.mask, mask))
            self.assertEqual(len(volume.content_sha256), 64)
            mapped, reference = volume.normalize()
            self.assertEqual(mapped.shape, (4, 3, 2))
            self.assertEqual(mapped.dtype, np.float32)
            self.assertAlmostEqual(reference.raw_low, 10.0)
            self.assertAlmostEqual(reference.raw_high, 50.0)

    def test_window_delta_reports_all_voxels(self) -> None:
        raw = np.asarray([9, 10, 15, 20, 21], dtype=np.uint16)
        delta, old, new = compare_window_mappings(raw, 15.0, 10.0, 14.0, 8.0)
        self.assertAlmostEqual(float(old[2]), 0.5, places=6)
        self.assertAlmostEqual(float(new[2]), 0.625, places=6)
        self.assertAlmostEqual(delta.max_abs_difference, 0.125, places=6)
        self.assertEqual(delta.affected_voxels, 1)
        self.assertEqual(delta.total_voxels, 5)

    def test_density_length_conflict_is_header_error(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "tiny.vol"
            small_uint16_volume(Path(directory))
            payload = bytearray(path.read_bytes())
            struct.pack_into("<Q", payload, 56, 12)
            path.write_bytes(payload)
            with self.assertRaises(VolumeRenderError) as raised:
                load_volume(path)
            self.assertEqual(raised.exception.code, ErrorCode.E_HEADER_INVALID)
            self.assertEqual(raised.exception.field, "density_bytes")

    def test_invalid_spacing_is_header_error(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "tiny.vol"
            small_uint16_volume(Path(directory))
            payload = bytearray(path.read_bytes())
            struct.pack_into("<f", payload, 28, 0.0)
            path.write_bytes(payload)
            with self.assertRaises(VolumeRenderError) as raised:
                load_volume(path)
            self.assertEqual(raised.exception.code, ErrorCode.E_HEADER_INVALID)
            self.assertEqual(raised.exception.field, "spacing_x")

    def test_truncated_density_is_header_error(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "density-only.vol"
            from tests.helpers import make_volume

            make_volume(Path(directory), size=8)
            source = Path(directory) / "test_8.vol"
            payload = bytearray(source.read_bytes())
            struct.pack_into("<H", payload, 14, 0)
            struct.pack_into("<Q", payload, 64, 0)
            struct.pack_into("<Q", payload, 72, 0)
            struct.pack_into("<III", payload, 80, 0, 0, 0)
            struct.pack_into("<Q", payload, 92, 8 * 8 * 8)
            # Recompute the declared density byte count for a mask-free file.
            density_bytes = 8 * 8 * 8 * 2
            struct.pack_into("<Q", payload, 56, density_bytes)
            no_mask = payload[: 128 + density_bytes]
            path.write_bytes(no_mask)
            path.write_bytes(path.read_bytes()[:-5])
            with self.assertRaises(VolumeRenderError) as raised:
                load_volume(path)
            self.assertEqual(raised.exception.code, ErrorCode.E_HEADER_INVALID)
            self.assertIn(raised.exception.field, {"file_size", "density_bytes"})

    def test_mask_dimension_conflict_is_mask_error(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "tiny.vol"
            small_uint16_volume(Path(directory))
            payload = bytearray(path.read_bytes())
            struct.pack_into("<I", payload, 80, 2)
            path.write_bytes(payload)
            with self.assertRaises(VolumeRenderError) as raised:
                load_volume(path)
            self.assertEqual(raised.exception.code, ErrorCode.E_MASK_INVALID)
            self.assertEqual(raised.exception.field, "mask_dims")

    def test_mask_offset_conflict_is_mask_error(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "tiny.vol"
            volume, _, _ = small_uint16_volume(Path(directory))
            payload = bytearray(path.read_bytes())
            struct.pack_into("<Q", payload, 64, volume.header.density_offset + 1)
            path.write_bytes(payload)
            with self.assertRaises(VolumeRenderError) as raised:
                load_volume(path)
            self.assertEqual(raised.exception.code, ErrorCode.E_MASK_INVALID)
            self.assertEqual(raised.exception.field, "mask_offset")

    def test_unknown_header_flag_is_header_error(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "tiny.vol"
            small_uint16_volume(Path(directory))
            payload = bytearray(path.read_bytes())
            struct.pack_into("<H", payload, 14, 3)
            path.write_bytes(payload)
            with self.assertRaises(VolumeRenderError) as raised:
                load_volume(path)
            self.assertEqual(raised.exception.code, ErrorCode.E_HEADER_INVALID)
            self.assertEqual(raised.exception.field, "flags")


if __name__ == "__main__":
    unittest.main()
