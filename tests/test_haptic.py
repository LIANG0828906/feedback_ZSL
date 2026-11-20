from __future__ import annotations

import json
import os
import subprocess
import tempfile
import threading
import time
import unittest
from dataclasses import replace
from pathlib import Path
from unittest import mock

import numpy as np

from haptic.cli import run_demo
from haptic.engine import (
    build_segment_signal,
    compile_signals,
    segment_sample_interval,
)
from haptic.errors import E_BUSY, E_RECOVERY_INVALID, E_VERSION_STALE, HapticError
from haptic.model import load_project, parse_project
from haptic.samples import write_manifest, write_samples
from haptic.snapshot import SnapshotManager
from haptic.tasks import RecomputeTaskManager
from haptic.trace import TraceLogger
from haptic.versioning import Publisher, chunk_signal_hash

ROOT = Path(__file__).resolve().parents[1]


def _project_data() -> dict:
    return {
        "sample_rate": 1000,
        "device": {
            "min": -1.0,
            "max": 1.0,
            "limiter": "hard",
            "soft_knee_ratio": 0.8,
        },
        "motor_map": {
            "left_ratio": 0.8,
            "right_ratio": 0.8,
            "left_polarity": 1,
            "right_polarity": 1,
        },
        "segments": [
            {
                "start_ms": 0,
                "duration_ms": 100,
                "frequency_hz": 20,
                "amplitude": 0.8,
                "envelope": "sine",
            },
            {
                "start_ms": 40,
                "duration_ms": 120,
                "frequency_hz": 35,
                "amplitude": 0.9,
                "envelope": "linear_decay",
            },
            {
                "start_ms": 80,
                "duration_ms": 90,
                "frequency_hz": 15,
                "amplitude": 0.7,
                "envelope": "rectangular",
            },
        ],
    }


def _samples_project() -> tuple[object, object]:
    with tempfile.TemporaryDirectory() as tmp:
        directory = Path(tmp)
        write_samples(directory)
        write_manifest(directory)
        project = load_project(directory / "mixed_envelopes.json")
        return project, project.segments


def _replace_segment(segment, **changes):
    return replace(segment, **changes)


def _chunk_fingerprint(version):
    return [
        (
            chunk.start_sample,
            chunk.end_sample,
            chunk.signal_hash,
            chunk.limited_samples,
            chunk.out_of_range_samples,
        )
        for chunk in version.chunks
    ]


class ModelValidationTests(unittest.TestCase):
    def test_missing_field_has_segment_index(self):
        data = _project_data()
        del data["segments"][1]["frequency_hz"]
        with self.assertRaises(HapticError) as caught:
            parse_project(data)
        self.assertEqual(caught.exception.code, "E_WAVE_INVALID")
        self.assertEqual(caught.exception.segment_index, 1)

    def test_negative_duration_has_segment_index(self):
        data = _project_data()
        data["segments"][2]["duration_ms"] = -1
        with self.assertRaises(HapticError) as caught:
            parse_project(data)
        self.assertEqual(caught.exception.code, "E_WAVE_INVALID")
        self.assertEqual(caught.exception.segment_index, 2)

    def test_frequency_above_nyquist_has_segment_index(self):
        data = _project_data()
        data["segments"][0]["frequency_hz"] = 501
        with self.assertRaises(HapticError) as caught:
            parse_project(data)
        self.assertEqual(caught.exception.code, "E_WAVE_INVALID")
        self.assertEqual(caught.exception.segment_index, 0)
        self.assertEqual(caught.exception.details["nyquist_hz"], 500.0)

    def test_unknown_envelope_is_invalid(self):
        data = _project_data()
        data["segments"][0]["envelope"] = "square"
        with self.assertRaises(HapticError) as caught:
            parse_project(data)
        self.assertEqual(caught.exception.code, "E_WAVE_INVALID")
        self.assertEqual(caught.exception.segment_index, 0)


