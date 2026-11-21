from __future__ import annotations

import threading
import time
import unittest

from loudness_diag.errors import E_BUSY, E_CONFIG_INVALID, DiagnosticError
from loudness_diag.scheduler import BlockScheduler, SchedulerConfig


class SchedulerTests(unittest.TestCase):
    def test_duplicate_material_start_returns_busy(self) -> None:
        config = SchedulerConfig(
            parallelism=2,
            memory_budget_bytes=2_000,
            block_memory_bytes=1_000,
        )
        started = threading.Event()
        release = threading.Event()
        failures: list[BaseException] = []

        def blocking_task(block_index: int) -> int:
            started.set()
            release.wait(timeout=5)
            return block_index

        with BlockScheduler(config) as scheduler:
            thread = threading.Thread(
                target=lambda: scheduler.run("busy-asset", [0, 1], blocking_task)
            )
            thread.start()
            self.assertTrue(started.wait(timeout=2))
            with self.assertRaises(DiagnosticError) as context:
                scheduler.run("busy-asset", [2], blocking_task)
            self.assertEqual(context.exception.code, E_BUSY)
            release.set()
            thread.join(timeout=5)
        self.assertFalse(failures)

    def test_config_rejects_oversized_block_memory(self) -> None:
        with self.assertRaises(DiagnosticError) as context:
            SchedulerConfig(
                parallelism=1,
                memory_budget_bytes=100,
                block_memory_bytes=101,
            )
        self.assertEqual(context.exception.code, E_CONFIG_INVALID)

    def test_submission_order_and_memory_wait_are_stable(self) -> None:
        config = SchedulerConfig(
            parallelism=4,
            memory_budget_bytes=2_000,
            block_memory_bytes=1_000,
        )
        seen: list[int] = []
        active = 0
        maximum_active = 0
        lock = threading.Lock()
        barrier = threading.Barrier(2)

        def task(index: int) -> int:
            nonlocal active, maximum_active
            with lock:
                active += 1
                maximum_active = max(maximum_active, active)
            barrier.wait(timeout=2)
            time.sleep(0.001)
            with lock:
                active -= 1
            seen.append(index)
            return index * 2

        with BlockScheduler(config) as scheduler:
            results, trace = scheduler.run("order", [3, 1, 2, 0], task)
        self.assertEqual(results, [0, 2, 4, 6])
        starts = [
            event
            for event in trace
            if event["event"] == "wave_started"
        ]
        self.assertEqual(starts[0]["block_indices"], [0, 1])
        self.assertEqual(starts[1]["block_indices"], [2, 3])
        self.assertEqual(maximum_active, 2)
        self.assertTrue(
            all(
                event["wait_reason"] == "memory_budget"
                for event in trace
                if event["event"] == "task_queued" and event["wave_index"] > 0
            )
        )


if __name__ == "__main__":
    unittest.main()

