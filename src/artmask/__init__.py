"""ArtMask Workbench: deterministic segmentation and mask diagnostics."""

from .config import ALGORITHM_VERSION, Budget, ScaleTolerance, SegmentationParams
from .errors import BusinessError

__all__ = [
    "ALGORITHM_VERSION",
    "Budget",
    "BusinessError",
    "ScaleTolerance",
    "SegmentationParams",
]

__version__ = "1.0.0"