class EnvelopeTests(unittest.TestCase):
    def test_envelopes_keep_declared_duration_across_sample_rates(self):
        for envelope in ("rectangular", "linear_decay", "sine"):
            with self.subTest(envelope=envelope):
                base = {
                    "start_ms": 25,
                    "duration_ms": 50,
                    "frequency_hz": 20,
                    "amplitude": 0.75,
                    "envelope": envelope,
                }
                project_1k = parse_project({"sample_rate": 1000, "segments": [base]})
                project_2k = parse_project({"sample_rate": 2000, "segments": [base]})
                signal_1k = build_segment_signal(
                    project_1k.segments[0],
                    project_1k.config,
                )
                signal_2k = build_segment_signal(
                    project_2k.segments[0],
                    project_2k.config,
                )
                self.assertEqual(signal_1k.start_sample, 25)
                self.assertEqual(signal_1k.end_sample, 75)
                self.assertEqual(signal_2k.start_sample, 50)
                self.assertEqual(signal_2k.end_sample, 150)
                self.assertEqual(signal_1k.sample_count, 50)
                self.assertEqual(signal_2k.sample_count, 100)
                np.testing.assert_array_equal(
                    signal_2k.left[::2],
                    signal_1k.left,
                )
                np.testing.assert_array_equal(
                    signal_2k.right[::2],
                    signal_1k.right,
                )

    def test_envelope_shapes(self):
        for envelope in ("rectangular", "linear_decay", "sine"):
            project = parse_project(
                {
                    "sample_rate": 1000,
                    "segments": [
                        {
                            "start_ms": 0,
                            "duration_ms": 100,
                            "frequency_hz": 5,
                            "amplitude": 1.0,
                            "envelope": envelope,
                            "left_ratio": 1.0,
                            "right_ratio": 1.0,
                        }
                    ],
                }
            )
            signal = build_segment_signal(
                project.segments[0],
                project.config,
            )
            carrier = np.sin(2 * np.pi * 5 * np.arange(100) / 1000.0)
            mask = np.abs(carrier) > 1e-12
            envelope_track = signal.left[mask] / carrier[mask]
            if envelope == "rectangular":
                np.testing.assert_allclose(envelope_track, 1.0)
            elif envelope == "linear_decay":
                np.testing.assert_allclose(
                    envelope_track,
                    (1.0 - np.arange(100) / 100.0)[mask],
                )
            else:
                np.testing.assert_allclose(
                    envelope_track,
                    (0.5 - 0.5 * np.cos(2 * np.pi * np.arange(100) / 100.0))[mask],
                )


class IncrementalTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.tmp = tempfile.TemporaryDirectory()
        cls.sample_dir = Path(cls.tmp.name)
        write_samples(cls.sample_dir)
        write_manifest(cls.sample_dir)
        cls.project = load_project(cls.sample_dir / "mixed_envelopes.json")
        cls.initial = compile_signals(cls.project.segments, cls.project.config)

    @classmethod
    def tearDownClass(cls):
        cls.tmp.cleanup()

    def test_frequency_amplitude_edit_matches_full_rebuild(self):
        edited = list(self.project.segments)
        edited[1] = _replace_segment(
            edited[1],
            frequency_hz=96.0,
            amplitude=0.61,
        )
        incremental = compile_signals(
            edited,
            self.project.config,
            previous=self.initial.state,
            modified_indices=[1],
        )
        full = compile_signals(edited, self.project.config)
        self.assertFalse(incremental.full_rebuild)
        self.assertEqual(incremental.changed_indices, (1,))
        self.assertEqual(
            incremental.version.signal_bytes(), full.version.signal_bytes()
        )
        self.assertEqual(
            _chunk_fingerprint(incremental.version),
            _chunk_fingerprint(full.version),
        )
        self.assertEqual(
            incremental.version.segment_records,
            full.version.segment_records,
        )
        old_start, old_end = segment_sample_interval(
            self.project.segments[1],
            self.project.config.sample_rate,
        )
        new_start, new_end = segment_sample_interval(
            edited[1],
            self.project.config.sample_rate,
        )
        self.assertEqual(
            incremental.dirty_ranges,
            ((min(old_start, new_start) - 1, max(old_end, new_end) + 1),),
        )
        for start, end in incremental.reused_ranges:
            self.assertEqual(
                incremental.version.left[start:end].tobytes(),
                self.initial.version.left[start:end].tobytes(),
            )
            self.assertEqual(
                incremental.version.right[start:end].tobytes(),
                self.initial.version.right[start:end].tobytes(),
            )
        self.assertGreater(incremental.reused_samples, 0)
        self.assertGreater(incremental.reused_chunks, 0)

    def test_envelope_and_ratio_edits_recompute_support_plus_transition(self):
        current = self.initial
        for changes in (
            {"envelope": "sine"},
            {"left_ratio": 0.21, "right_ratio": 1.15},
        ):
            with self.subTest(changes=changes):
                edited = list(current.version.segments)
                edited[1] = _replace_segment(edited[1], **changes)
                incremental = compile_signals(
                    edited,
                    self.project.config,
                    previous=current.state,
                )
                full = compile_signals(edited, self.project.config)
                self.assertEqual(
                    incremental.version.signal_bytes(),
                    full.version.signal_bytes(),
                )
                start, end = segment_sample_interval(
                    edited[1],
                    self.project.config.sample_rate,
                )
                self.assertEqual(
                    incremental.dirty_ranges,
                    ((start - 1, end + 1),),
                )
                current = incremental

    def test_global_device_change_is_conservative_full_rebuild(self):
        changed_config = replace(
            self.project.config,
            limiter="soft",
            soft_knee_ratio=0.65,
        )
        incremental = compile_signals(
            self.project.segments,
            changed_config,
            previous=self.initial.state,
        )
        full = compile_signals(self.project.segments, changed_config)
        self.assertTrue(incremental.full_rebuild)
        self.assertEqual(
            incremental.dirty_ranges,
            ((0, self.initial.version.sample_count),),
        )
        self.assertEqual(incremental.reused_samples, 0)
        self.assertEqual(
            incremental.version.signal_bytes(),
            full.version.signal_bytes(),
        )

    def test_duration_change_that_changes_length_rebuilds_conservatively(self):
        edited = list(self.project.segments)
        edited[4] = _replace_segment(edited[4], duration_ms=1000.0)
        incremental = compile_signals(
            edited,
            self.project.config,
            previous=self.initial.state,
        )
        self.assertTrue(incremental.full_rebuild)
        full = compile_signals(edited, self.project.config)
        self.assertEqual(
            incremental.version.signal_bytes(),
            full.version.signal_bytes(),
        )


