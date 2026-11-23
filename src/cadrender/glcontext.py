"""OpenGL 离屏上下文与帧缓冲探测。

仅做能力探测与可选加速路径选择。任何驱动/格式不支持都不致命：回退到
:mod:`cadrender.renderer` 软件光栅器，并在返回的 ``mem_events`` 中以
``E_MEM`` 记录原因，满足“硬件不支持缓冲区格式时软件回退并记录”。
"""
from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any, Dict, List, Optional

from .errors import E_MEM


@dataclass
class GlCapability:
    available: bool
    renderer: str = ""
    version: str = ""
    fbo_supported: bool = False
    backend: str = "software"
    mem_events: List[Dict[str, Any]] = field(default_factory=list)

    def to_dict(self) -> Dict[str, Any]:
        return {
            "available": self.available,
            "renderer": self.renderer,
            "version": self.version,
            "fbo_supported": self.fbo_supported,
            "backend": self.backend,
            "mem_events": self.mem_events,
        }


def probe_opengl(width: int = 256, height: int = 256) -> GlCapability:
    """尝试创建离屏 GL 上下文并验证 FBO 完整性；失败则回退软件实现。"""
    try:
        from PyQt5 import QtGui
        from PyQt5.QtGui import QOffscreenSurface, QOpenGLContext
    except Exception as exc:  # pragma: no cover - 环境相关
        return GlCapability(False, mem_events=[{
            "code": E_MEM, "reason": "pyqt5_unavailable",
            "detail": str(exc), "fallback": "software_rasterizer"}])

    app = QtGui.QGuiApplication.instance()
    owns_app = app is None
    if owns_app:
        app = QtGui.QGuiApplication([])
    try:
        surface = QOffscreenSurface()
        surface.create()
        ctx = QOpenGLContext()
        if not ctx.create() or not ctx.makeCurrent(surface):
            return GlCapability(False, mem_events=[{
                "code": E_MEM, "reason": "gl_context_create_failed",
                "fallback": "software_rasterizer"}])
        from OpenGL import GL
        renderer = _gs(GL.GL_RENDERER)
        version = _gs(GL.GL_VERSION)
        fbo = GL.glGenFramebuffers(1)
        GL.glBindFramebuffer(GL.GL_FRAMEBUFFER, fbo)
        rbo = GL.glGenRenderbuffers(1)
        GL.glBindRenderbuffer(GL.GL_RENDERBUFFER, rbo)
        GL.glRenderbufferStorage(GL.GL_RENDERBUFFER,
                                 GL.GL_RGBA8, width, height)
        GL.glFramebufferRenderbuffer(GL.GL_FRAMEBUFFER,
                                     GL.GL_COLOR_ATTACHMENT0,
                                     GL.GL_RENDERBUFFER, rbo)
        status = GL.glCheckFramebufferStatus(GL.GL_FRAMEBUFFER)
        ok = status == GL.GL_FRAMEBUFFER_COMPLETE
        events: List[Dict[str, Any]] = []
        if not ok:
            events.append({"code": E_MEM,
                           "reason": "fbo_unsupported_format",
                           "status": int(status),
                           "fallback": "software_rasterizer"})
        is_software = "llvmpipe" in renderer.lower() or "soft" in renderer.lower()
        backend = "software-gl" if is_software else "hardware-gl"
        return GlCapability(True, renderer=renderer, version=version,
                            fbo_supported=ok, backend=backend,
                            mem_events=events)
    except Exception as exc:
        return GlCapability(False, mem_events=[{
            "code": E_MEM, "reason": "gl_probe_exception",
            "detail": str(exc), "fallback": "software_rasterizer"}])


def _gs(name: int) -> str:
    from OpenGL import GL
    val = GL.glGetString(name)
    return val.decode() if isinstance(val, bytes) else str(val)
