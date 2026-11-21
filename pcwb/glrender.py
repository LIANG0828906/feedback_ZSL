"""离屏渲染一帧显示集到 PPM 图像（确定性字节输出，用于验收留痕）。

GL 函数指针通过 Qt 的 QOpenGLContext.getProcAddress 解析并用 ctypes
调用，绕开 PyOpenGL 对当前上下文的跟踪（Qt 离屏上下文对 PyOpenGL
不可见），保证在软件渲染后端（llvmpipe）上同样可用。
"""

from __future__ import annotations

import ctypes
import os

import numpy as np

GL_POINTS = 0x0000
GL_COLOR_BUFFER_BIT = 0x4000
GL_DEPTH_BUFFER_BIT = 0x0100
GL_PROJECTION = 0x1701
GL_MODELVIEW = 0x1700
GL_VERTEX_ARRAY = 0x8074
GL_COLOR_ARRAY = 0x8076
GL_FLOAT = 0x1406
GL_UNSIGNED_BYTE = 0x1401
GL_RGB = 0x1907


class _GL:
    """从当前 QOpenGLContext 解析固定管线函数指针。"""

    def __init__(self, ctx):
        self._refs = []

        def resolve(name, restype, *argtypes):
            addr = int(ctx.getProcAddress(name.encode("ascii")) or 0)
            if not addr:
                raise RuntimeError(f"GL 函数不可用: {name}")
            proto = ctypes.CFUNCTYPE(restype, *argtypes)
            fn = proto(addr)
            self._refs.append(fn)
            return fn

        c_int, c_float, c_double = ctypes.c_int, ctypes.c_float, ctypes.c_double
        c_uint, c_void_p = ctypes.c_uint, ctypes.c_void_p
        self.viewport = resolve("glViewport", None,
                                c_int, c_int, c_int, c_int)
        self.clear_color = resolve("glClearColor", None,
                                   c_float, c_float, c_float, c_float)
        self.clear = resolve("glClear", None, c_uint)
        self.matrix_mode = resolve("glMatrixMode", None, c_uint)
        self.load_identity = resolve("glLoadIdentity", None)
        self.ortho = resolve("glOrtho", None, c_double, c_double, c_double,
                             c_double, c_double, c_double)
        self.point_size = resolve("glPointSize", None, c_float)
        self.enable_client_state = resolve("glEnableClientState", None, c_uint)
        self.disable_client_state = resolve("glDisableClientState", None,
                                            c_uint)
        self.vertex_pointer = resolve("glVertexPointer", None, c_int, c_uint,
                                      c_int, c_void_p)
        self.color_pointer = resolve("glColorPointer", None, c_int, c_uint,
                                     c_int, c_void_p)
        self.draw_arrays = resolve("glDrawArrays", None, c_uint, c_int, c_int)
        self.finish = resolve("glFinish", None)
        self.read_pixels = resolve("glReadPixels", None, c_int, c_int, c_int,
                                   c_int, c_uint, c_uint, c_void_p)


def render_points_ppm(points: np.ndarray, colors: np.ndarray, path: str,
                      width: int = 512, height: int = 512,
                      viewpoint=None) -> str:
    """把显示集点渲染为俯视图 PPM。返回文件路径。"""
    os.environ.setdefault("QT_QPA_PLATFORM", "offscreen")
    from PyQt5.QtGui import (QGuiApplication, QOffscreenSurface,
                             QOpenGLContext, QOpenGLFramebufferObject,
                             QOpenGLFramebufferObjectFormat, QSurfaceFormat)

    app = QGuiApplication.instance() or QGuiApplication([])
    fmt = QSurfaceFormat()
    fmt.setRenderableType(QSurfaceFormat.OpenGL)
    ctx = QOpenGLContext()
    ctx.setFormat(fmt)
    if not ctx.create():
        raise RuntimeError("无法创建 OpenGL 离屏上下文")
    surface = QOffscreenSurface()
    surface.setFormat(fmt)
    surface.create()
    ctx.makeCurrent(surface)
    gl = _GL(ctx)

    fbo_fmt = QOpenGLFramebufferObjectFormat()
    fbo = QOpenGLFramebufferObject(width, height, fbo_fmt)
    fbo.bind()
    gl.viewport(0, 0, width, height)
    gl.clear_color(0.05, 0.05, 0.08, 1.0)
    gl.clear(GL_COLOR_BUFFER_BIT | GL_DEPTH_BUFFER_BIT)

    pts = np.ascontiguousarray(points, dtype=np.float32)
    cols = np.ascontiguousarray(colors, dtype=np.uint8)
    if pts.shape[0]:
        pmin = pts.min(axis=0)
        pmax = pts.max(axis=0)
    else:
        pmin = np.zeros(3, np.float32)
        pmax = np.ones(3, np.float32)
    gl.matrix_mode(GL_PROJECTION)
    gl.load_identity()
    gl.ortho(float(pmin[0]), float(pmax[0]), float(pmin[1]), float(pmax[1]),
             -200.0, 200.0)
    gl.matrix_mode(GL_MODELVIEW)
    gl.load_identity()
    gl.point_size(1.0)
    gl.enable_client_state(GL_VERTEX_ARRAY)
    gl.enable_client_state(GL_COLOR_ARRAY)
    gl.vertex_pointer(3, GL_FLOAT, 0, pts.ctypes.data)
    gl.color_pointer(3, GL_UNSIGNED_BYTE, 0, cols.ctypes.data)
    gl.draw_arrays(GL_POINTS, 0, int(pts.shape[0]))
    gl.disable_client_state(GL_VERTEX_ARRAY)
    gl.disable_client_state(GL_COLOR_ARRAY)
    gl.finish()

    img = np.empty((height, width, 3), dtype=np.uint8)
    gl.read_pixels(0, 0, width, height, GL_RGB, GL_UNSIGNED_BYTE,
                   img.ctypes.data)
    img = img[::-1]
    fbo.release()
    ctx.doneCurrent()

    header = f"P6\n{width} {height}\n255\n".encode("ascii")
    with open(path, "wb") as fh:
        fh.write(header + img.tobytes())
    return path
