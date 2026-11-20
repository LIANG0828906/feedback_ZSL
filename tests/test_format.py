import os
import struct
import tempfile
import unittest

import numpy as np

from pcwb.errors import E_POINTCLOUD_INVALID, PointCloudError
from pcwb.format import (
    HEADER_SIZE, MAGIC, POINT_RECORD_SIZE, compute_bbox, encode_cloud,
    parse_cloud, read_cloud, write_cloud,
)
from pcwb.samples import generate_cloud


class FormatTests(unittest.TestCase):
    def setUp(self):
        self.xyz, self.rgb = generate_cloud(7, 300)

    def test_header_layout_and_length(self):
        blob = encode_cloud(self.xyz, self.rgb)
        magic, version, header_size, count = struct.unpack_from(
            "<4sHHI", blob, 0)
        self.assertEqual(magic, MAGIC)
        self.assertEqual(version, 1)
        self.assertEqual(header_size, HEADER_SIZE)
        self.assertEqual(count, self.xyz.shape[0])
        self.assertEqual(
            len(blob),
            HEADER_SIZE + count * POINT_RECORD_SIZE)

    def test_write_read_roundtrip_and_hash(self):
        with tempfile.TemporaryDirectory() as tmp:
            path = os.path.join(tmp, "c.pclb")
            sha1 = write_cloud(path, self.xyz, self.rgb)
            x2, r2, bbox, sha2 = read_cloud(path)
            self.assertEqual(sha1, sha2)
            self.assertTrue(np.array_equal(self.rgb, r2))
            self.assertTrue(np.allclose(self.xyz, x2, atol=1e-6))
            self.assertTrue(np.allclose(compute_bbox(self.xyz), bbox))

    def test_byte_deterministic_serialization(self):
        b1 = encode_cloud(self.xyz, self.rgb)
        perm = np.random.permutation(self.xyz.shape[0])
        # same arrays, independent encoding -> identical bytes
        b2 = encode_cloud(self.xyz.copy(), self.rgb.copy())
        self.assertEqual(b1, b2)
        # different order must produce different record bytes
        b3 = encode_cloud(self.xyz[perm], self.rgb[perm])
        self.assertNotEqual(b1, b3)

    def _corrupt(self, blob: bytes, **changes) -> bytes:
        return blob  # helpers below rebuild specific corruptions

    def test_bad_magic_raises_invalid(self):
        blob = bytearray(encode_cloud(self.xyz, self.rgb))
        blob[0:4] = b"XXXX"
        with self.assertRaises(PointCloudError) as ctx:
            parse_cloud(bytes(blob), validate=False)
        self.assertEqual(ctx.exception.code, E_POINTCLOUD_INVALID)

    def test_length_mismatch_raises_invalid(self):
        blob = encode_cloud(self.xyz, self.rgb)
        with self.assertRaises(PointCloudError) as ctx:
            parse_cloud(blob + b"\x00\x00")
        self.assertEqual(ctx.exception.code, E_POINTCLOUD_INVALID)

    def test_truncated_header_raises_invalid(self):
        with self.assertRaises(PointCloudError) as ctx:
            parse_cloud(b"PCLB\x01\x00")
        self.assertEqual(ctx.exception.code, E_POINTCLOUD_INVALID)

    def test_crc_corruption_raises_invalid(self):
        blob = bytearray(encode_cloud(self.xyz, self.rgb))
        blob[20] = (blob[20] + 1) & 0xFF
        with self.assertRaises(PointCloudError) as ctx:
            parse_cloud(bytes(blob))
        self.assertEqual(ctx.exception.code, E_POINTCLOUD_INVALID)

    def test_out_of_bbox_coordinate_rejected(self):
        bbox = compute_bbox(self.xyz)
        bad_xyz = self.xyz.copy()
        bad_xyz[0, 0] = bbox[3] + 100.0
        blob = encode_cloud(bad_xyz, self.rgb, bbox=bbox)
        with self.assertRaises(PointCloudError) as ctx:
            parse_cloud(blob, validate=True)
        self.assertEqual(ctx.exception.code, E_POINTCLOUD_INVALID)

    def test_empty_cloud_is_valid(self):
        xyz = np.zeros((0, 3), dtype=np.float64)
        rgb = np.zeros((0, 3), dtype=np.uint8)
        blob = encode_cloud(xyz, rgb)
        x2, r2, bbox, _ = parse_cloud(blob)
        self.assertEqual(x2.shape, (0, 3))
        self.assertEqual(r2.shape, (0, 3))


if __name__ == "__main__":
    unittest.main()
