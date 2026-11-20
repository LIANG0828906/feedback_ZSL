"""Headless OpenGL renderer on an EGL pbuffer (Mesa/llvmpipe compatible).

The EGL context is created through ctypes (PyOpenGL's EGL python wrapper has
call-signature quirks), while drawing uses the regular PyOpenGL bindings once
the context is current. A single VBO streams position+color point vertices;
selection and diff highlights are encoded into per-vertex colors, so every
frame renders one complete committed cloud snapshot atomically.
"""

from __future__ import annotations

import ctypes
import ctypes.util

import numpy as np

from ..cloud import PointCloud
from .camera import OrbitCamera
from .cpu_renderer import RenderResult

EGL_NONE = 0x3038
EGL_DEFAULT_DISPLAY = ctypes.c_void_p(0)
EGL_PBUFFER_BIT = 0x0001
EGL_OPENGL_BIT = 0x0008
EGL_SURFACE_TYPE = 0x3033
EGL_RENDERABLE_TYPE = 0x3040
EGL_RED_SIZE, EGL_GREEN_SIZE, EGL_BLUE_SIZE = 0x3024, 0x3023, 0x3022
EGL_DEPTH_SIZE = 0x3025
EGL_WIDTH, EGL_HEIGHT = 0x3057, 0x3056
EGL_OPENGL_API = 0x30A2
EGL_CONTEXT_MAJOR_VERSION = 0x3098
EGL_CONTEXT_MINOR_VERSION = 0x30FB


def _load_egl():
    path = ctypes.util.find_library("EGL")
    if not path:
        raise RuntimeError("libEGL not found")
    egl = ctypes.CDLL(path)
    EGLint = ctypes.c_int32
    egl.eglGetDisplay.restype = ctypes.c_void_p
    egl.eglGetDisplay.argtypes = [ctypes.c_void_p]
    egl.eglInitialize.restype = ctypes.c_bool
    egl.eglInitialize.argtypes = [
        ctypes.c_void_p, ctypes.POINTER(EGLint), ctypes.POINTER(EGLint)]
    egl.eglBindAPI.restype = ctypes.c_bool
    egl.eglBindAPI.argtypes = [EGLint]
    egl.eglChooseConfig.restype = ctypes.c_bool
    egl.eglChooseConfig.argtypes = [
        ctypes.c_void_p, ctypes.POINTER(EGLint),
        ctypes.POINTER(ctypes.c_void_p), EGLint, ctypes.POINTER(EGLint)]
    egl.eglCreatePbufferSurface.restype = ctypes.c_void_p
    egl.eglCreatePbufferSurface.argtypes = [
        ctypes.c_void_p, ctypes.c_void_p, ctypes.POINTER(EGLint)]
    egl.eglCreateContext.restype = ctypes.c_void_p
    egl.eglCreateContext.argtypes = [
        ctypes.c_void_p, ctypes.c_void_p, ctypes.c_void_p,
        ctypes.POINTER(EGLint)]
    egl.eglMakeCurrent.restype = ctypes.c_bool
    egl.eglMakeCurrent.argtypes = [ctypes.c_void_p] * 4
    egl.eglQueryString.restype = ctypes.c_char_p
    egl.eglQueryString.argtypes = [ctypes.c_void_p, EGLint]
    egl.eglGetError.restype = EGLint
    egl.eglGetError.argtypes = []
    return egl


VERTEX_SHADER = b"""
#version 330 core
layout(location = 0) in vec3 a_pos;
layout(location = 1) in vec3 a_color;
uniform mat4 u_vp;
out vec3 v_color;
void main() {
    gl_Position = u_vp * vec4(a_pos, 1.0);
    gl_PointSize = 4.0;
    v_color = a_color;
}
"""

FRAGMENT_SHADER = b"""
#version 330 core
in vec3 v_color;
out vec4 frag;
void main() {
    vec2 d = gl_PointCoord - vec2(0.5);
    if (dot(d, d) > 0.25) discard;
    frag = vec4(v_color, 1.0);
}
"""


