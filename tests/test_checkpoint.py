from __future__ import annotations

import json
import tempfile
import threading
import unittest
from pathlib import Path

from scworkbench.compare import compare_runs
from scworkbench.engine import EngineConfig, SessionEngine
from scworkbench.loader import parse_session

from tests.helpers import manager_for, sample_data


class CheckpointTests(unittest.TestCase):
    def test_atomic_save_load_and_parallel_serialization(self) -> None:
        sample = parse_session(sample_data())
        with tempfile.TemporaryDirectory() as temp:
            manager = manager_for(temp, sample)
            errors: list[Exception] = []

            def writer(value: int) -> None:
                try:
                    manager.save(
                        {
                            "phase": "mirroring",
                            "timeline_index": value,
                            "payload": "x" * (value * 100),
                        }
                    )
                except Exception as exc:  # pragma: no cover - failure path
                    errors.append(exc)

            threads = [threading.Thread(target=writer, args=(i,)) for i in range(12)]
            for thread in threads:
                thread.start()
            for thread in threads:
                thread.join()
            self.assertEqual(errors, [])
            load = manager.load()
            self.assertEqual(load.status, "loaded")
            self.assertIsInstance(load.payload["timeline_index"], int)
            leftovers = list(Path(temp).glob(".checkpoint.tmp.*"))
            self.assertEqual(leftovers, [])

    def test_content_hash_mismatch_invalidates_whole_snapshot(self) -> None:
        sample = parse_session(sample_data())
        with tempfile.TemporaryDirectory() as temp:
            manager = manager_for(temp, sample)
            manager.save({"phase": "paused", "timeline_index": 9})
            mismatch = manager_for(temp, sample, content_hash="0" * 64)
            load = mismatch.load()
            self.assertEqual(load.status, "invalid")
            self.assertIn("内容哈希", load.reason)
            self.assertFalse(manager.path.exists())
            self.assertTrue(manager.invalid_path.exists())

    def test_capability_hash_mismatch_invalidates(self) -> None:
        sample = parse_session(sample_data())
        with tempfile.TemporaryDirectory() as temp:
            manager = manager_for(temp, sample)
            manager.save({"phase": "mirroring", "timeline_index": 2})
            mismatch = manager_for(temp, sample, capabilities_hash="f" * 64)
            load = mismatch.load()
            self.assertEqual(load.status, "invalid")
            self.assertIn("能力", load.reason)

    def test_protocol_version_mismatch_invalidates(self) -> None:
        sample = parse_session(sample_data())
        with tempfile.TemporaryDirectory() as temp:
            manager = manager_for(temp, sample)
            manager.save({"phase": "mirroring", "timeline_index": 2})
            mismatch = manager_for(temp, sample, protocol_version="scwb/9.9")
            load = mismatch.load()
            self.assertEqual(load.status, "invalid")
            self.assertIn("协议版本", load.reason)

    def test_payload_tampering_is_detected(self) -> None:
        sample = parse_session(sample_data())
        with tempfile.TemporaryDirectory() as temp:
            manager = manager_for(temp, sample)
            manager.save({"phase": "mirroring", "timeline_index": 3})
            envelope = json.loads(manager.path.read_text(encoding="utf-8"))
            envelope["payload"]["timeline_index"] = 999
            manager.path.write_text(
                json.dumps(envelope), encoding="utf-8"
            )
            load = manager.load()
            self.assertEqual(load.status, "invalid")
            self.assertIn("摘要", load.reason)

    def test_paused_checkpoint_resumes_without_restarting(self) -> None:
        sample = parse_session(sample_data())
        with tempfile.TemporaryDirectory() as temp:
            manager = manager_for(temp, sample)
            with SessionEngine(
                sample,
                config=EngineConfig(checkpoint_every=5),
                checkpoint_manager=manager,
            ) as first:
                while first.phase.value != "paused":
                    first.step()
                first.save_checkpoint()
                pause_stats = first.stats
                pause_frame_count = len(first.to_run_result("pause").frames)
            load = manager.load()
            self.assertEqual(load.status, "loaded")
            with SessionEngine(
                sample,
                config=EngineConfig(checkpoint_every=5),
                checkpoint_manager=manager,
            ) as restored:
                self.assertTrue(restored.restore(load.payload))
                self.assertEqual(restored.phase.value, "paused")
                self.assertEqual(restored.stats.frames_committed, pause_stats.frames_committed)
                self.assertEqual(len(restored.to_run_result("restored").frames), pause_frame_count)
                restored.run_to_end()
                recovered = restored.to_run_result("recovery")
            with SessionEngine(
                sample, config=EngineConfig(checkpoint_every=0)
            ) as baseline:
                baseline.run_to_end()
                control = baseline.to_run_result("realtime")
            report = compare_runs(control, recovered)
            self.assertTrue(report.ok, report.to_dict())
            self.assertEqual(recovered.stats.frames_committed, 40)
            self.assertEqual(recovered.stats.inputs_replayed, 27)

    def test_invalid_engine_payload_returns_false(self) -> None:
        sample = parse_session(sample_data())
        with SessionEngine(sample, EngineConfig(checkpoint_every=0)) as engine:
            self.assertFalse(engine.restore({"payload_version": 1}))


if __name__ == "__main__":
    unittest.main()
