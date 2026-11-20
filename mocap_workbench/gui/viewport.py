"""Three-dimensional skeleton viewport with a QPainter fallback."""

from __future__ import annotations

import math
import os
from collections.abc import Mapping

import numpy as np
from PyQt5.QtCore import Qt, pyqtSignal
from PyQt5.QtGui import QColor, QPainter, QPen
from PyQt5.QtWidgets import QWidget

try:
    from OpenGL import GL
    from PyQt5.QtWidgets import QOpenGLWidget

    HAVE_OPENGL = os.environ.get("QT_QPA_PLATFORM", "").lower() not in {
        "offscreen",
        "minimal",
    }
except ImportError:  # pragma: no cover - exercised only on minimal installs
    QOpenGLWidget = QWidget  # type: ignore[assignment,misc]
    GL = None  # type: ignore[assignment]
    HAVE_OPENGL = False


def _color_for_ratio(ratio: float) -> QColor:
    if not math.isfinite(ratio):
        return QColor(255, 80, 80)
    if ratio <= 0.6:
        return QColor(90, 220, 130)
    if ratio <= 1.0:
        t = (ratio - 0.6) / 0.4
        return QColor(
            int(90 + (250 - 90) * t),
            int(220 + (210 - 220) * t),
            90,
        )
    return QColor(245, 65, 65)


def _perspective_matrix(
    fov_degrees: float, aspect: float, near: float, far: float
) -> np.ndarray:
    scale = 1.0 / math.tan(math.radians(fov_degrees) * 0.5)
    matrix = np.zeros((4, 4), dtype=np.float64)
    matrix[0, 0] = scale / max(aspect, 1.0e-6)
    matrix[1, 1] = scale
    matrix[2, 2] = (far + near) / (near - far)
    matrix[2, 3] = (2.0 * far * near) / (near - far)
    matrix[3, 2] = -1.0
    return matrix


def _look_at_matrix(eye: np.ndarray, center: np.ndarray) -> np.ndarray:
    forward = center - eye
    forward /= max(float(np.linalg.norm(forward)), 1.0e-12)
    side = np.cross(forward, np.array([0.0, 1.0, 0.0]))
    if float(np.linalg.norm(side)) < 1.0e-8:
        side = np.array([1.0, 0.0, 0.0])
    side /= float(np.linalg.norm(side))
    up = np.cross(side, forward)
    matrix = np.eye(4, dtype=np.float64)
    matrix[0, :3] = side
    matrix[1, :3] = up
    matrix[2, :3] = -forward
    matrix[0, 3] = -float(np.dot(side, eye))
    matrix[1, 3] = -float(np.dot(up, eye))
    matrix[2, 3] = float(np.dot(forward, eye))
    return matrix


class _ViewportState:
    def __init__(self) -> None:
        self.transforms: np.ndarray | None = None
        self.parents: tuple[int | None, ...] = ()
        self.joint_names: tuple[str, ...] = ()
        self.ratios: dict[int, float] = {}
        self.targets: list[tuple[str, np.ndarray, float]] = []
        self.yaw = 28.0
        self.pitch = -18.0
        self.distance = 3.8
        self.last_mouse = None

    def bounds(self) -> tuple[np.ndarray, float]:
        if self.transforms is None or len(self.transforms) == 0:
            return np.zeros(3), 1.0
        positions = self.transforms[:, :3, 3]
        center = (positions.min(axis=0) + positions.max(axis=0)) * 0.5
        extent = float(np.max(positions.max(axis=0) - positions.min(axis=0)))
        return center, max(extent, 0.5)


