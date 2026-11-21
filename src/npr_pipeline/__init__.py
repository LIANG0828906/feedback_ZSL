"""Deterministic non-photorealistic layered stroke pipeline."""

from .errors import PipelineError
from .params import RenderParams
from .pipeline import RenderPipeline, RenderResult
from .preview import PreviewService, PreviewSnapshot, RenderService
from .recovery import RecoveryResult, recover
from .scene import Scene, load_scene, parse_scene
from .storage import Store

__all__ = [
    "PipelineError",
    "PreviewService",
    "PreviewSnapshot",
    "RenderParams",
    "RenderPipeline",
    "RenderResult",
    "RenderService",
    "RecoveryResult",
    "Scene",
    "Store",
    "load_scene",
    "parse_scene",
    "recover",
]

__version__ = "0.1.0"

