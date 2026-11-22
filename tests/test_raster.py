from __future__ import annotations

import math

import numpy as np
import pytest

from glyph_workbench.errors import WorkbenchError
from glyph_workbench.flatten import flatten_glyph
from glyph_workbench.params import RenderParams
from glyph_workbench.raster import (
    combine_components,
    make_raster_spec,
    rasterize_flat_glyph,
)


@pytest.mark.parametrize("glyph_id", ["A", "O", "B"])
def test_raster_dimensions_area_and_coverage(document, glyph_id: str) -> None:
    glyph = document.find_glyph(glyph_id)
    params = RenderParams(pixels_per_unit=0.25, tolerance=0.35, grid=8)
    flattened = flatten_glyph(glyph, params)
    result = rasterize_flat_glyph(glyph, flattened.contours, params)
    bbox = glyph.truth.bbox
    assert result.width == max(1, math.ceil((bbox[2] - bbox[0]) * params.pixels_per_unit))
    assert result.height == max(1, math.ceil((bbox[3] - bbox[1]) * params.pixels_per_unit))
    relative = abs(result.area - glyph.truth.area) / glyph.truth.area
    assert relative < 0.01
    assert result.coverage.shape == (result.height, result.width)
    assert result.coverage_counts.dtype == np.uint16
    assert result.coverage.min() >= 0.0
    assert result.coverage.max() <= 1.0
    assert result.coverage_u8.max() <= 255
    partial = (result.coverage_counts > 0) & (
        result.coverage_counts < params.grid * params.grid
    )
    assert partial.any()
    counts, _coverage, coverage_u8, area = combine_components(
        result.spec, result.components
    )
    assert np.array_equal(counts, result.coverage_counts)
    assert np.array_equal(coverage_u8, result.coverage_u8)
    assert area == result.area


def test_grid_out_of_range_returns_e_para(document) -> None:
    glyph = document.find_glyph("A")
    for grid in (0, 17, 32):
        with pytest.raises(WorkbenchError) as caught:
            make_raster_spec(
                glyph,
                RenderParams(
                    pixels_per_unit=0.25,
                    tolerance=0.35,
                    grid=grid,
                ),
            )
        assert caught.value.code == "E_PARA"


def test_per_thread_buffer_limit_returns_e_para(document) -> None:
    glyph = document.find_glyph("B")
    params = RenderParams(
        pixels_per_unit=1.0,
        tolerance=0.2,
        grid=16,
        max_buffer_bytes=2048,
    )
    with pytest.raises(WorkbenchError) as caught:
        flatten = flatten_glyph(glyph, params)
        rasterize_flat_glyph(glyph, flatten.contours, params)
    assert caught.value.code == "E_PARA"
    assert "contour_index" in caught.value.context

