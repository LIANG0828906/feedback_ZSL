from __future__ import annotations

import numpy as np
import pytest

from glyph_workbench.errors import WorkbenchError
from glyph_workbench.flatten import flatten_glyph
from glyph_workbench.params import RenderParams


def test_tolerance_monotonic_and_closed_without_duplicate_endpoint(document) -> None:
    glyph = document.find_glyph("A")
    previous = -1
    for tolerance in (2.0, 1.0, 0.5, 0.35, 0.2, 0.1, 0.05):
        result = flatten_glyph(
            glyph,
            RenderParams(
                pixels_per_unit=0.25,
                tolerance=tolerance,
                grid=8,
                max_workers=2,
            ),
        )
        counts = [contour.polyline_point_count for contour in result.contours]
        total = sum(counts)
        assert total >= previous
        previous = total
        for contour in result.contours:
            assert contour.closed is True
            assert contour.points.ndim == 2 and contour.points.shape[1] == 2
            assert np.isfinite(contour.points).all()
            adjacent = np.linalg.norm(np.diff(contour.points, axis=0), axis=1)
            assert np.all(adjacent > 1e-9)
            assert np.linalg.norm(contour.points[-1] - contour.points[0]) > 1e-9
            closing_edge = np.linalg.norm(contour.points[0] - contour.points[-1])
            assert closing_edge > 1e-9


def test_deviation_statistics_are_reported(document) -> None:
    glyph = document.find_glyph("O")
    tight = flatten_glyph(
        glyph,
        RenderParams(pixels_per_unit=0.4, tolerance=0.1, grid=8, max_workers=2),
    )
    loose = flatten_glyph(
        glyph,
        RenderParams(pixels_per_unit=0.4, tolerance=2.0, grid=8, max_workers=2),
    )
    assert tight.deviation.sample_count == loose.deviation.sample_count > 0
    assert tight.deviation.maximum <= loose.deviation.maximum


def test_invalid_tolerances_return_e_para(document) -> None:
    glyph = document.find_glyph("A")
    for tolerance in (0.0, -0.1, 100.0, float("nan")):
        with pytest.raises(WorkbenchError) as caught:
            flatten_glyph(
                glyph,
                RenderParams(
                    pixels_per_unit=0.25,
                    tolerance=tolerance,
                    grid=8,
                ),
            )
        assert caught.value.code == "E_PARA"

