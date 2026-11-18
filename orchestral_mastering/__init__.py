"""Deterministic offline orchestral mastering toolkit."""

from .manifest import Manifest, TrackSpec, load_manifest
from .pipeline import RenderConfig, render_project

__all__ = [
    "Manifest",
    "TrackSpec",
    "load_manifest",
    "RenderConfig",
    "render_project",
]

__version__ = "1.0.0"

