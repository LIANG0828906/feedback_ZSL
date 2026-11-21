"""Deterministic vector path boolean operations.

The public API intentionally keeps all geometric decisions explicit:
tolerances are passed down to parsing and intersections, errors are business
codes with structured locations, and renderable results are only returned when
their error report is accepted.
"""

from .boolean import BooleanResult, boolean_paths
from .errors import BusinessError, ErrorCode
from .evaluate import ErrorReport
from .intersect import (
    Intersection,
    IntersectionDiff,
    diff_intersections,
    intersect_segments,
    recompute_intersections,
)
from .model import Operation, PathSpec
from .parser import load_path, parse_path, parse_segment

__all__ = [
    "BooleanResult",
    "BusinessError",
    "ErrorCode",
    "ErrorReport",
    "Intersection",
    "IntersectionDiff",
    "Operation",
    "PathSpec",
    "boolean_paths",
    "diff_intersections",
    "intersect_segments",
    "load_path",
    "parse_path",
    "parse_segment",
    "recompute_intersections",
]

