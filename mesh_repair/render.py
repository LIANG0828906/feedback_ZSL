from __future__ import annotations

import binascii
import math
import os
import struct
import threading
import zlib
from dataclasses import dataclass
from pathlib import Path
from typing import Any

import numpy as np

from .snapshot import MeshSnapshot, SnapshotStore


PALETTE = np.asarray(
    [
        [54, 113, 178],
        [224, 132, 58],
        [77, 155, 104],
        [184, 76, 91],
        [129, 98, 168],
        [53, 160, 164],
        [202, 164, 54],
        [111, 117, 127],
    ],
    dtype=np.float64,
)


@dataclass
class RenderStatus:
    mode: str
    preview_path: str | None
    message: str
    gl_version: str | None = None
    gl_renderer: str | None = None
    frames: int = 0

    def to_dict(self) -> dict[str, Any]:
        return {
            "mode": self.mode,
            "preview_path": self.preview_path,
            "message": self.message,
            "gl_version": self.gl_version,
            "gl_renderer": self.gl_renderer,
            "frames": self.frames,
        }


def _face_normals(vertices: np.ndarray, faces: np.ndarray) -> np.ndarray:
    tri = vertices[faces]
    normals = np.cross(tri[:, 1] - tri[:, 0], tri[:, 2] - tri[:, 0])
    lengths = np.linalg.norm(normals, axis=1)
    good = lengths > 1e-15
    normals[good] /= lengths[good, None]
    normals[~good] = 0.0
    return normals


def _projection(vertices: np.ndarray) -> tuple[np.ndarray, np.ndarray, float]:
    if vertices.size == 0:
        return np.zeros((0, 3)), np.zeros(3), 1.0
    center = (vertices.min(axis=0) + vertices.max(axis=0)) * 0.5
    centered = vertices - center
    angle_y = math.radians(28.0)
    angle_x = math.radians(-22.0)
    cy, sy = math.cos(angle_y), math.sin(angle_y)
    cx, sx = math.cos(angle_x), math.sin(angle_x)
    y_rot = np.asarray([[cy, 0.0, sy], [0.0, 1.0, 0.0], [-sy, 0.0, cy]])
    x_rot = np.asarray([[1.0, 0.0, 0.0], [0.0, cx, -sx], [0.0, sx, cx]])
    rotated = centered @ y_rot.T @ x_rot.T
    extent = max(float(np.ptp(rotated[:, 0])), float(np.ptp(rotated[:, 1])), 1e-9)
    return rotated, center, extent


def write_static_preview(
    snapshot: MeshSnapshot | None,
    path: str | os.PathLike[str],
    *,
    width: int = 960,
    height: int = 620,
) -> str:
    target = Path(path)
    if target.suffix.lower() != ".svg":
        target = target.with_suffix(".svg")
    target.parent.mkdir(parents=True, exist_ok=True)
    margin = 42.0
    if snapshot is None or snapshot.face_count == 0:
        body = (
            f'<text x="{width/2}" y="{height/2}" text-anchor="middle" '
            'font-family="monospace" font-size="18" fill="#9aa4b2">'
            "no committed mesh snapshot</text>"
        )
    else:
        rotated, _, extent = _projection(snapshot.vertices)
        scale = min(width - 2 * margin, height - 2 * margin) / extent
        screen_x = width * 0.5 + rotated[:, 0] * scale
        screen_y = height * 0.5 - rotated[:, 1] * scale
        normals = _face_normals(snapshot.vertices, snapshot.faces)
        light = np.asarray([0.24, -0.32, 0.92], dtype=np.float64)
        intensity = np.clip(np.abs(normals @ light) * 0.72 + 0.28, 0.18, 1.0)
        depth = rotated[:, 2]
        order = np.argsort(
            (depth[snapshot.faces[:, 0]] + depth[snapshot.faces[:, 1]] + depth[snapshot.faces[:, 2]])
            / 3.0
        )
        polygons: list[str] = []
        for face_id in order:
            face = snapshot.faces[face_id]
            block_id = int(snapshot.face_block_ids[face_id]) % PALETTE.shape[0]
            rgb = np.clip(PALETTE[block_id] * intensity[face_id], 0, 255).astype(int)
            points = " ".join(
                f"{screen_x[vertex]:.2f},{screen_y[vertex]:.2f}" for vertex in face
            )
            polygons.append(
                f'<polygon points="{points}" fill="rgb({rgb[0]},{rgb[1]},{rgb[2]})" '
                'stroke="rgb(20,24,31)" stroke-width="0.18"/>'
            )
        body = "\n".join(polygons)
    svg = (
        f'<svg xmlns="http://www.w3.org/2000/svg" width="{width}" height="{height}" '
        f'viewBox="0 0 {width} {height}">\n'
        '<rect width="100%" height="100%" fill="#10151d"/>\n'
        f"{body}\n</svg>\n"
    )
    target.write_text(svg, encoding="utf-8")
    return str(target)


