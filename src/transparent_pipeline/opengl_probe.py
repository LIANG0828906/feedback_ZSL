"""Document the software-rendering fallback and optional OpenGL availability."""

from __future__ import annotations

import os
import platform
from typing import Any


def probe_opengl() -> dict[str, Any]:
    report: dict[str, Any] = {
        "platform": platform.platform(),
        "renderer": "software",
        "software_fallback": True,
        "pyopengl_imported": False,
        "window_context_available": False,
        "reason": "the pipeline uses a deterministic NumPy software rasterizer",
    }
    try:
        import OpenGL  # noqa: F401

        report["pyopengl_imported"] = True
    except Exception as exc:
        report["opengl_error"] = f"{type(exc).__name__}: {exc}"

    display = os.environ.get("DISPLAY") or os.environ.get("WAYLAND_DISPLAY")
    if display:
        report["display_environment"] = display
    else:
        report["display_environment"] = None
        report["reason"] += "; no local display server was found"
    if platform.system() == "Windows":
        report["reason"] += "; a Windows context provider is not bundled, software path is authoritative"
    return report


__all__ = ["probe_opengl"]

