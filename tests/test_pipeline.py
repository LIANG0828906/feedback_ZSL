from __future__ import annotations

import tempfile
import unittest
from dataclasses import replace
from pathlib import Path

import numpy as np

from glyphforge.atlas import AtlasConfig
from glyphforge.config import WorkbenchConfig
from glyphforge.errors import ErrorCode, GlyphForgeError
from glyphforge.parser import parse_font
from glyphforge.pipeline import _coverage_mismatch, run_pipeline
from glyphforge.raster import GlyphGeometry
from glyphforge.samples import build_font_specs
from glyphforge.scheduler import BudgetConfig


def uniform_coverage(width: int, height: int, value: int) -> np.ndarray:
    return np.full((height, width), value, dtype=np.uint8)


class PipelineTests(unittest.TestCase):
    def test_coverage_mismatch_reports_seam_and_pixel_count(self) -> None:
        geometry = GlyphGeometry(
            glyph_name="probe",
            contours=(),
            continuous_bbox=(0, 0, 8, 8),
            x0=0,
            y0=0,
            width=8,
            height=8,
            edges=None,
            samples_per_pixel=16,
        )
        tiled = uniform_coverage(8, 8, 100)
        truth = uniform_coverage(8, 8, 100)
        tiled[3, 5] = 180
        truth[3, 6] = 120
        error = _coverage_mismatch("probe", geometry, tiled, truth, 4, 0)
        self.assertIsNotNone(error)
        self.assertEqual(error.code, ErrorCode.E_COVERAGE_MISMATCH)
        self.assertEqual(error.details["glyph"], "probe")
        self.assertEqual(error.details["over_tolerance_pixels"], 2)
        self.assertIn("seam", error.details)
        self.assertEqual(error.details["tile"], {"row": 0, "col": 1})

    def test_budget_rejection_is_isolated_from_later_glyph(self) -> None:
        font = parse_font(build_font_specs()["holes.json"], path="holes.json")
        config = WorkbenchConfig(
            pixel_size=12,
            tile_size=64,
            budget=BudgetConfig(
                parallelism=2,
                global_memory_bytes=4 * 1024 * 1024,
                max_task_samples=1000,
                max_task_memory_bytes=512 * 1024,
                sampling_batch_size=1024,
                min_sampling_batch_size=128,
            ),
        )
        result = run_pipeline(font, "O8", config)
        by_name = {glyph.name: glyph for glyph in result.glyphs}
        self.assertEqual(by_name["O"].status, "budget_rejected")
        self.assertEqual(by_name["O"].error.code, ErrorCode.E_BUDGET_EXCEEDED)
        self.assertEqual(by_name["eight"].status, "ok")
        self.assertEqual([metric.name for metric in result.atlas.metrics], ["eight"])
        self.assertIsNone(by_name["O"].coverage)

    def test_atlas_overflow_prevents_output(self) -> None:
        font = parse_font(build_font_specs()["mixed.json"], path="mixed.json")
        config = WorkbenchConfig(
            pixel_size=40,
            tile_size=8,
            atlas=AtlasConfig(width=10, height=10, padding=1),
        )
        with tempfile.TemporaryDirectory() as temporary:
            output = Path(temporary) / "out"
            with self.assertRaises(GlyphForgeError) as caught:
                run_pipeline(font, "A", config, output_dir=output)
            self.assertEqual(caught.exception.code, ErrorCode.E_ATLAS_OVERFLOW)
            self.assertFalse((output / "atlas.png").exists())
            self.assertFalse((output / "metrics.json").exists())

    def test_quantization_change_recomputes_and_reports_delta(self) -> None:
        font = parse_font(build_font_specs()["mixed.json"], path="mixed.json")
        first_config = WorkbenchConfig(pixel_size=20, tile_size=5, quant_step=1 / 10)
        first = run_pipeline(font, "AOB", first_config)
        previous = {
            glyph.name: glyph.summary.to_dict()
            for glyph in first.glyphs
            if glyph.summary is not None
        }
        second_config = replace(first_config, quant_step=1 / 255)
        second = run_pipeline(
            font,
            "AOB",
            second_config,
            previous_summaries=previous,
        )
        self.assertTrue(all(glyph.comparison is not None for glyph in second.glyphs))
        for glyph in second.glyphs:
            comparison = glyph.comparison
            self.assertEqual(comparison.previous_step, first_config.quant_step)
            self.assertEqual(comparison.current_step, second_config.quant_step)
            self.assertAlmostEqual(
                comparison.abs_error_delta,
                comparison.current_abs_error - comparison.previous_abs_error,
            )
        self.assertTrue(
            any(
                abs(glyph.comparison.abs_error_delta) > 1e-12
                for glyph in second.glyphs
            )
        )

    def test_repeat_runs_are_byte_deterministic(self) -> None:
        font = parse_font(build_font_specs()["composites.json"], path="composites.json")
        config = WorkbenchConfig(
            pixel_size=24,
            subpixel_x=0.25,
            subpixel_y=0.5,
            tile_size=7,
            quant_step=1 / 128,
            fill_rule="evenodd",
        )
        with tempfile.TemporaryDirectory() as temporary:
            first_dir = Path(temporary) / "first"
            second_dir = Path(temporary) / "second"
            first = run_pipeline(font, "A\u00c1\u00c2", config, output_dir=first_dir)
            second = run_pipeline(font, "A\u00c1\u00c2", config, output_dir=second_dir)
            self.assertEqual(first.atlas.alpha_hash, second.atlas.alpha_hash)
            self.assertEqual(
                (first_dir / "trace.jsonl").read_bytes(),
                (second_dir / "trace.jsonl").read_bytes(),
            )
            first_cov = sorted((first_dir / "coverage").glob("*.pgm"))
            second_cov = sorted((second_dir / "coverage").glob("*.pgm"))
            self.assertEqual(
                [path.name for path in first_cov],
                [path.name for path in second_cov],
            )
            for left, right in zip(first_cov, second_cov):
                self.assertEqual(left.read_bytes(), right.read_bytes())


if __name__ == "__main__":
    unittest.main()
