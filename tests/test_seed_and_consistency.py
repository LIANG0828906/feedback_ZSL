from __future__ import annotations

import copy
import tempfile
import unittest
from concurrent.futures import ThreadPoolExecutor
from dataclasses import replace
from pathlib import Path

import numpy as np

from npr_workbench.consistency import compare_preview_exact, compare_three_paths
from npr_workbench.engine import NprEngine
from npr_workbench.errors import (
    DivergenceError,
    PathConsistencyError,
    SeedDivergenceError,
    VersionConflictError,
)
from npr_workbench.seeds import stroke_channels, stroke_fingerprint
from npr_workbench.strokes import build_stroke_plan, verify_plan_seed_contract

from tests.helpers import fast_params, sample_scene


class SeedAndConsistencyTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.scene = sample_scene("twin_signal")
        cls.hash = cls.scene.content_hash()

    def test_seed_channels_are_thread_and_rng_independent(self) -> None:
        ids = list(range(80))

        def derive(stroke_id: int) -> str:
            return stroke_fingerprint(self.hash, stroke_id)

        np.random.seed(7)
        baseline = [derive(stroke_id) for stroke_id in ids]
        np.random.seed(99)
        with ThreadPoolExecutor(max_workers=8) as pool:
            parallel = list(pool.map(derive, reversed(ids)))
        self.assertEqual(baseline, list(reversed(parallel)))
        channels = stroke_channels(self.hash, 42)
        self.assertEqual(channels, stroke_channels(self.hash, 42))
        self.assertTrue(0.0 < channels.presence < 1.0)

    def test_seed_divergence_identifies_stroke(self) -> None:
        plan = build_stroke_plan(
            self.scene,
            self.scene.camera_poses[0],
            fast_params(),
            self.hash,
        )
        verify_plan_seed_contract(plan, self.hash)
        bad_descriptor = replace(plan.descriptors[5], fingerprint="0" * 64)
        bad_plan = replace(
            plan,
            descriptors=plan.descriptors[:5]
            + (bad_descriptor,)
            + plan.descriptors[6:],
        )
        with self.assertRaises(SeedDivergenceError) as caught:
            verify_plan_seed_contract(bad_plan, self.hash)
        self.assertEqual(caught.exception.code, "E_SEED")
        self.assertEqual(
            caught.exception.details["stroke_id"],
            bad_descriptor.stroke_id,
        )

    def test_preview_exact_consistency_and_version_guard(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            engine = NprEngine(
                self.scene,
                fast_params(),
                self.scene.camera_poses[0],
                persist_dir=directory,
                diagnostics_dir=directory + "/diagnostics",
            )
            preview = engine.render_preview()
            result = engine.render_exact(preview=preview)
            self.assertTrue(result.complete)
            self.assertEqual(
                preview.stroke_fingerprint_root,
                result.frame.stroke_fingerprint_root,
            )
            self.assertIsNotNone(engine.last_consistency_report)
            report = engine.last_consistency_report
            self.assertTrue(report.passed)
            self.assertLessEqual(
                report.level_histogram_deviation,
                report.tolerances["level_histogram_deviation"],
            )
            self.assertLessEqual(
                report.outline_coverage_delta,
                report.tolerances["outline_coverage_delta"],
            )
            self.assertLessEqual(
                report.stroke_coverage_delta,
                report.tolerances["stroke_coverage_delta"],
            )

            old_snapshot = preview.snapshot
            engine.store.update(
                params=engine.store.latest().params.with_controls(levels=6)
            )
            engine.render_preview()
            with self.assertRaises(VersionConflictError) as caught:
                engine.render_exact(snapshot=old_snapshot, preview=preview)
            self.assertEqual(caught.exception.code, "E_VER")
            self.assertEqual(
                set(caught.exception.details["conflict_versions"]),
                {1, 2},
            )

    def test_divergence_raises_and_retains_both_images(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            engine = NprEngine(
                self.scene,
                fast_params(),
                self.scene.camera_poses[0],
                persist_dir=directory,
                diagnostics_dir=directory + "/diagnostics",
            )
            preview = engine.render_preview()
            result = engine.render_exact(preview=preview)
            with self.assertRaises(DivergenceError) as caught:
                compare_preview_exact(
                    preview,
                    result.frame,
                    tolerances={"level_histogram_deviation": 0.0},
                    diagnostic_dir=directory + "/divergence",
                    raise_on_diverge=True,
                )
            self.assertEqual(caught.exception.code, "E_DIVERGE")
            diagnostics = caught.exception.details["diagnostics"]
            self.assertTrue(Path(diagnostics["preview_tensors"]).exists())
            self.assertTrue(Path(diagnostics["exact_tensors"]).exists())
            self.assertTrue(Path(diagnostics["heatmap"]).exists())

    def test_path_mismatch_reports_first_pixel_and_tile(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            engine = NprEngine(
                self.scene,
                fast_params(),
                self.scene.camera_poses[0],
                persist_dir=directory,
                diagnostics_dir=directory + "/diagnostics",
            )
            frame = engine.render_one_shot()
            broken = copy.copy(frame)
            broken.styled_rgb = frame.styled_rgb.copy()
            broken.styled_rgb[7, 9, 0] ^= 255
            with self.assertRaises(PathConsistencyError) as caught:
                compare_three_paths(frame, frame, broken)
            self.assertEqual(caught.exception.code, "E_PATH")
            mismatch = caught.exception.report["mismatches"][0]
            self.assertEqual(mismatch["first_mismatch_pixel"], (9, 7))
            self.assertIsNotNone(mismatch["tile_id"])


if __name__ == "__main__":
    unittest.main()

