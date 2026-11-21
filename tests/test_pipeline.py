"""Unit tests for the retargeting pipeline (stdlib unittest)."""

import json
import math
import os
import shutil
import tempfile
import unittest

import numpy as np

from retarget.errors import (E_ANIM_INVALID, E_MAP_CONFLICT,
                             E_RECOVERY_INVALID, E_SCALE_APPLIED,
                             RetargetError)
from retarget.formats import load_target_skeleton, parse_animation
from retarget.mapping import build_mapping, normalize_name
from retarget.pipeline import Config, quantize
from retarget.quat import qangle_between, qfrom_matrix, qmul, qnormalize
from retarget.runner import finalize, resume, run
from retarget.samplegen import gen_sample, gen_suite
from retarget.util import read_json


class TempDirTest(unittest.TestCase):
    def setUp(self):
        self.dir = tempfile.mkdtemp(prefix="retarget_test_")

    def tearDown(self):
        shutil.rmtree(self.dir, ignore_errors=True)


class TestQuat(unittest.TestCase):
    def test_mul_identity(self):
        q = qnormalize(np.array([0.5, 0.5, 0.5, 0.5]))
        ident = qmul(q, np.array([1.0, 0.0, 0.0, 0.0]))
        self.assertTrue(np.allclose(q, ident))

    def test_angle_between_self_is_zero(self):
        q = qnormalize(np.array([0.9, 0.1, 0.2, 0.3]))
        self.assertAlmostEqual(qangle_between(q, q), 0.0, places=12)

    def test_matrix_roundtrip(self):
        from retarget.quat import mat_from_quat
        q = qnormalize(np.array([0.7, 0.1, -0.2, 0.3]))
        q2 = qfrom_matrix(mat_from_quat(q))
        self.assertAlmostEqual(qangle_between(q, q2), 0.0, places=10)


class TestQuantize(unittest.TestCase):
    def test_direction_and_value(self):
        v, d = quantize(0.123456, 4, "half_even")
        self.assertEqual(v, 0.1235)
        self.assertEqual(d, "up")
        v, d = quantize(0.12341, 4, "half_even")
        self.assertEqual(v, 0.1234)
        self.assertEqual(d, "down")
        v, d = quantize(0.5, 4, "half_even")
        self.assertEqual(d, "none")

    def test_modes(self):
        self.assertEqual(quantize(0.12345, 4, "half_up")[0], 0.1235)
        self.assertEqual(quantize(-0.12345, 4, "down")[0], -0.1234)
        self.assertEqual(quantize(0.12345, 4, "floor")[0], 0.1234)
        self.assertEqual(quantize(-0.12341, 4, "floor")[0], -0.1235)

    def test_deterministic(self):
        self.assertEqual(quantize(0.999995, 4, "half_even"),
                         quantize(0.999995, 4, "half_even"))


class TestFormats(TempDirTest):
    def _gen(self, version=3, fault=None):
        d = os.path.join(self.dir, "s")
        gen_sample(7, 8, 16, version, d, fault=fault)
        return d

    def test_parse_v1_upgrade_defaults(self):
        anim = parse_animation(os.path.join(self._gen(1), "anim.json"))
        self.assertEqual(anim.version, 1)
        self.assertFalse(anim.has_translations)
        self.assertFalse(anim.has_bind_pose)
        self.assertTrue(all(p is None for p in anim.hierarchy.values()))
        self.assertEqual(anim.aliases, {})

    def test_parse_v2_has_hierarchy_and_translations(self):
        anim = parse_animation(os.path.join(self._gen(2), "anim.json"))
        self.assertTrue(anim.has_translations)
        self.assertIsNone(anim.hierarchy[anim.joints[0]])

    def test_parse_v3_full(self):
        anim = parse_animation(os.path.join(self._gen(3), "anim.json"))
        self.assertTrue(anim.has_bind_pose)
        self.assertIn("Hips", anim.aliases)
        self.assertEqual(len(anim.bone_scales), len(anim.joints))

    def test_nonunit_quat_locates_first_joint_frame(self):
        with self.assertRaises(RetargetError) as ctx:
            parse_animation(os.path.join(self._gen(2, "nonunit_quat"),
                                         "anim.json"))
        self.assertEqual(ctx.exception.code, E_ANIM_INVALID)
        self.assertEqual(ctx.exception.details["joint"], "Spine")
        self.assertEqual(ctx.exception.details["frame"], 7)

    def test_dangling_parent(self):
        with self.assertRaises(RetargetError) as ctx:
            parse_animation(os.path.join(self._gen(2, "dangling_parent"),
                                         "anim.json"))
        self.assertEqual(ctx.exception.code, E_ANIM_INVALID)
        self.assertEqual(ctx.exception.details["parent"], "GhostJoint")

    def test_missing_field(self):
        with self.assertRaises(RetargetError) as ctx:
            parse_animation(os.path.join(self._gen(2, "missing_field"),
                                         "anim.json"))
        self.assertEqual(ctx.exception.code, E_ANIM_INVALID)

    def test_hierarchy_cycle(self):
        d = self._gen(2)
        p = os.path.join(d, "anim.json")
        raw = read_json(p)
        raw["hierarchy"]["Hips"] = "Chest"
        with open(p, "w") as fh:
            json.dump(raw, fh)
        with self.assertRaises(RetargetError) as ctx:
            parse_animation(p)
        self.assertEqual(ctx.exception.code, E_ANIM_INVALID)


