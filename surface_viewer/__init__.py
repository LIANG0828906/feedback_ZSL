"""Incremental surface viewer core.

The package is intentionally dependency-light: NumPy is used for deterministic
geometry, PyOpenGL is optional and is only imported by the OpenGL preview
backend.
"""

from .errors import ErrorCode, SurfaceViewerError
from .model import (
    MAX_PRECISION_LEVEL,
    MIN_PRECISION_LEVEL,
    SURFACE_TYPES,
    SurfaceAssembly,
    SurfacePatch,
    load_assembly,
    parse_assembly,
)

__all__ = [
    "ErrorCode",
    "SurfaceViewerError",
    "SurfaceAssembly",
    "SurfacePatch",
    "SURFACE_TYPES",
    "MIN_PRECISION_LEVEL",
    "MAX_PRECISION_LEVEL",
    "load_assembly",
    "parse_assembly",
]
