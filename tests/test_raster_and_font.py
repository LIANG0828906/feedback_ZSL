from __future__ import annotations

import json
import tempfile
import unittest
from pathlib import Path

import numpy as np

from glyph_atlas.errors import E_GLYPH_INVALID, GlyphAtlasError
from glyph_atlas.font import parse_font
from glyph_atlas.generator import _mixed_nonzero_font
from glyph_atlas.raster import (
    PixelBox,
    SamplingSpec,
    compare_coverage,
    make_render_plan,
    rasterize_tile,
    rasterize_whole,
)
from tests.helpers import square_hole_font


class FontParserTests(unittest.TestCase):
    def test_mixed_curves_and_composite_resolve(self) -> None:
        font = parse_font(_mixed_nonzero_font(1947))
        o = font.resolve("O")
        self.assertEqual(len(o.contours), 2)
        self.assertEqual(o.contours[0].orientation, "counterclockwise")
        self.assertEqual(o.contours[1].orientation, "clockwise")
        ring = font.resolve("Aring")
        self.assertGreaterEqual(len(ring.contours), 4)
        self.assertEqual(font.glyph(197).name, "Aring")

    def test_missing_point_segment_control_and_reference_are_located(self) -> None:
        cases = [
            (
                {
                    "units_per_em": 1000,
                    "glyphs": [
                        {
                            "name": "bad",
                            "advance_width": 100,
                            "contours": [{"points": [], "segments": [{"type": "line", "to": 0}]}],
                        }
                    ],
                },
                "bad",
                0,
            ),
            (
                {
                    "units_per_em": 1000,
                    "glyphs": [
                        {
                            "name": "bad",
                            "advance_width": 100,
                            "contours": [{"points": [[0, 0]], "segments": []}],
                        }
                    ],
                },
                "bad",
                0,
            ),
            (
                {
                    "units_per_em": 1000,
                    "glyphs": [
                        {
                            "name": "bad",
                            "advance_width": 100,
                            "contours": [
                                {
                                    "points": [[0, 0], [10, 10]],
                                    "segments": [{"type": "quadratic", "to": 1}],
                                }
                            ],
                        }
                    ],
                },
                "bad",
                0,
            ),
            (
                {
                    "units_per_em": 1000,
                    "glyphs": [
                        {
                            "name": "bad",
                            "advance_width": 100,
                            "components": [{"glyph": "missing"}],
                        }
                    ],
                },
                "bad",
                0,
            ),
        ]
        for document, glyph, contour in cases:
            with self.subTest(glyph=glyph, contour=contour):
                with self.assertRaises(GlyphAtlasError) as caught:
                    parse_font(document)
                self.assertEqual(caught.exception.code, E_GLYPH_INVALID)
                self.assertEqual(caught.exception.details.get("glyph"), glyph)
                self.assertEqual(caught.exception.details.get("contour"), contour)

    def test_self_reference_is_rejected(self) -> None:
        with self.assertRaises(GlyphAtlasError) as caught:
            parse_font(
                {
                "units_per_em": 1000,
                "glyphs": [
                    {
                        "name": "loop",
                        "advance_width": 100,
                        "components": [{"glyph": "loop"}],
                    }
                ],
                }
            )
        self.assertEqual(caught.exception.code, E_GLYPH_INVALID)
        self.assertEqual(caught.exception.details["glyph"], "loop")


class RasterTests(unittest.TestCase):
    def test_nonzero_and_evenodd_have_different_hole_semantics(self) -> None:
        font = parse_font(square_hole_font(same_orientation=True))
        sampling = SamplingSpec(4, 4, 1)
        nonzero = make_render_plan(font.resolve("hole", "nonzero"), 10, 1000)
        evenodd = make_render_plan(font.resolve("hole", "evenodd"), 10, 1000)
        nonzero_coverage, _ = rasterize_whole(nonzero, sampling)
        evenodd_coverage, _ = rasterize_whole(evenodd, sampling)
        self.assertEqual(int(nonzero_coverage[5, 5]), 255)
        self.assertEqual(int(evenodd_coverage[5, 5]), 0)
        self.assertEqual(int(evenodd_coverage[0, 0]), 255)

    def test_quantization_error_summary(self) -> None:
        font = parse_font(square_hole_font())
        plan = make_render_plan(font.resolve("hole"), 16, 1000)
        _, tile = rasterize_whole(plan, SamplingSpec(4, 4, 16))
        self.assertLessEqual(tile.max_quantization_error, 8.0)
        self.assertGreaterEqual(tile.quantization_error, 0.0)
        self.assertEqual(tile.sample_points, plan.width * plan.height * 16)

    def test_tile_assembly_matches_whole_for_composite_holes(self) -> None:
        font = parse_font(_mixed_nonzero_font(1947))
        sampling = SamplingSpec(3, 3, 1)
        for glyph_name in ("O", "A", "Aring", "Oacute"):
            with self.subTest(glyph=glyph_name):
                plan = make_render_plan(font.resolve(glyph_name), 24, 1000, 0.25, 0.125)
                whole, _ = rasterize_whole(plan, sampling)
                assembled = np.zeros_like(whole)
                tiles = plan.tile_boxes(7, 5)
                for index in range(len(tiles) - 1, -1, -1):
                    coord, box = tiles[index]
                    raster = rasterize_tile(plan, box, sampling, coord)
                    assembled[
                        box.y : box.y + box.height,
                        box.x : box.x + box.width,
                    ] = raster.coverage
                consistent, details = compare_coverage(assembled, whole, 0)
                self.assertTrue(consistent, details)
                self.assertEqual(details["max_difference"], 0)


if __name__ == "__main__":
    unittest.main()