class RenderLoop:
    """Render thread that only reads whole immutable snapshots."""

    def __init__(
        self,
        store: SnapshotStore,
        *,
        stop_event: threading.Event,
        preview_path: str | os.PathLike[str] | None = None,
        width: int = 800,
        height: int = 560,
        hidden: bool = True,
    ) -> None:
        self.store = store
        self.stop_event = stop_event
        self.preview_path = str(preview_path) if preview_path else None
        self.width = width
        self.height = height
        self.hidden = hidden
        self.status: RenderStatus | None = None
        self._last_version = -1
        self._last_snapshot: MeshSnapshot | None = None

    def run(self) -> RenderStatus:
        try:
            status = self._run_gl()
            self.status = status
            return status
        except Exception as exc:
            snapshot = self.store.latest()
            fallback_path = None
            if self.preview_path:
                fallback_path = write_static_preview(snapshot, self.preview_path)
            self.status = RenderStatus(
                mode="static-fallback",
                preview_path=fallback_path,
                message=f"{type(exc).__name__}: {exc}",
                frames=self._frame_count,
            )
            return self.status

    _frame_count = 0

    def _run_gl(self) -> RenderStatus:
        os.environ.setdefault("PYGAME_HIDE_SUPPORT_PROMPT", "1")
        import pygame
        from OpenGL import GL

        pygame.init()
        pygame.display.init()
        flags = pygame.OPENGL | (pygame.HIDDEN if self.hidden else 0)
        pygame.display.set_mode((self.width, self.height), flags)
        version = GL.glGetString(GL.GL_VERSION).decode("utf-8", errors="replace")
        renderer = GL.glGetString(GL.GL_RENDERER).decode("utf-8", errors="replace")
        GL.glViewport(0, 0, self.width, self.height)
        GL.glEnable(GL.GL_DEPTH_TEST)
        GL.glDepthFunc(GL.GL_LEQUAL)
        GL.glClearColor(0.063, 0.082, 0.114, 1.0)
        self._frame_count = 0
        try:
            while not self.stop_event.is_set():
                snapshot = self.store.latest()
                if snapshot is not None:
                    self._draw(snapshot, GL)
                    pygame.display.flip()
                    self._frame_count += 1
                    if self.preview_path and snapshot.version != self._last_version:
                        self._last_version = snapshot.version
                        self._save_png(GL)
                self.stop_event.wait(0.04)
            final_snapshot = self.store.latest()
            if final_snapshot is not None and final_snapshot.version != self._last_version:
                self._draw(final_snapshot, GL)
                pygame.display.flip()
                self._frame_count += 1
                self._last_version = final_snapshot.version
                if self.preview_path:
                    self._save_png(GL)
        finally:
            pygame.display.quit()
            pygame.quit()
        return RenderStatus(
            mode="opengl-offscreen",
            preview_path=self.preview_path,
            message="PyOpenGL hidden-window context created",
            gl_version=version,
            gl_renderer=renderer,
            frames=self._frame_count,
        )

    def _draw(self, snapshot: MeshSnapshot, GL: Any) -> None:
        GL.glClear(GL.GL_COLOR_BUFFER_BIT | GL.GL_DEPTH_BUFFER_BIT)
        if snapshot.face_count == 0:
            return
        center, extent = self._gl_view(snapshot)
        GL.glMatrixMode(GL.GL_PROJECTION)
        GL.glLoadIdentity()
        near, far = 0.01, 100.0
        top = near * math.tan(math.radians(35.0) * 0.5)
        right = top * self.width / max(self.height, 1)
        GL.glFrustum(-right, right, -top, top, near, far)
        GL.glMatrixMode(GL.GL_MODELVIEW)
        GL.glLoadIdentity()
        GL.glTranslatef(0.0, 0.0, -extent * 2.7)
        GL.glRotatef(22.0, 1.0, 0.0, 0.0)
        GL.glRotatef(-28.0, 0.0, 1.0, 0.0)
        GL.glTranslatef(-center[0], -center[1], -center[2])
        scaled_vertices = snapshot.vertices
        normals = _face_normals(scaled_vertices, snapshot.faces)
        light = np.asarray([0.24, -0.32, 0.92], dtype=np.float64)
        intensity = np.clip(np.abs(normals @ light) * 0.74 + 0.26, 0.18, 1.0)
        GL.glBegin(GL.GL_TRIANGLES)
        for face_id, face in enumerate(snapshot.faces):
            palette_id = int(snapshot.face_block_ids[face_id]) % PALETTE.shape[0]
            color = np.clip(PALETTE[palette_id] / 255.0 * intensity[face_id], 0.0, 1.0)
            GL.glColor3f(float(color[0]), float(color[1]), float(color[2]))
            for vertex_id in face:
                point = scaled_vertices[int(vertex_id)]
                GL.glVertex3f(float(point[0]), float(point[1]), float(point[2]))
        GL.glEnd()

    def _gl_view(self, snapshot: MeshSnapshot) -> tuple[np.ndarray, float]:
        center = (snapshot.vertices.min(axis=0) + snapshot.vertices.max(axis=0)) * 0.5
        extent = float(np.max(np.ptp(snapshot.vertices, axis=0)))
        return center, max(extent, 1e-6)

    def _save_png(self, GL: Any) -> None:
        assert self.preview_path is not None
        width, height = self.width, self.height
        pixels = GL.glReadPixels(0, 0, width, height, GL.GL_RGB, GL.GL_UNSIGNED_BYTE)
        array = np.frombuffer(pixels, dtype=np.uint8).reshape(height, width, 3)
        array = np.flipud(array)
        target = Path(self.preview_path)
        if target.suffix.lower() != ".png":
            target = target.with_suffix(".png")
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_bytes(encode_png(array))
        self.preview_path = str(target)


def encode_png(rgb: np.ndarray) -> bytes:
    height, width, _ = rgb.shape
    raw = b"".join(b"\x00" + rgb[row].tobytes() for row in range(height))
    compressed = zlib.compress(raw, level=6)

    def chunk(kind: bytes, data: bytes) -> bytes:
        return (
            struct.pack(">I", len(data))
            + kind
            + data
            + struct.pack(">I", binascii.crc32(kind + data) & 0xFFFFFFFF)
        )

    header = struct.pack(">IIBBBBB", width, height, 8, 2, 0, 0, 0)
    return b"\x89PNG\r\n\x1a\n" + chunk(b"IHDR", header) + chunk(b"IDAT", compressed) + chunk(b"IEND", b"")
