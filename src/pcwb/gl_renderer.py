"""Minimal OpenGL 3.3 point renderer (import is safe without a context)."""

from __future__ import annotations

import ctypes
from collections.abc import Iterable
from dataclasses import dataclass, field

import numpy as np

from .math3d import matrix_for_uniform
from .model import PointCloud

_POINT_VERTEX_SHADER = """
#version 330 core
layout(location = 0) in vec3 aPos;
layout(location = 1) in vec3 aColor;

uniform mat4 uViewProj;
uniform float uPointSize;
uniform vec3 uOverrideColor;
uniform float uOverrideMix;

out vec3 vColor;

void main() {
    gl_Position = uViewProj * vec4(aPos, 1.0);
    gl_PointSize = uPointSize;
    vColor = mix(aColor, uOverrideColor, uOverrideMix);
}
"""

_POINT_FRAGMENT_SHADER = """
#version 330 core
in vec3 vColor;
out vec4 FragColor;

void main() {
    vec2 centered = gl_PointCoord * 2.0 - 1.0;
    if (dot(centered, centered) > 1.0) {
        discard;
    }
    FragColor = vec4(vColor, 1.0);
}
"""

_OVERLAY_VERTEX_SHADER = """
#version 330 core
layout(location = 0) in vec2 aPos;

void main() {
    gl_Position = vec4(aPos, 0.0, 1.0);
}
"""

_OVERLAY_FRAGMENT_SHADER = """
#version 330 core
uniform vec4 uColor;
out vec4 FragColor;

void main() {
    FragColor = uColor;
}
"""


def _require_gl():
    from OpenGL import GL

    return GL


def _compile(GLenum, source: str, shader_type: int) -> int:
    shader = GLenum.glCreateShader(shader_type)
    GLenum.glShaderSource(shader, source)
    GLenum.glCompileShader(shader)
    if not GLenum.glGetShaderiv(shader, GLenum.GL_COMPILE_STATUS):
        log = GLenum.glGetShaderInfoLog(shader)
        if isinstance(log, bytes):
            log = log.decode("utf-8", "replace")
        raise RuntimeError(f"OpenGL shader compilation failed: {log}")
    return shader


def build_program(vertex_source: str, fragment_source: str) -> int:
    GL = _require_gl()
    vertex = _compile(GL, vertex_source, GL.GL_VERTEX_SHADER)
    fragment = _compile(GL, fragment_source, GL.GL_FRAGMENT_SHADER)
    program = GL.glCreateProgram()
    GL.glAttachShader(program, vertex)
    GL.glAttachShader(program, fragment)
    GL.glLinkProgram(program)
    GL.glDeleteShader(vertex)
    GL.glDeleteShader(fragment)
    if not GL.glGetProgramiv(program, GL.GL_LINK_STATUS):
        log = GL.glGetProgramInfoLog(program)
        if isinstance(log, bytes):
            log = log.decode("utf-8", "replace")
        raise RuntimeError(f"OpenGL program link failed: {log}")
    return program


