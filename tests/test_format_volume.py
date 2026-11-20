from __future__ import annotations

import hashlib
import json
import tempfile
import unittest
from pathlib import Path

import numpy as np

from volrender import (
    E_FORMAT,
    VolRenderError,
    VolumeData,
    read_volume_file,
    write_volume_file,
)


ROOT = Path(__file__).resolve().parents[1]


class FormatAndVolumeTests(unittest.TestCase):
    def test_volume_roundtrip_and_sha256(self) -> None:
        raw = np.arange(4 * 5 * 6, dtype=np.int16).reshape((4, 5, 6))
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / "sample.vol"
            header = write_volume_file(
                path,
                raw,
                spacing=(1.0, 2.0, 3.0),
                window_level=30.0,
                window_width=100.0,
            )
            parsed = read_volume_file(path)
            self.assertEqual(header.shape, (6, 5, 4))
            self.assertEqual(parsed.header.spacing, (1.0, 2.0, 3.0))
            np.testing.assert_array_equal(parsed.raw, raw)
            self.assertEqual(
                parsed.file_sha256,
                hashlib.sha256(path.read_bytes()).hexdigest(),
            )

    def test_corrupt_payload_crc_is_rejected(self) -> None:
        raw = np.zeros((3, 3, 3), dtype=np.int16)
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / "bad.vol"
            write_volume_file(
                path,
                raw,
                window_level=1.0,
                window_width=2.0,
            )
            data = bytearray(path.read_bytes())
            data[-1] ^= 0xFF
            path.write_bytes(data)
            with self.assertRaises(VolRenderError) as raised:
                read_volume_file(path)
            self.assertEqual(raised.exception.code, E_FORMAT)

            header_path = Path(tmp) / "bad_header.vol"
            write_volume_file(
                header_path,
                raw,
                window_level=1.0,
                window_width=2.0,
            )
            header_data = bytearray(header_path.read_bytes())
            header_data[8] ^= 0xFF
            header_path.write_bytes(header_data)
            with self.assertRaises(VolRenderError) as raised:
                read_volume_file(header_path)
            self.assertEqual(raised.exception.code, E_FORMAT)

    def test_window_and_spacing_always_rebuild_mapping(self) -> None:
        raw = np.linspace(-100.0, 200.0, 8 * 9 * 10).reshape((8, 9, 10))
        volume = VolumeData(
            np.rint(raw).astype(np.int16),
            spacing=(1.0, 1.0, 1.0),
            window_level=50.0,
            window_width=200.0,
        )
        old = volume.windowed.copy()
        old_revision = volume.mapping_revision
        old_digest = volume.mapping_digest
        reference = volume.normalization_reference
        self.assertEqual(reference.truth_source, "volume_minmax")
        self.assertEqual(reference.raw_min, float(np.min(raw)))
        self.assertEqual(reference.raw_max, float(np.max(raw)))

        change = volume.set_window(90.0, 40.0)
        self.assertEqual(change.reason, "window")
        self.assertEqual(change.old_revision, old_revision)
        self.assertEqual(change.new_revision, old_revision + 1)
        self.assertGreater(change.max_abs_diff, 0.0)
        self.assertEqual(change.old_digest, old_digest)
        self.assertNotEqual(change.new_digest, old_digest)
        self.assertGreater(np.max(np.abs(volume.windowed - old)), 0.0)
        self.assertGreaterEqual(float(np.min(volume.windowed)), 0.0)
        self.assertLessEqual(float(np.max(volume.windowed)), 1.0)

        before_spacing = volume.windowed.copy()
        spacing_change = volume.set_spacing((2.0, 1.0, 0.5))
        self.assertEqual(spacing_change.reason, "spacing")
        self.assertEqual(spacing_change.new_revision, old_revision + 2)
        self.assertEqual(spacing_change.max_abs_diff, 0.0)
        self.assertEqual(spacing_change.changed_voxels, 0)
        np.testing.assert_array_equal(volume.windowed, before_spacing)
        self.assertEqual(volume.spacing, (2.0, 1.0, 0.5))

    def test_generated_samples_match_manifest(self) -> None:
        manifest = json.loads(
            (ROOT / "samples" / "manifest.json").read_text(encoding="utf-8")
        )
        for filename, expected in manifest["files"].items():
            path = ROOT / "samples" / filename
            self.assertEqual(
                hashlib.sha256(path.read_bytes()).hexdigest(),
                expected["sha256"],
            )
        density = VolumeData.from_file(ROOT / "samples" / "density_64.vol")
        mask = VolumeData.from_file(ROOT / "samples" / "segmentation_64.vol")
        self.assertEqual(density.shape_xyz, (64, 64, 64))
        self.assertEqual(mask.shape_xyz, (64, 64, 64))
        np.testing.assert_array_equal(np.unique(mask.raw), np.array([0, 1, 2, 3, 4]))


if __name__ == "__main__":
    unittest.main()
