from __future__ import annotations

import math
from dataclasses import dataclass
from typing import Iterable, Protocol

import numpy as np

from .artifacts import (
    Artifact,
    decode_mesh,
    decode_normals,
    decode_preview,
    encode_preview,
)
from .hashing import sha256_bytes
from .model import SurfaceAssembly, SurfacePatch

PREVIEW_TILE_SIZE = 64
BACKGROUND_RGBA = (22, 26, 32, 255)


class PreviewRenderer(Protocol):
    backend_name: str

    def render_tile(
        self, patch: SurfacePatch, mesh_artifact: Artifact, normals_artifact: Artifact
    ) -> bytes:
        ...


def _normalized_projection(
    vertices: np.ndarray, width: int, height: int, margin: float = 0.06
) -> tuple[np.ndarray, np.ndarray]:
    points = np.asarray(vertices, dtype=np.float64)
    x = points[:, :, 0]
    y = points[:, :, 1]
    z = points[:, :, 2]
    min_x, max_x = float(x.min()), float(x.max())
    min_y, max_y = float(y.min()), float(y.max())
    span_x = max(max_x - min_x, 1e-9)
    span_y = max(max_y - min_y, 1e-9)
    usable = 1.0 - 2.0 * margin
    scale = min(usable * (width - 1) / span_x, usable * (height - 1) / span_y)
    center_x = (min_x + max_x) * 0.5
    center_y = (min_y + max_y) * 0.5
    screen_x = (x - center_x) * scale + (width - 1) * 0.5
    screen_y = (height - 1) * 0.5 - (y - center_y) * scale
    min_z, max_z = float(z.min()), float(z.max())
    depth = (z - min_z) / max(max_z - min_z, 1e-9)
    return np.stack([screen_x, screen_y], axis=2), depth


