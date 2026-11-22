from __future__ import annotations

import copy
import unittest

import numpy as np

from volrender.binary import build_scene, load_scene
from volrender.errors import BusinessError, ErrorCode
from volrender.fault_injection import load_scene_with_injection
from volrender.model import MEDIUM_BOX, Medium

from tests.helpers import tiny_scene


class BinarySceneTests(unittest.TestCase):
    def test_roundtrip_preserves_scene(self) -> None:
        scene = tiny_scene()
        payload = build_scene(scene)
        loaded = load_scene(payload)
        self.assertEqual(loaded.width, scene.width)
        self.assertEqual(loaded.height, scene.height)
        self.assertEqual(len(loaded.media), 1)
        self.assertEqual(loaded.media[0].sigma_s.tolist(), scene.media[0].sigma_s.tolist())
        self.assertEqual(
            loaded.density_fields[7].chunks[(2, 2, 2)].shape,
            scene.density_fields[7].chunks[(2, 2, 2)].shape,
        )
        self.assertEqual(build_scene(loaded), payload)

    def test_singular_camera_is_scene_invalid(self) -> None:
        scene = tiny_scene()
        scene.camera.world_to_camera[0, :] = 0.0
        with self.assertRaises(BusinessError) as caught:
            build_scene(scene)
        self.assertEqual(caught.exception.code, ErrorCode.SCENE_INVALID)
        self.assertGreaterEqual(caught.exception.offset, 0)

    def test_nonfinite_camera_injection_has_own_code(self) -> None:
        with self.assertRaises(BusinessError) as caught:
            load_scene_with_injection(build_scene(tiny_scene()), "camera_nonfinite")
        self.assertEqual(caught.exception.code, ErrorCode.CAMERA_NONFINITE)
        self.assertIn("camera", caught.exception.field)
        self.assertGreaterEqual(caught.exception.offset, 0)

    def test_chunk_index_injection_has_own_code(self) -> None:
        with self.assertRaises(BusinessError) as caught:
            load_scene_with_injection(build_scene(tiny_scene()), "chunk_index")
        self.assertEqual(caught.exception.code, ErrorCode.CHUNK_INDEX)
        self.assertIn("chunks", caught.exception.field)
        self.assertGreaterEqual(caught.exception.offset, 0)

    def test_missing_media_injection_has_own_code_and_offset(self) -> None:
        with self.assertRaises(BusinessError) as caught:
            load_scene_with_injection(build_scene(tiny_scene()), "missing_media")
        self.assertEqual(caught.exception.code, ErrorCode.MEDIA_MISSING)
        self.assertEqual(caught.exception.field, "media[0]")
        self.assertGreaterEqual(caught.exception.offset, 0)

    def test_truncated_density_payload_has_own_code_and_offset(self) -> None:
        payload = build_scene(tiny_scene())
        with self.assertRaises(BusinessError) as caught:
            load_scene(payload[:-13])
        self.assertEqual(caught.exception.code, ErrorCode.DENSITY_TRUNCATED)
        self.assertGreaterEqual(caught.exception.offset, 0)

    def test_truncated_density_injection_has_own_code(self) -> None:
        with self.assertRaises(BusinessError) as caught:
            load_scene_with_injection(
                build_scene(tiny_scene()), "density_truncated"
            )
        self.assertEqual(caught.exception.code, ErrorCode.DENSITY_TRUNCATED)
        self.assertGreaterEqual(caught.exception.offset, 0)

    def test_partial_medium_overlap_is_rejected(self) -> None:
        scene = tiny_scene()
        scene.media.append(
            Medium(
                id=11,
                kind=MEDIUM_BOX,
                density_field_id=7,
                sigma_s=np.array([0.1, 0.1, 0.1]),
                sigma_a=np.array([0.01, 0.01, 0.01]),
                g=0.0,
                density_scale=1.0,
                shape_a=np.array([0.3, -0.2, -0.2]),
                shape_b=np.array([1.4, 0.2, 0.2, 0.0]),
            )
        )
        scene.declared_media_count = len(scene.media)
        with self.assertRaises(BusinessError) as caught:
            build_scene(scene)
        self.assertEqual(caught.exception.code, ErrorCode.SCENE_INVALID)
        self.assertIn("nesting", caught.exception.field)

    def test_density_values_must_be_finite(self) -> None:
        scene = copy.deepcopy(tiny_scene())
        scene.density_fields[7].set_chunk(
            (0, 0, 0),
            np.array(
                [
                    [[1.0, 1.0], [1.0, 1.0]],
                    [[1.0, 1.0], [1.0, np.nan]],
                ]
            ),
        )
        with self.assertRaises(BusinessError) as caught:
            build_scene(scene)
        self.assertEqual(caught.exception.code, ErrorCode.SCENE_INVALID)
        self.assertIn("density", caught.exception.field)


if __name__ == "__main__":
    unittest.main()