class TestMapping(TempDirTest):
    def _build(self, version=3, fault=None):
        d = os.path.join(self.dir, "s")
        gen_sample(11, 12, 16, version, d, fault=fault)
        anim = parse_animation(os.path.join(d, "anim.json"))
        target = load_target_skeleton(os.path.join(d, "target.json"))
        return anim, target

    def test_alias_priority_and_confidence(self):
        anim, target = self._build(3)
        report = build_mapping(anim, target)
        by_source = {m["source"]: m for m in report["mappings"]}
        self.assertEqual(by_source["Hips"]["method"], "alias")
        self.assertEqual(by_source["Hips"]["confidence"], "high")
        self.assertEqual(by_source["Spine"]["method"], "name")
        self.assertEqual(by_source["Spine"]["confidence"], "medium")

    def test_alias_miss_no_silent_fallback(self):
        anim, target = self._build(3, "alias_miss")
        report = build_mapping(anim, target)
        miss = [u for u in report["unmatched"] if u["reason"] == "alias_miss"]
        self.assertEqual(len(miss), 1)
        self.assertEqual(miss[0]["alias_target"], "NoSuchJoint")
        self.assertTrue(miss[0]["ancestor_candidates"])
        self.assertNotIn(miss[0]["source"],
                         {m["source"] for m in report["mappings"]})

    def test_conflict_aborts(self):
        anim, target = self._build(3, "map_conflict")
        with self.assertRaises(RetargetError) as ctx:
            build_mapping(anim, target)
        self.assertEqual(ctx.exception.code, E_MAP_CONFLICT)
        sources = ctx.exception.details["conflicts"][0]["sources"]
        self.assertEqual(set(sources), {"Head", "Neck"})

    def test_normalize_name(self):
        self.assertEqual(normalize_name("UpperArm_L"), "upperarml")
        self.assertEqual(normalize_name("upperarm-l"), "upperarml")