class LimiterAndDeterminismTests(unittest.TestCase):
    def test_hard_and_soft_limiting_statistics(self):
        data = {
            "sample_rate": 1000,
            "device": {
                "min": -1.0,
                "max": 1.0,
                "limiter": "hard",
                "soft_knee_ratio": 0.7,
            },
            "segments": [
                {
                    "start_ms": 0,
                    "duration_ms": 100,
                    "frequency_hz": 20,
                    "amplitude": 1.0,
                    "envelope": "rectangular",
                    "left_ratio": 1.0,
                    "right_ratio": 1.0,
                },
                {
                    "start_ms": 0,
                    "duration_ms": 100,
                    "frequency_hz": 30,
                    "amplitude": 1.0,
                    "envelope": "rectangular",
                    "left_ratio": 1.0,
                    "right_ratio": 1.0,
                },
            ],
        }
        hard_project = parse_project(data)
        hard = compile_signals(hard_project.segments, hard_project.config)
        self.assertGreater(hard.version.limited_samples, 0)
        self.assertEqual(
            hard.version.limited_samples,
            hard.version.out_of_range_samples,
        )
        self.assertTrue(np.all(hard.version.left >= hard_project.config.device_min))
        self.assertTrue(np.all(hard.version.left <= hard_project.config.device_max))
        soft_project = parse_project(
            {
                **data,
                "device": {
                    "min": -1.0,
                    "max": 1.0,
                    "limiter": "soft",
                    "soft_knee_ratio": 0.7,
                },
            }
        )
        soft = compile_signals(soft_project.segments, soft_project.config)
        self.assertGreater(soft.version.limited_samples, 0)
        self.assertTrue(np.all(soft.version.left < 1.0))
        self.assertTrue(np.all(soft.version.left > -1.0))
        self.assertGreater(
            soft.version.limited_samples,
            soft.version.out_of_range_samples,
        )

    def test_repeated_full_calculation_and_trace_are_byte_identical(self):
        with tempfile.TemporaryDirectory() as tmp:
            directory = Path(tmp)
            samples = directory / "samples"
            write_samples(samples)
            project = load_project(samples / "dense_soft.json")
            edited = list(project.segments)
            edited[2] = _replace_segment(edited[2], amplitude=0.42)

            outputs = []
            for run in range(2):
                trace_path = directory / f"trace-{run}.jsonl"
                trace = TraceLogger(trace_path, reset=True)
                first = compile_signals(project.segments, project.config)
                trace.record_build(first)
                second = compile_signals(
                    edited,
                    project.config,
                    previous=first.state,
                )
                trace.record_build(second)
                outputs.append(
                    (
                        second.version.signal_bytes(),
                        trace_path.read_bytes(),
                        _chunk_fingerprint(second.version),
                    )
                )
            self.assertEqual(outputs[0], outputs[1])