class SkeletonViewport(QOpenGLWidget):
    clicked_frame = pyqtSignal(int)

    def __init__(self, parent=None):
        super().__init__(parent)
        self.state = _ViewportState()
        self.setMinimumSize(520, 360)
        self.setFocusPolicy(Qt.StrongFocus)

    def set_skeleton(
        self,
        joint_names: tuple[str, ...],
        parents: tuple[int | None, ...],
    ) -> None:
        self.state.joint_names = tuple(joint_names)
        self.state.parents = tuple(parents)
        self.update()

    def set_frame(
        self,
        transforms: np.ndarray | None,
        ratios: Mapping[int, float] | None = None,
        targets: list[tuple[str, np.ndarray, float]] | None = None,
    ) -> None:
        self.state.transforms = (
            None if transforms is None else np.asarray(transforms, dtype=np.float64)
        )
        self.state.ratios = dict(ratios or {})
        self.state.targets = list(targets or [])
        self.update()

    def initializeGL(self) -> None:  # pragma: no cover - requires a GL context
        if not HAVE_OPENGL:
            return
        GL.glClearColor(0.055, 0.065, 0.085, 1.0)
        GL.glEnable(GL.GL_DEPTH_TEST)
        GL.glEnable(GL.GL_BLEND)
        GL.glBlendFunc(GL.GL_SRC_ALPHA, GL.GL_ONE_MINUS_SRC_ALPHA)
        GL.glEnable(GL.GL_LINE_SMOOTH)
        GL.glHint(GL.GL_LINE_SMOOTH_HINT, GL.GL_NICEST)

    def resizeGL(self, width: int, height: int) -> None:  # pragma: no cover
        if not HAVE_OPENGL:
            return
        GL.glViewport(0, 0, max(width, 1), max(height, 1))

    def _draw_grid(self) -> None:  # pragma: no cover
        GL.glColor3f(0.18, 0.20, 0.24)
        GL.glLineWidth(1.0)
        GL.glBegin(GL.GL_LINES)
        for value in np.linspace(-1.5, 1.5, 13):
            GL.glVertex3f(value, -0.9, -1.5)
            GL.glVertex3f(value, -0.9, 1.5)
            GL.glVertex3f(-1.5, -0.9, value)
            GL.glVertex3f(1.5, -0.9, value)
        GL.glEnd()

    def paintGL(self) -> None:  # pragma: no cover - requires a GL context
        if not HAVE_OPENGL:
            return
        try:
            self._paint_scene()
        except Exception as exc:
            # Partial compatibility profiles may miss immediate-mode or
            # matrix entry points.  Keep the desktop event loop alive; data
            # panels and the controller do not depend on the GL context.
            self._paint_error = f"{type(exc).__name__}: {exc}"
            GL.glClear(GL.GL_COLOR_BUFFER_BIT | GL.GL_DEPTH_BUFFER_BIT)

    def _paint_scene(self) -> None:  # pragma: no cover - requires a GL context
        GL.glClear(GL.GL_COLOR_BUFFER_BIT | GL.GL_DEPTH_BUFFER_BIT)
        center, extent = self.state.bounds()
        distance = self.state.distance * max(extent, 1.0) * 0.68
        yaw = math.radians(self.state.yaw)
        pitch = math.radians(self.state.pitch)
        eye = center + np.array(
            [
                distance * math.cos(pitch) * math.sin(yaw),
                distance * math.sin(pitch) + 0.25 * extent,
                distance * math.cos(pitch) * math.cos(yaw),
            ]
        )
        width = max(self.width(), 1)
        height = max(self.height(), 1)
        projection = _perspective_matrix(42.0, width / height, 0.02, 100.0)
        view = _look_at_matrix(eye, center)
        GL.glMatrixMode(GL.GL_PROJECTION)
        GL.glLoadMatrixf(projection.flatten(order="F"))
        GL.glMatrixMode(GL.GL_MODELVIEW)
        GL.glLoadMatrixf(view.flatten(order="F"))
        self._draw_grid()
        transforms = self.state.transforms
        if transforms is None:
            return
        GL.glLineWidth(2.4)
        for index, parent in enumerate(self.state.parents):
            if parent is None:
                continue
            color = _color_for_ratio(self.state.ratios.get(index, 0.0))
            GL.glColor3f(color.redF(), color.greenF(), color.blueF())
            p0 = transforms[parent, :3, 3]
            p1 = transforms[index, :3, 3]
            GL.glBegin(GL.GL_LINES)
            GL.glVertex3f(float(p0[0]), float(p0[1]), float(p0[2]))
            GL.glVertex3f(float(p1[0]), float(p1[1]), float(p1[2]))
            GL.glEnd()
        GL.glPointSize(6.0)
        GL.glBegin(GL.GL_POINTS)
        for index in range(transforms.shape[0]):
            color = _color_for_ratio(self.state.ratios.get(index, 0.0))
            GL.glColor3f(color.redF(), color.greenF(), color.blueF())
            p = transforms[index, :3, 3]
            GL.glVertex3f(float(p[0]), float(p[1]), float(p[2]))
        GL.glEnd()
        for _, target, ratio in self.state.targets:
            color = _color_for_ratio(max(ratio, 1.05))
            GL.glColor3f(color.redF(), color.greenF(), color.blueF())
            GL.glPointSize(10.0)
            GL.glBegin(GL.GL_POINTS)
            GL.glVertex3f(float(target[0]), float(target[1]), float(target[2]))
            GL.glEnd()

    def mousePressEvent(self, event) -> None:
        if event.button() == Qt.LeftButton:
            self.state.last_mouse = event.pos()

    def mouseMoveEvent(self, event) -> None:
        if self.state.last_mouse is None:
            return
        delta = event.pos() - self.state.last_mouse
        self.state.last_mouse = event.pos()
        self.state.yaw += delta.x() * 0.45
        self.state.pitch = max(-85.0, min(85.0, self.state.pitch + delta.y() * 0.35))
        self.update()

    def mouseReleaseEvent(self, event) -> None:
        self.state.last_mouse = None

    def wheelEvent(self, event) -> None:
        factor = 0.9 if event.angleDelta().y() > 0 else 1.1
        self.state.distance = max(0.25, min(20.0, self.state.distance * factor))
        self.update()


