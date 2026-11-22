"""Deterministic multimedia container workbench."""

from .errors import WorkbenchError
from .model import (
    Chapter,
    ContainerModel,
    Frame,
    MediaTrack,
    Timebase,
)

__all__ = [
    "Chapter",
    "ContainerModel",
    "Frame",
    "MediaTrack",
    "Timebase",
    "WorkbenchError",
]

__version__ = "1.0.0"
ALGORITHM_VERSION = "mediaforge-pack/1.0.0"
