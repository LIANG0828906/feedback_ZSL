"""Deterministic four-track string quartet mastering component."""

from .errors import BusinessError
from .manifest import load_manifest
from .render import render_project
from .synth import derive_score, generate_project, synthesize_tracks

__all__ = [
    "BusinessError",
    "derive_score",
    "generate_project",
    "load_manifest",
    "render_project",
    "synthesize_tracks",
]

__version__ = "1.0.0"
