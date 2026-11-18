"""Deterministic offline soft rasterizer for stained-glass validation."""

from .errors import SceneValidationError
from .scene import load_scene, validate_scene

__all__ = ["SceneValidationError", "load_scene", "validate_scene"]
__version__ = "1.0.0"
