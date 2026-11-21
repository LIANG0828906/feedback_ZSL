"""Optional PyOpenGL/offscreen-context diagnostics for the desktop app."""

from __future__ import annotations

from typing import Any


def probe_opengl() -> dict[str, Any]:
    result: dict[str, Any] = {
        "available": False,
        "pyopengl_version": "",
        "context_created": False,
        "vendor": "",
        "renderer": "",
        "version": "",
    }
    try:
        import OpenGL
        from OpenGL import GL

        result["pyopengl_version"] = str(getattr(OpenGL, "__version__", "unknown"))
    except Exception as exc:
        result["error"] = f"PyOpenGL import failed: {exc}"
        return result

    try:
        from PyQt5.QtGui import QOffscreenSurface, QOpenGLContext

        surface = QOffscreenSurface()
        surface.create()
        context = QOpenGLContext()
        if not context.create():
            result["error"] = "QOpenGLContext.create failed"
            return result
        if not context.makeCurrent(surface):
            result["error"] = "QOpenGLContext.makeCurrent failed"
            return result
        try:
            result["context_created"] = True
            vendor = GL.glGetString(GL.GL_VENDOR)
            renderer = GL.glGetString(GL.GL_RENDERER)
            version = GL.glGetString(GL.GL_VERSION)
            result["vendor"] = vendor.decode("utf-8", "replace") if vendor else ""
            result["renderer"] = renderer.decode("utf-8", "replace") if renderer else ""
            result["version"] = version.decode("utf-8", "replace") if version else ""
            result["available"] = True
        finally:
            context.doneCurrent()
    except Exception as exc:
        result["error"] = f"offscreen OpenGL context failed: {exc}"
    return result
