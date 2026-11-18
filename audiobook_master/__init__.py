"""Offline, deterministic audiobook mastering."""

from .config import load_and_validate
from .render import inspect_packages, render_book
from .synth import generate_packages

__all__ = [
    "generate_packages",
    "inspect_packages",
    "load_and_validate",
    "render_book",
]
__version__ = "1.0.0"
