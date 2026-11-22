from __future__ import annotations

import json
from pathlib import Path

import pytest

from glyph_workbench.errors import WorkbenchError
from glyph_workbench.models import load_document, parse_document


def test_generated_samples_validate_and_have_mixed_curves(document) -> None:
    assert [glyph.glyph_id for glyph in document.glyphs] == ["A", "O", "B"]
    for glyph in document.glyphs:
        assert len(glyph.contours) >= 2
        assert glyph.truth is not None
        assert glyph.truth.area > 0
        kinds = {
            segment.kind
            for contour in glyph.contours
            for segment in contour.segments
        }
        assert "quadratic" in kinds
        assert "cubic" in kinds
        parsed = parse_document(
            {
                "schema": document.schema,
                "pixels_per_unit": document.pixels_per_unit,
                "glyphs": [glyph.content_dict()],
            }
        )
        assert parsed.find_glyph(glyph.glyph_id)


def test_corrupt_cases_return_expected_codes(sample_paths: dict[str, Path]) -> None:
    expected = {
        "bad_curve_type.json": "E_CURVE",
        "discontinuous.json": "E_SCHEMA",
        "malformed.json": "E_DECODE",
        "missing_field.json": "E_SCHEMA",
        "out_of_range.json": "E_SCHEMA",
        "unclosed.json": "E_SCHEMA",
    }
    for filename, code in expected.items():
        with pytest.raises(WorkbenchError) as caught:
            load_document(sample_paths["corrupt_dir"] / filename)
        assert caught.value.code == code
        if code != "E_DECODE":
            assert caught.value.context.get("glyph_id") == "A"
        if filename in {
            "bad_curve_type.json",
            "discontinuous.json",
            "out_of_range.json",
            "unclosed.json",
        }:
            assert "contour_index" in caught.value.context


def test_unknown_curve_is_located(sample_paths: dict[str, Path]) -> None:
    with pytest.raises(WorkbenchError) as caught:
        load_document(sample_paths["corrupt_dir"] / "bad_curve_type.json")
    assert caught.value.code == "E_CURVE"
    assert caught.value.context["glyph_id"] == "A"
    assert caught.value.context["contour_index"] == 0
    assert caught.value.context["segment_index"] == 0


def test_missing_truth_geometry_is_still_valid(sample_paths: dict[str, Path]) -> None:
    raw = json.loads(sample_paths["samples"].read_text(encoding="utf-8"))
    for glyph in raw["glyphs"]:
        glyph.pop("truth", None)
    parsed = parse_document(raw)
    assert len(parsed.glyphs) == 3

