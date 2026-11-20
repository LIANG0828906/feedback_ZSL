from __future__ import annotations

import copy
import unittest

from glyphforge.errors import ErrorCode, GlyphForgeError
from glyphforge.parser import parse_font
from glyphforge.samples import build_font_specs


def _rectangle(
    name: str,
    coordinates: tuple[float, float, float, float],
    *,
    direction: str,
) -> dict:
    x0, y0, x1, y1 = coordinates
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
        "_name": name,
    }


def _font(contours: list[dict]) -> dict:
    for contour in contours:
        contour.pop("_name", None)
    return {
        "format": "glyphforge.font/1",
        "units_per_em": 1000,
        "glyphs": {
            "test": {
                "advance": 600,
                "unicode": [65],
                "contours": contours,
            }
        },
    }


class ParserTests(unittest.TestCase):
    def test_sample_fonts_and_composites(self) -> None:
        for filename, spec in build_font_specs().items():
            font = parse_font(spec, path=filename)
            self.assertGreater(len(font.glyphs), 0)
            self.assertGreater(len(font.unicode_map), 0)
        composites = parse_font(build_font_specs()["composites.json"])
        acute = composites.glyphs["Aacute"]
        self.assertEqual(acute.source_kind, "composite")
        self.assertGreater(len(acute.contours), len(composites.glyphs["A"].contours))
        self.assertGreater(acute.bbox[3], composites.glyphs["A"].bbox[3])
        self.assertEqual(composites.unicode_map[193], "Aacute")

    def test_declared_direction_is_normalized(self) -> None:
        contour = _rectangle("x", (100, 100, 900, 900), direction="ccw")
        contour["_name"] = "x"
        # The supplied point order is clockwise while the declaration says ccw.
        contour["start"] = {"x": 100, "y": 100}
        contour["segments"] = [
            {"type": "line", "to": {"x": 100, "y": 900}},
            {"type": "line", "to": {"x": 900, "y": 900}},
            {"type": "line", "to": {"x": 900, "y": 100}},
            {"type": "line", "to": {"x": 100, "y": 100}},
        ]
        font = parse_font(_font([contour]))
        self.assertGreater(font.glyphs["test"].contours[0].signed_area, 0)

    def test_missing_point_reports_glyph_and_contour(self) -> None:
        contour = _rectangle("x", (100, 100, 900, 900), direction="ccw")
        contour["segments"][0]["to"].pop("x")
        with self.assertRaises(GlyphForgeError) as caught:
            parse_font(_font([contour]))
        error = caught.exception
        self.assertEqual(error.code, ErrorCode.E_GLYPH_INVALID)
        self.assertEqual(error.details["glyph"], "test")
        self.assertEqual(error.details["contour_index"], 0)

    def test_unclosed_contour_rejected(self) -> None:
        contour = _rectangle("", (100, 100, 900, 900), direction="ccw")
        contour["segments"][-1]["to"] = {"x": 150, "y": 100}
        with self.assertRaises(GlyphForgeError) as caught:
            parse_font(_font([contour]))
        self.assertEqual(caught.exception.code, ErrorCode.E_GLYPH_INVALID)
        self.assertEqual(caught.exception.details["glyph"], "test")
        self.assertEqual(caught.exception.details["contour_index"], 0)

    def test_dangling_composite_reference(self) -> None:
        raw = build_font_specs()["composites.json"]
        broken = copy.deepcopy(raw)
        broken["glyphs"]["Aacute"]["components"][1]["glyph"] = "does-not-exist"
        with self.assertRaises(GlyphForgeError) as caught:
            parse_font(broken)
        error = caught.exception
        self.assertEqual(error.code, ErrorCode.E_GLYPH_INVALID)
        self.assertEqual(error.details["glyph"], "Aacute")
        self.assertEqual(error.details["referenced_glyph"], "does-not-exist")
        self.assertIn("contour_index", error.details)


if __name__ == "__main__":
    unittest.main()
