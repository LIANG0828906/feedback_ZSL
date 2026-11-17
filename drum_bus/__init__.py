"""Deterministic offline percussion bus rendering."""

from .errors import ValidationFailure
from .manifest import Project, load_project
from .render import render_project
from .synth import DemoConfig, generate_demo_project

__all__ = [
    "DemoConfig",
    "Project",
    "ValidationFailure",
    "generate_demo_project",
    "load_project",
    "render_project",
]
__version__ = "1.0.0"

