"""Deterministic offline soft-raster renderer for layered lattice doors."""

from .scene import Scene, load_scene, make_sample_scene, write_sample_scene

__all__ = ["Scene", "load_scene", "make_sample_scene", "write_sample_scene"]

__version__ = "1.0.0"
RENDERER_VERSION = "serene-lattice-renderer-v1"