class PublisherIsolationTests(unittest.TestCase):
    def test_queries_are_complete_during_atomic_publication(self):
        with tempfile.TemporaryDirectory() as tmp:
            directory = Path(tmp)
            write_samples(directory)
            project = load_project(directory / "three_overlap.json")
            initial = compile_signals(project.segments, project.config)
            publisher = Publisher(initial=initial.version)
            stop = threading.Event()
            errors: list[BaseException] = []
            seen_versions: set[int] = set()

            def reader():
                try:
                    while not stop.is_set():
                        version = publisher.snapshot()
                        seen_versions.add(version.version)
                        self.assertEqual(
                            version.version,
                            publisher.snapshot().version,
                        )
                        for chunk in version.chunks:
                            self.assertEqual(
                                chunk.signal_hash,
                                chunk_signal_hash(
                                    chunk.start_sample,
                                    chunk.end_sample,
                                    version.left[chunk.start_sample : chunk.end_sample],
                                    version.right[
                                        chunk.start_sample : chunk.end_sample
                                    ],
                                ),
                            )
                        self.assertTrue(
                            np.all(version.left >= version.config.device_min)
                        )
                        self.assertTrue(
                            np.all(version.left <= version.config.device_max)
                        )
                except BaseException as exc:  # noqa: BLE001
                    errors.append(exc)

            thread = threading.Thread(target=reader)
            thread.start()
            state = initial.state
            try:
                for step in range(12):
                    edited = list(state.version.segments)
                    target = step % len(edited)
                    edited[target] = _replace_segment(
                        edited[target],
                        amplitude=0.3 + (step % 5) * 0.1,
                    )
                    result = compile_signals(
                        edited,
                        state.version.config,
                        previous=state,
                    )
                    publisher.publish(
                        result.version,
                        expected_parent_hash=state.version.content_hash,
                    )
                    state = result.state
                    time.sleep(0.002)
            finally:
                stop.set()
                thread.join(2.0)
            self.assertFalse(errors)
            self.assertGreaterEqual(len(seen_versions), 2)
            self.assertEqual(publisher.current().version, state.version.version)

    def test_stale_queries_are_rejected(self):
        project = parse_project(_project_data())
        first = compile_signals(project.segments, project.config)
        publisher = Publisher(initial=first.version)
        edited = list(project.segments)
        edited[0] = _replace_segment(edited[0], amplitude=0.2)
        second = compile_signals(
            edited,
            project.config,
            previous=first.state,
        )
        publisher.publish(
            second.version,
            expected_parent_hash=first.version.content_hash,
        )
        with self.assertRaises(HapticError) as caught:
            publisher.get_version(first.version.version, first.version.content_hash)
        self.assertEqual(caught.exception.code, E_VERSION_STALE)
        with self.assertRaises(HapticError) as caught:
            publisher.get_version(
                second.version.version,
                "f" * 64,
            )
        self.assertEqual(caught.exception.code, E_VERSION_STALE)


