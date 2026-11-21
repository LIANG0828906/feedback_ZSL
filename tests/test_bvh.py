from __future__ import annotations

import unittest

from motion_workbench.bvh import parse_bvh_text
from motion_workbench.errors import E_BVH_INVALID, WorkbenchError
from motion_workbench.generator import SampleSpec, generate_bvh_text
from motion_workbench.math3d import v_length


class BVHParserTests(unittest.TestCase):
    def setUp(self):
        self.text = generate_bvh_text(
            SampleSpec(frames=6, joints=5, conflict="none", seed=13)
        )

    def test_parse_and_fk_are_deterministic(self):
        first = parse_bvh_text(self.text)
        second = parse_bvh_text(self.text)
        self.assertEqual(first.joints, second.joints)
        self.assertEqual(first.frames, second.frames)
        self.assertEqual(first.content_digest, second.content_digest)
        self.assertEqual(first.frame_count, 6)
        for frame in range(first.frame_count):
            a = first.evaluate(frame).to_dict()
            b = second.evaluate(frame).to_dict()
            self.assertEqual(a, b)
        transform = first.evaluate(2)
        for index, joint in enumerate(first.joints):
            if joint.parent_index >= 0:
                parent = transform.world_positions[joint.parent_index]
                child = transform.world_positions[index]
                distance = v_length(
                    (child[0] - parent[0], child[1] - parent[1], child[2] - parent[2])
                )
                expected = v_length(joint.offset)
                self.assertAlmostEqual(distance, expected, places=10)

    def test_channel_count_mismatch_reports_joint_and_frame(self):
        lines = self.text.splitlines()
        for index, line in enumerate(lines):
            if line.startswith("0.000000") or (
                index > 0 and lines[index - 1].startswith("Frame Time:")
            ):
                lines[index] = " ".join(line.split()[:-1])
                break
        with self.assertRaises(WorkbenchError) as context:
            parse_bvh_text("\n".join(lines) + "\n")
        self.assertEqual(context.exception.code, E_BVH_INVALID)
        self.assertEqual(context.exception.details["frame"], 0)
        self.assertIsNotNone(context.exception.details["joint"])

    def test_missing_frame_count_value_reports_frame(self):
        text = self.text.replace("Frames: 6", "Frames: 7")
        with self.assertRaises(WorkbenchError) as context:
            parse_bvh_text(text)
        self.assertEqual(context.exception.code, E_BVH_INVALID)
        self.assertEqual(context.exception.details["frame"], 6)

    def test_missing_frames_header_is_invalid(self):
        text = self.text.replace("Frames: 6\n", "")
        with self.assertRaises(WorkbenchError) as context:
            parse_bvh_text(text)
        self.assertEqual(context.exception.code, E_BVH_INVALID)
        self.assertEqual(context.exception.details["joint"], "MOTION")

    def test_parent_reference_out_of_range(self):
        text = self.text.replace("ROOT root\n", "ROOT root PARENT 99\n", 1)
        with self.assertRaises(WorkbenchError) as context:
            parse_bvh_text(text)
        self.assertEqual(context.exception.code, E_BVH_INVALID)
        self.assertEqual(context.exception.details["joint"], "root")
        self.assertEqual(context.exception.details["parent_reference"], 99)


if __name__ == "__main__":
    unittest.main()
