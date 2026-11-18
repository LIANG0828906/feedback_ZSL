from __future__ import annotations

import json
import tempfile
import threading
import time
import unittest
from pathlib import Path
from typing import Any

from ordergroup.backends import (
    SimulatedKafkaBackend,
    crc32c,
    decode_record_set,
    encode_record_batch,
)
from ordergroup.config import Config
from ordergroup.coordinator import Coordinator
from ordergroup.errors import ApiError
from ordergroup.generator import GenerationOptions, generate_events


def make_config(
    root: Path,
    *,
    threshold: int = 99,
    cooldown_ms: int = 0,
    revoke_ack_timeout_ms: int = 0,
) -> Config:
    config = Config.load(None)
    config.data["backend"]["kind"] = "simulator"
    config.data["backend"]["simulator_dir"] = str(root / "sim")
    config.data["storage"]["path"] = str(root / "coordinator.sqlite3")
    config.data["kafka"]["partitions"] = 12
    config.data["rebalance"]["max_partitions_per_rebalance"] = threshold
    config.data["rebalance"]["cooldown_ms"] = cooldown_ms
    config.data["rebalance"]["partition_threshold_delay_ms"] = 0
    config.data["rebalance"]["suppression_max_delay_ms"] = 0
    config.data["rebalance"]["revoke_ack_timeout_ms"] = revoke_ack_timeout_ms
    return config


