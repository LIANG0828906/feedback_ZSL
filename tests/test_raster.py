from __future__ import annotations

import unittest

from glyphforge.errors import ErrorCode, GlyphForgeError
from glyphforge.parser import parse_font
from glyphforge.raster import (
    RasterOptions,
    compose_tiles,
    prepare_geometry,
    raster_region,
)


def _rect_contour(x0: float, y0: float, x1: float, y1: float, direction: str) -> dict:
    if direction == "ccw":
        points = [(x0, y0), (x1, y0), (x1, y1), (x0, y1), (x0, y0)]
    else:
        points = [(x0, y0), (x0, y1), (x1, y1), (x1, y0), (x0, y0)]
    return {
        "start": {"x": points[0][0], "y": points[0][1]},
        "direction": direction,
        "segments": [
            {"type": "line", "to": {"x": x, "y": y}} for x, y in points[1:]
        ],
    }


def _font(inner_direction: str | None) -> dict:
    contours = [_rect_contour(100, 100, 900, 900, "ccw")]
    if inner_direction is not None:
        contours.append(_rect_contour(300, 300, 700, 700, inner_direction))
    return {
        "format": "glyphforge.font/1",
        "units_per_em": 1000,
        "glyphs": {
            "box": {
                "advance": 1000,
                "unicode": [65],
                "contours": contours,
            }
        },
    }


class RasterTests(unittest.TestCase):
    def _raster_center(self, inner_direction: str | None, fill_rule: str) -> int:
        font = parse_font(_font(inner_direction))
        options = RasterOptions(
            pixel_size=64,
            units_per_em=1000,
            supersample=4,
            quant_step=1.0 / 255.0,
            fill_rule=fill_rule,
        )
        geometry = prepare_geometry(font.glyphs["box"], options)
        tile = raster_region(
            geometry,
            row=0,
            col=0,
            x0=geometry.x0,
            y0=geometry.y0,
            width=geometry.width,
            height=geometry.height,
            options=options,
            batch_size=512,
        )
        return int(tile.coverage[geometry.height // 2, geometry.width // 2])

    def test_nonzero_and_evenodd_holes(self) -> None:
        self.assertEqual(self._raster_center("ccw", "nonzero"), 255)
        self.assertEqual(self._raster_center("ccw", "evenodd"), 0)
        self.assertEqual(self._raster_center("cw", "nonzero"), 0)
        self.assertEqual(self._raster_center("cw", "evenodd"), 0)

    def test_invalid_quant_step(self) -> None:
        options = RasterOptions(
            pixel_size=24,
            units_per_em=1000,
            quant_step=0.0,
        )
        with self.assertRaises(GlyphForgeError) as caught:
            options.validate()
        self.assertEqual(caught.exception.code, ErrorCode.E_QUANT_INVALID)

    def test_tiled_coverage_matches_whole_truth(self) -> None:
        font = parse_font(_font("cw"))
        for fill_rule in ("nonzero", "evenodd"):
            options = RasterOptions(
                pixel_size=37,
                units_per_em=1000,
                subpixel_x=0.25,
                subpixel_y=0.375,
                supersample=5,
                quant_step=1.0 / 64.0,
                fill_rule=fill_rule,
            )
            geometry = prepare_geometry(font.glyphs["box"], options)
            truth = raster_region(
                geometry,
                row=0,
                col=0,
                x0=geometry.x0,
                y0=geometry.y0,
                width=geometry.width,
                height=geometry.height,
                options=options,
                batch_size=333,
            )
            tiles = [
                raster_region(
                    geometry,
                    row=row,
                    col=col,
                    x0=x,
                    y0=y,
                    width=width,
                    height=height,
                    options=options,
                    batch_size=277,
                )
                for row, col, x, y, width, height in geometry.tile_rects(7)
            ]
            stitched, summary = compose_tiles(
                tiles, geometry=geometry, quant_step=options.quant_step
            )
            self.assertTrue((stitched == truth.coverage).all())
            self.assertEqual(summary.samples, truth.summary.samples)


if __name__ == "__main__":
    unittest.main()
