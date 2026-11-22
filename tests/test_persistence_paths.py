from __future__ import annotations

import sqlite3
import tempfile
import unittest
from pathlib import Path

from haptics.errors import ErrorCode
from haptics.pipeline import (
    compare_results,
    run_realtime,
    run_resume,
    verify_three_paths,
)
from haptics.incremental import edit_event_priority
from haptics.dependencies import DependencyGraph
from haptics.persistence import CheckpointStore, CheckpointWindow
from haptics.sample import sample_capabilities, sample_events
from haptics.codec import encode_stream
from haptics.scheduler import schedule_serial
from haptics.transport import FaultPlan, MockDeviceService


class PersistenceAndPathsTests(unittest.TestCase):
    def test_three_paths_are_identical_and_cover_service_faults(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            summary = verify_three_paths(
                sample_events(), sample_capabilities(), directory, parallel=True
            )
            self.assertTrue(summary.ok)
            self.assertEqual(len(summary.comparisons), 3)
            self.assertTrue(all(item.equal for item in summary.comparisons))
            self.assertTrue(
                {"E_MISS", "E_DUP", "E_TIMEOUT", "E_REORDER"}.issubset(
                    set(summary.service_codes)
                )
            )
            self.assertEqual(
                summary.resume.reused_count, len(summary.realtime.packets)
            )
            self.assertGreaterEqual(summary.replay.persisted_windows, 11)

    def test_tampered_hash_invalidates_everything_and_full_replays(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            caps = sample_capabilities()
            stream = sample_events()
            store = CheckpointStore(directory)
            run_realtime(stream, caps, store=store)
            store.close()
            with sqlite3.connect(Path(directory) / "checkpoints.sqlite3") as db:
                db.execute(
                    "UPDATE checkpoints SET payload_json = replace(payload_json, '\"event_count\":1', '\"event_count\":1') WHERE order_index=0"
                )
                db.execute(
                    "UPDATE checkpoints SET payload_json = payload_json || ' ' WHERE order_index=0"
                )
            store = CheckpointStore(directory)
            result = run_resume(stream, caps, store)
            self.assertEqual(result.mode, "RESUME_FULL_REPLAY")
            self.assertIsNotNone(result.recovery_issue)
            self.assertEqual(result.recovery_issue.code, ErrorCode.E_HASH)
            self.assertGreater(store.count(), 0)
            store.close()

    def test_version_mismatch_invalidates_store(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            store = CheckpointStore(directory)
            run_realtime(sample_events(), sample_capabilities(), store=store)
            loaded = store.load_valid(
                expected_capability_version=999,
                expected_kernel_version="1.0.0",
            )
            self.assertFalse(loaded.valid)
            self.assertTrue(loaded.invalidated)
            self.assertEqual(store.count(), 0)
            store.close()

    def test_window_order_is_enforced(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            caps = sample_capabilities()
            store = CheckpointStore(directory)
            result = run_realtime(sample_events(), caps, store=store)
            self.assertEqual(store.count(), len(result.packets))
            store.close()

    def test_compare_locates_first_packet_divergence(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            summary = verify_three_paths(
                sample_events(), sample_capabilities(), directory
            )
            left = summary.realtime
            right = summary.resume
            tampered = list(right.packets)
            first = tampered[4]
            from dataclasses import replace

            tampered[4] = replace(first, data=first.data + b"\x00")
            changed = replace(right, packets=tuple(tampered))
            comparison = compare_results(left, changed)
            self.assertFalse(comparison.equal)
            self.assertEqual(comparison.divergence.issue.code, ErrorCode.E_DIVERGE)
            self.assertEqual(comparison.divergence.packet_seq, first.packet_seq)
            self.assertEqual(comparison.divergence.channel, first.channel)
            self.assertEqual(comparison.divergence.field, "data_hex")

    def test_partial_checkpoint_prefix_resumes_and_matches_full_realtime(self) -> None:
        caps = sample_capabilities()
        stream = sample_events()
        plan = FaultPlan.sample()
        with tempfile.TemporaryDirectory() as directory:
            store = CheckpointStore(directory)

            class StopAfterThree:
                def __init__(self) -> None:
                    self.calls = 0

                def __call__(self, packet_seq: int) -> None:
                    self.calls += 1
                    # One gate call is made before each checkpoint and again
                    # before dispatch. Stop after three durable windows.
                    if self.calls == 4:
                        raise RuntimeError("simulated exit after checkpoint 2")

            with MockDeviceService(fault_plan=plan) as service:
                with self.assertRaises(RuntimeError):
                    run_realtime(
                        stream,
                        caps,
                        store=store,
                        service=service,
                        pause_gate=StopAfterThree(),
                        run_id="partial",
                    )
            self.assertEqual(store.count(), 3)

            with MockDeviceService(fault_plan=plan) as resume_service:
                resumed = run_resume(
                    stream,
                    caps,
                    store,
                    service=resume_service,
                    run_id="partial-resume",
                )
            self.assertEqual(resumed.mode, "RESUME")
            self.assertEqual(resumed.reused_count, 3)
            self.assertEqual(store.count(), len(resumed.packets))

            with tempfile.TemporaryDirectory() as full_dir:
                full_store = CheckpointStore(full_dir)
                with MockDeviceService(fault_plan=plan) as full_service:
                    full = run_realtime(
                        stream,
                        caps,
                        store=full_store,
                        service=full_service,
                        run_id="full",
                    )
                comparison = compare_results(full, resumed)
                self.assertTrue(comparison.equal, comparison.divergence)
                full_store.close()
            store.close()

    def test_non_sequential_window_is_rejected_without_write(self) -> None:
        caps = sample_capabilities()
        scheduled = schedule_serial(sample_events(), caps)
        wire = encode_stream(scheduled, caps)[0]
        with tempfile.TemporaryDirectory() as directory:
            store = CheckpointStore(directory)
            window = CheckpointWindow(
                order_index=5,
                packet=wire,
                decisions=(),
                consumed_event_offset=1,
                capability_version=caps.capability_version,
                capability_fingerprint=caps.manifest_fingerprint,
                event_stream_hash=sample_events().content_hash(),
                kernel_version="1.0.0",
            )
            with self.assertRaises(Exception) as context:
                store.commit_window(window)
            self.assertEqual(context.exception.issue.code, ErrorCode.E_IO)
            self.assertEqual(context.exception.issue.field, "order_index")
            self.assertEqual(store.count(), 0)
            store.close()

    def test_metadata_column_tamper_invalidates_store(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            store = CheckpointStore(directory)
            run_realtime(sample_events(), sample_capabilities(), store=store)
            store.close()
            with sqlite3.connect(Path(directory) / "checkpoints.sqlite3") as db:
                db.execute("UPDATE checkpoints SET packet_seq = 999 WHERE order_index = 0")
            store = CheckpointStore(directory)
            loaded = store.load_valid(
                expected_capability_version=sample_capabilities().capability_version,
                expected_kernel_version="1.0.0",
            )
            self.assertFalse(loaded.valid)
            self.assertEqual(loaded.issue.code, ErrorCode.E_HASH)
            self.assertEqual(store.count(), 0)
            store.close()

    def test_resume_after_edit_rejects_stale_stream_snapshot(self) -> None:
        caps = sample_capabilities()
        original = sample_events()
        plan = FaultPlan.sample()
        with tempfile.TemporaryDirectory() as directory:
            store = CheckpointStore(directory)
            with MockDeviceService(fault_plan=plan) as service:
                baseline = run_realtime(
                    original,
                    caps,
                    store=store,
                    service=service,
                    run_id="edit-baseline",
                )
            edited = edit_event_priority(
                original,
                caps,
                "E0011_00",
                9,
                old_graph=baseline.dependencies,
                persisted={
                    (packet.channel, packet.local_index): packet
                    for packet in baseline.packets
                },
            )
            with MockDeviceService(fault_plan=plan) as service:
                resumed = run_resume(
                    edited.stream,
                    edited.capabilities,
                    store,
                    service=service,
                    run_id="edit-resume",
                )
            self.assertEqual(resumed.mode, "RESUME_FULL_REPLAY")
            self.assertEqual(resumed.recovery_issue.code, ErrorCode.E_HASH)
            self.assertEqual(store.count(), len(resumed.packets))
            store.close()


if __name__ == "__main__":
    unittest.main()

