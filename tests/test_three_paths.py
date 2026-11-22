from __future__ import annotations

import shutil
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

import numpy as np

from mshdr.comparison import compare_outputs
from mshdr.dataset import DatasetIndex
from mshdr.params import WorkbenchParams
from mshdr.paths import run_three_paths
from mshdr.pipeline import WorkbenchEngine

from support import generate_single_scene


class ThreePathTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls._temp = tempfile.TemporaryDirectory()
        cls.root = Path(cls._temp.name)
        cls.index = generate_single_scene(cls.root)
        cls.scene_id = cls.index.scene_records[0]["scene_id"]
        cls.scene = cls.index.load_scene(cls.scene_id)
        cls.params = WorkbenchParams(max_workers=3)
        reference = WorkbenchEngine(cls.scene, cls.params)
        reference.run(mode="reference")
        cls.reference = reference.assemble()

    @classmethod
    def tearDownClass(cls) -> None:
        cls._temp.cleanup()

    def test_three_paths_exact_and_report_written(self) -> None:
        report = run_three_paths(
            self.index.path,
            output_root=self.root / "three_paths",
            checkpoint_root=self.root / "three_path_checkpoints",
            params=self.params,
        )
        self.assertTrue(report["ok"])
        self.assertTrue((self.root / "three_paths" / "paths_report.json").exists())
        scene_report = report["scenes"][0]
        self.assertTrue(all(scene_report["comparisons"].values()))
        self.assertIsNone(scene_report["first_mismatch"]["full_vs_resume"])
        root = self.root / "three_paths" / "full" / self.scene_id
        for name in ("synthetic_rgb", "fused_radiance", "linear_rgb", "gamut_mask"):
            self.assertTrue(np.array_equal(np.load(root / f"{name}.npy"), self.reference[name]))

    def test_comparator_locates_first_pixel_field_and_segment(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            directory = Path(directory)
            left = directory / "left"
            right = directory / "right"
            left.mkdir()
            right.mkdir()
            left_engine = WorkbenchEngine(self.scene, self.params)
            left_engine.run(output_dir=left, mode="left")
            for path in left.iterdir():
                if path.is_file():
                    shutil.copy2(path, right / path.name)
            synthetic = np.load(right / "synthetic_rgb.npy")
            synthetic[40, 70, 1] = np.uint16((int(synthetic[40, 70, 1]) + 1) % 1024)
            np.save(right / "synthetic_rgb.npy", synthetic)
            report = compare_outputs(left, right, raise_on_diverge=False)
            self.assertTrue(Path(report["reproduce_script"]).exists())
        self.assertFalse(report["ok"])
        self.assertEqual(report["first_mismatch"]["field"], "synthetic_rgb")
        self.assertEqual(report["first_mismatch"]["coordinate"], [40, 70])
        self.assertEqual(report["first_mismatch"]["segment_index"], 6)

    def test_real_forced_exit_then_resume(self) -> None:
        checkpoint_root = self.root / "checkpoints" / "crash"
        output_dir = self.root / "crash_output"
        base_command = [
            sys.executable,
            "-m",
            "mshdr",
            "run",
            "--dataset",
            str(self.index.path),
            "--scene",
            self.scene_id,
            "--checkpoint-root",
            str(checkpoint_root),
            "--workers",
            "2",
        ]
        crashed = subprocess.run(
            base_command
            + ["--start-mode", "fresh", "--crash-after-commits", "4"],
            cwd=Path(__file__).resolve().parents[1],
            capture_output=True,
            text=True,
            timeout=60,
        )
        self.assertEqual(crashed.returncode, 91, crashed.stderr)
        resumed = subprocess.run(
            base_command
            + ["--start-mode", "resume", "--output", str(output_dir)],
            cwd=Path(__file__).resolve().parents[1],
            capture_output=True,
            text=True,
            timeout=60,
        )
        self.assertEqual(resumed.returncode, 0, resumed.stderr)
        for name in self.reference:
            self.assertTrue(
                np.array_equal(
                    np.load(output_dir / f"{name}.npy", allow_pickle=False),
                    self.reference[name],
                ),
                name,
            )


if __name__ == "__main__":
    unittest.main()