class SoftwarePreviewRenderer:
    """A small deterministic triangle rasterizer used as the default backend."""

    backend_name = "software"

    def __init__(self, tile_size: int = PREVIEW_TILE_SIZE):
        self.tile_size = tile_size

    def render_tile(
        self, patch: SurfacePatch, mesh_artifact: Artifact, normals_artifact: Artifact
    ) -> bytes:
        del patch
        vertices, _, _ = decode_mesh(mesh_artifact.data)
        normals, _, _ = decode_normals(normals_artifact.data)
        rgba = self._rasterize(
            np.asarray(vertices, dtype=np.float64),
            np.asarray(normals, dtype=np.float64),
            self.tile_size,
            self.tile_size,
        )
        return encode_preview(self.tile_size, self.tile_size, rgba)

    @classmethod
    def _rasterize(
        cls,
        vertices: np.ndarray,
        normals: np.ndarray,
        width: int,
        height: int,
    ) -> bytes:
        rows, cols = vertices.shape[:2]
        row_stride = max(1, math.ceil((rows - 1) / 16))
        col_stride = max(1, math.ceil((cols - 1) / 16))
        row_indices = sorted(set(range(0, rows, row_stride)) | {rows - 1})
        col_indices = sorted(set(range(0, cols, col_stride)) | {cols - 1})
        sampled_vertices = vertices[np.ix_(row_indices, col_indices)]
        sampled_normals = normals[np.ix_(row_indices, col_indices)]
        projected, depth = _normalized_projection(
            sampled_vertices, width, height
        )

        image = np.empty((height, width, 4), dtype=np.uint8)
        image[...] = np.asarray(BACKGROUND_RGBA, dtype=np.uint8)
        z_buffer = np.full((height, width), np.inf, dtype=np.float64)
        light = np.asarray([0.28, 0.42, 0.86], dtype=np.float64)
        light /= np.linalg.norm(light)
        base_color = np.asarray([92.0, 157.0, 216.0], dtype=np.float64)

        sampled_rows, sampled_cols = projected.shape[:2]
        for row in range(sampled_rows - 1):
            for col in range(sampled_cols - 1):
                for triangle in ((0, 1, 2), (1, 3, 2)):
                    if triangle == (0, 1, 2):
                        indices = (
                            (row, col),
                            (row, col + 1),
                            (row + 1, col),
                        )
                    else:
                        indices = (
                            (row, col + 1),
                            (row + 1, col + 1),
                            (row + 1, col),
                        )
                    cls._raster_triangle(
                        image,
                        z_buffer,
                        projected,
                        depth,
                        sampled_normals,
                        indices,
                        light,
                        base_color,
                    )
        return image.tobytes(order="C")

    @staticmethod
    def _raster_triangle(
        image: np.ndarray,
        z_buffer: np.ndarray,
        projected: np.ndarray,
        depth: np.ndarray,
        normals: np.ndarray,
        indices: tuple[tuple[int, int], ...],
        light: np.ndarray,
        base_color: np.ndarray,
    ) -> None:
        (r0, c0), (r1, c1), (r2, c2) = indices
        p0 = projected[r0, c0]
        p1 = projected[r1, c1]
        p2 = projected[r2, c2]
        area = (p1[0] - p0[0]) * (p2[1] - p0[1]) - (p1[1] - p0[1]) * (
            p2[0] - p0[0]
        )
        if abs(area) < 1e-12:
            return

        height, width = z_buffer.shape
        min_x = max(0, int(math.floor(min(p0[0], p1[0], p2[0]))))
        max_x = min(width - 1, int(math.ceil(max(p0[0], p1[0], p2[0]))))
        min_y = max(0, int(math.floor(min(p0[1], p1[1], p2[1]))))
        max_y = min(height - 1, int(math.ceil(max(p0[1], p1[1], p2[1]))))
        if min_x > max_x or min_y > max_y:
            return

        ys, xs = np.mgrid[min_y : max_y + 1, min_x : max_x + 1]
        denominator = area
        weight0 = (
            (p1[0] - xs) * (p2[1] - ys) - (p1[1] - ys) * (p2[0] - xs)
        ) / denominator
        weight1 = (
            (p2[0] - xs) * (p0[1] - ys) - (p2[1] - ys) * (p0[0] - xs)
        ) / denominator
        weight2 = 1.0 - weight0 - weight1
        inside = (weight0 >= -1e-9) & (weight1 >= -1e-9) & (weight2 >= -1e-9)
        if not np.any(inside):
            return

        interpolated_depth = (
            weight0 * depth[r0, c0]
            + weight1 * depth[r1, c1]
            + weight2 * depth[r2, c2]
        )
        visible = inside & (interpolated_depth < z_buffer[ys, xs])
        if not np.any(visible):
            return

        interpolated_normal = (
            weight0[..., None] * normals[r0, c0]
            + weight1[..., None] * normals[r1, c1]
            + weight2[..., None] * normals[r2, c2]
        )
        lengths = np.linalg.norm(interpolated_normal, axis=2, keepdims=True)
        interpolated_normal = np.divide(
            interpolated_normal,
            lengths,
            out=np.zeros_like(interpolated_normal),
            where=lengths > 1e-12,
        )
        intensity = 0.12 + 0.88 * np.clip(
            interpolated_normal @ light, 0.0, 1.0
        )
        colors = np.clip(
            np.rint(intensity[..., None] * base_color), 0, 255
        ).astype(np.uint8)

        target_y = ys[visible]
        target_x = xs[visible]
        z_buffer[target_y, target_x] = interpolated_depth[visible]
        image[target_y, target_x, :3] = colors[visible]
        image[target_y, target_x, 3] = 255


