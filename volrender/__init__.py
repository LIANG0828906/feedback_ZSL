"""Deterministic offline volumetric light preview renderer."""

from .binary import build_scene, decode_scene, load_scene
from .config import RenderConfig, load_config
from .errors import BusinessError, ErrorCode
from .incremental import ChangeSet, IncrementalRenderer
from .model import Camera, DensityField, DirectionalLight, Medium, Scene
from .renderer import RenderResult, render_scene
from .scheduler import partition_screen, schedule_tiles

__all__ = [
    "BusinessError",
    "Camera",
    "ChangeSet",
    "DensityField",
    "DirectionalLight",
    "ErrorCode",
    "IncrementalRenderer",
    "Medium",
    "RenderConfig",
    "RenderResult",
    "Scene",
    "build_scene",
    "decode_scene",
    "load_config",
    "load_scene",
    "partition_screen",
    "render_scene",
    "schedule_tiles",
]

__version__ = "1.0.0"
