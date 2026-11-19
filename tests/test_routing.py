from __future__ import annotations

import unittest

from iovflow.routing import build_routing_plan


class RoutingTests(unittest.TestCase):
    def test_skew_is_split_and_cap_is_respected(self) -> None:
        trips = {f"V{i:03d}": {f"T{j}": 25 for j in range(3)} for i in range(2, 40)}
        trips["V001"] = {f"H{j}": 650 for j in range(4)}
        samples = []
        for vid, tr in trips.items():
            for trip, count in tr.items():
                samples.extend((vid, trip, f"{vid}:{trip}:{k}") for k in range(count))
        total = len(samples)
        plan = build_routing_plan(trips, total, 2, 4, 0.40, samples=samples)
        self.assertIn("V001", plan.split_vehicles)
        self.assertGreater(plan.vehicles["V001"].part_count, 1)
        self.assertLessEqual(max(plan.shard_counts) / total, 0.40 + 1e-12)
        counts = [0] * plan.shard_count
        for vid, trip, fp in samples:
            counts[plan.shard_for(vid, trip, fp)] += 1
        self.assertEqual(counts, plan.shard_counts)

    def test_single_trip_hot_key_uses_event_split(self) -> None:
        trips = {f"V{i:02d}": {"T": 20} for i in range(2, 15)}
        trips["V01"] = {"ONE": 1800}
        samples = []
        for vid, tr in trips.items():
            for trip, count in tr.items():
                samples.extend((vid, trip, f"{vid}:{trip}:{k}") for k in range(count))
        plan = build_routing_plan(trips, len(samples), 2, 4, 0.40, samples=samples)
        route = plan.vehicles["V01"]
        self.assertTrue(route.event_split)
        self.assertGreater(route.part_count, 1)
        self.assertLessEqual(max(plan.shard_counts) / len(samples), 0.40 + 1e-12)

    def test_payment_and_backfill_same_trip_share_a_shard(self) -> None:
        trips = {f"V{i:02d}": {f"T{j}": 10 for j in range(3)} for i in range(2, 20)}
        trips["V01"] = {f"H{j}": 100 for j in range(4)}
        samples = []
        for vid, tr in trips.items():
            for trip, count in tr.items():
                samples.extend((vid, trip, f"{vid}:{trip}:{k}") for k in range(count))
        plan = build_routing_plan(trips, len(samples), 2, 4, 0.40, samples=samples)
        for trip in ("H0", "H1", "H2", "H3"):
            a = plan.shard_for("V01", trip, "payment-event")
            b = plan.shard_for("V01", trip, "backfill-event")
            self.assertEqual(a, b)

    def test_plan_is_deterministic(self) -> None:
        trips = {"A": {"T1": 60, "T2": 40}, "B": {"T1": 30}, "C": {"T1": 20}}
        samples = []
        for vid, tr in trips.items():
            for trip, count in tr.items():
                samples.extend((vid, trip, f"{vid}:{trip}:{k}") for k in range(count))
        a = build_routing_plan(trips, len(samples), 2, 4, 0.4, samples=samples)
        b = build_routing_plan(trips, len(samples), 2, 4, 0.4, samples=samples)
        self.assertEqual(a.to_dict(), b.to_dict())


if __name__ == "__main__":
    unittest.main()

