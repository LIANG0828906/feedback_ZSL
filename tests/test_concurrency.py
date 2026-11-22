from __future__ import annotations

import tempfile
import unittest

from haptics.codec import encode_stream, encode_stream_parallel
from haptics.errors import ErrorCode, HapticError
from haptics.limits import ResourceLimits
from haptics.pipeline import compare_results, run_realtime
from haptics.sample import sample_capabilities, sample_events
from haptics.scheduler import schedule_parallel, schedule_serial
from haptics.transport import FaultPlan, MockDeviceService


class ConcurrencyTests(unittest.TestCase):
    def test_bounded_parallel_pipeline_matches_serial_pipeline(self) -> None:
        caps = sample_capabilities()
        stream = sample_events()
        limits = ResourceLimits(
            max_parallelism=2,
            max_events_per_task=7,
            max_packets_per_task=3,
        )
        serial_schedule = schedule_serial(stream, caps)
        serial_encode = encode_stream(serial_schedule, caps)
        parallel_schedule = schedule_parallel(stream, caps, limits)
        parallel_encode = encode_stream_parallel(parallel_schedule, caps, limits)
        self.assertEqual(
            [packet.data for packet in serial_encode],
            [packet.data for packet in parallel_encode],
        )
        self.assertEqual(
            [decision.to_dict() for decision in serial_schedule.decisions],
            [decision.to_dict() for decision in parallel_schedule.decisions],
        )

        plan = FaultPlan.sample()
        with tempfile.TemporaryDirectory() as d1, tempfile.TemporaryDirectory() as d2:
            with MockDeviceService(port=0, fault_plan=plan) as s1:
                serial = run_realtime(
                    stream,
                    caps,
                    service=s1,
                    parallel=False,
                    run_id="serial",
                )
            with MockDeviceService(port=0, fault_plan=plan) as s2:
                parallel = run_realtime(
                    stream,
                    caps,
                    service=s2,
                    parallel=True,
                    limits=limits,
                    run_id="parallel",
                )
            comparison = compare_results(serial, parallel)
            self.assertTrue(comparison.equal, comparison.divergence)

    def test_resource_limits_are_validated(self) -> None:
        with self.assertRaises(HapticError) as context:
            ResourceLimits(max_parallelism=0).validate()
        self.assertEqual(context.exception.issue.code, ErrorCode.E_PARA)
        self.assertEqual(context.exception.issue.field, "max_parallelism")


if __name__ == "__main__":
    unittest.main()

