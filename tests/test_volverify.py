from __future__ import annotations

import copy
import json
import os
import shutil
import signal
import subprocess
import sys
import tempfile
import time
import unittest
from pathlib import Path

import numpy as np
from PIL import Image

from volverify.compare import compare_render_dirs
from volverify.io_utils import atomic_write_bytes, atomic_write_json, sha256_bytes
from volverify.pipeline import render_scene
from volverify.sample_data import (
    INVALID_PROFILES,
    generate_sample_suite,
)
from volverify.validation import load_validated_scene

REPO_ROOT = Path(__file__).resolve().parents[1]


def run_cli(
    *arguments: object,
    env: dict[str, str] | None = None,
) -> subprocess.CompletedProcess[str]:
    merged_env = os.environ.copy()
    if env:
        merged_env.update(env)
    return subprocess.run(
        [sys.executable, "-m", "volverify", *map(str, arguments)],
        cwd=REPO_ROOT,
        env=merged_env,
        capture_output=True,
        text=True,
        timeout=60,
    )


def wait_for_chunks(
    manifest_path: Path, minimum: int, timeout: float = 20.0
) -> int:
    deadline = time.monotonic() + timeout
    latest = 0
    while time.monotonic() < deadline:
        try:
            manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
            latest = len(manifest.get("completed", []))
            if latest >= minimum:
                return latest
        except (OSError, ValueError):
            pass
        time.sleep(0.02)
    raise AssertionError(
        f"timed out waiting for {minimum} chunks; saw {latest}"
    )


def copy_scene(
    source_scene: Path,
    destination: Path,
    mutate: callable,
) -> Path:
    destination.mkdir(parents=True, exist_ok=True)
    scene = json.loads(source_scene.read_text(encoding="utf-8"))
    source_volume = source_scene.parent / scene["volume"]["data_file"]
    shutil.copyfile(source_volume, destination / "volume.f32")
    mutate(scene)
    scene_path = destination / "scene.json"
    atomic_write_json(scene_path, scene)
    return scene_path


class VolumeVerifyAcceptanceTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.temp_dir = tempfile.TemporaryDirectory(prefix="volverify-tests-")
        cls.root = Path(cls.temp_dir.name)
        cls.samples = cls.root / "samples"
        generate_sample_suite(
            cls.samples,
            dims=(40, 32, 26),
            width=48,
            height=36,
        )
        cls.valid_scene = cls.samples / "valid" / "scene.json"
        cls.full_dir = cls.root / "full"
        result = run_cli(
            "render", "--scene", cls.valid_scene, "--out", cls.full_dir
        )
        if result.returncode != 0:
            raise RuntimeError(
                f"baseline render failed: {result.stdout}\n{result.stderr}"
            )

    @classmethod
    def tearDownClass(cls) -> None:
        cls.temp_dir.cleanup()

    def test_validation_codes_are_stable_and_reject_whole_frame(self) -> None:
        expected_codes = {
            "invalid_truncated": "VOLUME_TRUNCATED",
            "invalid_nan": "VOLUME_NONFINITE",
            "invalid_tf_overlap": "TF_INTERVAL_OVERLAP",
            "invalid_tf_uncovered": "TF_RANGE_UNCOVERED",
            "invalid_light_inside": "LIGHT_INSIDE_VOLUME",
            "invalid_camera": "CAMERA_FOV_INVALID",
            "invalid_range_violation": "VOLUME_RANGE_VIOLATION",
            "invalid_tf_boundary": "TF_BOUNDARY_DISCONTINUITY",
        }
        self.assertEqual(set(expected_codes), set(INVALID_PROFILES))
        for profile, expected_code in expected_codes.items():
            with self.subTest(profile=profile):
                scene_path = self.samples / profile / "scene.json"
                out_dir = self.root / "invalid-renders" / profile
                outcome = render_scene(scene_path, out_dir)
                self.assertEqual(outcome.exit_code, 2)
                self.assertEqual(outcome.status, "rejected")
                report = json.loads(
                    (out_dir / "frame_report.json").read_text(
                        encoding="utf-8"
                    )
                )
                self.assertIn(
                    expected_code,
                    [issue["code"] for issue in report["issues"]],
                )
                self.assertFalse((out_dir / "frame.rgba.png").exists())
                self.assertFalse((out_dir / "frame.depth.f32").exists())

    def test_infinite_density_is_rejected_with_location(self) -> None:
        scene_dir = self.root / "invalid-inf"
        scene_path = copy_scene(
            self.valid_scene, scene_dir, lambda scene: None
        )
        scene = json.loads(scene_path.read_text(encoding="utf-8"))
        volume_path = scene_dir / "volume.f32"
        values = np.fromfile(volume_path, dtype="<f4")
        flat_index = np.ravel_multi_index(
            (4, 5, 6), tuple(scene["volume"]["dims"])
        )
        values[flat_index] = np.inf
        raw = values.astype("<f4").tobytes()
        atomic_write_bytes(volume_path, raw)
        scene["volume"]["sha256"] = sha256_bytes(raw)
        atomic_write_json(scene_path, scene)
        outcome = render_scene(scene_path, self.root / "invalid-inf-out")
        self.assertEqual(outcome.exit_code, 2)
        issue = outcome.report["issues"][0]
        self.assertEqual(issue["code"], "VOLUME_NONFINITE")
        self.assertEqual(issue["details"]["count"], 1)
        self.assertEqual(issue["details"]["first_indices"], [[4, 5, 6]])
        self.assertEqual(issue["details"]["first_values"], ["Infinity"])

    def test_transfer_scan_clamps_and_is_continuous(self) -> None:
        scene = load_validated_scene(self.valid_scene)
        scan = scene.transfer.scan([-1.0, 0.0, 0.25, 0.5, 0.75, 1.0, 2.0])
        samples = scan["samples"]
        self.assertEqual(samples[0]["clamped_density"], 0.0)
        self.assertEqual(samples[-1]["clamped_density"], 1.0)
        for check in scan["boundary_checks"]:
            self.assertTrue(check["passed"])
            boundary = check["boundary"]
            left_absorption, left_emission = scene.transfer.evaluate(
                np.asarray([boundary - 1e-7])
            )
            right_absorption, right_emission = scene.transfer.evaluate(
                np.asarray([boundary + 1e-7])
            )
            self.assertLess(
                abs(float(left_absorption[0] - right_absorption[0])),
                1e-5,
            )
            self.assertLess(
                float(np.max(np.abs(left_emission[0] - right_emission[0]))),
                1e-5,
            )

    def test_render_artifacts_stats_and_suspicious_localization(self) -> None:
        stats = json.loads(
            (self.full_dir / "frame.stats.json").read_text(encoding="utf-8")
        )
        self.assertEqual(stats["status"], "rendered")
        self.assertTrue(stats["convergence"]["passed"])
        self.assertTrue(stats["convergence"]["monotonic"])
        deviations = stats["convergence"]["mean_relative_deviations"]
        self.assertGreaterEqual(len(deviations), 2)
        self.assertLessEqual(
            deviations[-1], stats["convergence"]["tolerance"]
        )
        self.assertTrue(
            all(
                later <= earlier + 1e-12
                for earlier, later in zip(deviations, deviations[1:])
            )
        )
        self.assertGreater(stats["sampling"]["total_samples"], 0)
        fraction_total = sum(
            light["fraction"] for light in stats["lights"]
        ) + stats["self_emission"]["fraction"]
        self.assertAlmostEqual(fraction_total, 1.0, places=9)
        self.assertEqual(
            stats["chunks"]["completed"], stats["chunks"]["total"]
        )
        self.assertGreater(stats["suspicious"]["pair_count"], 0)
        self.assertGreater(stats["suspicious"]["pixel_count"], 0)

        image = np.asarray(
            Image.open(self.full_dir / "frame.rgba.png")
        )
        depth_size = (self.full_dir / "frame.depth.f32").stat().st_size
        self.assertEqual(image.shape, (36, 48, 4))
        self.assertEqual(depth_size, 36 * 48 * 4)
        with Image.open(self.full_dir / "frame.rgba.png") as png:
            metadata = json.loads(png.info["volverify"])
        self.assertEqual(metadata["sampling"]["requested_step"], 0.02)
        self.assertIn("t_enter", metadata["sampling"]["ray_start_rule"])
        self.assertIn("midpoint", metadata["sampling"]["ray_start_rule"])

        suspicious = json.loads(
            (self.full_dir / "suspicious_pixels.json").read_text(
                encoding="utf-8"
            )
        )
        self.assertEqual(
            suspicious["pair_count"], stats["suspicious"]["pair_count"]
        )
        pair = next(
            item
            for item in suspicious["pairs"]
            if item["sampling_interval_a"] is not None
        )
        self.assertIn("opacity_delta", pair)
        interval = pair["sampling_interval_a"]
        self.assertIsNotNone(interval)
        self.assertEqual(len(interval["voxel_grid_min"]), 3)
        self.assertEqual(len(interval["peak_voxel"]), 3)
        self.assertGreater(interval["t_exit"], interval["t_enter"])

    def test_two_runs_are_byte_reproducible(self) -> None:
        second = self.root / "full-second"
        result = run_cli(
            "render", "--scene", self.valid_scene, "--out", second
        )
        self.assertEqual(result.returncode, 0, result.stderr)
        for name in (
            "frame.rgba.png",
            "frame.depth.f32",
            "frame.stats.json",
            "transfer_scan.json",
            "suspicious_pixels.json",
            "frame_report.json",
        ):
            with self.subTest(file=name):
                self.assertEqual(
                    (self.full_dir / name).read_bytes(),
                    (second / name).read_bytes(),
                )
        self.assertEqual(
            json.loads(
                (self.full_dir / "frame.stats.json").read_text(
                    encoding="utf-8"
                )
            ),
            json.loads(
                (second / "frame.stats.json").read_text(encoding="utf-8")
            ),
        )

    def test_chunk_size_and_light_order_do_not_change_visible_result(
        self,
    ) -> None:
        changed_scene = self.root / "changed-scene"

        def mutate(scene: dict) -> None:
            scene["render"]["chunk_size"] = [13, 10]
            scene["lights"].reverse()

        scene_path = copy_scene(self.valid_scene, changed_scene, mutate)
        changed_out = self.root / "changed-out"
        result = run_cli(
            "render", "--scene", scene_path, "--out", changed_out
        )
        self.assertEqual(result.returncode, 0, result.stderr)
        original_image = np.asarray(
            Image.open(self.full_dir / "frame.rgba.png")
        )
        changed_image = np.asarray(
            Image.open(changed_out / "frame.rgba.png")
        )
        np.testing.assert_array_equal(original_image, changed_image)
        self.assertEqual(
            (self.full_dir / "frame.depth.f32").read_bytes(),
            (changed_out / "frame.depth.f32").read_bytes(),
        )
        original_stats = json.loads(
            (self.full_dir / "frame.stats.json").read_text(
                encoding="utf-8"
            )
        )
        changed_stats = json.loads(
            (changed_out / "frame.stats.json").read_text(encoding="utf-8")
        )
        self.assertEqual(
            original_stats["sampling"]["total_samples"],
            changed_stats["sampling"]["total_samples"],
        )
        self.assertEqual(
            original_stats["lights"], changed_stats["lights"]
        )

    def test_convergence_failure_writes_no_image(self) -> None:
        scene_dir = self.root / "tight-tolerance"

        def mutate(scene: dict) -> None:
            scene["sampling"]["convergence_tolerance"] = 0.0

        scene_path = copy_scene(self.valid_scene, scene_dir, mutate)
        out_dir = self.root / "tight-tolerance-out"
        outcome = render_scene(scene_path, out_dir)
        self.assertEqual(outcome.exit_code, 2)
        self.assertEqual(outcome.status, "rejected")
        self.assertFalse((out_dir / "frame.rgba.png").exists())
        self.assertFalse((out_dir / "frame.depth.f32").exists())
        report = json.loads(
            (out_dir / "frame_report.json").read_text(encoding="utf-8")
        )
        self.assertEqual(report["issues"][0]["code"], "CONVERGENCE_FAILED")
        self.assertFalse(report["convergence"]["passed"])

    def test_batch_continues_past_invalid_frames(self) -> None:
        batch_out = self.root / "batch"
        result = run_cli(
            "batch",
            "--manifest",
            self.samples / "batch_manifest.json",
            "--out",
            batch_out,
        )
        self.assertEqual(result.returncode, 2)
        aggregate = json.loads(result.stdout)
        self.assertEqual(aggregate["status"], "completed_with_rejections")
        self.assertEqual(aggregate["frames_rendered"], 1)
        self.assertEqual(
            aggregate["frames_rejected"], len(INVALID_PROFILES)
        )
        self.assertTrue(
            (batch_out / "valid" / "frame.rgba.png").exists()
        )
        for profile in INVALID_PROFILES:
            self.assertFalse(
                (batch_out / profile / "frame.rgba.png").exists()
            )

    def test_sigkill_resume_matches_full_baseline(self) -> None:
        out_dir = self.root / "sigkill"
        env = os.environ.copy()
        env["VOLVERIFY_CHUNK_DELAY_MS"] = "80"
        process = subprocess.Popen(
            [
                sys.executable,
                "-m",
                "volverify",
                "render",
                "--scene",
                str(self.valid_scene),
                "--out",
                str(out_dir),
            ],
            cwd=REPO_ROOT,
            env=env,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
        )
        manifest_path = out_dir / ".volverify-state" / "manifest.json"
        before_kill = wait_for_chunks(manifest_path, 2)
        process.kill()
        stdout, stderr = process.communicate(timeout=10)
        self.assertLess(process.returncode, 0, stderr)
        manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
        completed_before_resume = len(manifest["completed"])
        self.assertEqual(manifest["status"], "running")
        self.assertGreaterEqual(len(manifest["completed"]), 2)
        self.assertLess(len(manifest["completed"]), len(manifest["chunk_layout"]))
        self.assertFalse((out_dir / "recovery_report.json").exists())

        result = run_cli(
            "resume",
            "--scene",
            self.valid_scene,
            "--out",
            out_dir,
            "--compare-full",
            self.full_dir,
        )
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        resume_report = json.loads(
            (out_dir / "resume_report.json").read_text(encoding="utf-8")
        )
        self.assertEqual(resume_report["code"], "SNAPSHOT_RESUMED")
        self.assertEqual(
            resume_report["resumed_chunks"], completed_before_resume
        )
        diff = json.loads(
            (out_dir / "diff.json").read_text(encoding="utf-8")
        )
        self.assertEqual(diff["status"], "identical")
        self.assertEqual(
            (out_dir / "frame.rgba.png").read_bytes(),
            (self.full_dir / "frame.rgba.png").read_bytes(),
        )
        self.assertEqual(
            (out_dir / "frame.depth.f32").read_bytes(),
            (self.full_dir / "frame.depth.f32").read_bytes(),
        )
        self.assertEqual(
            (out_dir / "frame.stats.json").read_bytes(),
            (self.full_dir / "frame.stats.json").read_bytes(),
        )
        self.assertEqual(
            (out_dir / "suspicious_pixels.json").read_bytes(),
            (self.full_dir / "suspicious_pixels.json").read_bytes(),
        )

    def test_sigterm_writes_recovery_report_and_resumes(self) -> None:
        out_dir = self.root / "sigterm"
        env = os.environ.copy()
        env["VOLVERIFY_CHUNK_DELAY_MS"] = "80"
        process = subprocess.Popen(
            [
                sys.executable,
                "-m",
                "volverify",
                "render",
                "--scene",
                str(self.valid_scene),
                "--out",
                str(out_dir),
            ],
            cwd=REPO_ROOT,
            env=env,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
        )
        manifest_path = out_dir / ".volverify-state" / "manifest.json"
        wait_for_chunks(manifest_path, 2)
        process.send_signal(signal.SIGTERM)
        stdout, stderr = process.communicate(timeout=10)
        self.assertEqual(process.returncode, 143, stdout + stderr)
        recovery = json.loads(
            (out_dir / "recovery_report.json").read_text(encoding="utf-8")
        )
        self.assertEqual(recovery["code"], "INTERRUPTED")
        self.assertEqual(recovery["signal"], int(signal.SIGTERM))
        self.assertGreater(recovery["completed_chunks"], 0)
        manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
        self.assertEqual(manifest["status"], "interrupted")

        result = run_cli(
            "resume",
            "--scene",
            self.valid_scene,
            "--out",
            out_dir,
            "--compare-full",
            self.full_dir,
        )
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual(
            json.loads(
                (out_dir / "diff.json").read_text(encoding="utf-8")
            )["status"],
            "identical",
        )
        self.assertEqual(
            (out_dir / "frame.stats.json").read_bytes(),
            (self.full_dir / "frame.stats.json").read_bytes(),
        )

    def test_snapshot_input_mismatch_invalidates_all_old_chunks(self) -> None:
        scene_dir = self.root / "mismatch-scene"
        scene_path = copy_scene(
            self.valid_scene, scene_dir, lambda scene: None
        )
        scene = json.loads(scene_path.read_text(encoding="utf-8"))
        scene["render"]["chunk_size"] = [20, 15]
        atomic_write_json(scene_path, scene)

        out_dir = self.root / "mismatch-out"
        env = os.environ.copy()
        env["VOLVERIFY_CHUNK_DELAY_MS"] = "100"
        process = subprocess.Popen(
            [
                sys.executable,
                "-m",
                "volverify",
                "render",
                "--scene",
                str(scene_path),
                "--out",
                str(out_dir),
            ],
            cwd=REPO_ROOT,
            env=env,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
        )
        manifest_path = out_dir / ".volverify-state" / "manifest.json"
        wait_for_chunks(manifest_path, 2)
        process.kill()
        process.communicate(timeout=10)
        old_manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
        old_chunks = len(old_manifest["completed"])

        scene["render"]["chunk_size"] = [13, 11]
        atomic_write_json(scene_path, scene)
        baseline = self.root / "mismatch-full"
        baseline_result = run_cli(
            "render", "--scene", scene_path, "--out", baseline
        )
        self.assertEqual(
            baseline_result.returncode, 0, baseline_result.stderr
        )
        result = run_cli(
            "resume",
            "--scene",
            scene_path,
            "--out",
            out_dir,
            "--compare-full",
            baseline,
        )
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        recovery = json.loads(
            (out_dir / "recovery_report.json").read_text(encoding="utf-8")
        )
        self.assertEqual(recovery["code"], "SNAPSHOT_INPUT_MISMATCH")
        self.assertEqual(recovery["invalidated_chunks"], old_chunks)
        manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
        expected_new_chunks = (
            (48 + 13 - 1) // 13
        ) * ((36 + 11 - 1) // 11)
        self.assertEqual(len(manifest["completed"]), expected_new_chunks)
        self.assertEqual(
            json.loads(
                (out_dir / "diff.json").read_text(encoding="utf-8")
            )["status"],
            "identical",
        )

    def test_compare_reports_chunk_pixel_and_delta(self) -> None:
        broken = self.root / "broken-full"
        shutil.copytree(self.full_dir, broken)
        image = np.asarray(Image.open(broken / "frame.rgba.png")).copy()
        image[8, 9] = [255, 0, 0, 255]
        Image.fromarray(image, "RGBA").save(broken / "frame.rgba.png")
        depth = np.fromfile(broken / "frame.depth.f32", dtype="<f4").copy()
        depth[8 * 48 + 9] += 0.25
        depth.tofile(broken / "frame.depth.f32")

        report_path = self.root / "diff.json"
        result = run_cli(
            "compare",
            "--scene",
            self.valid_scene,
            "--left",
            self.full_dir,
            "--right",
            broken,
            "--report",
            report_path,
        )
        self.assertEqual(result.returncode, 4)
        report = json.loads(report_path.read_text(encoding="utf-8"))
        self.assertEqual(report["code"], "INCREMENTAL_RESULT_MISMATCH")
        self.assertEqual(report["total_mismatch_pixels"], 1)
        chunk = report["chunks"][0]
        self.assertIn("chunk_id", chunk)
        self.assertEqual(chunk["samples"][0]["pixel"], [9, 8])
        self.assertGreater(chunk["max_rgba_delta"], 0)
        self.assertAlmostEqual(chunk["max_depth_delta"], 0.25, places=6)


if __name__ == "__main__":
    unittest.main()

