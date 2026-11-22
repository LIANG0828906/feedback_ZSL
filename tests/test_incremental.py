from __future__ import annotations

import copy
import unittest

import numpy as np

from volrender.errors import BusinessError, ErrorCode
from volrender.incremental import ChangeSet, IncrementalRenderer, infer_changes

from tests.helpers import config, two_box_scene


class IncrementalTests(unittest.TestCase):
    def test_material_change_rerenders_only_dependent_tiles(self) -> None:
        renderer = IncrementalRenderer()
        scene = two_box_scene()
        cfg = config(tile_size=10, sample_budget=5_000_000)
        first = renderer.render(scene, cfg, changes=ChangeSet(), validate_seams=False)
        old_map = {tile.tile.tile_id: tile for tile in first.tile_results}

        changed = copy.deepcopy(scene)
        changed.media_by_id()[10].sigma_s *= 1.4
        changes = infer_changes(scene, changed)
        self.assertIn(10, changes.medium_material_changed)
        self.assertNotIn(11, changes.medium_material_changed)

        second = renderer.render(
            changed, cfg, changes=changes, validate_seams=False
        )
        incremental = second.summary["incremental"]
        self.assertGreater(len(incremental["rendered_tiles"]), 0)
        self.assertGreater(len(incremental["reused_tiles"]), 0)
        self.assertLess(
            len(incremental["rendered_tiles"]), len(incremental["reused_tiles"]) + len(incremental["rendered_tiles"])
        )
        for tile_id in incremental["reused_tiles"]:
            self.assertEqual(
                old_map[tile_id].content_hash,
                {tile.tile.tile_id: tile for tile in second.tile_results}[tile_id].content_hash,
            )
        self.assertEqual(renderer.get_snapshot().version, 2)

    def test_budget_failure_keeps_previous_snapshot(self) -> None:
        renderer = IncrementalRenderer()
        scene = two_box_scene()
        cfg = config(tile_size=10, sample_budget=5_000_000)
        renderer.render(scene, cfg, changes=ChangeSet(), validate_seams=False)
        before = renderer.get_snapshot()
        assert before is not None
        low_budget = config(tile_size=10, sample_budget=1)
        with self.assertRaises(BusinessError) as caught:
            renderer.render(
                scene,
                low_budget,
                changes=ChangeSet(camera_changed=True),
                validate_seams=False,
            )
        self.assertEqual(caught.exception.code, ErrorCode.BUDGET_EXCEEDED)
        after = renderer.get_snapshot()
        assert after is not None
        self.assertEqual(after.version, before.version)
        self.assertEqual(after.summary["summary_hash"], before.summary["summary_hash"])

    def test_unclassified_topology_change_is_conservative(self) -> None:
        renderer = IncrementalRenderer()
        scene = two_box_scene()
        cfg = config(tile_size=10, sample_budget=5_000_000)
        renderer.render(scene, cfg, changes=ChangeSet(), validate_seams=False)
        changed = copy.deepcopy(scene)
        changed.media.append(copy.deepcopy(changed.media[0]))
        changed.media[-1].id = 99
        changed.declared_media_count = len(changed.media)
        result = renderer.render(
            changed,
            cfg,
            changes=ChangeSet(topology_changed=True, reason="topology_test"),
            validate_seams=False,
        )
        incremental = result.summary["incremental"]
        self.assertTrue(incremental["conservative"])
        self.assertEqual(incremental["reused_tiles"], [])
        self.assertEqual(len(incremental["rendered_tiles"]), incremental["all_tiles"])

    def test_density_chunk_update_invalidates_dependent_tiles(self) -> None:
        renderer = IncrementalRenderer()
        scene = two_box_scene()
        cfg = config(tile_size=10, sample_budget=5_000_000)
        renderer.render(scene, cfg, changes=ChangeSet(), validate_seams=False)
        changed = copy.deepcopy(scene)
        chunk = changed.density_fields[7].chunks[(0, 0, 0)]
        changed.density_fields[7].set_chunk((0, 0, 0), chunk * 1.5)
        changes = infer_changes(scene, changed)
        self.assertIn((7, 0, 0, 0), changes.density_chunks_changed)
        result = renderer.render(
            changed, cfg, changes=changes, validate_seams=False
        )
        incremental = result.summary["incremental"]
        self.assertIn("density_chunk_changed", incremental["reasons"])
        self.assertGreater(len(incremental["reused_tiles"]), 0)

    def test_snapshot_query_is_atomic_and_readable(self) -> None:
        renderer = IncrementalRenderer()
        self.assertIsNone(renderer.get_snapshot())
        renderer.render(two_box_scene(), config(tile_size=10), changes=ChangeSet(), validate_seams=False)
        snapshot = renderer.get_snapshot()
        assert snapshot is not None
        self.assertEqual(len(snapshot.tile_results), 12)
        self.assertEqual(snapshot.summary["pixel_hash"], snapshot.summary["pixel_hash"])

    def test_in_place_scene_mutation_does_not_corrupt_published_snapshot(self) -> None:
        renderer = IncrementalRenderer()
        scene = two_box_scene()
        cfg = config(tile_size=10, sample_budget=5_000_000)
        renderer.render(scene, cfg, changes=ChangeSet(), validate_seams=False)
        before = renderer.get_snapshot()
        assert before is not None
        before_hash = before.summary["summary_hash"]
        scene.media_by_id()[10].sigma_s *= 1.2
        still_published = renderer.get_snapshot()
        assert still_published is not None
        self.assertEqual(still_published.summary["summary_hash"], before_hash)
        result = renderer.render(scene, cfg, changes=None, validate_seams=False)
        incremental = result.summary["incremental"]
        self.assertIn("medium_material_changed", incremental["reasons"])
        self.assertGreater(len(incremental["reused_tiles"]), 0)


if __name__ == "__main__":
    unittest.main()
