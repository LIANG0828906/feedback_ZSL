from __future__ import annotations

import copy
import hashlib
import json
import subprocess
import sys
import tempfile
import threading
import unittest
from pathlib import Path

from livetranscode.engine import TranscodeEngine
from livetranscode.errors import BusinessError, ErrorCode
from livetranscode.samples import generate_samples
from livetranscode.simulator import simulate_adaptive_playback
from livetranscode.util import read_json
from livetranscode.verify import verify_workspace


def _tree_hashes(root: Path, ignored: set[str]) -> dict[str, str]:
    values: dict[str, str] = {}
    for path in sorted(root.rglob("*")):
        if not path.is_file():
            continue
        relative = path.relative_to(root).as_posix()
        if relative in ignored:
            continue
        values[relative] = hashlib.sha256(path.read_bytes()).hexdigest()
    return values


class PipelineTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.temp = tempfile.TemporaryDirectory()
        cls.root = Path(cls.temp.name)
        cls.samples = cls.root / "samples"
        generate_samples(cls.samples, seed=77)

    @classmethod
    def tearDownClass(cls) -> None:
        cls.temp.cleanup()

    def run_case(self, name: str, workspace_name: str) -> tuple[TranscodeEngine, dict]:
        workspace = self.root / workspace_name
        sample = self.samples / name
        engine = TranscodeEngine.prepare_workspace(
            workspace,
            sample / "audio.wav",
            sample / "frames.json",
            sample / "config.json",
        )
        report = engine.run()
        return engine, report

    def test_sample_generator_is_byte_deterministic(self) -> None:
        first = self.root / "generated_a"
        second = self.root / "generated_b"
        first_baseline = generate_samples(first, seed=991)
        second_baseline = generate_samples(second, seed=991)
        self.assertEqual(first_baseline, second_baseline)
        self.assertEqual(_tree_hashes(first, set()), _tree_hashes(second, set()))

    def test_multitrack_continuity_and_switch_alignment(self) -> None:
        engine, report = self.run_case("esports_48k_stereo", "standard")
        self.assertEqual(report["status"], "complete")
        verification = verify_workspace(engine.workspace)
        self.assertTrue(verification["ok"], verification["problems"])
        trace = simulate_adaptive_playback(
            engine.workspace,
            engine.config,
            engine.timeline,
            engine._indexes,
            read_json(self.samples / "esports_48k_stereo" / "bandwidth.json"),
        )
        self.assertTrue(trace["valid"])
        self.assertGreater(trace["switch_count"], 0)
        scheduler_events = [
            json.loads(line)
            for line in (engine.workspace / "logs" / "scheduler_trace.jsonl")
            .read_text(encoding="utf-8")
            .splitlines()
        ]
        self.assertTrue(any(event["event"] == "memory_wait" for event in scheduler_events))
        self.assertTrue(any(event["event"] == "degraded" for event in scheduler_events))
        for decision in trace["decisions"]:
            self.assertEqual(
                decision["audio_start_ticks"],
                decision["boundary_ticks"],
            )
            self.assertEqual(
                decision["video_start_ticks"],
                decision["boundary_ticks"],
            )
            self.assertLessEqual(
                decision["buffer_bytes_after"],
                engine.config.max_decode_buffer_bytes,
            )

    def test_budget_degrade_and_rejection_isolated(self) -> None:
        engine, report = self.run_case("budget_degrade", "budget")
        self.assertIn("degrade_auto", report["scheduler"]["completed_slice_counts"])
        self.assertEqual(report["scheduler"]["completed_slice_counts"]["degrade_auto"], 6)
        self.assertIn("degrade_min", report["failed_tracks"])
        self.assertIn("reject_tiny", report["failed_tracks"])
        self.assertEqual(report["failed_tracks"]["reject_tiny"]["code"], ErrorCode.BUDGET_EXCEEDED)
        degraded = []
        rejected = []
        for line in (engine.workspace / "logs" / "scheduler_trace.jsonl").read_text(encoding="utf-8").splitlines():
            event = json.loads(line)
            if event["event"] == "degraded":
                degraded.append(event)
            if event["event"] == "budget_rejected":
                rejected.append(event)
        self.assertTrue(degraded)
        self.assertIn("minimal_buffers", {event["details"]["stage"] for event in degraded})
        self.assertTrue(rejected)
        self.assertTrue(rejected[0]["details"]["context"]["attempts"])
        self.assertFalse(any(engine.slice_roots["reject_tiny"].glob("[0-9]*")))

    def test_commit_failure_stops_only_its_track(self) -> None:
        sample = self.samples / "voice_16k_mono"
        config = read_json(sample / "config.json")
        config["failure_injection"] = {"voice_low:2": "commit"}
        config_path = self.root / "commit_failure_config.json"
        config_path.write_text(json.dumps(config, indent=2, sort_keys=True) + "\n", encoding="utf-8")
        engine = TranscodeEngine.prepare_workspace(
            self.root / "commit_failure",
            sample / "audio.wav",
            sample / "frames.json",
            config_path,
        )
        report = engine.run()
        self.assertEqual(report["scheduler"]["completed_slice_counts"]["voice_low"], 2)
        self.assertEqual(report["scheduler"]["completed_slice_counts"]["voice_high"], 10)
        self.assertIn("voice_low", report["failed_tracks"])
        self.assertFalse((engine.slice_roots["voice_low"] / "000002").exists())
        self.assertTrue(verify_workspace(engine.workspace)["ok"])

    def test_deterministic_slices_switches_and_logs(self) -> None:
        first_engine, _ = self.run_case("music_44k1_mono", "determinism_a")
        second_engine, _ = self.run_case("music_44k1_mono", "determinism_b")
        bandwidth = read_json(self.samples / "music_44k1_mono" / "bandwidth.json")
        simulate_adaptive_playback(
            first_engine.workspace,
            first_engine.config,
            first_engine.timeline,
            first_engine._indexes,
            bandwidth,
        )
        simulate_adaptive_playback(
            second_engine.workspace,
            second_engine.config,
            second_engine.timeline,
            second_engine._indexes,
            bandwidth,
        )
        ignored = {"snapshot.json", "exit_report.json"}
        hashes_a = _tree_hashes(self.root / "determinism_a", ignored)
        hashes_b = _tree_hashes(self.root / "determinism_b", ignored)
        self.assertEqual(hashes_a, hashes_b)
        self.assertEqual(
            (self.root / "determinism_a" / "logs" / "scheduler_trace.jsonl").read_bytes(),
            (self.root / "determinism_b" / "logs" / "scheduler_trace.jsonl").read_bytes(),
        )

    def test_switch_gap_is_never_reported_as_valid(self) -> None:
        engine, _ = self.run_case("voice_16k_mono", "switch_gap")
        indexes = copy.deepcopy(engine._indexes)
        for track_id in indexes:
            indexes[track_id]["entries"] = indexes[track_id]["entries"][:3]
        with self.assertRaises(BusinessError) as caught:
            simulate_adaptive_playback(
                engine.workspace,
                engine.config,
                engine.timeline,
                indexes,
                read_json(self.samples / "voice_16k_mono" / "bandwidth.json"),
            )
        self.assertEqual(caught.exception.code, ErrorCode.SWITCH_INVALID)
        trace = read_json(engine.workspace / "switches.json")
        self.assertFalse(trace["valid"])
        self.assertIsNotNone(trace["error"])

    def test_switch_timestamp_mismatch_reports_conflict_fields(self) -> None:
        engine, _ = self.run_case("voice_16k_mono", "switch_timestamp")
        indexes = copy.deepcopy(engine._indexes)
        for track_id in indexes:
            entry = indexes[track_id]["entries"][0]
            entry["start_ticks"] = 1
            entry["audio_start_ticks"] = 1
            entry["video_start_ticks"] = 1
        with self.assertRaises(BusinessError) as caught:
            simulate_adaptive_playback(
                engine.workspace,
                engine.config,
                engine.timeline,
                indexes,
                read_json(self.samples / "voice_16k_mono" / "bandwidth.json"),
            )
        self.assertEqual(caught.exception.code, ErrorCode.SWITCH_INVALID)
        self.assertEqual(caught.exception.context["boundary_ticks"], 0)
        self.assertIn("start_ticks", caught.exception.context["conflict_fields"])
        self.assertIn("audio_start_ticks", caught.exception.context["conflict_fields"])
        self.assertIn("video_start_ticks", caught.exception.context["conflict_fields"])

    def test_snapshot_and_config_mismatch_are_rejected(self) -> None:
        self.run_case("voice_16k_mono", "mismatch")
        workspace = self.root / "mismatch"
        config_path = workspace / "config.json"
        config = read_json(config_path)
        config["tracks"][0]["audio"]["gain_db"] = -7.0
        config_path.write_text(json.dumps(config, indent=2, sort_keys=True) + "\n", encoding="utf-8")
        with self.assertRaises(BusinessError) as caught:
            TranscodeEngine.load_workspace(workspace)
        self.assertEqual(caught.exception.code, ErrorCode.SNAPSHOT_MISMATCH)

    def test_boundary_and_snapshot_hash_tampering_are_rejected(self) -> None:
        boundary_engine, _ = self.run_case("voice_16k_mono", "boundary_tamper")
        boundaries_path = boundary_engine.workspace / "boundaries.json"
        boundaries = read_json(boundaries_path)
        boundaries["slices"][0]["end_ticks"] += 1
        boundaries_path.write_text(json.dumps(boundaries, indent=2, sort_keys=True) + "\n", encoding="utf-8")
        with self.assertRaises(BusinessError) as boundary_error:
            TranscodeEngine.load_workspace(boundary_engine.workspace)
        self.assertEqual(boundary_error.exception.code, ErrorCode.SNAPSHOT_MISMATCH)

        snapshot_engine, _ = self.run_case("voice_16k_mono", "snapshot_tamper")
        snapshot_path = snapshot_engine.workspace / "snapshot.json"
        snapshot = read_json(snapshot_path)
        snapshot["tracks"]["voice_low"]["entries"][0]["chain_hash"] = "0" * 64
        snapshot_path.write_text(json.dumps(snapshot, indent=2, sort_keys=True) + "\n", encoding="utf-8")
        with self.assertRaises(BusinessError) as snapshot_error:
            TranscodeEngine.load_workspace(snapshot_engine.workspace)
        self.assertEqual(snapshot_error.exception.code, ErrorCode.SNAPSHOT_MISMATCH)

    def test_duplicate_running_track_returns_busy(self) -> None:
        sample = self.samples / "voice_16k_mono"
        engine = TranscodeEngine.prepare_workspace(
            self.root / "busy",
            sample / "audio.wav",
            sample / "frames.json",
            sample / "config.json",
        )
        original = engine._prepare_task
        started = threading.Event()
        release = threading.Event()

        def slow_prepare(task, budget, cancel_event):
            started.set()
            release.wait(5)
            return original(task, budget, cancel_event)

        engine._prepare_task = slow_prepare  # type: ignore[method-assign]
        result: dict[str, object] = {}

        def worker() -> None:
            result["report"] = engine.run(track_ids=["voice_low"])

        thread = threading.Thread(target=worker)
        thread.start()
        self.assertTrue(started.wait(5))
        with self.assertRaises(BusinessError) as caught:
            engine.run(track_ids=["voice_low"])
        self.assertEqual(caught.exception.code, ErrorCode.BUSY)
        release.set()
        thread.join(10)
        self.assertFalse(thread.is_alive())

    def test_cross_track_preparation_actually_runs_in_parallel(self) -> None:
        sample = self.samples / "music_44k1_mono"
        config = read_json(sample / "config.json")
        config["memory_budget_bytes"] = 3_000_000
        config["slice_memory_budget_bytes"] = 2_500_000
        for track in config["tracks"]:
            track["memory_limit_bytes"] = 2_500_000
        config_path = self.root / "parallel_config.json"
        config_path.write_text(json.dumps(config, indent=2, sort_keys=True) + "\n", encoding="utf-8")
        engine = TranscodeEngine.prepare_workspace(
            self.root / "parallel",
            sample / "audio.wav",
            sample / "frames.json",
            config_path,
        )
        original = engine._prepare_task
        lock = threading.Lock()
        arrived = 0
        parallel_ready = threading.Event()

        def parallel_prepare(task, budget, cancel_event):
            nonlocal arrived
            with lock:
                arrived += 1
                if arrived >= 2:
                    parallel_ready.set()
            self.assertTrue(parallel_ready.wait(2), "workers did not overlap across tracks")
            return original(task, budget, cancel_event)

        engine._prepare_task = parallel_prepare  # type: ignore[method-assign]
        report = engine.run()
        self.assertEqual(report["status"], "complete")

    def test_cooperative_cancel_keeps_last_committed_snapshot(self) -> None:
        sample = self.samples / "voice_16k_mono"
        workspace = self.root / "cancel"
        engine = TranscodeEngine.prepare_workspace(
            workspace,
            sample / "audio.wav",
            sample / "frames.json",
            sample / "config.json",
        )
        original = engine._prepare_task
        started = threading.Event()
        release = threading.Event()

        def slow_prepare(task, budget, cancel_event):
            started.set()
            release.wait(5)
            return original(task, budget, cancel_event)

        engine._prepare_task = slow_prepare  # type: ignore[method-assign]
        result: dict[str, object] = {}
        thread = threading.Thread(target=lambda: result.update(report=engine.run()))
        thread.start()
        self.assertTrue(started.wait(5))
        engine.cancel()
        release.set()
        thread.join(10)
        self.assertFalse(thread.is_alive())
        report = result["report"]
        self.assertEqual(report["status"], "cancelled")
        self.assertEqual(report["failed_tracks"], {})
        self.assertTrue((workspace / "snapshot.json").exists())
        self.assertFalse(any((workspace / "tracks" / "voice_low" / "slices").glob("[0-9]*")))
        loaded = TranscodeEngine.load_workspace(workspace)
        self.assertEqual(len(loaded._indexes["voice_low"]["entries"]), 0)

    def test_pause_stops_at_boundary_and_continue_resumes(self) -> None:
        sample = self.samples / "voice_16k_mono"
        workspace = self.root / "pause"
        engine = TranscodeEngine.prepare_workspace(
            workspace,
            sample / "audio.wav",
            sample / "frames.json",
            sample / "config.json",
        )
        original = engine._prepare_task
        started = threading.Event()
        release = threading.Event()
        calls = 0

        def slow_first_prepare(task, budget, cancel_event):
            nonlocal calls
            calls += 1
            if calls == 1:
                started.set()
                release.wait(5)
            return original(task, budget, cancel_event)

        engine._prepare_task = slow_first_prepare  # type: ignore[method-assign]
        result: dict[str, object] = {}
        thread = threading.Thread(target=lambda: result.update(report=engine.run()))
        thread.start()
        self.assertTrue(started.wait(5))
        engine.pause()
        release.set()
        thread.join(10)
        self.assertFalse(thread.is_alive())
        paused = result["report"]
        self.assertEqual(paused["status"], "paused")
        completed_after_pause = {
            track_id: len(engine._indexes[track_id]["entries"]) for track_id in engine._indexes
        }
        self.assertTrue(all(count <= 1 for count in completed_after_pause.values()))
        resumed = engine.run()
        self.assertEqual(resumed["status"], "complete")
        self.assertTrue(verify_workspace(workspace)["ok"])

    def test_forced_termination_resume_matches_clean_outputs(self) -> None:
        sample = self.samples / "esports_48k_stereo"
        clean = self.root / "clean_compare"
        crash = self.root / "crash_compare"
        clean_engine = TranscodeEngine.prepare_workspace(
            clean, sample / "audio.wav", sample / "frames.json", sample / "config.json"
        )
        clean_engine.run()
        command = [
            sys.executable,
            "-m",
            "livetranscode",
            "run",
            "--workspace",
            str(crash),
            "--sample",
            str(sample),
            "--crash-after-commits",
            "4",
        ]
        crashed = subprocess.run(command, cwd=Path(__file__).resolve().parents[1], capture_output=True, text=True)
        self.assertEqual(crashed.returncode, 137, crashed.stderr)
        self.assertFalse((crash / "exit_report.json").exists())
        resumed = subprocess.run(
            [sys.executable, "-m", "livetranscode", "resume", "--workspace", str(crash)],
            cwd=Path(__file__).resolve().parents[1],
            capture_output=True,
            text=True,
        )
        self.assertEqual(resumed.returncode, 0, resumed.stderr)
        ignored = {
            "snapshot.json",
            "exit_report.json",
            "bandwidth.json",
            "switches.json",
            "switches.jsonl",
            "logs/scheduler_trace.jsonl",
            "logs/run.log",
        }
        self.assertEqual(_tree_hashes(clean, ignored), _tree_hashes(crash, ignored))
        self.assertTrue(verify_workspace(crash)["ok"])


if __name__ == "__main__":
    unittest.main()

