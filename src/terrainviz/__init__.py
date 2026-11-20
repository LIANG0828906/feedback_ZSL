"""Deterministic CPU terrain visualization pipeline."""

from .camera import Camera
from .cancel import CancellationToken
from .demo import run_demo
from .errors import TerrainError
from .heightfield import HeightField
from .lod import choose_lod
from .material import MaterialMap
from .normals import NormalManager
from .render import RenderConfig, RenderResult, Renderer
from .samples import generate_sample_set, sample_camera
from .seams import repair_seams
from .service import RenderService
from .snapshot import load_snapshot, recover_terrain_from_disk, save_snapshot
from .terrain import Terrain

__all__ = [
    "Camera",
    "CancellationToken",
    "HeightField",
    "MaterialMap",
    "NormalManager",
    "RenderConfig",
    "RenderResult",
    "Renderer",
    "RenderService",
    "TerrainError",
    "Terrain",
    "choose_lod",
    "generate_sample_set",
    "load_snapshot",
    "recover_terrain_from_disk",
    "repair_seams",
    "run_demo",
    "sample_camera",
    "save_snapshot",
]

__version__ = "0.1.0"

