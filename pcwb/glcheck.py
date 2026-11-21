"""OpenGL 离屏上下文检查。

优先使用当前驱动创建离屏上下文；失败时回退到软件渲染后端
（设置 LIBGL_ALWAYS_SOFTWARE=1 后重试）。返回 (ok, info)。
"""

from __future__ import annotations

import os


def _try_create():
    from PyQt5.QtGui import (QGuiApplication, QOffscreenSurface,
                             QOpenGLContext, QSurfaceFormat)
    app = QGuiApplication.instance() or QGuiApplication([])
    fmt = QSurfaceFormat()
    fmt.setRenderableType(QSurfaceFormat.OpenGL)
    ctx = QOpenGLContext()
    ctx.setFormat(fmt)
    if not ctx.create():
        return None
    surface = QOffscreenSurface()
    surface.setFormat(fmt)
    surface.create()
    if not ctx.makeCurrent(surface):
        return None
    from OpenGL import GL
    version = GL.glGetString(GL.GL_VERSION)
    renderer = GL.glGetString(GL.GL_RENDERER)
    ctx.doneCurrent()
    return {
        "version": version.decode() if version else "unknown",
        "renderer": renderer.decode() if renderer else "unknown",
        "software": b"llvmpipe" in (renderer or b"") or b"softpipe" in (renderer or b""),
    }


def check_offscreen() -> tuple:
    """返回 (ok, info_dict)。info 含 version/renderer/software/backend。"""
    os.environ.setdefault("QT_QPA_PLATFORM", "offscreen")
    info = _try_create()
    if info is not None:
        info["backend"] = "driver"
        return True, info
    os.environ["LIBGL_ALWAYS_SOFTWARE"] = "1"
    info = _try_create()
    if info is not None:
        info["backend"] = "software-fallback"
        return True, info
    return False, {"backend": "none"}


if __name__ == "__main__":
    ok, info = check_offscreen()
    print("offscreen_gl:", "OK" if ok else "FAIL", info)
    raise SystemExit(0 if ok else 1)
