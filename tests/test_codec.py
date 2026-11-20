"""业务要求 1：三版本解析/序列化与格式错误定位。"""

import os
import tempfile
import unittest

import numpy as np

from retarget_pipeline import codec, samples
from retarget_pipeline.errors import E_ANIM_FORMAT_INVALID, PipelineError


class CodecTest(unittest.TestCase):
    def setUp(self):
        self.docs = samples.build_source_documents(7, 6, 5)

    def test_roundtrip_all_versions(self):
        for version, doc in self.docs.items():
            with tempfile.TemporaryDirectory() as tmp:
                path = os.path.join(tmp, f"v{version}.json")
                codec.dump_file(doc, path)
                loaded = codec.load_file(path)
                self.assertEqual(loaded.version, version)
                self.assertEqual(loaded.names, doc.names)
                np.testing.assert_allclose(
                    loaded.local_rotations, doc.local_rotations, atol=1e-12)
                if version >= 2:
                    np.testing.assert_allclose(
                        loaded.local_translations, doc.local_translations, atol=1e-12)
                if version >= 3:
                    self.assertEqual(loaded.joints[1].aliases, doc.joints[1].aliases)
                    self.assertAlmostEqual(loaded.joints[1].scale, doc.joints[1].scale)

    def test_unknown_version_rejected(self):
        raw = codec.to_json_dict(self.docs[1])
        raw["format_version"] = 99
        with self.assertRaises(PipelineError) as ctx:
            codec.parse_document(raw)
        self.assertEqual(ctx.exception.code, E_ANIM_FORMAT_INVALID)
        self.assertEqual(ctx.exception.locations[0]["field"], "format_version")

    def test_missing_field_located(self):
        raw = codec.to_json_dict(self.docs[2])
        del raw["joints"][2]["parent"]
        with self.assertRaises(PipelineError) as ctx:
            codec.parse_document(raw)
        self.assertEqual(ctx.exception.code, E_ANIM_FORMAT_INVALID)
        loc = ctx.exception.locations[0]
        self.assertEqual(loc["field"], "parent")
        self.assertEqual(loc["joint"], raw["joints"][2]["name"])

    def test_cycle_detected(self):
        raw = codec.to_json_dict(self.docs[2])
        raw["joints"][1]["parent"] = 2
        raw["joints"][2]["parent"] = 1
        with self.assertRaises(PipelineError) as ctx:
            codec.parse_document(raw)
        self.assertEqual(ctx.exception.code, E_ANIM_FORMAT_INVALID)
        self.assertIn("成环", ctx.exception.message)

    def test_degenerate_quaternion_rejected(self):
        raw = codec.to_json_dict(self.docs[1])
        raw["frames"][0]["rotations"][0] = [0, 0, 0, 0]
        with self.assertRaises(PipelineError) as ctx:
            codec.parse_document(raw)
        self.assertEqual(ctx.exception.code, E_ANIM_FORMAT_INVALID)
        self.assertEqual(ctx.exception.locations[0]["frame"], 0)

    def test_no_partial_load_on_bad_frame(self):
        raw = codec.to_json_dict(self.docs[1])
        raw["frames"][-1]["rotations"] = raw["frames"][-1]["rotations"][:-1]
        with self.assertRaises(PipelineError):
            codec.parse_document(raw)

    def test_atomic_write_leaves_no_temp(self):
        with tempfile.TemporaryDirectory() as tmp:
            path = os.path.join(tmp, "a.json")
            codec.dump_file(self.docs[3], path)
            leftovers = [f for f in os.listdir(tmp) if f.startswith(".tmp")]
            self.assertEqual(leftovers, [])


if __name__ == "__main__":
    unittest.main()
