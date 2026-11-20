"""Deterministic incremental acoustic field reconstruction."""

from .errors import AcousticError
from .model import Scene, load_materials
from .service import AcousticEngine, RebuildReport

__all__ = [
    "AcousticEngine",
    "AcousticError",
    "RebuildReport",
    "Scene",
    "load_materials",
]

__version__ = "0.1.0"

