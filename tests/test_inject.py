import unittest

from dualwrite.config import InjectionConfig, InterruptionConfig
from dualwrite.inject import DeliveryScheduler


class SchedulerTests(unittest.TestCase):
    def test_duplicate_delivery_preserves_change_identity(self):
        scheduler = DeliveryScheduler(
            InjectionConfig(
                enabled=True,
                duplicate_rate=1.0,
                delay_min_ms=0,
                delay_max_ms=0,
                seed=1,
            )
        )
        deliveries = scheduler.enqueue(
            "east",
            "south",
            {
                "change_id": 7,
                "table_name": "members",
                "row_id": 2,
                "row_version": 3,
            },
        )
        self.assertEqual(len(deliveries), 2)
        self.assertEqual({item.change_key for item in deliveries}, {"east:7"})
        self.assertEqual(deliveries[1].duplicate_of, deliveries[0].delivery_id)

    def test_interruption_defers_delivery(self):
        scheduler = DeliveryScheduler(
            InjectionConfig(
                enabled=True,
                delay_min_ms=0,
                delay_max_ms=0,
                interruption=InterruptionConfig(
                    start_after_seconds=0,
                    duration_seconds=10,
                    repeat_every_seconds=0,
                ),
            )
        )
        deliveries = scheduler.enqueue(
            "south",
            "east",
            {
                "change_id": 1,
                "table_name": "orders",
                "row_id": 1,
                "row_version": 2,
            },
        )
        self.assertIsNone(scheduler.pop_ready())
        self.assertGreater(deliveries[0].due_at, 0)


if __name__ == "__main__":
    unittest.main()
