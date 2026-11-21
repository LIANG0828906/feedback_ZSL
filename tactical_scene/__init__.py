"""Deterministic tactical-scene query core.

The package deliberately keeps the simulation/editor independent from Qt.  A
Qt front end can consume the immutable results, but all query, versioning and
trace behaviour is testable on a headless machine.
"""

from .camera import Camera
from .errors import ErrorCode, TacticalError
from .query import (
    BudgetConfig,
    HoverConfig,
    HoverResult,
    QueryEngine,
    SelectionResult,
)
from .scene import (
    AABBShape,
    CapsuleShape,
    Scene,
    SceneUnit,
    load_scene,
    write_scene,
)
from .versioning import SceneRepository, SceneEngine, SnapshotHandle

__all__ = [
    "AABBShape",
    "BudgetConfig",
    "Camera",
    "CapsuleShape",
    "ErrorCode",
    "HoverConfig",
    "HoverResult",
    "QueryEngine",
    "Scene",
    "SceneEngine",
    "SceneRepository",
    "SceneUnit",
    "SelectionResult",
    "SnapshotHandle",
    "TacticalError",
    "load_scene",
    "write_scene",
]
