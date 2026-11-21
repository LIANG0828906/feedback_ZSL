from __future__ import annotations

import unittest

import numpy as np

from motionreview.bvh import (
    BVHDocument,
    Joint,
    evaluate_fk,
    parse_bvh,
)
from motionreview.errors import BVHInvalidError


SIMPLE_BVH = """HIERARCHY
ROOT Hips
{
 OFFSET 0 0 0
 CHANNELS 6 Xposition Yposition Zposition Zrotation Xrotation Yrotation
 JOINT Spine
 {
  OFFSET 0 10 0
  CHANNELS 3 Zrotation Xrotation Yrotation
  End Site
  {
   OFFSET 0 5 0
  }
 }
}
MOTION
Frames: 3
Frame Time: 0.1
0 0 0 0 0 0 0 0 0
0 0 0 1.5707963267948966 0 0 0 0 0
0 0 0 0 0 0 0 0 0
"""


class BVHTest(unittest.TestCase):
    def test_parse_and_fk_is_deterministic(self) -> None:
        document = parse_bvh(SIMPLE_BVH, clip_id="simple")
        first = evaluate_fk(document)
        second = evaluate_fk(document)
        self.assertEqual(first.shape, (3, 2, 4, 4))
        np.testing.assert_array_equal(first, second)
        spine = first[1, 1]
        self.assertAlmostEqual(spine[0, 3], -10.0, places=12)
        self.assertAlmostEqual(spine[1, 3], 0.0, places=12)
        self.assertEqual(document.content_hash, document.content_hash)

    def test_frame_channel_mismatch_locates_frame_and_joint(self) -> None:
        broken = SIMPLE_BVH.replace(
            "0 0 0 0 0 0 0 0 0\nMOTION",
            "0 0 0 0 0 0 0 0 0\nMOTION",
        )
        lines = broken.splitlines()
        last_frame = len(lines) - 1
        lines[last_frame] = "0 0 0 0 0 0 0 0"
        with self.assertRaises(BVHInvalidError) as caught:
            parse_bvh("\n".join(lines), clip_id="broken")
        error = caught.exception
        self.assertEqual(error.code, "E_BVH_INVALID")
        self.assertEqual(error.frame, 2)
        self.assertEqual(error.joint, "Spine")

    def test_missing_frame_is_located(self) -> None:
        lines = SIMPLE_BVH.splitlines()[:-1]
        with self.assertRaises(BVHInvalidError) as caught:
            parse_bvh("\n".join(lines), clip_id="missing")
        self.assertEqual(caught.exception.frame, 2)
        self.assertEqual(caught.exception.code, "E_BVH_INVALID")

    def test_parent_out_of_bounds(self) -> None:
        joints = [
            Joint(0, "root", None, (0, 0, 0), ("Zrotation",)),
            Joint(1, "child", 7, (0, 1, 0), ("Zrotation",)),
        ]
        with self.assertRaises(BVHInvalidError) as caught:
            BVHDocument.from_data(joints, [[0, 0]], 0.1, clip_id="bad")
        self.assertEqual(caught.exception.joint, "child")
        self.assertEqual(caught.exception.code, "E_BVH_INVALID")

    def test_channel_count_mismatch_at_document_boundary(self) -> None:
        joints = [
            Joint(0, "root", None, (0, 0, 0), ("Zrotation",)),
        ]
        with self.assertRaises(BVHInvalidError) as caught:
            BVHDocument.from_data(
                joints, [[0.0, 1.0]], 0.1, clip_id="bad"
            )
        self.assertEqual(caught.exception.joint, "root")
        self.assertEqual(caught.exception.frame, 0)


if __name__ == "__main__":
    unittest.main()
