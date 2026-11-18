from __future__ import annotations

import struct
import tempfile
import unittest
from pathlib import Path

from wal_archive.errors import ArchiveError
from wal_archive.samplegen import build_segment_bytes
from wal_archive.wal import (
    DEFAULT_SEGMENT_SIZE,
    PAGE_CHECKSUM_INFO,
    WAL_BLOCK_SIZE,
    parse_wal_filename,
    validate_segment,
    wal_filename,
)


class WalFormatTests(unittest.TestCase):
    def test_filename_round_trip(self):
        name = wal_filename(3, 129, DEFAULT_SEGMENT_SIZE)
        parsed = parse_wal_filename(name, DEFAULT_SEGMENT_SIZE)
        self.assertEqual(parsed["timeline"], 3)
        self.assertEqual(parsed["segno"], 129)
        self.assertEqual(parsed["filename"], name)

    def test_filename_errors_are_stable(self):
        with self.assertRaises(ArchiveError) as caught:
            parse_wal_filename("not-a-wal", DEFAULT_SEGMENT_SIZE)
        self.assertEqual(caught.exception.code, "WAL003")
        with self.assertRaises(ArchiveError) as caught:
            parse_wal_filename("000000000000000000000001", DEFAULT_SEGMENT_SIZE)
        self.assertEqual(caught.exception.code, "WAL003")

    def test_complete_truncated_corrupt_and_magic(self):
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / "segment"
            data = build_segment_bytes(1, 1)
            path.write_bytes(data)
            complete = validate_segment(
                path, expected_size=DEFAULT_SEGMENT_SIZE, expected_timeline=1
            )
            self.assertEqual(complete.classification, "complete")
            self.assertTrue(complete.usable)
            self.assertEqual(complete.pages_total, 64)
            self.assertEqual(complete.pages_checked, 64)
            self.assertEqual(complete.pages_ok, 64)

            path.write_bytes(data[: DEFAULT_SEGMENT_SIZE // 2])
            truncated = validate_segment(
                path, expected_size=DEFAULT_SEGMENT_SIZE, expected_timeline=1
            )
            self.assertEqual(truncated.classification, "truncated")
            self.assertFalse(truncated.length_ok)
            self.assertFalse(truncated.usable)

            corrupt = bytearray(data)
            corrupt[80] ^= 0x5A
            path.write_bytes(bytes(corrupt))
            failed = validate_segment(
                path, expected_size=DEFAULT_SEGMENT_SIZE, expected_timeline=1
            )
            self.assertEqual(failed.classification, "checksum_failed")
            self.assertGreater(failed.pages_bad, 0)
            self.assertFalse(failed.usable)

            bad_magic = bytearray(data)
            struct.pack_into("<H", bad_magic, 0, 0x1234)
            path.write_bytes(bytes(bad_magic))
            failed_magic = validate_segment(
                path, expected_size=DEFAULT_SEGMENT_SIZE, expected_timeline=1
            )
            self.assertEqual(failed_magic.classification, "checksum_failed")
            self.assertFalse(failed_magic.header_ok)

    def test_record_crc_fallback(self):
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / "segment"
            data = bytearray(build_segment_bytes(1, 2))
            for page_index in range(len(data) // WAL_BLOCK_SIZE):
                offset = page_index * WAL_BLOCK_SIZE + 2
                info = struct.unpack_from("<H", data, offset)[0]
                struct.pack_into("<H", data, offset, info & ~PAGE_CHECKSUM_INFO)
            path.write_bytes(bytes(data))
            result = validate_segment(
                path, expected_size=DEFAULT_SEGMENT_SIZE, expected_timeline=1
            )
            self.assertEqual(result.classification, "complete")
            self.assertEqual(result.pages_checked, result.pages_total)

    def test_seed_changes_sample_content(self):
        self.assertNotEqual(
            build_segment_bytes(1, 1, seed=1),
            build_segment_bytes(1, 1, seed=2),
        )

if __name__ == "__main__":
    unittest.main()
