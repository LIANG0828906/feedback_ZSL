"""Deterministic, offline multi-camera drum stem alignment."""

from .errors import BusinessError
from .pipeline import AlignmentResult, align_project

__all__ = ["AlignmentResult", "BusinessError", "align_project"]
__version__ = "1.0.0"