class SnapshotTests(unittest.TestCase):
    def setUp(self):
        self.tmpdir = tempfile.TemporaryDirectory()
        self.directory = Path(self.tmpdir.name)
        write_samples(self.directory)
        write_manifest(self.directory)
        self.project = load_project(self.directory / "mixed_envelopes.json")
        self.initial = compile_signals(
            self.project.segments,
            self.project.config,
        )
        self.publisher = Publisher(self.initial.version)
        edited = list(self.project.segments)
        edited[1] = _replace_segment(
            edited[1],
            frequency_hz=91.0,
            envelope="rectangular",
        )
        self.second = compile_signals(
            edited,
            self.project.config,
            previous=self.initial.state,
        )
        self.publisher.publish(
            self.second.version,
            expected_parent_hash=self.initial.version.content_hash,
        )

    def tearDown(self):
        self.tmpdir.cleanup()

    def test_atomic_snapshot_round_trip(self):
        path = self.directory / "snapshot.json"
        SnapshotManager.save(path, self.publisher, self.second.state)
        recovered = SnapshotManager.load(path)
        self.assertEqual(recovered.recomputed_chunks, 0)
        self.assertEqual(
            recovered.state.version.content_hash,
            self.second.version.content_hash,
        )
        self.assertEqual(
            recovered.state.version.signal_bytes(),
            self.second.version.signal_bytes(),
        )
        self.assertEqual(
            [entry.to_dict() for entry in recovered.history],
            [entry.to_dict() for entry in self.publisher.history()],
        )
        self.assertEqual(
            recovered.state.version.segment_records,
            self.second.version.segment_records,
        )
        self.assertFalse(list(self.directory.glob("*.tmp")))

    def test_recovery_recomputes_only_incomplete_chunk(self):
        path = self.directory / "partial.json"
        target = self.second.version.chunks[3]
        SnapshotManager.save(
            path,
            self.publisher,
            self.second.state,
            incomplete_chunk_starts=[target.start_sample],
        )
        recovered = SnapshotManager.load(path)
        self.assertEqual(recovered.recomputed_chunks, 1)
        self.assertEqual(
            recovered.recomputed_chunk_ranges,
            ((target.start_sample, target.end_sample),),
        )
        self.assertEqual(
            recovered.state.version.signal_bytes(),
            self.second.version.signal_bytes(),
        )
        self.assertEqual(
            recovered.state.version.content_hash,
            self.second.version.content_hash,
        )

    def test_corrupt_chunk_payload_is_repaired_from_segment_cache(self):
        path = self.directory / "snapshot.json"
        SnapshotManager.save(path, self.publisher, self.second.state)
        payload = json.loads(path.read_text(encoding="utf-8"))
        chunk = payload["published"]["chunks"][2]
        chunk["left_b64"] = "A" * (len(chunk["left_b64"]))
        path.write_text(json.dumps(payload), encoding="utf-8")
        recovered = SnapshotManager.load(path)
        self.assertEqual(recovered.recomputed_chunks, 1)
        self.assertEqual(
            recovered.state.version.signal_bytes(),
            self.second.version.signal_bytes(),
        )

    def test_broken_hash_chain_is_rejected(self):
        path = self.directory / "snapshot.json"
        SnapshotManager.save(path, self.publisher, self.second.state)
        payload = json.loads(path.read_text(encoding="utf-8"))
        payload["history"][1]["parent_hash"] = "f" * 64
        path.write_text(json.dumps(payload), encoding="utf-8")
        with self.assertRaises(HapticError) as caught:
            SnapshotManager.load(path)
        self.assertEqual(caught.exception.code, E_RECOVERY_INVALID)

    def test_missing_required_field_is_rejected(self):
        path = self.directory / "snapshot.json"
        SnapshotManager.save(path, self.publisher, self.second.state)
        payload = json.loads(path.read_text(encoding="utf-8"))
        del payload["published"]["segment_records"]
        path.write_text(json.dumps(payload), encoding="utf-8")
        with self.assertRaises(HapticError) as caught:
            SnapshotManager.load(path)
        self.assertEqual(caught.exception.code, E_RECOVERY_INVALID)

    def test_abandoned_temp_file_does_not_affect_valid_snapshot(self):
        path = self.directory / "snapshot.json"
        SnapshotManager.save(path, self.publisher, self.second.state)
        (self.directory / ".snapshot.json.crashed.tmp").write_bytes(b"{")
        recovered = SnapshotManager.load(path)
        self.assertEqual(
            recovered.state.version.content_hash,
            self.second.version.content_hash,
        )

    def test_load_latest_skips_broken_newer_snapshot(self):
        initial = compile_signals(self.project.segments, self.project.config)
        publisher = Publisher(initial.version)
        old_path = self.directory / "snapshot-001.json"
        SnapshotManager.save(old_path, publisher, initial.state)
        edited = list(self.project.segments)
        edited[2] = _replace_segment(edited[2], amplitude=0.31)
        newer = compile_signals(
            edited,
            self.project.config,
            previous=initial.state,
        )
        publisher.publish(
            newer.version,
            expected_parent_hash=initial.version.content_hash,
        )
        newer_path = self.directory / "snapshot-002.json"
        SnapshotManager.save(newer_path, publisher, newer.state)
        payload = json.loads(newer_path.read_text(encoding="utf-8"))
        del payload["history"]
        newer_path.write_text(json.dumps(payload), encoding="utf-8")
        selected_path, recovered = SnapshotManager.load_latest(
            self.directory,
        )
        self.assertEqual(selected_path, old_path)
        self.assertEqual(recovered.state.version.version, 1)


