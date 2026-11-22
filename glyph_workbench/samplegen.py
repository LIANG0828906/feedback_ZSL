from __future__ import annotations

import copy
import json
from pathlib import Path

from .geometry import compute_truth, contour_signed_area, reverse_contour
from .models import Contour, Glyph, GlyphDocument, Segment


def _line(p0: tuple[int, int], p1: tuple[int, int]) -> Segment:
    return Segment("line", p0, p1)


def _quad(
    p0: tuple[int, int], p1: tuple[int, int], p2: tuple[int, int]
) -> Segment:
    return Segment("quadratic", p0, p1, p2)


def _cubic(
    p0: tuple[int, int],
    p1: tuple[int, int],
    p2: tuple[int, int],
    p3: tuple[int, int],
) -> Segment:
    return Segment("cubic", p0, p1, p2, p3)


def _contour(index: int, start: tuple[int, int], segments: list[Segment]) -> Contour:
    return Contour(index=index, start=start, segments=segments)


def _oriented(contour: Contour, positive: bool) -> Contour:
    area = contour_signed_area(contour)
    if (area > 0.0) != positive:
        return reverse_contour(contour)
    return contour


def _glyph_a() -> Glyph:
    outer = _contour(
        0,
        (80, 0),
        [
            _line((80, 0), (230, 0)),
            _cubic((230, 0), (245, 60), (260, 120), (280, 180)),
            _line((280, 180), (430, 180)),
            _cubic((430, 180), (450, 120), (465, 60), (480, 0)),
            _line((480, 0), (650, 0)),
            _cubic((650, 0), (610, 260), (480, 550), (390, 700)),
            _quad((390, 700), (370, 715), (350, 700)),
            _cubic((350, 700), (190, 520), (110, 250), (80, 0)),
        ],
    )
    hole = _contour(
        1,
        (330, 300),
        [
            _line((330, 300), (385, 300)),
            _line((385, 300), (360, 470)),
            _line((360, 470), (330, 300)),
        ],
    )
    contours = [_oriented(outer, True), _oriented(hole, False)]
    return Glyph(
        glyph_id="A",
        baseline=0,
        advance_width=720,
        units_per_em=1000,
        coordinate_quantum=1,
        coordinate_min=-4096,
        coordinate_max=8192,
        contours=contours,
    )


