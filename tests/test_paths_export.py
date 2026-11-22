from __future__ import annotations

import copy
import tempfile
import unittest
from pathlib import Path

import numpy as np

from spectraforge.compare import compare_outputs
from spectraforge.errors import WorkbenchError
from spectraforge.export import export_output
from spectraforge.pipeline import prepare_scene
from spectraforge.util import read_json, sha256_bytes
from spectraforge.verification import verify_paths
from tests.helpers import SCENE_PATH, run_complete


class TestThreePathsAndExport(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls._temp = tempfile.TemporaryDirectory(prefix="sf-paths-")
        cls.workspace = Path(cls._temp.name)
        cls.report = verify_paths(
            SCENE_PATH,
            cls.workspace / "verify",
            live_workers=3,
            tile_width=32,
            tile_height=32,
        )
        cls.scene, cls.manager, cls.output, cls.result = run_complete(
            cls.workspace / "baseline",
            workers=3,
            tile=32,
        )

    @classmethod
    def tearDownClass(cls) -> None:
        cls.manager.close()
        cls._temp.cleanup()

    def test_three_path_outputs_are_pixel_exact(self) -> None:
        self.assertTrue(self.report["ok"])
        for name, comparison in self.report["comparisons"].items():
            with self.subTest(name=name):
                self.assertTrue(comparison["equal"])
                self.assertIsNone(comparison["first_mismatch"])
        peaks = {item["peak_radiance"] for item in self.report["paths"].values()}
        gamut = {item["gamut_count"] for item in self.report["paths"].values()}
        self.assertEqual(len(peaks), 1)
        self.assertEqual(len(gamut), 1)
        self.assertGreaterEqual(self.report["checkpoint_resume"]["recovered_committed"], 6)
        self.assertLess(self.report["checkpoint_resume"]["recovered_committed"], 12)

    def test_band_segmented_path_is_also_pixel_exact(self) -> None:
        with tempfile.TemporaryDirectory(prefix="sf-band-paths-") as temp:
            report = verify_paths(
                SCENE_PATH,
                Path(temp),
                split_mode="bands",
                live_workers=3,
                tile_width=32,
                tile_height=32,
            )
            self.assertTrue(report["ok"])
            self.assertEqual(report["segmentation"]["total"], 8)
            for comparison in report["comparisons"].values():
                self.assertTrue(comparison["equal"])

    def test_comparison_detects_one_ulp_and_reports_segment_stage(self) -> None:
        mutated = copy.deepcopy(self.output)
        y, x, channel = 19, 71, 2
        original = np.float32(mutated.linear_rgb[y, x, channel])
        mutated.linear_rgb[y, x, channel] = np.nextafter(original, np.float32(np.inf))
        report = compare_outputs(self.output, mutated)
        self.assertFalse(report.equal)
        mismatch = report.first_mismatch
        self.assertEqual(mismatch["stage"], "linear_rgb")
        self.assertEqual(mismatch["x"], x)
        self.assertEqual(mismatch["y"], y)
        self.assertEqual(mismatch["segment_id"], int(self.output.segment_index[y, x]))

    def test_peak_and_gamut_set_are_compared_exactly(self) -> None:
        peak_mutated = copy.deepcopy(self.output)
        peak_mutated.peak_radiance = float(np.nextafter(peak_mutated.peak_radiance, np.inf))
        peak_report = compare_outputs(self.output, peak_mutated)
        self.assertFalse(peak_report.equal)
        self.assertEqual(peak_report.first_mismatch["stage"], "peak_radiance")
        gamut_mutated = copy.deepcopy(self.output)
        gamut_mutated.gamut_count += 1
        gamut_report = compare_outputs(self.output, gamut_mutated)
        self.assertFalse(gamut_report.equal)
        self.assertIn(
            gamut_report.first_mismatch["stage"],
            {"gamut_channel_mask", "gamut_set"},
        )

    def test_export_is_complete_and_ordered(self) -> None:
        with tempfile.TemporaryDirectory(prefix="sf-export-") as temp:
            manifest = export_output(
                self.output,
                temp,
                scene_id=self.scene.metadata.scene_id,
                title=self.scene.metadata.title,
                segment_order=list(range(len(self.manager.segments))),
                alignment=prepare_scene(self.scene, "bilinear"),
            )
            output_dir = Path(temp)
            self.assertEqual(manifest["segment_order"], list(range(len(self.manager.segments))))
            self.assertTrue(manifest["complete"])
            self.assertEqual((output_dir / "display.png").read_bytes()[:8], b"\x89PNG\r\n\x1a\n")
            self.assertTrue((output_dir / "aligned_bands.npy").exists())
            self.assertTrue((output_dir / "alignment_report.json").exists())
            stats = read_json(output_dir / "stats.json")
            self.assertTrue(stats["complete"])
            self.assertEqual(stats["segment_order"], list(range(len(self.manager.segments))))
            for name, expected_hash in manifest["files"].items():
                self.assertEqual(sha256_bytes((output_dir / name).read_bytes()), expected_hash)

    def test_incomplete_output_refuses_export(self) -> None:
        with tempfile.TemporaryDirectory(prefix="sf-partial-") as temp:
            scene, manager, _output, _result = run_complete(
                Path(temp) / "partial-source",
                workers=1,
                tile=64,
            )
            # Create a partial snapshot in a different workspace.
            partial_ws = Path(temp) / "partial"
            from spectraforge.checkpoint import CheckpointManager
            from spectraforge.pipeline import assemble_output
            from spectraforge.scheduler import ProcessingJob, ResourceConfig
            from spectraforge.segmentation import make_segments, segmentation_spec

            segments = make_segments(scene.width, scene.height, scene.band_count, tile_width=64, tile_height=64)
            config = segmentation_spec(
                scene.width,
                scene.height,
                scene.band_count,
                split_mode="tiles",
                tile_width=64,
                tile_height=64,
                interpolation="bilinear",
            )
            manager.close()
            partial = CheckpointManager(partial_ws, scene, segments, config)
            try:
                job = ProcessingJob(
                    scene,
                    partial,
                    resource=ResourceConfig(1, 64),
                    params=scene.metadata.default_parameters,
                )
                job.run(pause_after=1)
                output = assemble_output(scene, partial, require_complete=False)
                with self.assertRaises(WorkbenchError) as raised:
                    export_output(
                        output,
                        Path(temp) / "nope",
                        scene_id=scene.metadata.scene_id,
                        title=scene.metadata.title,
                    )
                self.assertEqual(raised.exception.code, "E_INCOMPLETE")
            finally:
                partial.close()


if __name__ == "__main__":
    unittest.main()