class TaskBoundaryTests(unittest.TestCase):
    def setUp(self):
        self.tmpdir = tempfile.TemporaryDirectory()
        self.directory = Path(self.tmpdir.name)
        write_samples(self.directory)
        self.project = load_project(self.directory / "three_overlap.json")
        initial = compile_signals(self.project.segments, self.project.config)
        self.publisher = Publisher(initial.version)
        self.trace = TraceLogger(self.directory / "trace.jsonl", reset=True)
        self.manager = RecomputeTaskManager(
            self.publisher,
            initial.state,
            self.trace,
        )
        self.initial = initial
        edited = list(self.project.segments)
        edited[1] = _replace_segment(edited[1], amplitude=0.42)
        self.edited = edited

    def tearDown(self):
        self.manager.cancel()
        self.manager.wait(2.0)
        self.tmpdir.cleanup()

    def test_busy_cancel_and_old_version_visibility(self):
        from haptic import tasks as task_module

        real_compile = task_module.compile_signals

        def delayed_compile(*args, **kwargs):
            time.sleep(0.12)
            return real_compile(*args, **kwargs)

        with mock.patch.object(
            task_module,
            "compile_signals",
            side_effect=delayed_compile,
        ):
            self.manager.submit(
                self.edited,
                self.project.config,
                modified_indices=[1],
            )
            with self.assertRaises(HapticError) as caught:
                self.manager.submit(
                    self.edited,
                    self.project.config,
                    modified_indices=[1],
                )
            self.assertEqual(caught.exception.code, E_BUSY)
            self.assertTrue(self.manager.cancel())
            self.assertTrue(self.manager.wait(2.0))
            status = self.manager.status()
            self.assertEqual(status.state, "cancelled")
            current = self.publisher.require_current()
            self.assertEqual(current.version, 1)
            self.assertEqual(
                current.content_hash,
                self.initial.version.content_hash,
            )
            self.assertEqual(self.manager.state().version.version, 1)

        self.manager.submit(
            self.edited,
            self.project.config,
            modified_indices=[1],
        )
        self.assertTrue(self.manager.wait(2.0))
        status = self.manager.status()
        self.assertEqual(status.state, "succeeded")
        self.assertEqual(self.publisher.require_current().version, 2)

    def test_exception_keeps_last_published_version(self):
        from haptic import tasks as task_module

        with mock.patch.object(
            task_module,
            "compile_signals",
            side_effect=HapticError("E_TEST", "forced failure"),
        ):
            self.manager.submit(
                self.edited,
                self.project.config,
                modified_indices=[1],
            )
            self.assertTrue(self.manager.wait(2.0))
        status = self.manager.status()
        self.assertEqual(status.state, "failed")
        self.assertEqual(status.error["code"], "E_TEST")
        self.assertEqual(self.publisher.require_current().version, 1)
        self.assertEqual(self.manager.state().version.version, 1)


class DeterministicDemoTests(unittest.TestCase):
    def test_demo_trace_and_signal_are_reproducible(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            results = []
            for run in range(2):
                out = root / f"run-{run}"
                summary = run_demo(root / "samples", out)
                results.append(
                    (
                        (out / "trace.jsonl").read_bytes(),
                        (out / "signal-latest.bin").read_bytes(),
                        summary,
                    )
                )
            self.assertEqual(results[0], results[1])
            self.assertTrue(results[0][2]["incremental_equals_full"])
            self.assertTrue(results[0][2]["recovery_equal"])
            self.assertTrue(results[0][2]["partial_recovery_equal"])
            self.assertTrue(results[0][2]["stale_query_rejected"])


class GuiSmokeTests(unittest.TestCase):
    @unittest.skipUnless(
        os.environ.get("DISPLAY") and (ROOT / "run_gui.sh").exists(),
        "DISPLAY is required for Tk smoke test",
    )
    def test_tk_window_can_start_and_close(self):
        check = subprocess.run(
            [str(ROOT / "run_gui.sh"), "--check"],
            cwd=ROOT,
            capture_output=True,
            text=True,
            timeout=20,
            check=False,
        )
        if check.returncode == 2:
            self.skipTest(f"Tk unavailable: {check.stdout}{check.stderr}")
        self.assertEqual(check.returncode, 0, check.stderr)
        smoke = subprocess.run(
            [
                str(ROOT / "run_gui.sh"),
                "--smoke-test",
                "--outdir",
                str(ROOT / "artifacts" / "test-gui-smoke"),
            ],
            cwd=ROOT,
            capture_output=True,
            text=True,
            timeout=30,
            check=False,
        )
        self.assertEqual(smoke.returncode, 0, smoke.stderr)


if __name__ == "__main__":
    unittest.main()