def _ellipse_contour(
    index: int,
    center: tuple[int, int],
    radius_x: int,
    radius_y: int,
) -> Contour:
    cx, cy = center
    start = (cx, cy - radius_y)
    return _contour(
        index,
        start,
        [
            _cubic(
                start,
                (cx + (radius_x * 55) // 100, cy - radius_y),
                (cx + radius_x, cy - (radius_y * 55) // 100),
                (cx + radius_x, cy),
            ),
            _quad(
                (cx + radius_x, cy),
                (cx + radius_x, cy + (radius_y * 80) // 100),
                (cx, cy + radius_y),
            ),
            _cubic(
                (cx, cy + radius_y),
                (cx - (radius_x * 55) // 100, cy + radius_y),
                (cx - radius_x, cy + (radius_y * 55) // 100),
                (cx - radius_x, cy),
            ),
            _quad(
                (cx - radius_x, cy),
                (cx - radius_x, cy - (radius_y * 80) // 100),
                start,
            ),
        ],
    )


def _glyph_o() -> Glyph:
    outer = _oriented(_ellipse_contour(0, (300, 350), 210, 350), True)
    inner = _oriented(_ellipse_contour(1, (300, 350), 105, 175), False)
    return Glyph(
        glyph_id="O",
        baseline=0,
        advance_width=620,
        units_per_em=1000,
        coordinate_quantum=1,
        coordinate_min=-4096,
        coordinate_max=8192,
        contours=[outer, inner],
    )


def _glyph_b() -> Glyph:
    outer = _contour(
        0,
        (100, 0),
        [
            _line((100, 0), (420, 0)),
            _cubic((420, 0), (510, 0), (555, 160), (460, 330)),
            _cubic((460, 330), (590, 390), (610, 610), (440, 700)),
            _line((440, 700), (100, 700)),
            _quad((100, 700), (30, 560), (30, 140)),
            _quad((30, 140), (30, 35), (100, 0)),
        ],
    )
    upper = _oriented(_ellipse_contour(1, (300, 500), 92, 108), False)
    lower = _oriented(_ellipse_contour(2, (300, 220), 100, 112), False)
    return Glyph(
        glyph_id="B",
        baseline=0,
        advance_width=760,
        units_per_em=1000,
        coordinate_quantum=1,
        coordinate_min=-4096,
        coordinate_max=8192,
        contours=[_oriented(outer, True), upper, lower],
    )


def build_document() -> GlyphDocument:
    glyphs = [_glyph_a(), _glyph_o(), _glyph_b()]
    for glyph in glyphs:
        glyph.truth = compute_truth(glyph)
    return GlyphDocument(
        schema="glyph-workbench.samples/1",
        pixels_per_unit=0.25,
        glyphs=glyphs,
    )


def _clone_with_first_glyph(raw: dict) -> dict:
    clone = copy.deepcopy(raw)
    return clone


def _first_segment(raw: dict) -> dict:
    return raw["glyphs"][0]["contours"][0]["segments"][0]


def _write_json(path: Path, payload: dict) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    text = json.dumps(payload, ensure_ascii=True, indent=2, sort_keys=False)
    path.write_text(text + "\n", encoding="utf-8")


def generate_samples(output_dir: str | Path) -> dict[str, Path]:
    output = Path(output_dir)
    output.mkdir(parents=True, exist_ok=True)
    document = build_document()
    sample_path = output / "glyph_samples.json"
    truth_path = output / "glyph_truth.json"
    _write_json(sample_path, document.to_dict(include_truth=True))
    _write_json(
        truth_path,
        {
            glyph.glyph_id: glyph.truth.to_dict()
            for glyph in document.glyphs
            if glyph.truth is not None
        },
    )

    raw = document.to_dict(include_truth=True)
    corrupt_dir = output / "corrupt"
    corrupt_dir.mkdir(parents=True, exist_ok=True)

    missing = _clone_with_first_glyph(raw)
    del missing["glyphs"][0]["baseline"]
    _write_json(corrupt_dir / "missing_field.json", missing)

    unclosed = _clone_with_first_glyph(raw)
    last_segment = unclosed["glyphs"][0]["contours"][0]["segments"][-1]
    key = "p3" if last_segment["type"] == "cubic" else "p2" if last_segment["type"] == "quadratic" else "p1"
    last_segment[key] = [9, 9]
    _write_json(corrupt_dir / "unclosed.json", unclosed)

    bad_curve = _clone_with_first_glyph(raw)
    _first_segment(bad_curve)["type"] = "spline"
    _write_json(corrupt_dir / "bad_curve_type.json", bad_curve)

    discontinuous = _clone_with_first_glyph(raw)
    if len(discontinuous["glyphs"][0]["contours"][0]["segments"]) > 1:
        discontinuous["glyphs"][0]["contours"][0]["segments"][1]["p0"] = [7, 7]
    _write_json(corrupt_dir / "discontinuous.json", discontinuous)

    out_of_range = _clone_with_first_glyph(raw)
    _first_segment(out_of_range)["p1"] = [999999, 0]
    _write_json(corrupt_dir / "out_of_range.json", out_of_range)

    malformed = _clone_with_first_glyph(raw)
    malformed_text = json.dumps(malformed, indent=2)[:-8] + "\n"
    (corrupt_dir / "malformed.json").write_text(malformed_text, encoding="utf-8")

    # Make the intentional failure mode discoverable without hidden state.
    _write_json(
        output / "manifest.json",
        {
            "schema": document.schema,
            "sample_file": sample_path.name,
            "truth_file": truth_path.name,
            "corrupt_cases": {
                "missing_field.json": "E_SCHEMA",
                "unclosed.json": "E_SCHEMA",
                "bad_curve_type.json": "E_CURVE",
                "discontinuous.json": "E_SCHEMA",
                "out_of_range.json": "E_SCHEMA",
                "malformed.json": "E_DECODE",
            },
        },
    )
    return {
        "samples": sample_path,
        "truth": truth_path,
        "corrupt_dir": corrupt_dir,
    }


def main() -> int:
    import argparse

    parser = argparse.ArgumentParser(description="Generate deterministic glyph samples")
    parser.add_argument("--output", default="samples", help="output directory")
    args = parser.parse_args()
    paths = generate_samples(args.output)
    for name, path in paths.items():
        print(f"{name}: {path}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

