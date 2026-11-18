from __future__ import annotations

import dataclasses
import hashlib
import json
import math
import shutil
import tempfile
import unittest
from pathlib import Path

import numpy as np

from binaural_bus.crosstalk import estimate_and_cancel_crosstalk
from binaural_bus.errors import ValidationFailure
from binaural_bus.generate import generate_material, write_generated_material
from binaural_bus.loudness import integrated_loudness, master_binaural_bus, true_peak_dbtp
from binaural_bus.project import ExportConfig, TimelinePoint, load_project
from binaural_bus.render import synthesize
from binaural_bus.spatial import render_actor
from binaural_bus.wavio import inspect_wav, read_wav


ROOT = Path(__file__).resolve().parents[1]
EXAMPLE_PROJECT = ROOT / "examples" / "project.json"


class BinauralBusTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.temp = Path(tempfile.mkdtemp(prefix="binaural-bus-test-"))
        cls.input_dir = cls.temp / "inputs"
        cls.project_template = load_project(EXAMPLE_PROJECT, validate_files=False)
        cls.generated = write_generated_material(
            cls.project_template, cls.input_dir, write_truth=True
        )
        cls.project = load_project(EXAMPLE_PROJECT, cls.input_dir)
        cls.full_dir = cls.temp / "full"
        cls.full = synthesize(
            EXAMPLE_PROJECT, cls.input_dir, cls.full_dir, fresh=True
        )
        cls.second_dir = cls.temp / "full_second"
        cls.second = synthesize(
            EXAMPLE_PROJECT, cls.input_dir, cls.second_dir, fresh=True
        )

    @classmethod
    def tearDownClass(cls) -> None:
        shutil.rmtree(cls.temp, ignore_errors=True)

    def test_generation_is_byte_deterministic(self) -> None:
        first = self.temp / "generation_a"
        second = self.temp / "generation_b"
        write_generated_material(self.project, first, write_truth=True)
        write_generated_material(self.project, second, write_truth=True)
        first_hashes = _tree_hashes(first)
        second_hashes = _tree_hashes(second)
        self.assertEqual(first_hashes, second_hashes)
        self.assertIn("generation_manifest.json", first_hashes)
        manifest = json.loads((first / "generation_manifest.json").read_text("utf-8"))
        self.assertEqual(len(manifest["crosstalk_paths"]), 4)

    def test_validation_rejects_each_business_violation_without_bus(self) -> None:
        cases = [
            (
                "sample_rate",
                lambda raw: _set_global_rate(raw, 44100),
                "SAMPLE_RATE_MISMATCH",
            ),
            (
                "bit_depth",
                lambda raw: _set_global_depth(raw, 16),
                "BIT_DEPTH_MISMATCH",
            ),
            (
                "channel_count",
                lambda raw: raw["actors"][0].__setitem__("channels", 2),
                "CHANNEL_COUNT_MISMATCH",
            ),
            (
                "azimuth",
                lambda raw: raw["speakers"][0].__setitem__("azimuth_deg", 181.0),
                "SPEAKER_AZIMUTH_OUT_OF_RANGE",
            ),
            (
                "duplicate_speaker",
                lambda raw: raw["speakers"][1].__setitem__(
                    "id", raw["speakers"][0]["id"]
                ),
                "DUPLICATE_SPEAKER_ID",
            ),
        ]
        for name, mutate, code in cases:
            with self.subTest(name=name):
                raw = json.loads(EXAMPLE_PROJECT.read_text("utf-8"))
                mutate(raw)
                project_path = self.temp / f"invalid_{name}.json"
                project_path.write_text(json.dumps(raw), encoding="utf-8")
                output = self.temp / f"invalid_{name}_bus"
                with self.assertRaises(ValidationFailure) as context:
                    synthesize(project_path, self.input_dir, output, fresh=True)
                self.assertIn(code, [item["code"] for item in context.exception.errors])
                self.assertTrue(
                    all("channel" in item for item in context.exception.errors)
                )
                self.assertFalse((output / "binaural_master.wav").exists())

    def test_missing_channel_file_is_structured(self) -> None:
        raw = json.loads(EXAMPLE_PROJECT.read_text("utf-8"))
        raw["actors"][0]["channel"] = "missing_actor.wav"
        project_path = self.temp / "invalid_missing.json"
        project_path.write_text(json.dumps(raw), encoding="utf-8")
        with self.assertRaises(ValidationFailure) as context:
            load_project(project_path, self.input_dir)
        errors = context.exception.errors
        self.assertEqual(errors[0]["code"], "CHANNEL_FILE_MISSING")
        self.assertEqual(errors[0]["channel"], "missing_actor.wav")

    def test_crosstalk_estimates_match_project_and_energy_drops(self) -> None:
        material = generate_material(self.project)
        result = estimate_and_cancel_crosstalk(
            self.project, material.actor_mics, material.speakers
        )
        declared = {
            (path.actor_id, path.speaker_id): path
            for path in self.project.crosstalk.feedback
        }
        for actor_id, actor_result in result.actors.items():
            self.assertLess(
                actor_result.residual_ratio,
                self.project.crosstalk.residual_energy_ratio_max,
            )
            self.assertIsNotNone(actor_result.direct_loss_ratio)
            self.assertLessEqual(
                actor_result.direct_loss_ratio,
                self.project.crosstalk.max_direct_energy_loss_ratio,
            )
            for pair in actor_result.pairs:
                expected = declared[(actor_id, pair.speaker_id)]
                expected_delay = round(expected.delay_ms * self.project.sample_rate / 1000.0)
                self.assertEqual(pair.delay_samples, expected_delay)
                self.assertAlmostEqual(pair.gain_db, expected.gain_db, places=5)
                self.assertLess(pair.after_energy, pair.before_energy * 1e-8)
            residual = actor_result.clean - material.actor_direct[actor_id]
            residual_ratio = float(
                np.dot(residual, residual)
                / max(np.dot(material.actor_direct[actor_id], material.actor_direct[actor_id]), 1e-30)
            )
            self.assertLess(residual_ratio, 1e-8)

    def test_spatial_direction_sign_and_acoustic_monotonicity(self) -> None:
        actor = self.project.actors[0]
        signal = self.generated.actor_direct[actor.id]
        ild_values = []
        itd_values = []
        for azimuth in (-60.0, -30.0, 0.0, 30.0, 60.0):
            constant = dataclasses.replace(
                actor, timeline=(TimelinePoint(0.0, azimuth, 0.0),)
            )
            rendered = render_actor(self.project, constant, signal)
            left_energy = float(np.dot(rendered.left, rendered.left))
            right_energy = float(np.dot(rendered.right, rendered.right))
            ild = 10.0 * math.log10(right_energy / left_energy)
            itd = _estimate_itd_us(rendered.left, rendered.right, self.project.sample_rate)
            ild_values.append(ild)
            itd_values.append(itd)
            self.assertAlmostEqual(
                ild, rendered.summary["rendered_energy_ild_db"], places=10
            )
        self.assertTrue(all(a < b for a, b in zip(ild_values, ild_values[1:])))
        self.assertTrue(all(a < b for a, b in zip(itd_values, itd_values[1:])))
        self.assertAlmostEqual(ild_values[2], 0.0, places=10)
        self.assertAlmostEqual(itd_values[2], 0.0, places=3)
        constant = dataclasses.replace(
            actor, timeline=(TimelinePoint(0.0, 37.0, 9.0),)
        )
        first = render_actor(self.project, constant, signal)
        second = render_actor(self.project, constant, signal)
        self.assertEqual(first.left.tobytes(), second.left.tobytes())
        self.assertEqual(first.right.tobytes(), second.right.tobytes())

    def test_report_rate_limits_and_bus_compliance(self) -> None:
        report = json.loads(Path(self.full.report_path).read_text("utf-8"))
        spatial = report["spatial"]
        self.assertTrue(spatial["all_rate_limits_compliant"])
        self.assertFalse(spatial["sample_discontinuity_detected"])
        for actor in spatial["actors"]:
            self.assertLessEqual(
                actor["max_ild_rate_db_per_s"],
                report["spatial"]["rate_limits"]["max_ild_rate_db_per_s"] + 1e-6,
            )
            self.assertLessEqual(
                actor["max_itd_rate_us_per_s"],
                report["spatial"]["rate_limits"]["max_itd_rate_us_per_s"] + 1e-6,
            )
            self.assertGreater(len(actor["segments"]), 0)
        bus = report["bus"]
        self.assertLessEqual(abs(bus["loudness_deviation_lu"]), 0.5)
        self.assertLessEqual(bus["true_peak_dbtp"], bus["true_peak_limit_dbtp"])
        self.assertGreaterEqual(bus["suppressed_samples"], 0)
        self.assertAlmostEqual(
            sum(item["ratio"] for item in bus["contributions"]), 1.0, places=10
        )

        info = inspect_wav(self.full.master_path)
        self.assertEqual((info.sample_rate, info.bit_depth, info.channels), (48000, 24, 2))
        self.assertEqual(info.frames, 384000)
        samples, sample_rate = read_wav(self.full.master_path)
        measured_tp = true_peak_dbtp(samples[0], samples[1], sample_rate)
        measured_lufs = integrated_loudness(samples[0], samples[1], sample_rate)
        limit_linear = 10.0 ** (bus["true_peak_limit_dbtp"] / 20.0)
        self.assertLessEqual(measured_tp, -1.0 + 1e-7)
        self.assertLessEqual(float(np.max(np.abs(samples))), limit_linear)
        self.assertIsNotNone(measured_lufs)
        self.assertLessEqual(abs(measured_lufs - (-16.0)), 0.5)

    def test_limiter_meets_target_and_true_peak_on_high_crest_signal(self) -> None:
        fs = 48000
        time = np.arange(3 * fs) / fs
        signal = 0.08 * np.sin(2.0 * np.pi * 440.0 * time)
        for start in range(0, signal.size, 4800):
            count = min(12, signal.size - start)
            signal[start : start + count] += 0.9 * np.exp(-np.arange(count) / 3.0)
        export = ExportConfig("wav", fs, 24, 2, -16.0, -1.0, 4096)
        result = master_binaural_bus(signal, 0.98 * signal, export)
        self.assertTrue(result.limiter_engaged)
        self.assertGreater(result.suppressed_samples, 0)
        self.assertLessEqual(abs(result.loudness_deviation_lu), 0.5)
        self.assertLessEqual(result.true_peak_dbtp, -1.0)
        self.assertLessEqual(
            float(np.max(np.abs(np.concatenate([result.left, result.right])))),
            10.0 ** (-1.0 / 20.0),
        )

    def test_resume_is_byte_identical_and_corruption_falls_back(self) -> None:
        output = self.temp / "resume"
        interrupted = synthesize(
            EXAMPLE_PROJECT,
            self.input_dir,
            output,
            fresh=True,
            interrupt_after_blocks=4,
        )
        self.assertEqual(interrupted.status, "interrupted")
        recovery = json.loads(Path(interrupted.recovery_path).read_text("utf-8"))
        self.assertEqual(recovery["exit_code"], 0)
        self.assertEqual(recovery["completed_blocks"], 4)
        self.assertFalse((output / "production_report.json").exists())
        resumed = synthesize(EXAMPLE_PROJECT, self.input_dir, output)
        self.assertEqual(resumed.status, "complete")
        self.assertEqual(
            _sha256(Path(resumed.master_path)),
            _sha256(Path(self.full.master_path)),
        )
        snapshot = sorted((output / ".progress" / "snapshots").glob("snapshot_*.json"))[-1]
        snapshot_payload = json.loads(snapshot.read_text("utf-8"))
        self.assertIn("crosstalk_estimates", snapshot_payload)
        self.assertIn("spatial_parameters", snapshot_payload)

        corrupt_output = self.temp / "corrupt"
        synthesize(
            EXAMPLE_PROJECT,
            self.input_dir,
            corrupt_output,
            fresh=True,
            interrupt_after_blocks=5,
        )
        snapshots = sorted(
            (corrupt_output / ".progress" / "snapshots").glob("snapshot_*.json")
        )
        snapshots[-1].write_text("{corrupt", encoding="utf-8")
        fallback = synthesize(EXAMPLE_PROJECT, self.input_dir, corrupt_output)
        self.assertEqual(fallback.status, "complete")
        self.assertEqual(fallback.resume_code, "E_SNAPSHOT_CORRUPT_FALLBACK")
        self.assertEqual(
            _sha256(Path(fallback.master_path)),
            _sha256(Path(self.full.master_path)),
        )

    def test_project_digest_mismatch_discards_progress(self) -> None:
        output = self.temp / "mismatch"
        synthesize(
            EXAMPLE_PROJECT,
            self.input_dir,
            output,
            fresh=True,
            interrupt_after_blocks=5,
        )
        raw = json.loads(EXAMPLE_PROJECT.read_text("utf-8"))
        raw["actors"][0]["timeline"][0]["azimuth_deg"] = -70.0
        changed_project = self.temp / "changed_project.json"
        changed_project.write_text(json.dumps(raw), encoding="utf-8")
        result = synthesize(changed_project, self.input_dir, output)
        self.assertEqual(result.status, "complete")
        self.assertEqual(result.resume_code, "E_RESUME_MISMATCH")
        self.assertEqual(result.report["resume"]["status"], "mismatch_restarted")
        self.assertNotEqual(
            _sha256(Path(result.master_path)),
            _sha256(Path(self.full.master_path)),
        )

    def test_report_is_stable_and_has_no_timestamps_or_absolute_paths(self) -> None:
        first = Path(self.full.report_path).read_bytes()
        second = Path(self.second.report_path).read_bytes()
        self.assertEqual(first, second)
        text = first.decode("utf-8")
        self.assertNotIn(str(self.temp), text)
        self.assertNotIn("/tmp/", text)
        self.assertNotIn("timestamp", text.lower())


def _set_global_rate(raw: dict, rate: int) -> None:
    raw["sample_rate"] = rate
    raw["export"]["sample_rate"] = rate


def _set_global_depth(raw: dict, depth: int) -> None:
    raw["bit_depth"] = depth
    raw["export"]["bit_depth"] = depth


def _tree_hashes(directory: Path) -> dict[str, str]:
    return {
        str(path.relative_to(directory)).replace("\\", "/"): _sha256(path)
        for path in sorted(directory.rglob("*"))
        if path.is_file()
    }


def _sha256(path: Path) -> str:
    return hashlib.sha256(path.read_bytes()).hexdigest()


def _estimate_itd_us(left: np.ndarray, right: np.ndarray, sample_rate: int) -> float:
    n = left.size
    nfft = 1 << (2 * n - 1).bit_length()
    correlation = np.fft.irfft(
        np.fft.rfft(left, nfft) * np.conj(np.fft.rfft(right, nfft)), nfft
    )
    lags = np.concatenate((np.arange(200), np.arange(-200, 0)))
    values = np.concatenate((correlation[:200], correlation[-200:]))
    lag = int(lags[int(np.argmax(values))])
    return lag / sample_rate * 1_000_000.0


if __name__ == "__main__":
    unittest.main()