class TestPipeline(TempDirTest):
    def _run(self, name="s", config=None, fault=None, version=3, seed=21):
        d = os.path.join(self.dir, name)
        gen_sample(seed, 12, 32, version, d, fault=fault)
        out = os.path.join(self.dir, name + "_out")
        run(os.path.join(d, "anim.json"), os.path.join(d, "target.json"),
            out, config or Config())
        return out

    def test_float_path_exact(self):
        report = read_json(os.path.join(self._run(), "report.json"))
        self.assertLess(report["paths"]["float"]["max_deviation"]["deg"],
                        1e-4)
        self.assertEqual(report["paths"]["float"]["verdict"], "pass")

    def test_fixed_path_within_baseline(self):
        report = read_json(os.path.join(self._run(), "report.json"))
        fixed = report["paths"]["fixed"]
        self.assertEqual(fixed["verdict"], "pass")
        self.assertLess(fixed["max_deviation"]["deg"], 0.5)
        self.assertGreater(report["quant_events_total"], 0)

    def test_scale_applied_once(self):
        report = read_json(os.path.join(self._run(), "report.json"))
        for path in ("float", "fixed"):
            self.assertLessEqual(
                report["scale"][path]["max_apply_diff"], 1e-6)

    def test_scale_dup_injection_raises(self):
        d = os.path.join(self.dir, "dup")
        gen_sample(22, 12, 32, 3, d)
        out = os.path.join(self.dir, "dup_out")
        with self.assertRaises(RetargetError) as ctx:
            run(os.path.join(d, "anim.json"), os.path.join(d, "target.json"),
                out, Config(inject_scale_dup=True))
        self.assertEqual(ctx.exception.code, E_SCALE_APPLIED)
        self.assertIn("joint", ctx.exception.details)
        self.assertIn("frame", ctx.exception.details)
        self.assertFalse(os.path.exists(os.path.join(out,
                                                     "poses_float.jsonl")))

    def test_dominant_node_attribution(self):
        d = os.path.join(self.dir, "attr")
        gen_sample(23, 5, 8, 2, d)
        p = os.path.join(d, "anim.json")
        raw = read_json(p)
        raw["quat_norm_tolerance"] = 0.05
        q = raw["tracks"]["rotations"]["Hips"][3]
        raw["tracks"]["rotations"]["Hips"][3] = [c * 1.04 for c in q]
        with open(p, "w") as fh:
            json.dump(raw, fh)
        out = os.path.join(self.dir, "attr_out")
        run(p, os.path.join(d, "target.json"), out, Config(mode="float"))
        report = read_json(os.path.join(out, "report.json"))
        self.assertEqual(report["paths"]["float"]["verdict"], "pass")

    def test_untrusted_marks_at_low_precision(self):
        out = self._run("low", Config(decimals=2, tolerance_deg=0.5))
        report = read_json(os.path.join(out, "report.json"))
        self.assertGreater(report["paths"]["fixed"]["untrusted_marks"], 0)
        self.assertEqual(report["paths"]["fixed"]["verdict"], "fail")
        causes = {e["cause"]
                  for e in report["paths"]["fixed"]["exceedances"]}
        self.assertEqual(causes, {"quantization"})


class TestRecovery(TempDirTest):
    def _gen(self):
        d = os.path.join(self.dir, "s")
        gen_sample(31, 12, 40, 3, d)
        return os.path.join(d, "anim.json"), os.path.join(d, "target.json")

    def test_resume_recomputes_only_missing_intervals(self):
        anim, target = self._gen()
        out = os.path.join(self.dir, "out")
        run(anim, target, out, Config(interval_frames=8))
        ref = {f: open(os.path.join(out, f), "rb").read()
               for f in ("poses_float.jsonl", "poses_fixed.jsonl",
                         "trajectory.log", "report.json")}
        ckpt = os.path.join(out, "ckpt")
        for name in os.listdir(ckpt):
            if name.startswith("interval_") and name >= "interval_000002":
                os.unlink(os.path.join(ckpt, name))
        manifest = read_json(os.path.join(ckpt, "manifest.json"))
        manifest["committed_intervals"] = 2
        manifest["complete"] = False
        manifest["head_hash"] = read_json(
            os.path.join(ckpt, "interval_000001.json"))["chain_hash"]
        with open(os.path.join(ckpt, "manifest.json"), "w") as fh:
            json.dump(manifest, fh)
        for f in ref:
            os.unlink(os.path.join(out, f))
        info = resume(out)
        self.assertEqual(info["resumed_from_interval"], 2)
        self.assertFalse(os.path.exists(os.path.join(out, "report.json")))
        finalize(out)
        for f, data in ref.items():
            self.assertEqual(open(os.path.join(out, f), "rb").read(), data,
                             f)

    def test_tampered_snapshot_detected(self):
        anim, target = self._gen()
        out = os.path.join(self.dir, "out")
        run(anim, target, out, Config(interval_frames=8))
        p = os.path.join(out, "ckpt", "interval_000001.json")
        doc = read_json(p)
        doc["poses"]["float"][0]["local_rot"]["Pelvis"][0] += 0.5
        with open(p, "w") as fh:
            json.dump(doc, fh)
        with self.assertRaises(RetargetError) as ctx:
            resume(out)
        self.assertEqual(ctx.exception.code, E_RECOVERY_INVALID)
        self.assertEqual(ctx.exception.details["first_broken_frame"], 8)


class TestDeterminism(TempDirTest):
    def test_same_seed_byte_identical(self):
        d1, d2 = os.path.join(self.dir, "g1"), os.path.join(self.dir, "g2")
        gen_suite(99, d1)
        gen_suite(99, d2)
        for root, _, files in os.walk(d1):
            for f in files:
                p1 = os.path.join(root, f)
                p2 = p1.replace(d1, d2, 1)
                self.assertEqual(open(p1, "rb").read(),
                                 open(p2, "rb").read(), p1)


if __name__ == "__main__":
    unittest.main()
