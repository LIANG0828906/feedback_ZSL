import os
import tempfile
import unittest

from hdrworkbench.errors import E_HDR_INVALID, HDRInvalidError
from hdrworkbench.generator import SampleSpec, write_sample
from hdrworkbench.hdrformat import (
    HEADER_SIZE,
    MAGIC,
    TF_PQ,
    load_sequence,
    parse_header,
    write_sequence,
)


class FormatTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.mkdtemp()
        self.path = os.path.join(self.tmp, "sample.hdr")
        self.spec = SampleSpec(10, 8, 3, 10, 2, TF_PQ, 1000, seed=42)
        write_sample(self.path, self.spec)

    def test_roundtrip_and_determinism(self):
        seq = load_sequence(self.path)
        self.assertEqual(seq.header.width, 10)
        self.assertEqual(seq.header.frame_count, 3)
        self.assertEqual(seq.raw_frames[0].dtype.name, "uint16")
        again = os.path.join(self.tmp, "again.hdr")
        write_sample(again, self.spec)
        with open(self.path, "rb") as a, open(again, "rb") as b:
            self.assertEqual(a.read(), b.read())

    def test_truncated_frame_locates_bad_frame(self):
        with open(self.path, "rb") as handle:
            blob = handle.read()
        broken = os.path.join(self.tmp, "trunc.hdr")
        # 保留第 0 帧完整、截断第 1 帧
        cut = HEADER_SIZE + seq_frame_bytes(self.spec) + 5
        with open(broken, "wb") as handle:
            handle.write(blob[:cut])
        with self.assertRaises(HDRInvalidError) as ctx:
            load_sequence(broken)
        self.assertEqual(ctx.exception.code, E_HDR_INVALID)
        self.assertEqual(ctx.exception.details["bad_frame"], 1)

    def test_unknown_transfer_code(self):
        with open(self.path, "rb") as handle:
            blob = bytearray(handle.read())
        # 头部布局：magic(8) width(4) height(4) count(4) depth(2)
        #           colorspace(1, 偏移 22) transfer(1, 偏移 23)
        blob[23] = 99
        with self.assertRaises(HDRInvalidError) as ctx:
            parse_header(bytes(blob))
        self.assertEqual(ctx.exception.details["reason"], "unknown_transfer")
        self.assertEqual(ctx.exception.details["bad_frame"], -1)

    def test_bad_magic_and_short_header(self):
        with self.assertRaises(HDRInvalidError) as ctx:
            parse_header(b"xx")
        self.assertEqual(ctx.exception.details["reason"], "header_truncated")
        bad = MAGIC + b"\x00" * 40
        with self.assertRaises(HDRInvalidError):
            parse_header(bad)

    def test_atomic_write_leaves_no_tmp(self):
        seq = load_sequence(self.path)
        out = os.path.join(self.tmp, "copy.hdr")
        write_sequence(out, seq.header, seq.raw_frames)
        self.assertFalse(os.path.exists(out + ".tmp"))


def seq_frame_bytes(spec: SampleSpec) -> int:
    bytes_per_sample = 1 if spec.bit_depth == 8 else 2
    return spec.width * spec.height * 3 * bytes_per_sample


if __name__ == "__main__":
    unittest.main()