class OpenGLOffscreenPreviewRenderer:
    """Optional EGL surfaceless renderer using PyOpenGL.

    The software backend remains the deterministic baseline.  This backend is
    useful for validating that the same immutable mesh/normal artifacts can be
    drawn in an actual offscreen OpenGL framebuffer without a display server.
    """

    backend_name = "opengl"

    def __init__(self, tile_size: int = PREVIEW_TILE_SIZE):
        self.tile_size = tile_size

    @classmethod
    def available(cls) -> bool:
        try:
            context = _EGLSurfacelessContext()
            with context:
                from OpenGL import GL

                return bool(GL.glGetString(GL.GL_VERSION))
        except Exception:
            return False

    def render_tile(
        self, patch: SurfacePatch, mesh_artifact: Artifact, normals_artifact: Artifact
    ) -> bytes:
        del patch
        vertices, _, _ = decode_mesh(mesh_artifact.data)
        normals, _, _ = decode_normals(normals_artifact.data)
        rgba = self._render(
            np.asarray(vertices, dtype=np.float64),
            np.asarray(normals, dtype=np.float64),
            self.tile_size,
        )
        return encode_preview(self.tile_size, self.tile_size, rgba)

    @staticmethod
    def _render(vertices: np.ndarray, normals: np.ndarray, size: int) -> bytes:
        with _EGLSurfacelessContext():
            from OpenGL import GL

            framebuffer = GL.glGenFramebuffers(1)
            color = GL.glGenTextures(1)
            depth = GL.glGenRenderbuffers(1)
            try:
                GL.glBindTexture(GL.GL_TEXTURE_2D, color)
                GL.glTexImage2D(
                    GL.GL_TEXTURE_2D,
                    0,
                    GL.GL_RGBA8,
                    size,
                    size,
                    0,
                    GL.GL_RGBA,
                    GL.GL_UNSIGNED_BYTE,
                    None,
                )
                GL.glTexParameteri(
                    GL.GL_TEXTURE_2D, GL.GL_TEXTURE_MIN_FILTER, GL.GL_NEAREST
                )
                GL.glTexParameteri(
                    GL.GL_TEXTURE_2D, GL.GL_TEXTURE_MAG_FILTER, GL.GL_NEAREST
                )
                GL.glBindRenderbuffer(GL.GL_RENDERBUFFER, depth)
                GL.glRenderbufferStorage(
                    GL.GL_RENDERBUFFER, GL.GL_DEPTH_COMPONENT24, size, size
                )
                GL.glBindFramebuffer(GL.GL_FRAMEBUFFER, framebuffer)
                GL.glFramebufferTexture2D(
                    GL.GL_FRAMEBUFFER,
                    GL.GL_COLOR_ATTACHMENT0,
                    GL.GL_TEXTURE_2D,
                    color,
                    0,
                )
                GL.glFramebufferRenderbuffer(
                    GL.GL_FRAMEBUFFER,
                    GL.GL_DEPTH_ATTACHMENT,
                    GL.GL_RENDERBUFFER,
                    depth,
                )
                if (
                    GL.glCheckFramebufferStatus(GL.GL_FRAMEBUFFER)
                    != GL.GL_FRAMEBUFFER_COMPLETE
                ):
                    raise RuntimeError("offscreen framebuffer is incomplete")

                GL.glViewport(0, 0, size, size)
                GL.glClearColor(22 / 255, 26 / 255, 32 / 255, 1.0)
                GL.glClear(GL.GL_COLOR_BUFFER_BIT | GL.GL_DEPTH_BUFFER_BIT)
                GL.glEnable(GL.GL_DEPTH_TEST)
                GL.glEnable(GL.GL_LIGHTING)
                GL.glEnable(GL.GL_LIGHT0)
                GL.glEnable(GL.GL_COLOR_MATERIAL)
                GL.glColorMaterial(GL.GL_FRONT_AND_BACK, GL.GL_AMBIENT_AND_DIFFUSE)
                GL.glEnable(GL.GL_NORMALIZE)
                GL.glLightfv(
                    GL.GL_LIGHT0,
                    GL.GL_POSITION,
                    (0.3, 0.5, 1.0, 0.0),
                )
                GL.glLightfv(
                    GL.GL_LIGHT0, GL.GL_DIFFUSE, (0.85, 0.9, 1.0, 1.0)
                )
                GL.glLightfv(
                    GL.GL_LIGHT0, GL.GL_AMBIENT, (0.2, 0.22, 0.25, 1.0)
                )
                GL.glMatrixMode(GL.GL_PROJECTION)
                GL.glLoadIdentity()
                GL.glOrtho(-1.1, 1.1, -1.1, 1.1, -2.0, 2.0)
                GL.glMatrixMode(GL.GL_MODELVIEW)
                GL.glLoadIdentity()
                GL.glColor3f(0.36, 0.62, 0.85)

                projected, depth = _normalized_projection(vertices, size, size)
                rows, cols = projected.shape[:2]
                row_stride = max(1, math.ceil((rows - 1) / 16))
                col_stride = max(1, math.ceil((cols - 1) / 16))
                row_indices = sorted(set(range(0, rows, row_stride)) | {rows - 1})
                col_indices = sorted(set(range(0, cols, col_stride)) | {cols - 1})
                sampled = projected[np.ix_(row_indices, col_indices)]
                sampled_normals = normals[np.ix_(row_indices, col_indices)]
                sampled_depth = depth[np.ix_(row_indices, col_indices)]

                GL.glBegin(GL.GL_TRIANGLES)
                for row in range(sampled.shape[0] - 1):
                    for col in range(sampled.shape[1] - 1):
                        cells = (
                            ((row, col), (row, col + 1), (row + 1, col)),
                            (
                                (row, col + 1),
                                (row + 1, col + 1),
                                (row + 1, col),
                            ),
                        )
                        for triangle in cells:
                            for rr, cc in triangle:
                                x = sampled[rr, cc, 0] / (size - 1) * 2.2 - 1.1
                                y = 1.1
                                - sampled[rr, cc, 1] / (size - 1) * 2.2
                                z = sampled_depth[rr, cc] * 2.0 - 1.0
                                normal = sampled_normals[rr, cc]
                                GL.glNormal3f(*normal)
                                GL.glVertex3f(float(x), float(y), float(z))
                GL.glEnd()
                GL.glFinish()
                GL.glReadBuffer(GL.GL_COLOR_ATTACHMENT0)
                pixels = GL.glReadPixels(
                    0, 0, size, size, GL.GL_RGBA, GL.GL_UNSIGNED_BYTE
                )
                return bytes(pixels)
            finally:
                GL.glBindFramebuffer(GL.GL_FRAMEBUFFER, 0)
                GL.glDeleteRenderbuffers(
                    1, np.asarray([depth], dtype=np.uint32)
                )
                GL.glDeleteTextures(np.asarray([color], dtype=np.uint32))
                GL.glDeleteFramebuffers(
                    1, np.asarray([framebuffer], dtype=np.uint32)
                )


