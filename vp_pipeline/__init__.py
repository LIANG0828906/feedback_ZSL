"""Deterministic virtual-production compositing pipeline."""

from .errors import PipelineError
from .loader import load_scene, load_scene_result
from .model import Canvas, Layer, Rect, SceneModel, Timeline
from .renderer import ALGORITHM_VERSION, render_all_frames, render_frame
from .session import CompositorSession

__all__ = [
    "ALGORITHM_VERSION",
    "Canvas",
    "CompositorSession",
    "Layer",
    "PipelineError",
    "Rect",
    "SceneModel",
    "Timeline",
    "load_scene",
    "load_scene_result",
    "render_all_frames",
    "render_frame",
]
