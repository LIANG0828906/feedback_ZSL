"""Deterministic vector glyph rasterization workbench."""

from .errors import ErrorCode, GlyphForgeError
from .model import Affine, Font, Glyph, GlyphContour
from .parser import load_font, parse_font
from .raster import (
    CoverageSummary,
    GlyphGeometry,
    RasterOptions,
    TileCoverage,
    compose_tiles,
    prepare_geometry,
    raster_region,
)

__all__ = [
    "Affine",
    "CoverageSummary",
    "ErrorCode",
    "Font",
    "Glyph",
    "GlyphContour",
    "GlyphForgeError",
    "GlyphGeometry",
    "RasterOptions",
    "TileCoverage",
    "compose_tiles",
    "load_font",
    "parse_font",
    "prepare_geometry",
    "raster_region",
]

__version__ = "1.0.0"