class _EGLSurfacelessContext:
    EGL_PLATFORM_SURFACELESS_MESA = 0x31DD

    def __init__(self) -> None:
        self._display = None
        self._context = None

    def __enter__(self):
        from OpenGL import EGL

        display = EGL.eglGetPlatformDisplayEXT(
            self.EGL_PLATFORM_SURFACELESS_MESA, EGL.EGL_NO_DISPLAY, None
        )
        if display == EGL.EGL_NO_DISPLAY:
            raise RuntimeError("EGL surfaceless display is unavailable")
        major = EGL.EGLint()
        minor = EGL.EGLint()
        if not EGL.eglInitialize(display, major, minor):
            raise RuntimeError("eglInitialize failed")
        if not EGL.eglBindAPI(EGL.EGL_OPENGL_API):
            raise RuntimeError("eglBindAPI(OpenGL) failed")
        attributes = (EGL.EGLint * 3)(
            EGL.EGL_RENDERABLE_TYPE, EGL.EGL_OPENGL_BIT, EGL.EGL_NONE
        )
        config = EGL.EGLConfig()
        count = EGL.EGLint()
        if not EGL.eglChooseConfig(display, attributes, config, 1, count):
            raise RuntimeError("eglChooseConfig failed")
        context_attributes = (EGL.EGLint * 3)(
            EGL.EGL_CONTEXT_MAJOR_VERSION, 3, EGL.EGL_NONE
        )
        context = EGL.eglCreateContext(
            display, config, EGL.EGL_NO_CONTEXT, context_attributes
        )
        if context == EGL.EGL_NO_CONTEXT:
            raise RuntimeError("eglCreateContext failed")
        if not EGL.eglMakeCurrent(
            display, EGL.EGL_NO_SURFACE, EGL.EGL_NO_SURFACE, context
        ):
            raise RuntimeError("eglMakeCurrent failed")
        self._display = display
        self._context = context
        return self

    def __exit__(self, exc_type, exc, traceback) -> None:
        from OpenGL import EGL

        if self._display is not None:
            EGL.eglMakeCurrent(
                self._display,
                EGL.EGL_NO_SURFACE,
                EGL.EGL_NO_SURFACE,
                EGL.EGL_NO_CONTEXT,
            )
            if self._context is not None:
                EGL.eglDestroyContext(self._display, self._context)
            EGL.eglTerminate(self._display)


