"""Offline deterministic software rasterizer for a five-layer paper-cut lightbox.

The package is self-contained: every sample, texture and shadow pattern is
derived from integer seeds declared in the scene JSON.  It depends only on
NumPy and Pillow.
"""

from .errors import SceneError, render_error_payload

__all__ = ["SceneError", "render_error_payload"]
__version__ = "1.0.0"
