from __future__ import annotations

import os
import tempfile
import unittest
import zlib
from dataclasses import replace
from pathlib import Path
from unittest import mock

from tileforge.binary import (
    BLOCK_HEADER,
    HEADER_SIZE,
    HEADER_STRUCT,
    atomic_write_bytes,
    parse_sequence,
    parse_sequence_bytes,
    serialize_sequence,
    write_sequence_atomic,
)
from tileforge.errors import (
    E_BIT_DEPTH_INVALID,
    E_BLOCK_HASH,
    E_DATA_LENGTH,
    E_GRID_INVALID,
    E_HEADER_INVALID,
    E_IO,
    TileForgeError,
)
from tileforge.samplegen import SAMPLE_SPECS, generate_sequence


def _code(callable_obj, *args):
    with unittest.TestCase().assertRaises(TileForgeError) as context:
        callable_obj(*args)
    return context.exception


class BinaryFormatTests(unittest.TestCase):
    def setUp(self) -> None:
        self.sequence = generate_sequence(SAMPLE_SPECS[0], 71)
        self.encoded = serialize_sequence(self.sequence)

    def test_round_trip_is_byte_deterministic(self) -> None:
        parsed = parse_sequence_bytes(self.encoded)
        self.assertEqual(parsed.file_sha256, __import__("hashlib").sha256(self.encoded).hexdigest())
        self.assertEqual(serialize_sequence(parsed), self.encoded)
        self.assertEqual(parsed.header, self.sequence.header)
        self.assertEqual([item.content_hash for item in parsed.ordered_blocks()], [item.content_hash for item in self.sequence.ordered_blocks()])
        self.assertEqual(len(parsed.blocks), parsed.header.block_count)

    def test_non_divisible_grid_has_business_details(self) -> None:
        broken = replace(self.sequence.header, width=self.sequence.header.width + 1)
        exc = _code(serialize_sequence, type(self.sequence)(header=broken, blocks=self.sequence.blocks))
        self.assertEqual(exc.code, E_GRID_INVALID)
        self.assertEqual(exc.details["collision_fields"], ["width", "tiles_x"])

    def test_bad_bit_depth(self) -> None:
        broken = replace(self.sequence.header, bit_depth=9)
        exc = _code(serialize_sequence, type(self.sequence)(header=broken, blocks=self.sequence.blocks))
        self.assertEqual(exc.code, E_BIT_DEPTH_INVALID)
        self.assertEqual(exc.details["field"], "bit_depth")

    def test_truncated_payload_is_data_length_error(self) -> None:
        exc = _code(parse_sequence_bytes, self.encoded[:-1])
        self.assertEqual(exc.code, E_DATA_LENGTH)

    def test_block_hash_tamper_is_detected(self) -> None:
        tampered = bytearray(self.encoded)
        tampered[HEADER_SIZE + BLOCK_HEADER.size] ^= 0x01
        exc = _code(parse_sequence_bytes, bytes(tampered))
        self.assertEqual(exc.code, E_BLOCK_HASH)
        self.assertEqual(exc.details["tile_id"], 0)

    def test_header_crc_detects_dimension_tamper(self) -> None:
        tampered = bytearray(self.encoded)
        # width is the fourth field; corrupt it without recomputing CRC.
        tampered[12] ^= 0x01
        exc = _code(parse_sequence_bytes, bytes(tampered))
        self.assertEqual(exc.code, E_HEADER_INVALID)
        self.assertEqual(exc.details["field"], "header_crc")

    def test_declared_data_length_tamper_with_valid_crc(self) -> None:
        fields = list(HEADER_STRUCT.unpack_from(self.encoded, 0))
        fields[13] += 1
        raw = bytearray(HEADER_STRUCT.pack(*fields[:16], 0))
        crc = zlib.crc32(raw[:-4]) & 0xFFFFFFFF
        header = HEADER_STRUCT.pack(*fields[:16], crc)
        tampered = header + self.encoded[HEADER_SIZE:]
        exc = _code(parse_sequence_bytes, tampered)
        self.assertEqual(exc.code, E_DATA_LENGTH)
        self.assertEqual(exc.details["field"], "data_length")

    def test_atomic_write_preserves_previous_file_on_failure(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            target = Path(temp) / "published.tcseq"
            target.write_bytes(b"previous-valid-version")
            with mock.patch("tileforge.binary.os.replace", side_effect=OSError("disk full")):
                exc = _code(atomic_write_bytes, target, b"new-version")
            self.assertEqual(exc.code, E_IO)
            self.assertEqual(target.read_bytes(), b"previous-valid-version")
            self.assertFalse(any(path.name.startswith(".published") for path in Path(temp).iterdir()))

    def test_file_entry_point(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            path = Path(temp) / "sample.tcseq"
            digest = write_sequence_atomic(path, self.sequence)
            self.assertEqual(len(digest), 64)
            parsed = parse_sequence(path)
            self.assertEqual(parsed.file_sha256, digest)


if __name__ == "__main__":
    unittest.main()
