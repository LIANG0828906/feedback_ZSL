from __future__ import annotations

import threading
import unittest

from sync_kernel.concurrency import (
    BoundedExecutor,
    ConcurrencyLimits,
    run_parallel_version_validation,
)
from sync_kernel.errors import E_RESOURCE
from sync_kernel.samples import build_sample_model


class ConcurrencyTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        (
            cls.bundle,
            cls.context,
            cls.initial,
            cls.inputs,
            cls.states,
        ) = build_sample_model(ticks=20)

    def test_parallel_versions_have_stable_bytes(self) -> None:
        limits = ConcurrencyLimits(
            max_workers=4,
            max_queue=8,
            max_buffer_bytes=4 * 1024 * 1024,
        )
        first, stats = run_parallel_version_validation(
            context=self.context,
            initial_state=self.initial,
            inputs_by_tick=self.inputs,
            expected_states=self.states,
            versions=[1, 2, 3],
            limits=limits,
        )
        second, _ = run_parallel_version_validation(
            context=self.context,
            initial_state=self.initial,
            inputs_by_tick=self.inputs,
            expected_states=self.states,
            versions=[1, 2, 3],
            limits=limits,
        )
        for version in (1, 2, 3):
            self.assertEqual(first[version].final_hash, self.states[-1].snapshot_hash)
            self.assertIsNone(first[version].first_divergence)
            self.assertEqual(
                first[version].frame_bytes_hashes,
                second[version].frame_bytes_hashes,
            )
        self.assertLessEqual(stats["max_observed_pending"], limits.max_queue)
        self.assertLessEqual(
            stats["max_observed_buffer_bytes"],
            limits.max_buffer_bytes,
        )

    def test_queue_and_memory_bounds_return_e_resource(self) -> None:
        release = threading.Event()
        executor = BoundedExecutor(
            ConcurrencyLimits(max_workers=1, max_queue=1, max_buffer_bytes=1024)
        )
        first = executor.submit(release.wait, estimated_bytes=1)
        with self.assertRaises(Exception) as queue_error:
            executor.submit(lambda: None, estimated_bytes=1)
        self.assertEqual(getattr(queue_error.exception, "code", ""), E_RESOURCE)
        release.set()
        first.result(timeout=5)
        executor.close()

        small = BoundedExecutor(
            ConcurrencyLimits(max_workers=1, max_queue=1, max_buffer_bytes=128)
        )
        with self.assertRaises(Exception) as memory_error:
            small.submit(lambda: None, estimated_bytes=129)
        self.assertEqual(getattr(memory_error.exception, "code", ""), E_RESOURCE)
        small.close()


if __name__ == "__main__":
    unittest.main()

