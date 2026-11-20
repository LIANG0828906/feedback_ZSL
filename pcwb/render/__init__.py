"""Rendering backends for the point-cloud workbench.

* :class:`~pcwb.render.camera.OrbitCamera` — deterministic orbit/zoom camera;
* :class:`~pcwb.render.gl_renderer.EGLRenderer` — OpenGL 3.3 core-profile
  point renderer on a headless EGL pbuffer (primary verified path here,
  since no freeglut/OSMesa native libraries exist in this environment);
* :class:`~pcwb.render.cpu_renderer.CPURenderer` — dependency-free software
  projection fallback so headless validation never depends on GL drivers;
* :func:`~pcwb.render.factory.create_renderer` picks an available backend.
"""

from .camera import OrbitCamera
from .cpu_renderer import CPURenderer, RenderResult

__all__ = ["OrbitCamera", "CPURenderer", "RenderResult", "create_renderer"]


def create_renderer(width: int = 640, height: int = 480,
                    prefer: str = "auto"):
    """Return a working renderer, preferring EGL OpenGL when available."""
    if prefer in ("auto", "egl"):
        try:
            from .gl_renderer import EGLRenderer
            return EGLRenderer(width, height)
        except Exception as exc:  # pragma: no cover - environment dependent
            if prefer == "egl":
                raise
            print(f"[render] EGL unavailable ({exc}); using CPU renderer")
    return CPURenderer(width, height)
