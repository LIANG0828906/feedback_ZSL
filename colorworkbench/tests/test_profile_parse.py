"""业务要求 1：多版本配置解析与错误码链路。"""

import json
import os
import unittest

from cwb.errors import CwbError, E_PARA, E_VER
from cwb.profile_io import parse_profile


def _load(root, name):
    with open(os.path.join(root, "profiles", name + ".json"),
              encoding="utf-8") as fh:
        return json.load(fh)


class ProfileParseTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.root = os.path.join(os.path.dirname(__file__), "..", "samples")

    def test_three_versions_unified_model(self):
        for name, ver, trc_kind in (
                ("src_v1", "1", "parametric"),
                ("src_v2", "2", "table"),
                ("src_v3", "3.0", "parametric")):
            profile = parse_profile(_load(self.root, name))
            self.assertEqual(profile.source_version, ver)
            self.assertEqual(profile.trc.kind, trc_kind)
            self.assertEqual(len(profile.matrix), 9)
            self.assertIn(profile.default_intent,
                          ("relative_colorimetric", "absolute_colorimetric",
                           "perceptual"))

    def test_version2_optional_white_point_uses_declared_default(self):
        raw = _load(self.root, "src_v2")
        self.assertIsNone(raw.get("wp"))
        profile = parse_profile(raw)
        self.assertIsNone(profile.white_point)
        self.assertIsNotNone(profile.declared_default_white_point)

    def test_unknown_version_returns_E_VER(self):
        raw = _load(self.root, "src_v1")
        raw["profile_version"] = 99
        with self.assertRaises(CwbError) as ctx:
            parse_profile(raw)
        self.assertEqual(ctx.exception.code, E_VER)

    def test_missing_required_tag_returns_E_VER(self):
        raw = _load(self.root, "src_v3")
        del raw["profileId"]
        with self.assertRaises(CwbError) as ctx:
            parse_profile(raw)
        self.assertEqual(ctx.exception.code, E_VER)
        self.assertEqual(ctx.exception.tag, "profileId")

    def test_out_of_range_gamma_returns_E_PARA_with_tag(self):
        raw = _load(self.root, "src_v1")
        raw["red_trc"]["value"] = 99.0
        with self.assertRaises(CwbError) as ctx:
            parse_profile(raw)
        self.assertEqual(ctx.exception.code, E_PARA)
        self.assertEqual(ctx.exception.tag, "red_trc")

    def test_out_of_range_matrix_returns_E_PARA_with_tag(self):
        raw = _load(self.root, "src_v2")
        raw["matrix3x3"][0] = 99.0
        with self.assertRaises(CwbError) as ctx:
            parse_profile(raw)
        self.assertEqual(ctx.exception.code, E_PARA)
        self.assertEqual(ctx.exception.tag, "matrix3x3")

    def test_bad_intent_enum_returns_E_PARA(self):
        raw = _load(self.root, "src_v3")
        raw["renderingIntent"] = "rainbow"
        with self.assertRaises(CwbError) as ctx:
            parse_profile(raw)
        self.assertEqual(ctx.exception.code, E_PARA)

    def test_lut3d_parsed_only_for_version2(self):
        v2 = parse_profile(_load(self.root, "src_v2"))
        v1 = parse_profile(_load(self.root, "src_v1"))
        self.assertIsNotNone(v2.lut3d)
        self.assertIsNone(v1.lut3d)


if __name__ == "__main__":
    unittest.main()
