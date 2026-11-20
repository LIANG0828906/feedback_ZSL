from __future__ import annotations

import unittest

from glyphforge.errors import ErrorCode, GlyphForgeError
from glyphforge.parser import parse_font
from glyphforge.raster import RasterOptions, prepare_geometry
from glyphforge.samples import build_font_specs
from glyphforge.scheduler import (
    BudgetConfig,
    DeterministicScheduler,
    GlyphSubmission,
    TileTask,
    temporary_scanline_bytes,
)


def _tasks(task_count: int = 1) -> list[TileTask]:
    font = parse_font(build_font_specs()["holes.json"])
    options = RasterOptions(pixel_size=20, units_per_em=1000)
    geometry = prepare_geometry(font.glyphs["eight"], options)
    tasks = []
    for index in range(task_count):
        tasks.append(
            TileTask(
                glyph_name="eight",
                glyph_index=0,
                row=index // 2,
                col=index % 2,
                x=geometry.x0 + (index % 2) * 4,
                y=geometry.y0 + (index // 2) * 4,
                width=4,
                height=4,
                geometry=geometry,
                sample_count=4 * 4 * 16,
                coverage_buffer_bytes=4 * 4 * 4,
            )
        )
    return tasks


class SchedulerTests(unittest.TestCase):
    def test_batch_degradation_is_declared(self) -> None:
        budget = BudgetConfig(
            parallelism=2,
            global_memory_bytes=1024 * 1024,
            max_task_samples=10_000,
            max_task_memory_bytes=100_000,
            sampling_batch_size=2048,
            min_sampling_batch_size=128,
        )
        scheduler = DeterministicScheduler(budget)
        plan = scheduler.plan(
            [GlyphSubmission(name="eight", glyph_index=0, tasks=_tasks())]
        )
        task_plan = plan.task_order[0]
        self.assertTrue(task_plan.accepted)
        self.assertEqual(task_plan.measurement.requested_batch_size, 2048)
        self.assertEqual(task_plan.measurement.effective_batch_size, 1024)
        self.assertIn(2048, task_plan.measurement.degradation_attempts)
        self.assertLess(
            task_plan.measurement.temporary_scanline_bytes,
            temporary_scanline_bytes(2048),
        )

    def test_rejected_task_reports_measurement_and_keeps_glyph_isolated(self) -> None:
        budget = BudgetConfig(
            parallelism=2,
            global_memory_bytes=1024 * 1024,
            max_task_samples=10_000,
            max_task_memory_bytes=1000,
            sampling_batch_size=2048,
            min_sampling_batch_size=256,
        )
        scheduler = DeterministicScheduler(budget)
        plan = scheduler.plan(
            [GlyphSubmission(name="eight", glyph_index=0, tasks=_tasks(2))]
        )
        self.assertFalse(plan.glyphs[0].accepted)
        self.assertEqual(len(plan.task_order), 0)
        self.assertEqual(len(plan.rejected_tasks), 2)
        error = plan.glyphs[0].rejection
        self.assertIsNotNone(error)
        self.assertEqual(error.code, ErrorCode.E_BUDGET_EXCEEDED)
        self.assertEqual(error.details["glyph"], "eight")
        self.assertIn("measurement", error.details)
        self.assertEqual(error.details["tile"], {"row": 0, "col": 0})

    def test_duplicate_glyph_submission_is_busy(self) -> None:
        scheduler = DeterministicScheduler(BudgetConfig())
        with self.assertRaises(GlyphForgeError) as caught:
            scheduler.plan(
                [
                    GlyphSubmission(name="eight", glyph_index=0, tasks=_tasks()),
                    GlyphSubmission(name="eight", glyph_index=1, tasks=_tasks()),
                ]
            )
        self.assertEqual(caught.exception.code, ErrorCode.E_BUSY)

    def test_stable_submission_order_and_parallel_waves(self) -> None:
        budget = BudgetConfig(
            parallelism=2,
            global_memory_bytes=1024 * 1024,
            max_task_samples=10_000,
            max_task_memory_bytes=1024 * 1024,
        )
        scheduler = DeterministicScheduler(budget)
        second_tasks = _tasks(3)
        for task in second_tasks:
            object.__setattr__(task, "glyph_name", "O")
            object.__setattr__(task, "glyph_index", 1)
        plan = scheduler.plan(
            [
                GlyphSubmission(name="eight", glyph_index=0, tasks=_tasks(2)),
                GlyphSubmission(name="O", glyph_index=1, tasks=second_tasks),
            ]
        )
        self.assertEqual([len(wave) for wave in plan.waves], [2, 2, 1])
        self.assertEqual(
            [task.submit_order for task in plan.task_order],
            [0, 1, 2, 3, 4],
        )
        self.assertTrue(all(len(wave) <= 2 for wave in plan.waves))


if __name__ == "__main__":
    unittest.main()
