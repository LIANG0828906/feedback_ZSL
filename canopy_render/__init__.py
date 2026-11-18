"""Deterministic offline renderer for a backlit cave-shrine canopy."""

from .scene import generate_scene, canonical_scene, scene_digest
from .validation import validate_scene, ValidationIssue
from .renderer import render_scene, RenderResult

__all__ = [
    "generate_scene",
    "canonical_scene",
    "scene_digest",
    "validate_scene",
    "ValidationIssue",
    "render_scene",
    "RenderResult",
]

__version__ = "1.0.0"