if not HAVE_OPENGL:  # pragma: no cover - fallback for machines without PyOpenGL
    class SkeletonViewport(QWidget):  # type: ignore[no-redef]
        clicked_frame = pyqtSignal(int)

        def __init__(self, parent=None):
            super().__init__(parent)
            self.state = _ViewportState()
            self.setMinimumSize(520, 360)

        def set_skeleton(self, joint_names, parents) -> None:
            self.state.joint_names = tuple(joint_names)
            self.state.parents = tuple(parents)
            self.update()

        def set_frame(self, transforms, ratios=None, targets=None) -> None:
            self.state.transforms = (
                None if transforms is None else np.asarray(transforms, dtype=np.float64)
            )
            self.state.ratios = dict(ratios or {})
            self.state.targets = list(targets or [])
            self.update()

        def paintEvent(self, event) -> None:
            painter = QPainter(self)
            painter.setRenderHint(QPainter.Antialiasing, True)
            painter.fillRect(self.rect(), QColor(14, 17, 22))
            transforms = self.state.transforms
            if transforms is None:
                painter.setPen(QColor(150, 160, 175))
                painter.drawText(self.rect(), Qt.AlignCenter, "Open the BVH and solve a range")
                return
            positions = transforms[:, :3, 3]
            lo = positions.min(axis=0)
            hi = positions.max(axis=0)
            span = np.maximum(hi - lo, 0.2)
            scale = min(
                (self.width() - 40) / span[0],
                (self.height() - 40) / span[1],
            )
            center = (lo + hi) * 0.5

            def project(point):
                x = self.width() * 0.5 + (point[0] - center[0]) * scale
                y = self.height() * 0.5 - (point[1] - center[1]) * scale
                return x, y

            for index, parent in enumerate(self.state.parents):
                if parent is None:
                    continue
                color = _color_for_ratio(self.state.ratios.get(index, 0.0))
                painter.setPen(QPen(color, 2.4))
                x0, y0 = project(transforms[parent, :3, 3])
                x1, y1 = project(transforms[index, :3, 3])
                painter.drawLine(int(x0), int(y0), int(x1), int(y1))
            for index in range(transforms.shape[0]):
                color = _color_for_ratio(self.state.ratios.get(index, 0.0))
                x, y = project(transforms[index, :3, 3])
                painter.setBrush(color)
                painter.setPen(Qt.NoPen)
                painter.drawEllipse(int(x) - 3, int(y) - 3, 6, 6)

