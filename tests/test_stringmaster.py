from __future__ import annotations

import hashlib
import json
import math
import os
import signal
import subprocess
import sys
import tempfile
import time
import unittest

import numpy as np

from stringmaster.audio import read_wav, sha256_file, write_wav
from stringmaster.cli import _demo_manifest, create_demo
from stringmaster.dsp import (
    compensate_first_violin,
    detect_transition_intervals,
)
from stringmaster.errors import BusinessError, ManifestError
from stringmaster.manifest import load_manifest, validate_manifest_structure
from stringmaster.render import render_project
from stringmaster.synth import derive_score, generate_project


def _read_json(path: str) -> dict:
    with open(path, "r", encoding="utf-8") as handle:
        return json.load(handle)


def _write_json(path: str, value: dict) -> None:
    with open(path, "w", encoding="utf-8") as handle:
        json.dump(value, handle, ensure_ascii=False)


def _read_bytes(path: str) -> bytes:
    with open(path, "rb") as handle:
        return handle.read()


class EndToEndTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.temp = tempfile.TemporaryDirectory(prefix="stringmaster-test-")
        cls.root = cls.temp.name
        cls.demo_root = os.path.join(cls.root, "demo")
        cls.truth = create_demo(cls.demo_root, duration=5.0, seed=20261118)
        cls.manifest_path = os.path.join(cls.demo_root, "manifest.json")
        cls.context = load_manifest(cls.manifest_path, require_files=True)
        cls.score = derive_score(cls.context)
        cls.base_dir = os.path.join(cls.root, "baseline")
        cls.report = render_project(
            cls.manifest_path,
            output_path=os.path.join(cls.base_dir, "master.wav"),
            state_path=os.path.join(cls.base_dir, "state.json"),
            report_path=os.path.join(cls.base_dir, "report.json"),
            recovery_report_path=os.path.join(cls.base_dir, "recovery.json"),
        )
        cls.first_violin, _ = read_wav(
            cls.context.track("第一小提琴").abs_path
        )
        cls.detected = detect_transition_intervals(
            cls.first_violin,
            cls.context.sample_rate,
            cls.context.band_low_hz,
            cls.context.band_high_hz,
            instrument="第一小提琴",
        )

    @classmethod
    def tearDownClass(cls) -> None:
        cls.temp.cleanup()

    def _manifest_variant(self, name: str, mutator) -> str:
        raw = json.loads(json.dumps(_demo_manifest(5.0, 20261118)))
        mutator(raw)
        path = os.path.join(self.demo_root, name)
        _write_json(path, raw)
        return path

    def test_validation_reports_business_codes_and_paths(self) -> None:
        missing = self._manifest_variant(
            "missing.json",
            lambda raw: raw["tracks"][0].__setitem__("path", "absent.wav"),
        )
        with self.assertRaises(ManifestError) as caught:
            load_manifest(missing, require_files=True)
        issue = next(i for i in caught.exception.issues if i.code == "E_FILE_MISSING")
        self.assertTrue(issue.file.endswith("absent.wav"))

        rate = self._manifest_variant(
            "rate.json", lambda raw: raw.__setitem__("sample_rate", 44100)
        )
        with self.assertRaises(ManifestError) as caught:
            load_manifest(rate, require_files=True)
        issue = next(
            i for i in caught.exception.issues if i.code == "E_SAMPLE_RATE_MISMATCH"
        )
        self.assertTrue(issue.file.endswith(".wav"))

        ratio = self._manifest_variant(
            "ratio.json",
            lambda raw: raw["tracks"][0].__setitem__("target_energy_ratio", 0.4),
        )
        with self.assertRaises(ManifestError) as caught:
            load_manifest(ratio, require_files=True)
        self.assertIn("E_RATIO_SUM", {i.code for i in caught.exception.issues})

        band = self._manifest_variant(
            "band.json",
            lambda raw: raw["crossover_band"].__setitem__("high_hz", 30000.0),
        )
        with self.assertRaises(ManifestError) as caught:
            load_manifest(band, require_files=True)
        self.assertIn("E_BAND_INVALID", {i.code for i in caught.exception.issues})

        missing_instrument = self._manifest_variant(
            "instrument-missing.json", lambda raw: raw["tracks"].pop()
        )
        with self.assertRaises(ManifestError) as caught:
            load_manifest(missing_instrument, require_files=True)
        self.assertIn(
            "E_INSTRUMENT_MISSING", {i.code for i in caught.exception.issues}
        )

        duplicate = self._manifest_variant(
            "instrument-duplicate.json",
            lambda raw: raw["tracks"][1].__setitem__("instrument", "第一小提琴"),
        )
        with self.assertRaises(ManifestError) as caught:
            load_manifest(duplicate, require_files=True)
        self.assertIn(
            "E_INSTRUMENT_DUPLICATE", {i.code for i in caught.exception.issues}
        )

        depth = self._manifest_variant(
            "depth.json", lambda raw: raw.__setitem__("bit_depth", 24)
        )
        with self.assertRaises(ManifestError) as caught:
            load_manifest(depth, require_files=True)
        self.assertIn(
            "E_BIT_DEPTH_MISMATCH", {i.code for i in caught.exception.issues}
        )

    def test_channel_count_mismatch_is_rejected(self) -> None:
        raw = _read_json(self.manifest_path)
        stereo, info = read_wav(self.context.track("第二小提琴").abs_path)
        mono_path = os.path.join(self.demo_root, "tracks", "violin_2_mono.wav")
        write_wav(mono_path, stereo[:, :1], info.sample_rate, info.bit_depth)
        raw["tracks"][1]["path"] = "tracks/violin_2_mono.wav"
        path = os.path.join(self.demo_root, "channel.json")
        _write_json(path, raw)
        with self.assertRaises(ManifestError) as caught:
            load_manifest(path, require_files=True)
        self.assertIn("E_CHANNEL_MISMATCH", {i.code for i in caught.exception.issues})

    def test_generation_is_byte_deterministic(self) -> None:
        paths = [track.abs_path for track in self.context.tracks]
        before = {path: sha256_file(path) for path in paths}
        generate_project(self.context)
        after = {path: sha256_file(path) for path in paths}
        self.assertEqual(before, after)

    def test_detection_matches_truth_and_has_no_other_false_positives(self) -> None:
        self.assertEqual(len(self.detected), len(self.score.mutations))
        errors: list[float] = []
        for truth in self.score.mutations:
            matches = [
                interval
                for interval in self.detected
                if interval.end_seconds >= truth.start_seconds
                and interval.start_seconds <= truth.end_seconds
            ]
            self.assertTrue(matches)
            detected = min(
                matches,
                key=lambda interval: abs(interval.start_seconds - truth.start_seconds)
                + abs(interval.end_seconds - truth.end_seconds),
            )
            errors.extend(
                [
                    detected.start_seconds - truth.start_seconds,
                    detected.end_seconds - truth.end_seconds,
                ]
            )
        for interval in self.detected:
            self.assertTrue(
                any(
                    interval.end_seconds >= truth.start_seconds
                    and interval.start_seconds <= truth.end_seconds
                    for truth in self.score.mutations
                )
            )
        rmse = math.sqrt(sum(value * value for value in errors) / len(errors))
        self.assertLessEqual(rmse, 0.050)
        for instrument in ("第二小提琴", "中提琴", "大提琴"):
            data, _ = read_wav(self.context.track(instrument).abs_path)
            self.assertEqual(
                detect_transition_intervals(
                    data,
                    self.context.sample_rate,
                    self.context.band_low_hz,
                    self.context.band_high_hz,
                    instrument=instrument,
                ),
                (),
            )

    def test_compensation_is_band_local_and_reduces_jump(self) -> None:
        result = compensate_first_violin(
            self.first_violin,
            self.context.sample_rate,
            self.context.band_low_hz,
            self.context.band_high_hz,
            self.detected,
        )
        untouched = np.ones(self.first_violin.shape[0], dtype=bool)
        for interval in self.detected:
            start = int(round(interval.start_seconds * self.context.sample_rate))
            end = int(round(interval.end_seconds * self.context.sample_rate))
            untouched[start:end] = False
        self.assertEqual(
            float(
                np.max(
                    np.abs(
                        result.compensated[untouched] - self.first_violin[untouched]
                    )
                )
            ),
            0.0,
        )
        self.assertEqual(len(result.entries), len(self.detected))
        for entry in result.entries:
            self.assertGreaterEqual(float(entry["jump_reduction_db"]), 8.0)
            self.assertGreater(float(entry["max_abs_modification"]), 0.0)

    def test_master_report_meets_business_targets(self) -> None:
        report = self.report
        self.assertEqual(report["status"], "complete")
        self.assertLessEqual(report["max_energy_ratio_deviation"], 0.02)
        self.assertLessEqual(abs(report["loudness"]["deviation_lu"]), 0.5)
        self.assertLessEqual(
            report["true_peak"]["dbtp"],
            self.context.true_peak_limit_dbtp + 1e-6,
        )
        self.assertEqual(report["master"]["frames"], self.context.track("第一小提琴").wav.frames)
        master, info = read_wav(os.path.join(self.base_dir, "master.wav"))
        self.assertEqual(info.channels, 2)
        self.assertEqual(master.shape[0], report["master"]["frames"])
        self.assertEqual(
            report["master"]["sha256"],
            hashlib.sha256(_read_bytes(os.path.join(self.base_dir, "master.wav"))).hexdigest(),
        )

    def test_two_renders_are_byte_identical(self) -> None:
        second = os.path.join(self.root, "second")
        render_project(
            self.manifest_path,
            output_path=os.path.join(second, "master.wav"),
            state_path=os.path.join(second, "state.json"),
            report_path=os.path.join(second, "report.json"),
            recovery_report_path=os.path.join(second, "recovery.json"),
        )
        self.assertEqual(
            _read_bytes(os.path.join(self.base_dir, "master.wav")),
            _read_bytes(os.path.join(second, "master.wav")),
        )
        self.assertEqual(
            _read_bytes(os.path.join(self.base_dir, "report.json")),
            _read_bytes(os.path.join(second, "report.json")),
        )

    def test_master_is_independent_of_block_size(self) -> None:
        directory = os.path.join(self.root, "small-blocks")
        render_project(
            self.manifest_path,
            output_path=os.path.join(directory, "master.wav"),
            state_path=os.path.join(directory, "state.json"),
            report_path=os.path.join(directory, "report.json"),
            recovery_report_path=os.path.join(directory, "recovery.json"),
            block_size=1937,
        )
        self.assertEqual(
            _read_bytes(os.path.join(self.base_dir, "master.wav")),
            _read_bytes(os.path.join(directory, "master.wav")),
        )

    def _render_command(self, directory: str) -> list[str]:
        return [
            sys.executable,
            "-m",
            "stringmaster",
            "render",
            "--manifest",
            self.manifest_path,
            "--output",
            os.path.join(directory, "master.wav"),
            "--state",
            os.path.join(directory, "state.json"),
            "--report",
            os.path.join(directory, "report.json"),
            "--recovery-report",
            os.path.join(directory, "recovery.json"),
        ]

    def _wait_for_blocks(self, state_path: str, minimum: int) -> int:
        deadline = time.time() + 30.0
        seen = 0
        while time.time() < deadline:
            try:
                seen = int(_read_json(state_path).get("completed_blocks", 0))
            except (OSError, ValueError, json.JSONDecodeError):
                seen = 0
            if seen >= minimum:
                return seen
            time.sleep(0.01)
        self.fail(f"state never reached {minimum} completed blocks")

    def test_sigkill_resume_matches_one_shot(self) -> None:
        directory = os.path.join(self.root, "kill-resume")
        os.makedirs(directory, exist_ok=True)
        env = dict(os.environ)
        env["STRINGMASTER_TEST_BLOCK_SLEEP_MS"] = "12"
        command = self._render_command(directory)
        process = subprocess.Popen(
            command,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            env=env,
            text=True,
        )
        completed = self._wait_for_blocks(os.path.join(directory, "state.json"), 2)
        self.assertLess(completed, 30)
        process.kill()
        process.communicate(timeout=10)
        resumed = subprocess.run(
            command,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
        )
        self.assertEqual(resumed.returncode, 0, resumed.stderr)
        self.assertEqual(
            _read_bytes(os.path.join(self.base_dir, "master.wav")),
            _read_bytes(os.path.join(directory, "master.wav")),
        )
        self.assertEqual(
            _read_bytes(os.path.join(self.base_dir, "report.json")),
            _read_bytes(os.path.join(directory, "report.json")),
        )

    @unittest.skipIf(os.name == "nt", "POSIX SIGINT subprocess assertion")
    def test_ctrl_c_commits_and_exits_zero(self) -> None:
        directory = os.path.join(self.root, "ctrl-c")
        os.makedirs(directory, exist_ok=True)
        env = dict(os.environ)
        env["STRINGMASTER_TEST_BLOCK_SLEEP_MS"] = "18"
        command = self._render_command(directory)
        process = subprocess.Popen(
            command,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            env=env,
            text=True,
        )
        completed = self._wait_for_blocks(os.path.join(directory, "state.json"), 2)
        self.assertLess(completed, 30)
        process.send_signal(signal.SIGINT)
        stdout, stderr = process.communicate(timeout=20)
        self.assertEqual(process.returncode, 0, stderr or stdout)
        self.assertFalse(os.path.exists(os.path.join(directory, "report.json")))
        recovery = _read_json(os.path.join(directory, "recovery.json"))
        self.assertEqual(recovery["status"], "interrupted")
        resumed = subprocess.run(
            command,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
        )
        self.assertEqual(resumed.returncode, 0, resumed.stderr)
        self.assertEqual(
            _read_bytes(os.path.join(self.base_dir, "master.wav")),
            _read_bytes(os.path.join(directory, "master.wav")),
        )

    def test_limiter_branch_is_compliant_and_keeps_balance(self) -> None:
        raw = _read_json(self.manifest_path)
        raw["true_peak_limit_dbtp"] = -12.0
        path = os.path.join(self.demo_root, "manifest-limited.json")
        _write_json(path, raw)
        directory = os.path.join(self.root, "limited")
        report = render_project(
            path,
            output_path=os.path.join(directory, "master.wav"),
            state_path=os.path.join(directory, "state.json"),
            report_path=os.path.join(directory, "report.json"),
            recovery_report_path=os.path.join(directory, "recovery.json"),
        )
        self.assertGreater(report["limiter"]["triggered_samples"], 0)
        self.assertLessEqual(report["true_peak"]["dbtp"], -12.0 + 1e-6)
        self.assertLessEqual(report["max_energy_ratio_deviation"], 0.02)
        self.assertLessEqual(abs(report["loudness"]["deviation_lu"]), 0.5)

    def test_resume_mismatch_discards_progress(self) -> None:
        directory = os.path.join(self.root, "mismatch")
        render_project(
            self.manifest_path,
            output_path=os.path.join(directory, "master.wav"),
            state_path=os.path.join(directory, "state.json"),
            report_path=os.path.join(directory, "report.json"),
            recovery_report_path=os.path.join(directory, "recovery.json"),
        )
        raw = _read_json(self.manifest_path)
        raw["target_loudness_lufs"] = -21.0
        changed = os.path.join(self.demo_root, "manifest-changed.json")
        _write_json(changed, raw)
        with self.assertRaises(BusinessError) as caught:
            render_project(
                changed,
                output_path=os.path.join(directory, "master.wav"),
                state_path=os.path.join(directory, "state.json"),
                report_path=os.path.join(directory, "report.json"),
                recovery_report_path=os.path.join(directory, "recovery.json"),
            )
        self.assertEqual(caught.exception.code, "E_RESUME_MISMATCH")
        self.assertFalse(os.path.exists(os.path.join(directory, "state.json")))
        self.assertFalse(os.path.exists(os.path.join(directory, "master.wav")))


if __name__ == "__main__":
    unittest.main()
