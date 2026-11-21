from __future__ import annotations

import struct
import tempfile
import unittest
from pathlib import Path

import numpy as np

from pcwb.errors import PointCloudInvalid
from pcwb.format import (
    HEADER_SIZE,
    HEADER_STRUCT,
    MAGIC,
    POINT_SIZE,
    POINT_STRUCT,
    parse_point_cloud,
    read_point_cloud,
    serialize_point_cloud,
    write_point_cloud,
)
from pcwb.model import Bounds, PointCloud
from tests.helpers import sample_cloud


class FormatTests(unittest.TestCase):
    def test_round_trip_is_byte_exact(self) -> None:
        cloud = sample_cloud(7)
        payload = serialize_point_cloud(cloud)
        self.assertEqual(len(payload), HEADER_SIZE + 7 * POINT_SIZE)
        parsed = parse_point_cloud(payload)
        self.assertEqual(serialize_point_cloud(parsed), payload)
        self.assertEqual(parsed.content_hash, cloud.content_hash)

    def test_write_is_atomic_and_hash_matches(self) -> None:
        cloud = sample_cloud(5)
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "cloud.pcwb"
            digest = write_point_cloud(path, cloud)
            self.assertEqual(digest, cloud.content_hash)
            self.assertEqual(read_point_cloud(path).content_hash, digest)
            self.assertFalse(list(Path(directory).glob("*.tmp")))

    def test_truncated_file_reports_expected_length(self) -> None:
        payload = bytearray(serialize_point_cloud(sample_cloud(3)))
        payload = payload[:-1]
        with self.assertRaises(PointCloudInvalid) as caught:
            parse_point_cloud(payload)
        self.assertEqual(caught.exception.code, "E_PC_INVALID")
        self.assertEqual(
            caught.exception.details["expected_length"], HEADER_SIZE + 3 * POINT_SIZE
        )

    def test_declared_count_mismatch_is_invalid(self) -> None:
        payload = bytearray(serialize_point_cloud(sample_cloud(3)))
        struct.pack_into("<Q", payload, 8, 4)
        with self.assertRaises(PointCloudInvalid) as caught:
            parse_point_cloud(payload)
        self.assertEqual(caught.exception.code, "E_PC_INVALID")
        self.assertEqual(caught.exception.details["point_count"], 4)

    def test_first_out_of_bounds_point_index_is_reported(self) -> None:
        bounds = Bounds((-1.0, -1.0, -1.0), (1.0, 1.0, 1.0))
        header = HEADER_STRUCT.pack(MAGIC, 3, *bounds.minimum, *bounds.maximum)
        records = b"".join(
            [
                POINT_STRUCT.pack(0.0, 0.0, 0.0, 1, 2, 3),
                POINT_STRUCT.pack(1.5, 0.0, 0.0, 4, 5, 6),
                POINT_STRUCT.pack(2.0, 0.0, 0.0, 7, 8, 9),
            ]
        )
        with self.assertRaises(PointCloudInvalid) as caught:
            parse_point_cloud(header + records)
        self.assertEqual(caught.exception.code, "E_PC_INVALID")
        self.assertEqual(caught.exception.point_index, 1)

    def test_non_finite_point_index_is_reported(self) -> None:
        bounds = Bounds((-1.0, -1.0, -1.0), (1.0, 1.0, 1.0))
        header = HEADER_STRUCT.pack(MAGIC, 2, *bounds.minimum, *bounds.maximum)
        records = POINT_STRUCT.pack(0.0, 0.0, 0.0, 0, 0, 0) + POINT_STRUCT.pack(
            float("nan"), 0.0, 0.0, 0, 0, 0
        )
        with self.assertRaises(PointCloudInvalid) as caught:
            parse_point_cloud(header + records)
        self.assertEqual(caught.exception.point_index, 1)

    def test_invalid_magic_and_bbox(self) -> None:
        payload = bytearray(serialize_point_cloud(sample_cloud(1)))
        payload[:8] = b"NOTPCWB!"
        with self.assertRaises(PointCloudInvalid):
            parse_point_cloud(payload)

        header = HEADER_STRUCT.pack(MAGIC, 0, 2, 0, 0, 1, 1, 1)
        with self.assertRaises(PointCloudInvalid):
            parse_point_cloud(header)

    def test_point_cloud_rejects_outside_bounds(self) -> None:
        bounds = Bounds((0.0, 0.0, 0.0), (1.0, 1.0, 1.0))
        xyz = np.asarray([[2.0, 0.0, 0.0]], dtype=np.float32)
        rgb = np.asarray([[1, 2, 3]], dtype=np.uint8)
        with self.assertRaises(PointCloudInvalid) as caught:
            PointCloud(xyz, rgb, bounds)
        self.assertEqual(caught.exception.point_index, 0)


if __name__ == "__main__":
    unittest.main()
