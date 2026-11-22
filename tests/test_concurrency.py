from __future__ import annotations

import tempfile
import threading
import time
import unittest
from pathlib import Path

from tickarena import BoundedParallel, CheckpointStore, E_OK, E_ORDER


class ConcurrencyTests(unittest.TestCase):
    def test_bounded_pool_limits_workers_and_preserves_result_order(self) -> None:
        active = 0
        maximum_active = 0
        lock = threading.Lock()

        def work(value: int) -> int:
            nonlocal active, maximum_active
            with lock:
                active += 1
                maximum_active = max(maximum_active, active)
            time.sleep(0.01)
            with lock:
                active -= 1
            return value * value

        results = BoundedParallel(max_workers=2, queue_limit=3).map(
            work,
            range(12),
        )
        self.assertEqual(results, [value * value for value in range(12)])
        self.assertLessEqual(maximum_active, 2)

    def test_concurrent_checkpoint_commits_leave_latest_tick(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            store = CheckpointStore(tmp)

            def commit(tick: int):
                time.sleep(0.01 * (3 - tick))
                return tick, store.commit({"source_tick": tick}, tick)

            outcomes = BoundedParallel(max_workers=2, queue_limit=2).map(
                commit,
                [1, 2],
            )
            codes = {result.code for _tick, result in outcomes}
            self.assertTrue(codes)
            self.assertTrue(codes <= {E_OK, E_ORDER})
            loaded = store.load()
            self.assertTrue(loaded.result.ok)
            self.assertEqual(loaded.envelope["engine_tick"], 2)
            self.assertEqual(loaded.payload["source_tick"], 2)
            self.assertFalse(
                any(path.name.endswith(".tmp") for path in Path(tmp).iterdir())
            )


if __name__ == "__main__":
    unittest.main()

