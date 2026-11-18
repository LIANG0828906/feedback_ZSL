from __future__ import annotations

import copy
import json
import shutil
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

import numpy as np
from PIL import Image

from canopy_render.scene import generate_scene
from canopy_render.snapshot import SnapshotRejected, TileLayout, load_snapshot
from canopy_render.util import write_json_atomic
from canopy_render.validation import validate_scene

REPO_ROOT = Path(__file__).resolve().parents[1]


def _run_cli(*args: str) -> subprocess.CompletedProcess:
    return subprocess.run(
        [sys.executable, "-m", "canopy_render", *args],
        cwd=REPO_ROOT,
        capture_output=True,
        text=True,
    )


class ResumeTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.temporary = tempfile.TemporaryDirectory()
        cls.root = Path(cls.temporary.name)
        cls.scene = generate_scene(96, 72, 32, 24)
        cls.scene_path = cls.root / "scene.json"
        write_json_atomic(cls.scene_path, cls.scene)
        cls.reference_dir = cls.root / "reference"
        result = _run_cli(
            "render",
            "--scene",
            str(cls.scene_path),
            "--out",
            str(cls.reference_dir),
        )
        assert result.returncode == 0, result.stderr

    @classmethod
    def tearDownClass(cls) -> None:
        cls.temporary.cleanup()

    def test_hard_kill_then_resume_is_pixel_identical(self) -> None:
        out_dir = self.root / "hard_kill"
        killed = _run_cli(
            "render",
            "--scene",
            str(self.scene_path),
            "--out",
            str(out_dir),
            "--hard-kill-after-tiles",
            "2",
        )
        self.assertNotEqual(killed.returncode, 0)
        self.assertFalse((out_dir / "render.png").exists())
        with open(out_dir / "snapshot.json", "r", encoding="utf-8") as handle:
            snapshot = json.load(handle)
        self.assertEqual(snapshot["completed_tile_indices"], [0, 1])

        resumed = _run_cli(
            "render",
            "--scene",
            str(self.scene_path),
            "--out",
            str(out_dir),
            "--reference",
            str(self.reference_dir),
        )
        self.assertEqual(resumed.returncode, 0, resumed.stderr)
        self.assertEqual(
            (out_dir / "render.png").read_bytes(),
            (self.reference_dir / "render.png").read_bytes(),
        )
        self.assertEqual(
            (out_dir / "ownership.png").read_bytes(),
            (self.reference_dir / "ownership.png").read_bytes(),
        )
        with open(out_dir / "render_report.json", "r", encoding="utf-8") as handle:
            report = json.load(handle)
        with open(
            self.reference_dir / "render_report.json", "r", encoding="utf-8"
        ) as handle:
            reference_report = json.load(handle)
        self.assertEqual(report["recovery"]["recovery_count"], 1)
        self.assertEqual(report["recovery"]["snapshot_status"], "resumed")
        self.assertEqual(
            report["projection"]["points"],
            reference_report["projection"]["points"],
        )
        self.assertEqual(
            report["image_summary"],
            reference_report["image_summary"],
        )
        self.assertEqual(report["layers"], reference_report["layers"])
        self.assertEqual(
            report["soft_shadows"]["soft_shadow_sample_count"],
            reference_report["soft_shadows"]["soft_shadow_sample_count"],
        )

    def test_graceful_interrupt_finishes_tile_and_exits_zero(self) -> None:
        out_dir = self.root / "graceful"
        interrupted = _run_cli(
            "render",
            "--scene",
            str(self.scene_path),
            "--out",
            str(out_dir),
            "--interrupt-after-tiles",
            "2",
        )
        self.assertEqual(interrupted.returncode, 0, interrupted.stderr)
        self.assertFalse((out_dir / "render.png").exists())
        with open(
            out_dir / "recovery_report.json", "r", encoding="utf-8"
        ) as handle:
            recovery_report = json.load(handle)
        self.assertEqual(recovery_report["status"], "interrupted")
        self.assertEqual(
            recovery_report["tile_layout"]["completed_tile_indices"],
            [0, 1],
        )
        with open(out_dir / "snapshot.json", "r", encoding="utf-8") as handle:
            snapshot = json.load(handle)
        self.assertEqual(snapshot["completed_tile_indices"], [0, 1])

        resumed = _run_cli(
            "render",
            "--scene",
            str(self.scene_path),
            "--out",
            str(out_dir),
            "--reference",
            str(self.reference_dir),
        )
        self.assertEqual(resumed.returncode, 0, resumed.stderr)
        self.assertEqual(
            (out_dir / "render.png").read_bytes(),
            (self.reference_dir / "render.png").read_bytes(),
        )

    def test_corrupt_snapshot_is_discarded_and_recomputed(self) -> None:
        out_dir = self.root / "corrupt"
        out_dir.mkdir()
        corrupted = self.root / "corrupt_snapshot_source.json"
        shutil.copyfile(self.reference_dir / "snapshot.json", corrupted)
        with open(corrupted, "r", encoding="utf-8") as handle:
            snapshot = json.load(handle)
        snapshot["tiles"][0]["rgba_b64"] = snapshot["tiles"][0]["rgba_b64"][:-8] + "AAAA"
        write_json_atomic(out_dir / "snapshot.json", snapshot)
        result = _run_cli(
            "render",
            "--scene",
            str(self.scene_path),
            "--out",
            str(out_dir),
        )
        self.assertEqual(result.returncode, 0, result.stderr)
        with open(out_dir / "render_report.json", "r", encoding="utf-8") as handle:
            report = json.load(handle)
        self.assertEqual(report["recovery"]["snapshot_status"], "discarded")
        self.assertEqual(report["recovery"]["recovery_count"], 0)
        self.assertIsNotNone(report["recovery"]["snapshot_rejection_reason"])
        self.assertEqual(
            (out_dir / "render.png").read_bytes(),
            (self.reference_dir / "render.png").read_bytes(),
        )

    def test_snapshot_scene_digest_mismatch_is_rejected(self) -> None:
        validation = validate_scene(self.scene)
        self.assertTrue(validation.ok)
        changed = copy.deepcopy(self.scene)
        for tassel in changed["tassels"]:
            for segment in tassel["segments"]:
                segment["a"][1] -= 0.1
                segment["b"][1] -= 0.1
        changed_validation = validate_scene(changed)
        self.assertTrue(changed_validation.ok)
        with self.assertRaises(SnapshotRejected) as context:
            load_snapshot(
                self.reference_dir / "snapshot.json",
                changed_validation.data,
                TileLayout(
                    changed_validation.data.width,
                    changed_validation.data.height,
                    changed_validation.data.tile_width,
                    changed_validation.data.tile_height,
                ),
            )
        self.assertEqual(
            context.exception.reason,
            "snapshot_scene_digest_mismatch",
        )

    def test_mismatch_report_has_tile_pixel_and_channel(self) -> None:
        damaged = self.root / "damaged"
        shutil.copytree(self.reference_dir, damaged)
        image_path = damaged / "render.png"
        with Image.open(image_path) as image:
            pixels = np.asarray(image.convert("RGBA")).copy()
        pixels[5, 7, 1] = int(pixels[5, 7, 1]) ^ 0x01
        Image.fromarray(pixels, "RGBA").save(image_path, format="PNG")
        result = _run_cli(
            "compare",
            "--scene",
            str(self.scene_path),
            "--output",
            str(damaged),
            "--reference",
            str(self.reference_dir),
        )
        self.assertEqual(result.returncode, 3, result.stderr)
        with open(damaged / "differences.json", "r", encoding="utf-8") as handle:
            differences = json.load(handle)
        self.assertEqual(differences["business_code"], "RESUME_MISMATCH")
        self.assertGreater(differences["difference_count"], 0)
        first = differences["differences"][0]
        self.assertIn("tile_index", first)
        self.assertIn("x", first)
        self.assertIn("y", first)
        self.assertIn("channel", first)
        self.assertEqual((first["x"], first["y"]), (7, 5))
        self.assertEqual(first["channel"], "G")


if __name__ == "__main__":
    unittest.main()

