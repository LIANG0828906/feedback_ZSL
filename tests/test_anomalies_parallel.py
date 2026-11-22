from __future__ import annotations

import tempfile
import threading
import time
import unittest
from pathlib import Path

import numpy as np

from mshdr.errors import WorkbenchError
from mshdr.params import WorkbenchParams
from mshdr.pipeline import WorkbenchEngine
from mshdr.segments import PixelBudget, SegmentState

from support import generate_single_scene


class AnomalyParallelTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls._temp = tempfile.TemporaryDirectory()
        cls.root = Path(cls._temp.name)
        cls.index = generate_single_scene(cls.root)
        cls.scene = cls.index.load_scene(cls.index.scene_records[0]["scene_id"])
        cls.params = WorkbenchParams(max_workers=4, max_pixels_per_thread=250_000)
        reference = WorkbenchEngine(cls.scene, cls.params)
        reference.run(mode="reference")
        cls.reference = reference.assemble()

    @classmethod
    def tearDownClass(cls) -> None:
        cls._temp.cleanup()

    def test_bad_segment_fails_in_isolation_and_can_retry(self) -> None:
        engine = WorkbenchEngine(self.scene, self.params)
        with self.assertRaises(WorkbenchError) as context:
            engine.run(bad_segment=4, mode="bad")
        self.assertEqual(context.exception.code, "E_SEG")
        self.assertEqual(engine.state_machine.records[4].state, SegmentState.FAILED)
        self.assertEqual(engine.committed_count, engine.total_count - 1)
        self.assertNotIn(4, engine.committed_arrays)
        with tempfile.TemporaryDirectory() as directory:
            engine.run(output_dir=Path(directory) / "out", mode="retry")
        assembled = engine.assemble()
        for name in assembled:
            self.assertTrue(np.array_equal(assembled[name], self.reference[name]), name)

    def test_parallel_and_serial_outputs_are_pixel_identical(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            serial = WorkbenchEngine(self.scene, self.params.with_updates(max_workers=1))
            serial_result = serial.run(output_dir=Path(directory) / "serial", mode="serial")
            parallel = WorkbenchEngine(self.scene, self.params.with_updates(max_workers=4))
            parallel_result = parallel.run(output_dir=Path(directory) / "parallel", mode="parallel")
        for name in self.reference:
            self.assertTrue(np.array_equal(serial.assemble()[name], parallel.assemble()[name]), name)
            self.assertTrue(np.array_equal(serial.assemble()[name], self.reference[name]), name)
        self.assertEqual(serial_result.parallelism["mode"], "serial")
        self.assertEqual(parallel_result.parallelism["mode"], "parallel")
        self.assertLessEqual(
            parallel_result.parallelism["budget"]["peak_reserved_pixels"],
            parallel_result.parallelism["budget"]["capacity_pixels"],
        )

    def test_pixel_budget_blocks_instead_of_dropping(self) -> None:
        budget = PixelBudget(max_workers=1, max_pixels_per_thread=100)
        acquired = threading.Event()
        released = threading.Event()

        def first() -> None:
            budget.acquire(100)
            acquired.set()
            released.wait(timeout=2.0)
            budget.release(100)

        def second() -> None:
            budget.acquire(50)
            budget.release(50)

        first_thread = threading.Thread(target=first)
        first_thread.start()
        self.assertTrue(acquired.wait(timeout=1.0))
        second_thread = threading.Thread(target=second)
        second_thread.start()
        time.sleep(0.05)
        self.assertEqual(budget.used, 100)
        self.assertGreaterEqual(budget.blocked_reservations, 1)
        released.set()
        first_thread.join(timeout=2.0)
        second_thread.join(timeout=2.0)
        self.assertEqual(budget.used, 0)
        self.assertLessEqual(budget.peak, budget.capacity)

    def test_too_small_thread_buffer_is_parameter_error(self) -> None:
        engine = WorkbenchEngine(
            self.scene,
            WorkbenchParams(max_workers=2, max_pixels_per_thread=200),
        )
        with self.assertRaises(WorkbenchError) as context:
            engine.run(mode="tiny-buffer")
        self.assertEqual(context.exception.code, "E_PARA")
        self.assertEqual(context.exception.field, "max_pixels_per_thread")


if __name__ == "__main__":
    unittest.main()