@dataclass
class PointBatch:
    program: int
    vao: int = 0
    vbo: int = 0
    ebo: int = 0
    count: int = 0
    content_hash: str | None = None
    _index_key: tuple[int, ...] | None = field(default=None, repr=False)

    def __post_init__(self) -> None:
        GL = _require_gl()
        self.vao = GL.glGenVertexArrays(1)
        self.vbo = GL.glGenBuffers(1)
        self.ebo = GL.glGenBuffers(1)
        GL.glBindVertexArray(self.vao)
        GL.glBindBuffer(GL.GL_ARRAY_BUFFER, self.vbo)
        GL.glBindBuffer(GL.GL_ELEMENT_ARRAY_BUFFER, self.ebo)
        stride = 6 * 4
        GL.glEnableVertexAttribArray(0)
        GL.glVertexAttribPointer(0, 3, GL.GL_FLOAT, GL.GL_FALSE, stride, None)
        GL.glEnableVertexAttribArray(1)
        GL.glVertexAttribPointer(
            1, 3, GL.GL_FLOAT, GL.GL_FALSE, stride, ctypes.c_void_p(3 * 4)
        )
        GL.glBindVertexArray(0)

    def update(self, cloud: PointCloud) -> None:
        if self.content_hash == cloud.content_hash:
            return
        GL = _require_gl()
        interleaved = np.empty((cloud.count, 6), dtype=np.float32)
        interleaved[:, :3] = cloud.xyz
        interleaved[:, 3:] = cloud.rgb.astype(np.float32) / 255.0
        GL.glBindBuffer(GL.GL_ARRAY_BUFFER, self.vbo)
        GL.glBufferData(
            GL.GL_ARRAY_BUFFER,
            interleaved.nbytes,
            np.ascontiguousarray(interleaved),
            GL.GL_STATIC_DRAW,
        )
        self.count = cloud.count
        self.content_hash = cloud.content_hash
        self._index_key = None

    def _upload_indices(self, indices: np.ndarray) -> None:
        GL = _require_gl()
        key = tuple(int(value) for value in indices)
        if self._index_key == key:
            return
        GL.glBindBuffer(GL.GL_ELEMENT_ARRAY_BUFFER, self.ebo)
        GL.glBufferData(
            GL.GL_ELEMENT_ARRAY_BUFFER,
            indices.nbytes,
            np.ascontiguousarray(indices, dtype=np.uint32),
            GL.GL_DYNAMIC_DRAW,
        )
        self._index_key = key

    def draw_all(
        self,
        view_projection: np.ndarray,
        *,
        point_size: float = 2.0,
        override: tuple[float, float, float] | None = None,
    ) -> None:
        if self.count == 0:
            return
        GL = _require_gl()
        self._set_uniforms(view_projection, point_size, override)
        GL.glBindVertexArray(self.vao)
        GL.glDrawArrays(GL.GL_POINTS, 0, self.count)
        GL.glBindVertexArray(0)

    def draw_indices(
        self,
        indices: Iterable[int] | np.ndarray,
        view_projection: np.ndarray,
        *,
        point_size: float = 4.0,
        override: tuple[float, float, float] | None = (1.0, 0.85, 0.1),
    ) -> None:
        array = np.asarray(tuple(indices), dtype=np.uint32)
        if array.size == 0:
            return
        GL = _require_gl()
        self._set_uniforms(view_projection, point_size, override)
        self._upload_indices(array)
        GL.glBindVertexArray(self.vao)
        GL.glDrawElements(GL.GL_POINTS, int(array.size), GL.GL_UNSIGNED_INT, None)
        GL.glBindVertexArray(0)

    def _set_uniforms(
        self,
        view_projection: np.ndarray,
        point_size: float,
        override: tuple[float, float, float] | None,
    ) -> None:
        GL = _require_gl()
        GL.glUseProgram(self.program)
        location = GL.glGetUniformLocation(self.program, "uViewProj")
        GL.glUniformMatrix4fv(
            location, 1, GL.GL_TRUE, matrix_for_uniform(view_projection)
        )
        GL.glUniform1f(GL.glGetUniformLocation(self.program, "uPointSize"), point_size)
        mix = 0.0 if override is None else 1.0
        color = (1.0, 1.0, 1.0) if override is None else override
        GL.glUniform3f(GL.glGetUniformLocation(self.program, "uOverrideColor"), *color)
        GL.glUniform1f(GL.glGetUniformLocation(self.program, "uOverrideMix"), mix)

    def destroy(self) -> None:
        GL = _require_gl()
        if self.ebo:
            GL.glDeleteBuffers(1, [self.ebo])
        if self.vbo:
            GL.glDeleteBuffers(1, [self.vbo])
        if self.vao:
            GL.glDeleteVertexArrays(1, [self.vao])
        self.ebo = self.vbo = self.vao = 0


