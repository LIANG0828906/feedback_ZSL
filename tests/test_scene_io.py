from __future__ import annotations

import unittest
from dataclasses import replace

from tactical_pick.canonical import sha256_bytes
from tactical_pick.errors import E_SCENE_INVALID, E_SNAPSHOT_INVALID, PickError
from tactical_pick.model import validate_scene
from tactical_pick.scene_io import (
    HEADER,
    SNAPSHOT_MAGIC,
    decode_scene,
    decode_snapshot,
    encode_scene,
    encode_snapshot,
    scene_content_hash,
)

from tests.helpers import simple_scene


class SceneIoTests(unittest.TestCase):
    def test_scene_binary_round_trip(self) -> None:
        scene = simple_scene()
        data = encode_scene(scene)
        decoded = decode_scene(data)
        self.assertEqual(scene, decoded)
        self.assertEqual(data, encode_scene(decoded))

    def test_degenerate_unit_bounds_have_location(self) -> None:
        scene = simple_scene()
        unit = scene.units[0]
        degenerate = replace(unit, minimum=unit.maximum)
        with self.assertRaises(PickError) as caught:
            validate_scene(replace(scene, units=(degenerate, *scene.units[1:])))
        self.assertEqual(caught.exception.code, E_SCENE_INVALID)
        self.assertEqual(caught.exception.location["entity_id"], unit.unit_id)
        self.assertIn("maximum", caught.exception.location["field"])

    def test_invalid_face_index_has_cover_entity(self) -> None:
        scene = simple_scene()
        bad_cover = replace(scene.covers[0], i2=999)
        with self.assertRaises(PickError) as caught:
            validate_scene(replace(scene, covers=(bad_cover,)))
        self.assertEqual(caught.exception.code, E_SCENE_INVALID)
        self.assertEqual(caught.exception.location["entity_id"], 100)
        self.assertIn("i2", caught.exception.location["field"])

    def test_mask_length_mismatch_has_grid_location(self) -> None:
        scene = simple_scene()
        bad_grid = replace(scene.grid, walkable=b"\x00")
        with self.assertRaises(PickError) as caught:
            validate_scene(replace(scene, grid=bad_grid))
        self.assertEqual(caught.exception.code, E_SCENE_INVALID)
        self.assertEqual(caught.exception.location["field"], "grid.walkable")
        self.assertEqual(caught.exception.location["cells"], 25)

    def test_byte_corruption_is_rejected(self) -> None:
        data = bytearray(encode_scene(simple_scene()))
        data[-1] ^= 0x40
        with self.assertRaises(PickError) as caught:
            decode_scene(bytes(data))
        self.assertEqual(caught.exception.code, E_SCENE_INVALID)
        self.assertEqual(caught.exception.location["field"], "header.checksum")

    def test_snapshot_round_trip_and_corruption(self) -> None:
        scene = simple_scene()
        digest = scene_content_hash(scene)
        encoded = encode_snapshot(3, digest, scene)
        decoded = decode_snapshot(encoded)
        self.assertEqual(decoded.version, 3)
        self.assertEqual(decoded.content_hash, digest)
        self.assertEqual(decoded.scene, scene)
        broken = encoded[:-4] + b"junk"
        with self.assertRaises(PickError) as caught:
            decode_snapshot(broken)
        self.assertEqual(caught.exception.code, E_SNAPSHOT_INVALID)

    def test_snapshot_hash_mismatch_is_rejected(self) -> None:
        scene = simple_scene()
        valid = encode_snapshot(2, scene_content_hash(scene), scene)
        payload = bytearray(valid[HEADER.size :])
        declared_hash_offset = 8
        payload[declared_hash_offset] ^= 0xFF
        checksum = bytes.fromhex(sha256_bytes(bytes(payload)))
        forged = HEADER.pack(SNAPSHOT_MAGIC, 1, 0, len(payload), checksum) + payload
        with self.assertRaises(PickError) as caught:
            decode_snapshot(forged)
        self.assertEqual(caught.exception.code, E_SNAPSHOT_INVALID)


if __name__ == "__main__":
    unittest.main()
