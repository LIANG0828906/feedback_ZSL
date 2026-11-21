"""Deterministic tactical battlefield picking workbench.

The package intentionally keeps the simulation core independent from Qt and
NumPy.  Both are useful for the interactive shell, but the command line,
sample generation, trace replay and regression checks only require the Python
standard library.
"""

from .camera import Camera, Ray
from .errors import PickError
from .model import Cover, Grid, Scene, Unit
from .versioning import RegionSnapshot, SnapshotHandle, VersionStore

__all__ = [
    "Camera",
    "Cover",
    "Grid",
    "PickError",
    "Ray",
    "RegionSnapshot",
    "Scene",
    "SnapshotHandle",
    "Unit",
    "VersionStore",
]

__version__ = "1.0.0"
