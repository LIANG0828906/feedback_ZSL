"""Deterministic picking, versioned snapshots, placement checks and replay."""

from .camera import Camera
from .errors import ToolError
from .query import Hit, Picker, PickRequest, PickResult
from .region import PlacementRequest, RegionJudge
from .scene import CoverTriangle, SceneData, Unit, read_scene, write_scene
from .snapshot import SceneManager, SceneSnapshot
from .trace import TraceLog

__all__ = [
    "Camera",
    "CoverTriangle",
    "Hit",
    "PickRequest",
    "PickResult",
    "Picker",
    "PlacementRequest",
    "RegionJudge",
    "SceneData",
    "SceneManager",
    "SceneSnapshot",
    "ToolError",
    "TraceLog",
    "Unit",
    "read_scene",
    "write_scene",
]

