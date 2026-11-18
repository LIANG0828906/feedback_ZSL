from __future__ import annotations

import hashlib
import json
import shutil
import tempfile
import unittest
from pathlib import Path

import numpy as np

from shadowbox.pipeline import render_to_directory
from shadowbox.scene import load_scene

from tests.helpers import invoke_cli, small_scene, write_scene_dict


def _digest(path: Path) -> str:
    return hashlib.sha256(path.read_bytes()).hexdigest()


class RenderResumeTests(unittest.TestCase):
    def setUp(self) -> None:
        self.temporary = tempfile.TemporaryDirectory()
        self.directory = Path(self.temporary.name)
        self.scene_path = self.directory / "scene.json"
        write_scene_dict(self.scene_path, small_scene())
        self.scene = load_scene(self.scene_path)
        self.baseline = self.directory / "baseline"
        result = render_to_directory(self.scene, self.baseline, fresh=True)
        self.assertEqual(result.exit_code, 0)

    def tearDown(self) -> None:
        self.temporary.cleanup()

    def test_repeat_render_report_is_stable(self) -> None:
        repeated = self.directory / "repeated"
        result = render_to_directory(self.scene, repeated, fresh=True)
        self.assertEqual(result.exit_code, 0)
        reference_report = json.loads(
            (self.baseline / "report.json").read_text(encoding="utf-8")
        )
        repeated_report = json.loads(
            (repeated / "report.json").read_text(encoding="utf-8")
        )
        self.assertEqual(reference_report, repeated_report)
        self.assertEqual(
            _digest(self.baseline / "rgba.raw"),
            _digest(repeated / "rgba.raw"),
        )
        self.assertEqual(
            _digest(self.baseline / "optical_depth.f32"),
            _digest(repeated / "optical_depth.f32"),
        )

    def test_reversed_layer_input_order_renders_identically(self) -> None:
        reversed_scene = small_scene()
        reversed_scene["layers"] = list(reversed(reversed_scene["layers"]))
        reversed_path = self.directory / "reversed.json"
        write_scene_dict(reversed_path, reversed_scene)
        scene = load_scene(reversed_path)
        output = self.directory / "reversed"
        result = render_to_directory(scene, output, fresh=True)
        self.assertEqual(result.exit_code, 0)
        self.assertEqual(
            _digest(self.baseline / "rgba.raw"),
            _digest(output / "rgba.raw"),
        )
        self.assertEqual(
            _digest(self.baseline / "optical_depth.f32"),
            _digest(output / "optical_depth.f32"),
        )
        self.assertEqual(scene.digest, self.scene.digest)

    def test_interrupt_then_resume_is_pixel_identical(self) -> None:
        resumed = self.directory / "resumed"
        interrupted = render_to_directory(
            self.scene,
            resumed,
            fresh=True,
            interrupt_after_blocks=2,
        )
        self.assertEqual(interrupted.exit_code, 0)
        self.assertTrue(interrupted.interrupted)
        self.assertTrue((resumed / "resume_report.json").exists())
        self.assertFalse((resumed / "rgba.raw").exists())
        resume_payload = json.loads(
            (resumed / "resume_report.json").read_text(encoding="utf-8")
        )
        self.assertEqual(resume_payload["completed_count"], 2)
        self.assertEqual(resume_payload["next_block"], 2)

        completed = render_to_directory(
            self.scene,
            resumed,
            reference_directory=self.baseline,
        )
        self.assertEqual(completed.exit_code, 0)
        self.assertEqual(completed.recovery_count, 1)
        self.assertEqual(
            _digest(self.baseline / "rgba.raw"),
            _digest(resumed / "rgba.raw"),
        )
        self.assertEqual(
            _digest(self.baseline / "optical_depth.f32"),
            _digest(resumed / "optical_depth.f32"),
        )
        report = json.loads((resumed / "report.json").read_text(encoding="utf-8"))
        self.assertEqual(report["summary"]["recovery_count"], 1)
        checkpoint = json.loads(
            (resumed / "checkpoint.json").read_text(encoding="utf-8")
        )
        self.assertEqual(checkpoint["state"], "complete")
        self.assertEqual(len(checkpoint["completed_blocks"]), 9)

    def test_corrupt_checkpoint_is_discarded_entirely(self) -> None:
        resumed = self.directory / "corrupt"
        render_to_directory(
            self.scene,
            resumed,
            fresh=True,
            interrupt_after_blocks=3,
        )
        checkpoint_path = resumed / "checkpoint.json"
        checkpoint_path.write_bytes(b'{"broken": true')
        completed = render_to_directory(
            self.scene,
            resumed,
            reference_directory=self.baseline,
        )
        self.assertEqual(completed.exit_code, 0)
        self.assertEqual(completed.recovery_count, 0)
        self.assertEqual(completed.discarded_checkpoints, 1)
        report = json.loads((resumed / "report.json").read_text(encoding="utf-8"))
        self.assertEqual(report["summary"]["recovery_count"], 0)
        self.assertEqual(report["summary"]["discarded_checkpoints"], 1)
        self.assertEqual(
            _digest(self.baseline / "rgba.raw"),
            _digest(resumed / "rgba.raw"),
        )

    def test_scene_digest_mismatch_discards_all_blocks(self) -> None:
        resumed = self.directory / "scene-mismatch"
        render_to_directory(
            self.scene,
            resumed,
            fresh=True,
            interrupt_after_blocks=2,
        )
        changed = small_scene()
        changed["layers"][4]["projection_gain"] = 1.77
        changed_path = self.directory / "changed.json"
        write_scene_dict(changed_path, changed)
        changed_scene = load_scene(changed_path)
        result = render_to_directory(changed_scene, resumed)
        self.assertEqual(result.exit_code, 0)
        self.assertEqual(result.recovery_count, 0)
        self.assertEqual(result.discarded_checkpoints, 1)
        checkpoint = json.loads(
            (resumed / "checkpoint.json").read_text(encoding="utf-8")
        )
        self.assertEqual(checkpoint["scene_sha256"], changed_scene.digest)

    def test_compare_cli_reports_block_pixel_and_channel(self) -> None:
        candidate = self.directory / "candidate"
        shutil.copytree(self.baseline, candidate)
        raw_path = candidate / "rgba.raw"
        array = np.frombuffer(raw_path.read_bytes(), dtype=np.uint8).reshape(
            self.scene.camera.height, self.scene.camera.width, 4
        ).copy()
        visible = np.argwhere(array[:, :, 3] > 0)
        self.assertGreater(len(visible), 0)
        y, x = (int(value) for value in visible[len(visible) // 2])
        old = int(array[y, x, 0])
        array[y, x, 0] = 0 if old != 0 else 1
        raw_path.write_bytes(array.tobytes(order="C"))
        diff_path = candidate / "diff.json"
        code, stdout, stderr = invoke_cli(
            [
                "compare",
                "--scene",
                str(self.scene_path),
                "--reference",
                str(self.baseline),
                "--candidate",
                str(candidate),
                "--report",
                str(diff_path),
            ]
        )
        self.assertEqual(code, 4)
        self.assertEqual(stdout, "")
        payload = json.loads(stderr)
        self.assertFalse(payload["equal"])
        self.assertGreater(payload["mismatch_pixels"], 0)
        difference = payload["differences"][0]
        expected_block = (y // 24) * 3 + (x // 32)
        self.assertEqual(difference["block_index"], expected_block)
        self.assertEqual(difference["pixel"], [x, y])
        self.assertIn("R", difference["channels"])

    def test_verify_command_recomputes_statistics(self) -> None:
        code, stdout, stderr = invoke_cli(
            [
                "verify",
                "--scene",
                str(self.scene_path),
                "--output",
                str(self.baseline),
            ]
        )
        self.assertEqual(code, 0, stderr)
        payload = json.loads(stdout)
        self.assertTrue(payload["ok"])
        self.assertEqual(payload["report_mismatches"], [])


if __name__ == "__main__":
    unittest.main()
