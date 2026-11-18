from __future__ import annotations

import base64
import copy
import json
import tempfile
import unittest
import zlib
from pathlib import Path

import numpy as np

from elytron.checkpoint import load_snapshot, save_snapshot
from elytron.fileio import write_json_atomic
from elytron.render import BlockSpec, TileResult, enumerate_blocks
from elytron.report import make_block_record
from elytron.scene import canonical_bytes, generate_scene, sha256_bytes, validate_scene


class CheckpointTests(unittest.TestCase):
    def setUp(self) -> None:
        self.bundle = validate_scene(generate_scene(254))
        self.specs = enumerate_blocks(self.bundle.scene)

    def _fake_tiles(self, count: int) -> tuple[dict[str, TileResult], dict[str, dict]]:
        tiles = {}
        records = {}
        for spec in self.specs[:count]:
            rgba = np.zeros((spec.height, spec.width, 4), dtype=np.uint8)
            rgba[..., 0] = spec.block_index % 256
            rgba[..., 3] = 255
            face_id = np.zeros((spec.height, spec.width), dtype=np.int32)
            tile = TileResult(
                rgba=rgba,
                face_id=face_id,
                thickness_sum=0.0,
                thickness_count=0,
                visible_face_count=0,
            )
            tiles[spec.block_id] = tile
            records[spec.block_id] = make_block_record(spec, tile)
        return tiles, records

    def test_snapshot_round_trip(self) -> None:
        tiles, records = self._fake_tiles(3)
        with tempfile.TemporaryDirectory() as temporary:
            path = Path(temporary) / "progress_snapshot.json"
            save_snapshot(path, self.bundle, self.specs, tiles, records)
            loaded = load_snapshot(path, self.bundle, self.specs)
            self.assertTrue(loaded.granted)
            self.assertEqual(loaded.reasons, [])
            self.assertEqual(set(loaded.tiles), set(tiles))
            for block_id in tiles:
                self.assertTrue(np.array_equal(loaded.tiles[block_id].rgba, tiles[block_id].rgba))
                self.assertTrue(np.array_equal(loaded.tiles[block_id].face_id, tiles[block_id].face_id))

    def test_checksum_corruption_discards_all_blocks(self) -> None:
        tiles, records = self._fake_tiles(2)
        with tempfile.TemporaryDirectory() as temporary:
            path = Path(temporary) / "progress_snapshot.json"
            save_snapshot(path, self.bundle, self.specs, tiles, records)
            envelope = json.loads(path.read_text(encoding="utf-8"))
            envelope["payload"]["next_block_index"] = 1
            path.write_text(json.dumps(envelope), encoding="utf-8")
            loaded = load_snapshot(path, self.bundle, self.specs)
            self.assertFalse(loaded.granted)
            self.assertTrue(loaded.snapshot_found)
            self.assertEqual(loaded.tiles, {})
            self.assertTrue(any("checksum" in reason for reason in loaded.reasons))

    def test_stale_block_offset_is_rejected_even_with_valid_checksum(self) -> None:
        tiles, records = self._fake_tiles(2)
        with tempfile.TemporaryDirectory() as temporary:
            path = Path(temporary) / "progress_snapshot.json"
            save_snapshot(path, self.bundle, self.specs, tiles, records)
            envelope = json.loads(path.read_text(encoding="utf-8"))
            envelope["payload"]["blocks"][1]["x"] += 1
            envelope["checksum"] = sha256_bytes(canonical_bytes(envelope["payload"]))
            write_json_atomic(path, envelope)
            loaded = load_snapshot(path, self.bundle, self.specs)
            self.assertFalse(loaded.granted)
            self.assertTrue(any("stale x" in reason for reason in loaded.reasons))
            self.assertEqual(loaded.tiles, {})

    def test_scene_and_film_changes_discard_snapshot(self) -> None:
        tiles, records = self._fake_tiles(2)
        with tempfile.TemporaryDirectory() as temporary:
            path = Path(temporary) / "progress_snapshot.json"
            save_snapshot(path, self.bundle, self.specs, tiles, records)
            changed_scene = copy.deepcopy(self.bundle.scene)
            changed_scene["film"]["base_thickness_nm"] += 1.0
            changed_bundle = validate_scene(changed_scene)
            loaded = load_snapshot(path, changed_bundle, enumerate_blocks(changed_scene))
            self.assertFalse(loaded.granted)
            self.assertTrue(loaded.snapshot_found)
            self.assertEqual(loaded.tiles, {})
            self.assertTrue(
                any("scene digest" in reason or "film parameter" in reason for reason in loaded.reasons)
            )

    def test_damaged_block_payload_discards_all_blocks(self) -> None:
        tiles, records = self._fake_tiles(2)
        with tempfile.TemporaryDirectory() as temporary:
            path = Path(temporary) / "progress_snapshot.json"
            save_snapshot(path, self.bundle, self.specs, tiles, records)
            envelope = json.loads(path.read_text(encoding="utf-8"))
            data = base64.b64decode(envelope["payload"]["blocks"][0]["rgba"]["data"])
            data = data[:-3] + b"bad"
            envelope["payload"]["blocks"][0]["rgba"]["data"] = base64.b64encode(data).decode("ascii")
            envelope["checksum"] = sha256_bytes(canonical_bytes(envelope["payload"]))
            write_json_atomic(path, envelope)
            loaded = load_snapshot(path, self.bundle, self.specs)
            self.assertFalse(loaded.granted)
            self.assertEqual(loaded.tiles, {})


if __name__ == "__main__":
    unittest.main()
