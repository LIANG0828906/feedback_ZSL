from __future__ import annotations

import tempfile
import threading
import time
import unittest
from pathlib import Path

from tileforge.budget import choose_profile, estimate_tile_memory
from tileforge.config import MemoryConfig
from tileforge.errors import E_BUDGET_EXCEEDED, TileForgeError
from tileforge.model import SequenceHeader, TransferCode
from tileforge.scheduler import ConversionScheduler
from tileforge.trace import TraceLog


def _header() -> SequenceHeader:
    return SequenceHeader(
        width=16,
        height=16,
        tiles_x=2,
        tiles_y=2,
        bit_depth=16,
        channels=3,
        source_transfer=TransferCode.SDR_GAMMA,
        target_transfer=TransferCode.SDR_GAMMA,
        conversion_id=1,
        block_count=4,
        data_length=0,
    )


class BudgetAndSchedulerTests(unittest.TestCase):
    def test_profile_downgrade_and_rejection(self) -> None:
        header = _header()
        full = estimate_tile_memory(header, "full", 0).total_bytes
        banded = estimate_tile_memory(header, "banded", 0).total_bytes
        memory = MemoryConfig(
            parallelism=2,
            tile_budget_bytes=full - 1,
            run_budget_bytes=full * 2,
            downgrade_profiles=("full", "banded", "streaming"),
            estimator_overhead_bytes=0,
        )
        chosen, attempts = choose_profile(header, memory)
        self.assertEqual(chosen.profile, "banded")
        self.assertEqual([attempt.profile for attempt in attempts], ["full", "banded"])
        self.assertLess(banded, full)
        rejected = MemoryConfig(
            parallelism=2,
            tile_budget_bytes=1,
            run_budget_bytes=2,
            downgrade_profiles=("full", "banded", "streaming"),
            estimator_overhead_bytes=0,
        )
        with self.assertRaises(TileForgeError) as context:
            choose_profile(header, rejected)
        self.assertEqual(context.exception.code, E_BUDGET_EXCEEDED)
        self.assertFalse(context.exception.details["precision_changed"])

    def test_run_budget_queues_and_commits_in_input_order(self) -> None:
        header = _header()
        estimate = estimate_tile_memory(header, "full", 0).total_bytes
        memory = MemoryConfig(
            parallelism=3,
            tile_budget_bytes=estimate,
            run_budget_bytes=estimate,
            downgrade_profiles=("full",),
            estimator_overhead_bytes=0,
        )
        with tempfile.TemporaryDirectory() as temp:
            trace = TraceLog(Path(temp) / "trace.jsonl")
            scheduler = ConversionScheduler(header, memory, trace)
            active = 0
            maximum_active = 0
            lock = threading.Lock()
            commits: list[int] = []

            def task(tile_id: int, profile: str) -> int:
                nonlocal active, maximum_active
                with lock:
                    active += 1
                    maximum_active = max(maximum_active, active)
                time.sleep(0.01 * (4 - tile_id))
                with lock:
                    active -= 1
                return tile_id

            def commit(tile_id: int, value: int) -> None:
                commits.append(value)

            results = scheduler.run([0, 1, 2, 3], task, commit)
            self.assertEqual(maximum_active, 1)
            self.assertEqual(commits, [0, 1, 2, 3])
            self.assertEqual(results, [0, 1, 2, 3])
            events = [record["event"] for record in trace.records]
            self.assertIn("task_queued", events)
            self.assertIn("scheduler_end", events)

    def test_out_of_order_completion_still_commits_stably(self) -> None:
        header = _header()
        estimate = estimate_tile_memory(header, "full", 0).total_bytes
        memory = MemoryConfig(
            parallelism=2,
            tile_budget_bytes=estimate,
            run_budget_bytes=estimate * 2,
            downgrade_profiles=("full",),
            estimator_overhead_bytes=0,
        )
        with tempfile.TemporaryDirectory() as temp:
            scheduler = ConversionScheduler(header, memory, TraceLog(Path(temp) / "trace.jsonl"))
            commits: list[int] = []

            def task(tile_id: int, profile: str) -> int:
                time.sleep(0.06 if tile_id == 0 else 0.005)
                return tile_id

            scheduler.run([0, 1, 2], task, lambda tile_id, value: commits.append(value))
            self.assertEqual(commits, [0, 1, 2])


if __name__ == "__main__":
    unittest.main()
