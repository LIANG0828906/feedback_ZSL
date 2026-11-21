"""Deterministic vector-path boolean pipeline for UI asset preparation."""

from .errors import PipelineError
from .model import Contour, PathData, Point, Segment, VertexRef, VertexShare

__all__ = [
    "Contour",
    "PathData",
    "PipelineError",
    "Point",
    "Segment",
    "VertexRef",
    "VertexShare",
]