@dataclass(frozen=True)
class ScreenPreview:
    width: int
    height: int
    rgba: bytes
    content_hash: str
    dirty_rects: tuple[tuple[int, int, int, int], ...]


def preview_artifacts(
    artifacts: Iterable[Artifact],
) -> dict[str, Artifact]:
    return {
        artifact.patch_id: artifact
        for artifact in artifacts
        if artifact.kind == "preview"
    }


def preview_layout(assembly: SurfaceAssembly) -> tuple[int, int, int]:
    count = len(assembly.patches)
    columns = max(1, math.ceil(math.sqrt(count)))
    rows = max(1, math.ceil(count / columns))
    return columns, rows, PREVIEW_TILE_SIZE


def compose_screen_preview(
    assembly: SurfaceAssembly,
    artifacts: Iterable[Artifact],
    previous: ScreenPreview | None = None,
    dirty_patch_ids: set[str] | None = None,
) -> ScreenPreview:
    columns, rows, tile_size = preview_layout(assembly)
    width = columns * tile_size
    height = rows * tile_size
    tiles = preview_artifacts(artifacts)

    if previous is not None and (
        previous.width != width or previous.height != height
    ):
        previous = None
    if previous is None:
        canvas = bytearray(bytes(BACKGROUND_RGBA) * (width * height))
    else:
        canvas = bytearray(previous.rgba)

    dirty_rects: list[tuple[int, int, int, int]] = []
    ordered_patches = sorted(assembly.patches, key=lambda patch: patch.block_id)
    for index, patch in enumerate(ordered_patches):
        tile_artifact = tiles.get(patch.patch_id)
        if tile_artifact is None:
            raise ValueError(f"missing preview artifact for {patch.patch_id}")
        if dirty_patch_ids is not None and patch.patch_id not in dirty_patch_ids:
            continue
        tile_width, tile_height, tile = decode_preview(tile_artifact.data)
        if tile_width != tile_size or tile_height != tile_size:
            raise ValueError("preview tile has unexpected dimensions")
        column = index % columns
        row = index // columns
        x0 = column * tile_size
        y0 = row * tile_size
        for tile_row in range(tile_height):
            destination = ((y0 + tile_row) * width + x0) * 4
            source = tile_row * tile_width * 4
            canvas[destination : destination + tile_width * 4] = tile[
                source : source + tile_width * 4
            ]
        dirty_rects.append((x0, y0, tile_size, tile_size))

    rgba = bytes(canvas)
    return ScreenPreview(
        width=width,
        height=height,
        rgba=rgba,
        content_hash=sha256_bytes(rgba),
        dirty_rects=tuple(dirty_rects),
    )
