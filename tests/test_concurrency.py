from __future__ import annotations

import unittest

from gamepad_studio.concurrency import (
    BoundedEventBuffer,
    BufferOverflowError,
    ReplayCoordinator,
)
from gamepad_studio.errors import ParameterError

from tests.helpers import load_bundle


class ConcurrencyTests(unittest.TestCase):
    def test_parallel_replays_are_deterministic(self) -> None:
        with ReplayCoordinator(max_parallel=2, event_buffer_limit=64) as coordinator:
            report = coordinator.parallel_determinism_check(
                load_bundle("dual_stick"),
                runs=4,
            )
        self.assertTrue(report.all_equal)
        self.assertEqual(len(set(report.hashes)), 1)
        self.assertEqual(report.max_parallel, 2)
        self.assertEqual(report.event_buffer_limit, 64)

    def test_bounded_event_buffer_enforces_limit(self) -> None:
        buffer = BoundedEventBuffer[int](2)
        buffer.push(1)
        buffer.push(2)
        with self.assertRaises(BufferOverflowError) as caught:
            buffer.push(3)
        self.assertEqual(caught.exception.code, "E_CONCURRENCY")
        self.assertEqual(tuple(buffer.drain()), (1, 2))
        self.assertEqual(buffer.max_observed, 2)

    def test_invalid_worker_limits_return_parameter_error(self) -> None:
        with self.assertRaises(ParameterError):
            ReplayCoordinator(max_parallel=5)
        with self.assertRaises(ParameterError):
            ReplayCoordinator(event_buffer_limit=0)


if __name__ == "__main__":
    unittest.main()

