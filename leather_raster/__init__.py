"""Deterministic offline rasterizer for layered leather shadow puppets."""

from .errors import SceneValidationError
from .scene import load_scene

__all__ = ["SceneValidationError", "load_scene"]
__version__ = "1.0.0"

