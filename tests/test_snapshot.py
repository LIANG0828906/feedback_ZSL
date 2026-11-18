from __future__ import annotations

import json
import os
import subprocess
import sys
import tempfile
import time
import unittest
from pathlib import Path

from leather_raster.snapshot import snapshot_file

from .support import ROOT, load_json, run_cli, write_scene


class SnapshotTests(unittest.TestCase):
    def test_graceful_interrupt_then_resume_is_byte_identical(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            scene_path, _ = write_scene(root, width=96, height=72, block_width=32, block_height=24)
            full = root / "full"
            resumed = root / "resumed"
            self.assertEqual(run_cli("render", str(scene_path), "--out", str(full)).returncode, 0)

            interrupted = run_cli(
                "render",
                str(scene_path),
                "--out",
                str(resumed),
                "--interrupt-after-blocks",
                "3",
            )
            self.assertEqual(interrupted.returncode, 0, interrupted.stderr)
            report = load_json(resumed / "recovery_report.json")
            self.assertEqual(report["status"], "interrupted")
            self.assertEqual(report["exit_code"], 0)
            self.assertEqual(report["completed_blocks"], 3)
            self.assertFalse((resumed / "rgba.bin").exists())
            checkpoint = load_json(snapshot_file(resumed))
            self.assertEqual(len(checkpoint["blocks"]), 3)
            self.assertIn("color_summary", checkpoint["blocks"][0])

            resumed_result = run_cli(
                "resume",
                str(scene_path),
                "--out",
                str(resumed),
                "--verify-baseline",
                str(full),
            )
            self.assertEqual(resumed_result.returncode, 0, resumed_result.stderr)
            report = load_json(resumed / "recovery_report.json")
            self.assertEqual(report["status"], "completed")
            self.assertGreater(report["loaded_blocks_at_start"], 0)
            for name in ("rgba.bin", "layer_map.bin", "projection_points.json", "stats.json"):
                self.assertEqual((full / name).read_bytes(), (resumed / name).read_bytes())

    def test_corrupt_snapshot_is_discarded_whole(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            scene_path, _ = write_scene(root, width=80, height=60, block_width=20, block_height=20)
            full = root / "full"
            damaged = root / "damaged"
            run_cli("render", str(scene_path), "--out", str(full))
            run_cli("render", str(scene_path), "--out", str(damaged), "--interrupt-after-blocks", "4")
            checkpoint = snapshot_file(damaged)
            payload = bytearray(checkpoint.read_bytes())
            payload[len(payload) // 2] ^= 0x40
            checkpoint.write_bytes(payload)
            result = run_cli(
                "resume",
                str(scene_path),
                "--out",
                str(damaged),
                "--verify-baseline",
                str(full),
            )
            self.assertEqual(result.returncode, 0, result.stderr)
            report = load_json(damaged / "recovery_report.json")
            self.assertTrue(report["snapshot_rejected_on_load"])
            self.assertEqual(report["loaded_blocks_at_start"], 0)
            self.assertEqual((full / "rgba.bin").read_bytes(), (damaged / "rgba.bin").read_bytes())

    def test_scene_digest_mismatch_discards_snapshot(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            scene_path, scene = write_scene(root, width=80, height=60, block_width=20, block_height=20)
            partial = root / "partial"
            run_cli("render", str(scene_path), "--out", str(partial), "--interrupt-after-blocks", "3")
            old_digest = load_json(partial / "recovery_report.json")
            self.assertEqual(old_digest["status"], "interrupted")

            changed = json.loads(json.dumps(scene))
            changed["render"]["exposure"] = 0.76
            changed_path = root / "changed.json"
            changed_path.write_text(json.dumps(changed), encoding="utf-8")
            changed_result = run_cli("resume", str(changed_path), "--out", str(partial))
            self.assertEqual(changed_result.returncode, 0, changed_result.stderr)
            report = load_json(partial / "recovery_report.json")
            self.assertTrue(report["snapshot_rejected_on_load"])
            self.assertEqual(report["loaded_blocks_at_start"], 0)

    def test_projection_samples_change_edge_coverage_deterministically(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            scene_path, scene = write_scene(root, width=64, height=48, block_width=32, block_height=24)
            results = {}
            rgba_by_density = {}
            for density in (1, 4):
                changed = json.loads(json.dumps(scene))
                changed["render"]["projection_samples"] = density
                path = root / f"scene_{density}.json"
                path.write_text(json.dumps(changed), encoding="utf-8")
                for repeat in ("a", "b"):
                    out = root / f"density_{density}_{repeat}"
                    self.assertEqual(run_cli("render", str(path), "--out", str(out)).returncode, 0)
                    results[(density, repeat)] = (out / "stats.json").read_bytes()
                    rgba_by_density[(density, repeat)] = (out / "rgba.bin").read_bytes()
                self.assertEqual(results[(density, "a")], results[(density, "b")])
            self.assertNotEqual(rgba_by_density[(1, "a")], rgba_by_density[(4, "a")])

    def test_force_kill_then_resume(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            scene_path, _ = write_scene(root, width=96, height=72, block_width=32, block_height=24)
            baseline = root / "baseline"
            self.assertEqual(run_cli("render", str(scene_path), "--out", str(baseline)).returncode, 0)
            killed = root / "killed"
            env = dict(os.environ)
            env["LEATHER_RASTER_BLOCK_DELAY_MS"] = "80"
            process = subprocess.Popen(
                [
                    sys.executable,
                    "-m",
                    "leather_raster",
                    "render",
                    str(scene_path),
                    "--out",
                    str(killed),
                ],
                cwd=ROOT,
                env=env,
                stdout=subprocess.DEVNULL,
                stderr=subprocess.DEVNULL,
            )
            checkpoint = snapshot_file(killed)
            deadline = time.time() + 20.0
            while time.time() < deadline and process.poll() is None:
                if checkpoint.exists():
                    try:
                        data = json.loads(checkpoint.read_text(encoding="utf-8"))
                    except (OSError, json.JSONDecodeError):
                        time.sleep(0.02)
                        continue
                    if data.get("blocks"):
                        break
                time.sleep(0.02)
            process.kill()
            process.wait(timeout=10)
            self.assertTrue(checkpoint.exists())
            snapshot = json.loads(checkpoint.read_text(encoding="utf-8"))
            self.assertGreaterEqual(len(snapshot["blocks"]), 1)
            resumed = run_cli(
                "resume",
                str(scene_path),
                "--out",
                str(killed),
                "--verify-baseline",
                str(baseline),
            )
            self.assertEqual(resumed.returncode, 0, resumed.stderr)
            self.assertEqual((baseline / "rgba.bin").read_bytes(), (killed / "rgba.bin").read_bytes())
            self.assertEqual(
                (baseline / "layer_map.bin").read_bytes(),
                (killed / "layer_map.bin").read_bytes(),
            )
            self.assertEqual(
                (baseline / "projection_points.json").read_bytes(),
                (killed / "projection_points.json").read_bytes(),
            )

    def test_mismatch_diff_has_block_pixel_and_channel(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            scene_path, _ = write_scene(root, width=64, height=48, block_width=16, block_height=16)
            baseline = root / "baseline"
            actual = root / "actual"
            run_cli("render", str(scene_path), "--out", str(baseline))
            data = bytearray((baseline / "rgba.bin").read_bytes())
            data[0] = (data[0] + 1) % 255
            (baseline / "rgba.bin").write_bytes(data)
            result = run_cli(
                "resume",
                str(scene_path),
                "--out",
                str(actual),
                "--verify-baseline",
                str(baseline),
            )
            # actual has no snapshot; render once, then compare on the second pass
            if result.returncode != 4:
                result = run_cli(
                    "resume",
                    str(scene_path),
                    "--out",
                    str(actual),
                    "--verify-baseline",
                    str(baseline),
                )
            self.assertEqual(result.returncode, 4)
            diff = load_json(actual / "diff.json")
            self.assertFalse(diff["ok"])
            first = diff["differences"][0]
            self.assertIn("block_id", first)
            self.assertIn("x", first)
            self.assertIn("y", first)
            self.assertIn("channel", first)


if __name__ == "__main__":
    unittest.main()

