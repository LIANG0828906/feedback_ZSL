from __future__ import annotations

import unittest

from volrender.errors import BusinessError, ErrorCode
from volrender.renderer import render_scene
from volrender.scheduler import partition_screen, schedule_tiles

from tests.helpers import config, tiny_scene


class SchedulerTests(unittest.TestCase):
    def test_partition_is_row_major_and_stable(self) -> None:
        tiles = partition_screen(20, 13, 8)
        self.assertEqual(
            [(tile.x, tile.y, tile.width, tile.height) for tile in tiles],
            [(0, 0, 8, 8), (8, 0, 8, 8), (16, 0, 4, 8), (0, 8, 8, 5), (8, 8, 8, 5), (16, 8, 4, 5)],
        )
        self.assertEqual([tile.sequence for tile in tiles], list(range(6)))

    def test_budget_degradation_is_traced_and_stably_sorted(self) -> None:
        scene = tiny_scene()
        tiles = partition_screen(scene.width, scene.height, 8)
        high = config(sample_budget=2_000_000)
        baseline = schedule_tiles(scene, tiles, high)
        max_samples = max(admission.sample_count for admission in baseline.admitted)
        low = config(sample_budget=max(10, int(max_samples * 0.45)))
        result = schedule_tiles(scene, tiles, low)
        self.assertFalse(result.waiting)
        self.assertEqual(
            [admission.tile.sequence for admission in result.admitted],
            sorted(admission.tile.sequence for admission in result.admitted),
        )
        self.assertTrue(
            any(admission.degradation_index > 0 for admission in result.admitted)
        )
        decisions = [item for item in result.decisions if item["status"] == "ADMITTED"]
        self.assertTrue(any(len(item["trials"]) > 1 for item in decisions))
        self.assertTrue(
            all(item["sample_count"] <= low.sample_budget for item in decisions)
        )
        self.assertTrue(all(item["budget_used_ratio"] <= 1.0 for item in decisions))

    def test_rejected_tiles_enter_waiting_queue(self) -> None:
        scene = tiny_scene()
        tiles = partition_screen(scene.width, scene.height, 8)
        low = config(sample_budget=1)
        result = schedule_tiles(scene, tiles, low)
        self.assertEqual(len(result.waiting), len(tiles))
        self.assertTrue(all(item["status"] == "WAITING" for item in result.decisions))
        with self.assertRaises(BusinessError) as caught:
            render_scene(scene, low, validate_seams=False)
        self.assertEqual(caught.exception.code, ErrorCode.BUDGET_EXCEEDED)
        self.assertEqual(
            len(caught.exception.details["waiting"]), len(tiles)
        )

    def test_configured_parallelism_is_not_raised(self) -> None:
        scene = tiny_scene()
        cfg = config(max_parallelism=1, sample_budget=2_000_000)
        result = render_scene(scene, cfg, validate_seams=False)
        self.assertEqual(result.summary["parallelism"], 1)
        self.assertTrue(
            all(record["parallelism"] == 1 for record in result.trace if record["event"] == "schedule")
        )


if __name__ == "__main__":
    unittest.main()