class Overlay:
    def __init__(self) -> None:
        GL = _require_gl()
        self.program = build_program(_OVERLAY_VERTEX_SHADER, _OVERLAY_FRAGMENT_SHADER)
        self.vao = GL.glGenVertexArrays(1)
        self.vbo = GL.glGenBuffers(1)
        GL.glBindVertexArray(self.vao)
        GL.glBindBuffer(GL.GL_ARRAY_BUFFER, self.vbo)
        GL.glEnableVertexAttribArray(0)
        GL.glVertexAttribPointer(0, 2, GL.GL_FLOAT, GL.GL_FALSE, 0, None)
        GL.glBindVertexArray(0)

    def draw(
        self,
        points_ndc: Iterable[tuple[float, float]],
        *,
        color: tuple[float, float, float, float] = (1.0, 0.8, 0.1, 1.0),
        line_width: float = 1.5,
        mode: int | None = None,
    ) -> None:
        values = np.asarray(tuple(points_ndc), dtype=np.float32)
        if values.size == 0:
            return
        GL = _require_gl()
        primitive = GL.GL_LINE_LOOP if mode is None else mode
        GL.glDisable(GL.GL_DEPTH_TEST)
        GL.glUseProgram(self.program)
        GL.glUniform4f(GL.glGetUniformLocation(self.program, "uColor"), *color)
        GL.glLineWidth(line_width)
        GL.glBindVertexArray(self.vao)
        GL.glBindBuffer(GL.GL_ARRAY_BUFFER, self.vbo)
        GL.glBufferData(
            GL.GL_ARRAY_BUFFER,
            values.nbytes,
            np.ascontiguousarray(values),
            GL.GL_DYNAMIC_DRAW,
        )
        GL.glDrawArrays(primitive, 0, values.shape[0])
        GL.glBindVertexArray(0)
        GL.glEnable(GL.GL_DEPTH_TEST)

    def destroy(self) -> None:
        GL = _require_gl()
        if self.vbo:
            GL.glDeleteBuffers(1, [self.vbo])
        if self.vao:
            GL.glDeleteVertexArrays(1, [self.vao])
        GL.glDeleteProgram(self.program)
        self.vbo = self.vao = self.program = 0


class PointCloudGLRenderer:
    def __init__(self) -> None:
        _require_gl()
        self.program = build_program(_POINT_VERTEX_SHADER, _POINT_FRAGMENT_SHADER)
        self.current = PointBatch(self.program)
        self.ghost = PointBatch(self.program)
        self.overlay = Overlay()

    def begin_frame(
        self,
        width: int,
        height: int,
        *,
        background: tuple[float, float, float] = (0.035, 0.045, 0.06),
    ) -> None:
        GL = _require_gl()
        GL.glViewport(0, 0, max(1, width), max(1, height))
        GL.glClearColor(*background, 1.0)
        GL.glClear(GL.GL_COLOR_BUFFER_BIT | GL.GL_DEPTH_BUFFER_BIT)
        GL.glEnable(GL.GL_DEPTH_TEST)
        GL.glEnable(GL.GL_PROGRAM_POINT_SIZE)
        GL.glEnable(GL.GL_BLEND)
        GL.glBlendFunc(GL.GL_SRC_ALPHA, GL.GL_ONE_MINUS_SRC_ALPHA)

    def set_current(self, cloud: PointCloud) -> None:
        self.current.update(cloud)
        if self.ghost.content_hash not in {None, cloud.content_hash}:
            self.ghost.content_hash = None

    def set_ghost(self, cloud: PointCloud | None) -> None:
        if cloud is None:
            self.ghost.content_hash = None
            self.ghost.count = 0
        else:
            self.ghost.update(cloud)

    def draw(
        self,
        view_projection: np.ndarray,
        *,
        selected_indices: Iterable[int] = (),
        added_indices: Iterable[int] = (),
        changed_indices: Iterable[int] = (),
    ) -> None:
        self.current.draw_all(view_projection, point_size=2.0)
        self.ghost.draw_all(
            view_projection, point_size=2.0, override=(0.85, 0.12, 0.10)
        )
        self.current.draw_indices(
            selected_indices,
            view_projection,
            point_size=5.0,
            override=(1.0, 0.83, 0.08),
        )
        self.current.draw_indices(
            added_indices,
            view_projection,
            point_size=5.5,
            override=(0.15, 1.0, 0.45),
        )
        self.current.draw_indices(
            changed_indices,
            view_projection,
            point_size=5.5,
            override=(0.85, 0.3, 1.0),
        )

    def destroy(self) -> None:
        self.current.destroy()
        self.ghost.destroy()
        self.overlay.destroy()
        GL = _require_gl()
        GL.glDeleteProgram(self.program)
