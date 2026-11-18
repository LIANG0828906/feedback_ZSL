from __future__ import annotations

import json
import subprocess
import sys
import tempfile
import time
import unittest
from pathlib import Path

from paper_lantern.cli import main
from paper_lantern.generator import generate_scene, write_scene
from paper_lantern.report import compare_resume_outputs
from paper_lantern.state import build_output_artifacts
from paper_lantern.pipeline import render_state_fresh
from paper_lantern.scene import validate_scene_data


class RecoveryTests(unittest.TestCase):
    def setUp(self) -> None:
        self.temporary = tempfile.TemporaryDirectory()
        self.root = Path(self.temporary.name)
        self.scene_data = generate_scene(
            width=64,
            height=48,
            tile_width=32,
            tile_height=24,
            shadow_samples=4,
        )
        self.scene_path = self.root / "scene.json"
        write_scene(self.scene_data, self.scene_path)

    def tearDown(self) -> None:
        self.temporary.cleanup()

    def _render(self, output_name: str, *extra: str) -> int:
        return main(
            [
                "render",
                str(self.scene_path),
                "-o",
                str(self.root / output_name),
                *extra,
            ]
        )

    def _assert_same_outputs(self, first: Path, second: Path) -> None:
        for relative in (
            "image.rgba",
            "image.png",
            "depth.f32",
            "layers/layer_00.rgba",
            "layers/layer_04.rgba",
        ):
            self.assertEqual(
                (first / relative).read_bytes(),
                (second / relative).read_bytes(),
                relative,
            )

    def test_graceful_interrupt_resume_matches_full_render(self) -> None:
        self.assertEqual(self._render("full"), 0)
        self.assertEqual(self._render("resumed", "--stop-after-blocks", "2"), 0)
        resumed_dir = self.root / "resumed"
        self.assertTrue((resumed_dir / "snapshot.bin").exists())
        self.assertFalse((resumed_dir / "image.rgba").exists())
        interrupt_report = json.loads(
            (resumed_dir / "resume_report.json").read_text(encoding="utf-8")
        )
        self.assertEqual(interrupt_report["completed_count"], 2)
        self.assertEqual(interrupt_report["next_block"], 2)
        self.assertEqual(self._render("resumed"), 0)
        self._assert_same_outputs(self.root / "full", resumed_dir)
        report = json.loads((resumed_dir / "report.json").read_text(encoding="utf-8"))
        self.assertEqual(report["recovery_count"], 1)
        self.assertEqual(report["snapshot_status"], "resumed")
        self.assertEqual(report["tiles"]["completed"], report["tiles"]["total"])
        self.assertFalse((resumed_dir / "snapshot.bin").exists())
        self.assertFalse((resumed_dir / "resume_report.json").exists())

    def test_corrupt_snapshot_is_discarded(self) -> None:
        self.assertEqual(self._render("full"), 0)
        self.assertEqual(self._render("corrupt", "--stop-after-blocks", "1"), 0)
        snapshot_path = self.root / "corrupt" / "snapshot.bin"
        payload = bytearray(snapshot_path.read_bytes())
        payload[len(payload) // 2] ^= 0xFF
        snapshot_path.write_bytes(bytes(payload))
        self.assertEqual(self._render("corrupt"), 0)
        self._assert_same_outputs(self.root / "full", self.root / "corrupt")
        report = json.loads(
            (self.root / "corrupt" / "report.json").read_text(encoding="utf-8")
        )
        self.assertEqual(report["snapshot_status"], "discarded_corrupt")
        self.assertEqual(report["recovery_count"], 0)

    def test_scene_mismatch_snapshot_is_discarded(self) -> None:
        self.assertEqual(self._render("mismatch", "--stop-after-blocks", "1"), 0)
        changed = dict(self.scene_data)
        changed["seed"] = self.scene_data["seed"] + 1
        changed_path = self.root / "changed.json"
        write_scene(changed, changed_path)
        output = self.root / "mismatch"
        self.assertEqual(
            main(["render", str(changed_path), "-o", str(output)]),
            0,
        )
        report = json.loads((output / "report.json").read_text(encoding="utf-8"))
        self.assertEqual(report["snapshot_status"], "discarded_scene_mismatch")
        self.assertEqual(report["recovery_count"], 0)

    def test_resume_mismatch_reports_block_pixel_and_channel(self) -> None:
        state = render_state_fresh(validate_scene_data(self.scene_data))
        artifacts = build_output_artifacts(validate_scene_data(self.scene_data), state)
        mutated = build_output_artifacts(validate_scene_data(self.scene_data), state)
        mutated.rgba_u8[5, 7, 2] = (int(mutated.rgba_u8[5, 7, 2]) + 1) % 256
        consistent, report = compare_resume_outputs(
            validate_scene_data(self.scene_data), artifacts, mutated
        )
        self.assertFalse(consistent)
        self.assertGreater(report["difference_count"], 0)
        self.assertEqual(report["differences"][0]["block"], 0)
        self.assertEqual(report["differences"][0]["x"], 7)
        self.assertEqual(report["differences"][0]["y"], 5)
        self.assertEqual(report["differences"][0]["channel"], "B")

    def test_forced_termination_resumes_from_last_atomic_snapshot(self) -> None:
        self.assertEqual(self._render("full"), 0)
        kill_dir = self.root / "killed"
        command = [
            sys.executable,
            "-m",
            "paper_lantern",
            "render",
            str(self.scene_path),
            "-o",
            str(kill_dir),
            "--block-delay-ms",
            "120",
        ]
        process = subprocess.Popen(
            command,
            cwd=Path(__file__).resolve().parents[1],
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
        )
        snapshot_path = kill_dir / "snapshot.bin"
        deadline = time.monotonic() + 10.0
        while time.monotonic() < deadline:
            if snapshot_path.exists() and snapshot_path.stat().st_size > 0:
                break
            if process.poll() is not None:
                break
            time.sleep(0.02)
        self.assertTrue(snapshot_path.exists())
        process.kill()
        process.wait(timeout=10)
        self.assertEqual(
            main(["render", str(self.scene_path), "-o", str(kill_dir)]),
            0,
        )
        self._assert_same_outputs(self.root / "full", kill_dir)
        report = json.loads((kill_dir / "report.json").read_text(encoding="utf-8"))
        self.assertEqual(report["snapshot_status"], "resumed")
        self.assertEqual(report["recovery_count"], 1)


if __name__ == "__main__":
    unittest.main()