class EGLRenderer:
    """OpenGL 3.3 core point renderer on a headless EGL pbuffer."""

    backend = "egl-opengl"

    def __init__(self, width: int = 640, height: int = 480,
                 background=(18 / 255, 22 / 255, 28 / 255)):
        self.width, self.height = int(width), int(height)
        self.background = tuple(float(v) for v in background)
        self._egl = _load_egl()
        self._create_context()
        self._init_gl()

    def _create_context(self) -> None:
        egl = self._egl
        EGLint = ctypes.c_int32
        self._dpy = egl.eglGetDisplay(EGL_DEFAULT_DISPLAY)
        if not self._dpy:
            raise RuntimeError("eglGetDisplay failed")
        major, minor = EGLint(), EGLint()
        if not egl.eglInitialize(
                self._dpy, ctypes.byref(major), ctypes.byref(minor)):
            raise RuntimeError("eglInitialize failed")
        if not egl.eglBindAPI(EGL_OPENGL_API):
            raise RuntimeError("eglBindAPI(OpenGL) failed")
        attribs = (EGLint * 13)(
            EGL_SURFACE_TYPE, EGL_PBUFFER_BIT,
            EGL_RENDERABLE_TYPE, EGL_OPENGL_BIT,
            EGL_RED_SIZE, 8, EGL_GREEN_SIZE, 8, EGL_BLUE_SIZE, 8,
            EGL_DEPTH_SIZE, 24, EGL_NONE)
        cfg = ctypes.c_void_p()
        count = EGLint()
        if not egl.eglChooseConfig(
                self._dpy, attribs, ctypes.byref(cfg), 1,
                ctypes.byref(count)) or not count.value:
            raise RuntimeError("eglChooseConfig found no config")
        pb = (EGLint * 5)(EGL_WIDTH, self.width, EGL_HEIGHT,
                          self.height, EGL_NONE)
        self._surf = egl.eglCreatePbufferSurface(self._dpy, cfg, pb)
        if not self._surf:
            raise RuntimeError("eglCreatePbufferSurface failed")
        ctx_attribs = (EGLint * 5)(
            EGL_CONTEXT_MAJOR_VERSION, 3,
            EGL_CONTEXT_MINOR_VERSION, 3, EGL_NONE)
        self._ctx = egl.eglCreateContext(
            self._dpy, cfg, None, ctx_attribs)
        if not self._ctx:
            raise RuntimeError(
                f"eglCreateContext failed: {hex(egl.eglGetError())}")
        if not egl.eglMakeCurrent(
                self._dpy, self._surf, self._surf, self._ctx):
            raise RuntimeError("eglMakeCurrent failed")

    def _init_gl(self) -> None:
        from OpenGL import GL
        self.gl = GL
        self._program = self._compile_program()
        self._u_vp = GL.glGetUniformLocation(self._program, b"u_vp")
        self._vao = GL.glGenVertexArrays(1)
        self._vbo = GL.glGenBuffers(1)
        GL.glBindVertexArray(self._vao)
        GL.glBindBuffer(GL.GL_ARRAY_BUFFER, self._vbo)
        stride = 6 * 4
        GL.glEnableVertexAttribArray(0)
        GL.glVertexAttribPointer(0, 3, GL.GL_FLOAT, GL.GL_FALSE, stride,
                                 ctypes.c_void_p(0))
        GL.glEnableVertexAttribArray(1)
        GL.glVertexAttribPointer(1, 3, GL.GL_FLOAT, GL.GL_FALSE, stride,
                                 ctypes.c_void_p(3 * 4))
        GL.glBindVertexArray(0)
        GL.glEnable(GL.GL_DEPTH_TEST)
        GL.glClearColor(*self.background, 1.0)

    def _compile_shader(self, kind: int, source: bytes) -> int:
        GL = self.gl
        shader = GL.glCreateShader(kind)
        GL.glShaderSource(shader, source)
        GL.glCompileShader(shader)
        if not GL.glGetShaderiv(shader, GL.GL_COMPILE_STATUS):
            log = GL.glGetShaderInfoLog(shader)
            raise RuntimeError(f"shader compile failed: {log}")
        return shader

    def _compile_program(self) -> int:
        GL = self.gl
        vs = self._compile_shader(GL.GL_VERTEX_SHADER, VERTEX_SHADER)
        fs = self._compile_shader(GL.GL_FRAGMENT_SHADER, FRAGMENT_SHADER)
        program = GL.glCreateProgram()
        GL.glAttachShader(program, vs)
        GL.glAttachShader(program, fs)
        GL.glLinkProgram(program)
        if not GL.glGetProgramiv(program, GL.GL_LINK_STATUS):
            log = GL.glGetProgramInfoLog(program)
            raise RuntimeError(f"shader link failed: {log}")
        GL.glDeleteShader(vs)
        GL.glDeleteShader(fs)
        return program

    def render(self, cloud: PointCloud, camera: OrbitCamera,
               highlight_indices: np.ndarray | None = None,
               highlight_color=(1.0, 0.84, 0.0),
               diff_added: np.ndarray | None = None,
               diff_deleted: np.ndarray | None = None,
               diff_color_changed: np.ndarray | None = None
               ) -> RenderResult:
        GL = self.gl
        n = cloud.count
        vertices = np.zeros((n, 6), dtype=np.float32)
        vertices[:, :3] = cloud.xyz.astype(np.float32)
        vertices[:, 3:] = (cloud.rgb.astype(np.float32) / 255.0)
        if highlight_indices is not None and highlight_indices.size:
            vertices[highlight_indices, 3:] = np.array(
                highlight_color, dtype=np.float32)
        if diff_added is not None and diff_added.size:
            vertices[diff_added, 3:] = (0.0, 0.9, 0.46)
        if diff_color_changed is not None and diff_color_changed.size:
            vertices[diff_color_changed, 3:] = (0.31, 0.63, 1.0)

        GL.glViewport(0, 0, self.width, self.height)
        GL.glClear(GL.GL_COLOR_BUFFER_BIT | GL.GL_DEPTH_BUFFER_BIT)
        if n:
            GL.glBindBuffer(GL.GL_ARRAY_BUFFER, self._vbo)
            GL.glBufferData(GL.GL_ARRAY_BUFFER, vertices.nbytes,
                            vertices, GL.GL_DYNAMIC_DRAW)
            GL.glUseProgram(self._program)
            vp = camera.view_proj(self.width, self.height).astype(np.float32)
            GL.glUniformMatrix4fv(self._u_vp, 1, GL.GL_FALSE, vp)
            GL.glBindVertexArray(self._vao)
            GL.glDrawArrays(GL.GL_POINTS, 0, n)
            GL.glBindVertexArray(0)
        GL.glFinish()

        buf = np.empty((self.height, self.width, 3), dtype=np.uint8)
        GL.glReadPixels(0, 0, self.width, self.height,
                        GL.GL_RGB, GL.GL_UNSIGNED_BYTE, buf)
        pixels = np.ascontiguousarray(buf[::-1])  # bottom-up -> top-down
        return RenderResult(self.width, self.height, pixels, self.backend)

    def close(self) -> None:
        try:
            self.gl.glDeleteBuffers(1, [self._vbo])
            self.gl.glDeleteVertexArrays(1, [self._vao])
            self.gl.glDeleteProgram(self._program)
        finally:
            egl = self._egl
            egl.eglMakeCurrent(self._dpy, None, None, None)
            egl.eglDestroySurface(self._dpy, self._surf)
            egl.eglDestroyContext(self._dpy, self._ctx)
