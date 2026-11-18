from __future__ import annotations

import json
import os
import shutil
import signal
import struct
import subprocess
import sys
import tempfile
import time
import unittest
from pathlib import Path

import numpy as np
import soundfile as sf


ROOT = Path(__file__).resolve().parents[1]
SRC = ROOT / "src"
sys.path.insert(0, str(SRC))

from rainforest_master.demo import demo_manifest  # noqa: E402
from rainforest_master.errors import STRUCTURED_ERROR, BusinessError  # noqa: E402
from rainforest_master.manifest import load_manifest  # noqa: E402
from rainforest_master.render import Renderer  # noqa: E402
from rainforest_master.synthesis import synthesize_project  # noqa: E402
from rainforest_master.wavio import write_float32_wav  # noqa: E402


CLI = ROOT / "rainforest_master.py"


def run_cli(*args: str, timeout: float = 120.0) -> subprocess.CompletedProcess[str]:
    return subprocess.run(
        [sys.executable, str(CLI), *args],
        cwd=str(ROOT),
        text=True,
        capture_output=True,
        timeout=timeout,
    )


class PipelineTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls._temp = tempfile.TemporaryDirectory(prefix="rainforest-tests-")
        cls.base = Path(cls._temp.name)
        cls.project = cls.base / "project"
        demo_manifest(cls.project, seed=249, duration_seconds=4.0, block_frames=12_000)
        synthesize_project(cls.project / "manifest.json")

    @classmethod
    def tearDownClass(cls) -> None:
        cls._temp.cleanup()

    def copy_project(self, name: str) -> Path:
        target = self.base / name
        shutil.copytree(self.project, target)
        return target

    def test_synthesis_is_byte_deterministic_and_events_align(self) -> None:
        second = self.base / "synthesis-second"
        shutil.copytree(self.project, second)
        synthesize_project(second / "manifest.json")
        first_manifest = load_manifest(self.project / "manifest.json")
        second_manifest = load_manifest(second / "manifest.json")
        for first_track, second_track in zip(first_manifest.tracks, second_manifest.tracks):
            self.assertEqual(first_track.path.read_bytes(), second_track.path.read_bytes())

        manifest = load_manifest(self.project / "manifest.json")
        for scene in ("雨滴", "鸟鸣", "脚步"):
            track = manifest.track(scene)
            audio, sample_rate = sf.read(str(track.path), dtype="float32", always_2d=False)
            self.assertEqual(sample_rate, 48_000)
            for seconds in track.transient_reference_times:
                start = int(round(seconds * sample_rate))
                attack = float(np.max(np.abs(audio[start : start + int(0.008 * sample_rate)])))
                before = float(
                    np.max(
                        np.abs(
                            audio[
                                max(0, start - int(0.010 * sample_rate)) : max(1, start - 2)
                            ]
                        )
                    )
                )
                self.assertGreater(attack, before * 2.0)

    def test_manifest_validation_business_errors(self) -> None:
        cases: list[tuple[str, callable]] = [
            (
                "missing",
                lambda root: (root / "stems/01_rain_drops.wav").unlink(),
            ),
            (
                "sample_rate",
                lambda root: write_float32_wav(
                    root / "stems/01_rain_drops.wav",
                    np.zeros(1000, dtype=np.float32),
                    44_100,
                ),
            ),
            (
                "channels",
                lambda root: write_float32_wav(
                    root / "stems/01_rain_drops.wav",
                    np.zeros((1000, 2), dtype=np.float32),
                    48_000,
                ),
            ),
            (
                "bit_depth",
                lambda root: sf.write(
                    str(root / "stems/01_rain_drops.wav"),
                    np.zeros(1000, dtype=np.float32),
                    48_000,
                    subtype="PCM_16",
                    format="WAV",
                ),
            ),
            (
                "duplicate",
                lambda root: self._mutate_manifest(
                    root, lambda data: data["tracks"][3].__setitem__("scene_name", "雨滴")
                ),
            ),
            (
                "order",
                lambda root: self._mutate_manifest(
                    root,
                    lambda data: data["tracks"][4].__setitem__(
                        "transient_reference_times",
                        list(reversed(data["tracks"][4]["transient_reference_times"])),
                    ),
                ),
            ),
        ]
        for name, mutate in cases:
            with self.subTest(name=name):
                project = self.copy_project(f"invalid-{name}")
                mutate(project)
                with self.assertRaises(BusinessError) as context:
                    load_manifest(project / "manifest.json")
                payload = context.exception.as_dict()
                self.assertEqual(payload["code"], STRUCTURED_ERROR)
                self.assertTrue(payload["errors"])
                for item in payload["errors"]:
                    self.assertEqual(item["code"], STRUCTURED_ERROR)
                    self.assertTrue(item["channel"])

    @staticmethod
    def _mutate_manifest(root: Path, mutation) -> None:
        path = root / "manifest.json"
        data = json.loads(path.read_text(encoding="utf-8"))
        mutation(data)
        path.write_text(json.dumps(data, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")

    def test_render_compliance_and_repeatability(self) -> None:
        out_a = self.base / "render-a"
        out_b = self.base / "render-b"
        first = run_cli(
            "render",
            "--manifest",
            str(self.project / "manifest.json"),
            "--out-dir",
            str(out_a),
            "--quiet",
        )
        second = run_cli(
            "render",
            "--manifest",
            str(self.project / "manifest.json"),
            "--out-dir",
            str(out_b),
            "--quiet",
        )
        self.assertEqual(first.returncode, 0, first.stderr)
        self.assertEqual(second.returncode, 0, second.stderr)
        for name in (
            "rainforest_stereo_master.wav",
            "rainforest_mono_master.wav",
            "render_report.json",
        ):
            self.assertEqual((out_a / name).read_bytes(), (out_b / name).read_bytes())

        report = json.loads((out_a / "render_report.json").read_text(encoding="utf-8"))
        self.assertEqual(report["status"], "PASS")
        self.assertTrue(all(report["checks"].values()))
        self.assertEqual(report["frames"], 4 * 48_000)
        self.assertLessEqual(
            report["loop"]["seam_sample_delta"],
            report["loop"]["max_allowed_sample_delta"],
        )
        self.assertLessEqual(report["mastering"]["loudness_delta_lu"], 0.5)
        self.assertLessEqual(report["mastering"]["true_peak_after_mastering_dbtp"], -1.0 + 1e-6)
        for channel in report["panning"]["channels"].values():
            self.assertTrue(channel["passes_stability"])
            self.assertFalse(channel["full_side_jump_detected"])
        for item in report["masking"]["channels"].values():
            self.assertGreaterEqual(item["snr_improvement_db"], 6.0)
            self.assertGreater(item["modified_sample_count"], 0)
        self.assertLessEqual(
            report["mono_compatibility"]["bird_insect_scene_ratio"]["deviation_db"],
            report["mono_compatibility"]["tolerance_db"],
        )
        stereo, sample_rate = sf.read(
            str(out_a / "rainforest_stereo_master.wav"), dtype="float32", always_2d=True
        )
        mono, _ = sf.read(
            str(out_a / "rainforest_mono_master.wav"), dtype="float32", always_2d=False
        )
        self.assertEqual(sample_rate, 48_000)
        self.assertEqual(stereo.shape, (4 * 48_000, 2))
        self.assertEqual(mono.shape, (4 * 48_000,))
        self.assertLess(
            float(np.max(np.abs(mono - 0.5 * (stereo[:, 0] + stereo[:, 1])))),
            1.0e-6,
        )

    def test_masking_never_modifies_samples_outside_declared_windows(self) -> None:
        manifest = load_manifest(self.project / "manifest.json")
        probe_dir = self.base / "mask-probe"
        with Renderer(manifest, probe_dir) as renderer:
            first_window = min(renderer.plan.windows, key=lambda item: item.start)
            start = max(0, first_window.start - 500)
            end = min(manifest.frames, first_window.end + 500)
            ducked = renderer._render_mix_block(start, end, apply_duck=True)
            untouched = renderer._render_mix_block(start, end, apply_duck=False)
            indices = np.arange(start, end)
            mask = np.zeros(indices.shape[0], dtype=bool)
            for window in renderer.plan.windows:
                mask |= (indices >= window.start) & (indices < window.end)
            self.assertTrue(np.any(mask))
            self.assertTrue(np.allclose(ducked[~mask], untouched[~mask], rtol=0.0, atol=0.0))
            self.assertGreater(float(np.max(np.abs(ducked[mask] - untouched[mask]))), 0.0)

    def test_snapshot_resume_is_byte_identical(self) -> None:
        one_shot = self.base / "resume-oneshot"
        resumed = self.base / "resume-resumed"
        self.assertEqual(
            run_cli(
                "render",
                "--manifest",
                str(self.project / "manifest.json"),
                "--out-dir",
                str(one_shot),
                "--quiet",
            ).returncode,
            0,
        )
        stopped = run_cli(
            "render",
            "--manifest",
            str(self.project / "manifest.json"),
            "--out-dir",
            str(resumed),
            "--stop-after-blocks",
            "5",
            "--quiet",
        )
        self.assertEqual(stopped.returncode, 0, stopped.stderr)
        self.assertTrue((resumed / "recovery_report.json").is_file())
        continued = run_cli(
            "render",
            "--manifest",
            str(self.project / "manifest.json"),
            "--out-dir",
            str(resumed),
            "--quiet",
        )
        self.assertEqual(continued.returncode, 0, continued.stderr)
        compared = run_cli(
            "verify",
            "--reference",
            str(one_shot),
            "--candidate",
            str(resumed),
            "--block-frames",
            "12000",
        )
        self.assertEqual(compared.returncode, 0, compared.stderr)
        self.assertEqual(
            json.loads(compared.stdout)["code"],
            "OUTPUTS_IDENTICAL",
        )

    def test_resume_mismatch_discards_progress(self) -> None:
        project = self.copy_project("mismatch-project")
        out = self.base / "mismatch-output"
        stopped = run_cli(
            "render",
            "--manifest",
            str(project / "manifest.json"),
            "--out-dir",
            str(out),
            "--stop-after-blocks",
            "3",
            "--quiet",
        )
        self.assertEqual(stopped.returncode, 0, stopped.stderr)
        self.assertTrue((out / ".render_state/snapshot.json").is_file())
        self._mutate_manifest(project, lambda data: data.__setitem__("seed", 250))
        mismatch = run_cli(
            "render",
            "--manifest",
            str(project / "manifest.json"),
            "--out-dir",
            str(out),
            "--quiet",
        )
        self.assertEqual(mismatch.returncode, 3)
        payload = json.loads(mismatch.stderr)
        self.assertEqual(payload["code"], "E_RESUME_MISMATCH")
        self.assertTrue(payload["old_progress_discarded"])
        self.assertFalse((out / ".render_state/snapshot.json").exists())

    def test_verify_reports_block_channel_and_difference_type(self) -> None:
        reference = self.base / "verify-reference"
        candidate = self.base / "verify-candidate"
        self.assertEqual(
            run_cli(
                "render",
                "--manifest",
                str(self.project / "manifest.json"),
                "--out-dir",
                str(reference),
                "--quiet",
            ).returncode,
            0,
        )
        shutil.copytree(reference, candidate)
        wav = candidate / "rainforest_stereo_master.wav"
        data = bytearray(wav.read_bytes())
        data[56 + 1] ^= 0x01
        wav.write_bytes(data)
        verification = run_cli(
            "verify",
            "--reference",
            str(reference),
            "--candidate",
            str(candidate),
            "--block-frames",
            "12000",
        )
        self.assertEqual(verification.returncode, 4)
        payload = json.loads(verification.stderr)
        self.assertEqual(payload["code"], "E_RENDER_DIFFERENCE")
        audio_differences = [
            item for item in payload["differences"] if item["difference_type"] == "byte_mismatch"
        ]
        self.assertTrue(audio_differences)
        self.assertEqual(audio_differences[0]["block"], 0)
        self.assertEqual(audio_differences[0]["channel"], "left")

    def test_sigint_commits_current_block_and_exits_zero(self) -> None:
        out = self.base / "sigint-output"
        process = subprocess.Popen(
            [
                sys.executable,
                str(CLI),
                "render",
                "--manifest",
                str(self.project / "manifest.json"),
                "--out-dir",
                str(out),
                "--quiet",
            ],
            cwd=str(ROOT),
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
        )
        snapshot = out / ".render_state/snapshot.json"
        deadline = time.time() + 10.0
        while time.time() < deadline and not snapshot.exists() and process.poll() is None:
            time.sleep(0.01)
        if process.poll() is None:
            process.send_signal(signal.SIGINT)
        stdout, stderr = process.communicate(timeout=30)
        self.assertEqual(process.returncode, 0, stderr)
        payload = json.loads(stdout)
        self.assertEqual(payload["code"], "CHECKPOINT_COMMITTED")
        self.assertLess(payload["completed_blocks"], payload["total_blocks"])
        recovery = json.loads((out / "recovery_report.json").read_text(encoding="utf-8"))
        self.assertEqual(recovery["status"], "INTERRUPTED")
        self.assertEqual(recovery["completed_blocks"], payload["completed_blocks"])


if __name__ == "__main__":
    unittest.main()