class CoreTests(unittest.TestCase):
    def setUp(self) -> None:
        self.tempdir = tempfile.TemporaryDirectory()
        self.root = Path(self.tempdir.name)
        self.config = make_config(self.root)
        generate_events(
            GenerationOptions(
                count=120,
                seed=290,
                topic="order-events",
                partitions=12,
                backend="simulator",
                bootstrap_servers="127.0.0.1:9092",
                output_dir=self.root / "sim",
            )
        )
        self.backend = SimulatedKafkaBackend(self.root / "sim", 12)
        self.coordinator = Coordinator(self.config, self.backend)
        self._coordinators = [self.coordinator]

    def tearDown(self) -> None:
        for coordinator in reversed(self._coordinators):
            coordinator.stop()
        self.tempdir.cleanup()

    def join(self, member_id: str, timeout_ms: int = 10000) -> dict[str, Any]:
        return self.coordinator.join(
            {
                "member_id": member_id,
                "session_timeout_ms": timeout_ms,
                "heartbeat_interval_ms": 100,
            }
        )

    def test_three_members_get_exclusive_balanced_partitions(self) -> None:
        first = self.join("w1")
        self.assertEqual(len(first["owned_partitions"]), 12)
        second = self.join("w2")
        third = self.join("w3")
        self.assertEqual(third["generation"], 3)
        status = self.coordinator.assignments_status()
        owners: dict[int, str] = {}
        for assignment in status["assignments"]:
            partition = assignment["partition"]
            self.assertNotIn(partition, owners)
            owners[partition] = assignment["member_id"]
        self.assertEqual(set(owners), set(range(12)))
        counts = {
            member: sum(1 for owner in owners.values() if owner == member)
            for member in ("w1", "w2", "w3")
        }
        self.assertEqual(counts, {"w1": 4, "w2": 4, "w3": 4})
        self.assertEqual(second["member_id"], "w2")

    def test_heartbeat_timeout_reclaims_and_quarantines(self) -> None:
        self.join("w1", timeout_ms=50)
        time.sleep(0.08)
        with self.coordinator.lock:
            self.coordinator._tick_locked(time.time_ns() // 1_000_000)
            self.assertEqual(self.coordinator.members["w1"].status, "timed_out")
            self.assertEqual(self.coordinator.assignments, {})
            self.assertEqual(set(self.coordinator.quarantine), set(range(12)))
        commit = self.coordinator.offset_status()
        self.assertTrue(all(item["committed_offset"] == 0 for item in commit["offsets"]))
        with self.assertRaises(ApiError) as caught:
            self.coordinator.commit("w1", {"partition": 0, "offset": 1})
        self.assertIn(caught.exception.code, ("MEMBER_NOT_ACTIVE", "UNKNOWN_MEMBER"))
        timeout_metrics = self.coordinator.metrics()
        self.assertIn(
            'order_group_member_heartbeat_timeouts_total{member="w1"} 1',
            timeout_metrics,
        )

    def test_suppression_keeps_old_owners_and_quarantines_lost_partitions(self) -> None:
        config = make_config(
            self.root / "suppressed",
            threshold=99,
            cooldown_ms=0,
            revoke_ack_timeout_ms=0,
        )
        backend = SimulatedKafkaBackend(self.root / "sim", 12)
        coordinator = Coordinator(config, backend)
        self._coordinators.append(coordinator)
        self.coordinator = coordinator
        try:
            coordinator.join(
                {
                    "member_id": "a",
                    "session_timeout_ms": 10000,
                    "heartbeat_interval_ms": 100,
                }
            )
            coordinator.join(
                {
                    "member_id": "b",
                    "session_timeout_ms": 10000,
                    "heartbeat_interval_ms": 100,
                }
            )
            b_partitions = set(coordinator._owned_partitions_locked("b"))
            self.assertTrue(b_partitions)
            config.data["rebalance"]["max_partitions_per_rebalance"] = 2
            config.data["rebalance"]["cooldown_ms"] = 1000
            config.data["rebalance"]["partition_threshold_delay_ms"] = 500
            config.data["rebalance"]["suppression_max_delay_ms"] = 500
            coordinator.join(
                {
                    "member_id": "c",
                    "session_timeout_ms": 10000,
                    "heartbeat_interval_ms": 100,
                }
            )
            status = coordinator.status()
            self.assertEqual(status["phase"], "suppressed")
            self.assertIn("partition_threshold", status["pending_rebalance"]["suppression_reason"])
            # Existing members keep consuming stable partitions while suppressed.
            self.coordinator.commit("a", {"partition": 0, "offset": 3})
            # A timed-out member's partition remains quarantined during suppression.
            with coordinator.lock:
                coordinator.members["b"].last_heartbeat_ms = 0
                coordinator._tick_locked(time.time_ns() // 1_000_000)
                self.assertEqual(coordinator.members["b"].status, "timed_out")
                for partition in b_partitions:
                    self.assertNotIn(partition, coordinator.assignments)
                    self.assertIn(partition, coordinator.quarantine)
                self.assertEqual(coordinator.assignments[0].member_id, "a")
        finally:
            self.coordinator = coordinator

    def test_commit_binding_regression_and_duplicate(self) -> None:
        self.join("w1")
        self.join("w2")
        self.coordinator.commit("w1", {"partition": 0, "offset": 10})
        duplicate = self.coordinator.commit("w1", {"partition": 0, "offset": 10})
        self.assertTrue(duplicate["duplicate"])
        self.assertEqual(duplicate["committed_offset"], 10)
        with self.assertRaises(ApiError) as regression:
            self.coordinator.commit("w1", {"partition": 0, "offset": 9})
        self.assertEqual(regression.exception.code, "OFFSET_REGRESSION")
        self.assertEqual(regression.exception.numeric_code, 40005)
        with self.assertRaises(ApiError) as not_owner:
            self.coordinator.commit("w2", {"partition": 0, "offset": 11})
        self.assertEqual(not_owner.exception.code, "PARTITION_NOT_OWNED")
        queried = self.coordinator.offset_status(0)
        self.assertEqual(queried["committed_offset"], 10)
        self.assertEqual(queried["member_id"], "w1")

    def test_rebalance_waits_for_delivered_batch_to_finish(self) -> None:
        self.join("w1")
        batch = self.coordinator.fetch("w1", {"partition": 1, "max_records": 2})
        self.assertEqual(len(batch["records"]), 2)
        joined = self.join("w2")
        self.assertEqual(joined["generation"], 1)
        self.assertIsNotNone(self.coordinator.in_progress)
        with self.coordinator.lock:
            self.coordinator._tick_locked(time.time_ns() // 1_000_000)
            self.assertIsNotNone(self.coordinator.in_progress)
            self.assertEqual(self.coordinator.assignments[1].state, "revoking")
        committed = self.coordinator.commit(
            "w1",
            {
                "partition": 1,
                "offset": batch["next_commit_offset"],
                "generation": batch["generation"],
            },
        )
        self.assertEqual(committed["committed_offset"], 2)
        with self.coordinator.lock:
            self.coordinator._tick_locked(time.time_ns() // 1_000_000)
            self.assertIsNone(self.coordinator.in_progress)
            self.assertEqual(self.coordinator.generation, 2)
        owners = {
            item["partition"]: item["member_id"]
            for item in self.coordinator.assignments_status()["assignments"]
        }
        self.assertEqual(owners[1], "w2")

    def test_continuous_owner_can_commit_batch_across_generation_change(self) -> None:
        self.join("w1")
        batch = self.coordinator.fetch("w1", {"partition": 0, "max_records": 2})
        old_generation = batch["generation"]
        self.join("w2")
        status = self.coordinator.assignments_status()
        owners = {
            item["partition"]: item["member_id"] for item in status["assignments"]
        }
        self.assertEqual(owners[0], "w1")
        self.assertGreater(self.coordinator.generation, old_generation)
        committed = self.coordinator.commit(
            "w1",
            {
                "partition": 0,
                "offset": batch["next_commit_offset"],
                "generation": old_generation,
            },
        )
        self.assertEqual(committed["committed_offset"], 2)

    def test_followup_rebalance_is_merged_without_extra_generation(self) -> None:
        config = make_config(
            self.root / "cooldown",
            threshold=99,
            cooldown_ms=5000,
            revoke_ack_timeout_ms=0,
        )
        backend = SimulatedKafkaBackend(self.root / "sim", 12)
        coordinator = Coordinator(config, backend)
        self._coordinators.append(coordinator)
        self.coordinator = coordinator
        coordinator.join(
            {
                "member_id": "a",
                "session_timeout_ms": 10000,
                "heartbeat_interval_ms": 100,
            }
        )
        with coordinator.lock:
            coordinator.last_rebalance_completed_ms = (
                time.time_ns() // 1_000_000
            ) - 6000
        batch = coordinator.fetch("a", {"partition": 1, "max_records": 1})
        joined_b = coordinator.join(
            {
                "member_id": "b",
                "session_timeout_ms": 10000,
                "heartbeat_interval_ms": 100,
            }
        )
        self.assertEqual(joined_b["generation"], 1)
        self.assertIsNotNone(coordinator.in_progress)
        coordinator.join(
            {
                "member_id": "c",
                "session_timeout_ms": 10000,
                "heartbeat_interval_ms": 100,
            }
        )
        coordinator.commit(
            "a",
            {
                "partition": 1,
                "offset": batch["next_commit_offset"],
                "generation": batch["generation"],
            },
        )
        with coordinator.lock:
            now = time.time_ns() // 1_000_000
            coordinator._tick_locked(now)
            self.assertEqual(coordinator.generation, 1)
            self.assertIsNotNone(coordinator.in_progress)
            coordinator._tick_locked(now + 1)
            self.assertEqual(coordinator.generation, 2)
            self.assertIsNone(coordinator.in_progress)
            self.assertIsNotNone(coordinator.pending)
            coordinator._tick_locked(now + 2)
            self.assertEqual(coordinator.generation, 2)
            self.assertIsNone(coordinator.pending)

    def test_pause_freezes_offset_and_pins_partition_during_rebalance(self) -> None:
        self.join("w1")
        paused = self.coordinator.pause("w1", {"partition": 0})
        self.assertTrue(paused["paused"])
        self.assertEqual(paused["committed_offset"], 0)
        self.join("w2")
        status = self.coordinator.assignments_status()
        owners = {
            item["partition"]: item["member_id"] for item in status["assignments"]
        }
        self.assertEqual(owners[0], "w1")
        self.assertTrue(
            next(
                item["paused"]
                for item in status["assignments"]
                if item["partition"] == 0
            )
        )
        fetch = self.coordinator.fetch("w1", {"partition": 0})
        self.assertEqual(fetch["records"], [])
        self.assertTrue(fetch["paused"])
        with self.assertRaises(ApiError) as rejected:
            self.coordinator.commit("w1", {"partition": 0, "offset": 1})
        self.assertEqual(rejected.exception.code, "PARTITION_PAUSED")
        self.assertEqual(self.coordinator.offset_status(0)["committed_offset"], 0)
        resumed = self.coordinator.resume("w1", {"partition": 0})
        self.assertFalse(resumed["paused"])
        fetch = self.coordinator.fetch("w1", {"partition": 0})
        self.assertTrue(fetch["records"])
        self.assertEqual(fetch["fetch_offset"], 0)
        events = self.coordinator.store.recent_events(20)
        paused_events = [event for event in events if event["kind"] == "pause"]
        resumed_events = [event for event in events if event["kind"] == "resume"]
        self.assertEqual(len(paused_events), 1)
        self.assertEqual(len(resumed_events), 1)
        self.assertEqual(paused_events[0]["details"]["partition"], 0)
        self.assertEqual(paused_events[0]["details"]["member_id"], "w1")

    def test_lag_snapshot_is_not_changed_by_concurrent_commit(self) -> None:
        self.join("w1")
        self.coordinator.commit("w1", {"partition": 0, "offset": 2})
        entered = threading.Event()
        release = threading.Event()
        coordinator = self.coordinator

        class BlockingBackend:
            def earliest_offsets(self, partitions: Any) -> dict[int, int]:
                return self.backend.earliest_offsets(partitions)

            def fetch(self, *args: Any, **kwargs: Any) -> Any:
                return self.backend.fetch(*args, **kwargs)

            def end_offsets(self, partitions: Any) -> dict[int, int]:
                entered.set()
                release.wait(timeout=2)
                return self.backend.end_offsets(partitions)

            backend = self.backend

        coordinator.backend = BlockingBackend()
        result: dict[str, Any] = {}
        verifier = threading.Thread(target=lambda: result.update(coordinator.verify_lag()))
        verifier.start()
        self.assertTrue(entered.wait(timeout=1))
        coordinator.commit("w1", {"partition": 0, "offset": 9})
        release.set()
        verifier.join(timeout=2)
        partition_zero = result["partitions"][0]
        self.assertEqual(partition_zero["committed_offset"], 2)
        self.assertEqual(self.coordinator.offset_status(0)["committed_offset"], 9)

    def test_metrics_have_required_partition_series(self) -> None:
        self.join("w1")
        self.coordinator.commit("w1", {"partition": 0, "offset": 4})
        with self.assertRaises(ApiError):
            self.coordinator.commit("w1", {"partition": 1, "offset": -1})
        metrics = self.coordinator.metrics()
        self.assertIn(
            'order_group_committed_offset{topic="order-events",partition="0"} 4',
            metrics,
        )
        self.assertIn('order_group_lag{topic="order-events",partition="0"}', metrics)
        self.assertIn(
            'order_group_partition_assigned{topic="order-events",partition="0",member="w1"} 1',
            metrics,
        )
        self.assertIn(
            'order_group_offset_commits_total{partition="0",outcome="accepted"} 1',
            metrics,
        )
        self.assertIn(
            'order_group_offset_commits_total{partition="1",outcome="rejected"} 1',
            metrics,
        )
        self.assertIn(
            'order_group_offset_rejections_total{partition="1",reason="INVALID_REQUEST"} 1',
            metrics,
        )
        self.assertIn("order_group_rebalance_total", metrics)

    def test_generator_is_deterministic(self) -> None:
        options = GenerationOptions(
            count=25,
            seed=290,
            topic="order-events",
            partitions=12,
            backend="simulator",
            bootstrap_servers="127.0.0.1:9092",
            output_dir=self.root / "repeat",
        )
        first = generate_events(options)
        first_lines = [
            (self.root / "repeat" / f"partition-{p:02d}.jsonl").read_bytes()
            for p in range(12)
        ]
        second = generate_events(options)
        second_lines = [
            (self.root / "repeat" / f"partition-{p:02d}.jsonl").read_bytes()
            for p in range(12)
        ]
        self.assertEqual(first["stream_sha256"], second["stream_sha256"])
        self.assertEqual(first_lines, second_lines)
        self.assertEqual(first["partition_counts"], [3, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2])

    def test_record_batch_crc_and_round_trip(self) -> None:
        self.assertEqual(crc32c(b"123456789"), 0xE3069283)
        batch = encode_record_batch(
            [("A", b'{"order_id":"A"}'), ("B", b'{"order_id":"B"}')],
            timestamp_ms=1234,
        )
        records = decode_record_set(batch)
        self.assertEqual([record.offset for record in records], [0, 1])
        self.assertEqual([record.key for record in records], ["A", "B"])
        self.assertEqual(records[0].value, b'{"order_id":"A"}')


if __name__ == "__main__":
    unittest.main()

