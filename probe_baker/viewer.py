"""Optional OpenGL preview. The bake engine never requires a GL context."""

from __future__ import annotations

import dataclasses
import math
from typing import Any

import numpy as np

from .bake import BakeState
from .errors import ErrorCode, ProbeError
from .jobs import BakeJobManager
from .publish import PublishedStore
from .scene import BoxOccluder, Scene
from .snapshot import SnapshotStore
from .strategy import BakeConfig


def gl_context_available() -> bool:
    try:
        from OpenGL import GLUT

        GLUT.glutInit()
        return True
    except Exception:
        return False


def _wire_box(lower: tuple[float, float, float], upper: tuple[float, float, float]) -> None:
    from OpenGL import GL

    x0, y0, z0 = lower
    x1, y1, z1 = upper
    edges = (
        ((x0, y0, z0), (x1, y0, z0)),
        ((x1, y0, z0), (x1, y1, z0)),
        ((x1, y1, z0), (x0, y1, z0)),
        ((x0, y1, z0), (x0, y0, z0)),
        ((x0, y0, z1), (x1, y0, z1)),
        ((x1, y0, z1), (x1, y1, z1)),
        ((x1, y1, z1), (x0, y1, z1)),
        ((x0, y1, z1), (x0, y0, z1)),
        ((x0, y0, z0), (x0, y0, z1)),
        ((x1, y0, z0), (x1, y0, z1)),
        ((x1, y1, z0), (x1, y1, z1)),
        ((x0, y1, z0), (x0, y1, z1)),
    )
    GL.glBegin(GL.GL_LINES)
    for start, stop in edges:
        GL.glVertex3f(*start)
        GL.glVertex3f(*stop)
    GL.glEnd()


class OpenGLProbeViewer:
    def __init__(
        self,
        scene: Scene,
        config: BakeConfig,
        *,
        publisher: PublishedStore,
        job: BakeJobManager,
        initial_state: BakeState,
        snapshot_store: SnapshotStore | None = None,
    ) -> None:
        try:
            from OpenGL import GL, GLU, GLUT
        except Exception as exc:
            raise ProbeError(
                ErrorCode.IO,
                f"OpenGL/PyOpenGL import failed: {exc}; use the headless CLI entry",
            ) from exc
        self.GL = GL
        self.GLU = GLU
        self.GLUT = GLUT
        self.scene = scene
        self.config = config
        self.publisher = publisher
        self.job = job
        self.initial_state = initial_state
        self.snapshot_store = snapshot_store
        self.angle = 35.0
        self.heat_cache_version: int | None = None
        self.heat: np.ndarray | None = None
        self.blocker_enabled = True

    def run(self) -> None:
        GL, GLU, GLUT = self.GL, self.GLU, self.GLUT
        GLUT.glutInit()
        GLUT.glutInitDisplayMode(GLUT.GLUT_DOUBLE | GLUT.GLUT_RGB | GLUT.GLUT_DEPTH)
        GLUT.glutInitWindowSize(1100, 760)
        GLUT.glutCreateWindow(b"Local light-probe baker")
        GL.glEnable(GL.GL_DEPTH_TEST)
        GL.glPointSize(6.0)
        GL.glLineWidth(1.4)
        GLUT.glutDisplayFunc(self._display)
        GLUT.glutKeyboardFunc(self._keyboard)
        GLUT.glutIdleFunc(self._idle)
        try:
            GLUT.glutMainLoop()
        finally:
            self.job.cancel()

    def _refresh_heat(self) -> None:
        try:
            snapshot = self.publisher.pin()
        except ProbeError:
            return
        if self.heat_cache_version == snapshot.version:
            return
        result = snapshot.query_probes()
        self.heat = np.asarray(result["heat"], dtype=np.float64)
        self.heat_cache_version = snapshot.version

    def _display(self) -> None:
        GL, GLU = self.GL, self.GLU
        GL.glClearColor(0.035, 0.045, 0.06, 1.0)
        GL.glClear(GL.GL_COLOR_BUFFER_BIT | GL.GL_DEPTH_BUFFER_BIT)
        width = max(1, self.GLUT.glutGet(self.GLUT.GLUT_WINDOW_WIDTH))
        height = max(1, self.GLUT.glutGet(self.GLUT.GLUT_WINDOW_HEIGHT))
        GL.glViewport(0, 0, width, height)
        GL.glMatrixMode(GL.GL_PROJECTION)
        GL.glLoadIdentity()
        GLU.gluPerspective(48.0, width / float(height), 0.1, 200.0)
        GL.glMatrixMode(GL.GL_MODELVIEW)
        GL.glLoadIdentity()
        center = np.asarray(self.scene.bounds_min) * 0.5 + np.asarray(self.scene.bounds_max) * 0.5
        span = float(np.linalg.norm(np.asarray(self.scene.bounds_max) - np.asarray(self.scene.bounds_min)))
        distance = max(6.0, span * 1.8)
        radians = math.radians(self.angle)
        eye = center + np.asarray([math.cos(radians) * distance, span * 0.75, math.sin(radians) * distance])
        GLU.gluLookAt(
            float(eye[0]), float(eye[1]), float(eye[2]),
            float(center[0]), float(center[1]), float(center[2]),
            0.0, 1.0, 0.0,
        )

        GL.glColor3f(0.3, 0.38, 0.48)
        _wire_box(self.scene.bounds_min, self.scene.bounds_max)

        GL.glColor3f(0.82, 0.44, 0.25)
        for occluder in self.scene.occluders:
            if occluder.occluder_id in self.scene.dynamic_mask:
                GL.glColor3f(0.35, 0.35, 0.4)
            else:
                GL.glColor3f(0.82, 0.44, 0.25)
            if isinstance(occluder, BoxOccluder):
                _wire_box(occluder.lower, occluder.upper)
            else:
                GL.glPushMatrix()
                GL.glTranslatef(*occluder.center)
                GLUT = self.GLUT
                GLUT.glutWireSphere(occluder.radius, 16, 10)
                GL.glPopMatrix()

        try:
            snapshot = self.publisher.pin()
        except ProbeError:
            snapshot = None
        if snapshot is not None:
            self._refresh_heat()
            heat = self.heat
            if heat is not None:
                low, high = np.percentile(heat, [5, 95]) if heat.size > 2 else (0.0, 1.0)
                scale = max(1e-9, float(high - low))
                GL.glBegin(GL.GL_POINTS)
                for index, center_probe in enumerate(snapshot.grid.centers):
                    normalized = min(1.0, max(0.0, (float(heat[index]) - low) / scale))
                    if normalized < 0.5:
                        blend = normalized * 2.0
                        color = (0.08, 0.35 + 0.5 * blend, 0.9 - 0.55 * blend)
                    else:
                        blend = (normalized - 0.5) * 2.0
                        color = (0.08 + 0.9 * blend, 0.85 - 0.25 * blend, 0.35 - 0.25 * blend)
                    GL.glColor3f(*color)
                    GL.glVertex3f(*center_probe)
                GL.glEnd()
        status = self.job.status()
        title = (
            f"spacing={self.config.spacing if self.config.spacing is not None else self.scene.spacing:.2f} "
            f"order={self.config.sh_order} comp={self.config.compensation_strength:.2f} "
            f"version={status.published_version} "
            f"{'BUSY' if status.running else 'idle'}"
        )
        self.GLUT.glutSetWindowTitle(title.encode("utf-8"))
        self.GLUT.glutSwapBuffers()

    def _idle(self) -> None:
        if self.job.status().running:
            self.GLUT.glutPostRedisplay()

    def _rebuild(self) -> None:
        try:
            self.job.start(self.scene, self.config)
        except ProbeError as exc:
            if exc.code != ErrorCode.BUSY:
                raise

    def _keyboard(self, key: bytes, _x: int, _y: int) -> None:
        character = key.decode("latin-1")
        if character in ("\x1b", "q", "Q"):
            self.GLUT.glutLeaveMainLoop()
            return
        changed = True
        if character in ("+", "="):
            spacing = self.config.spacing or self.scene.spacing
            self.config = dataclasses.replace(self.config, spacing=max(0.25, spacing * 0.8))
        elif character in ("-", "_"):
            spacing = self.config.spacing or self.scene.spacing
            self.config = dataclasses.replace(self.config, spacing=spacing * 1.25)
        elif character == "[":
            self.config = dataclasses.replace(
                self.config, sh_order=max(2, self.config.sh_order - 1)
            )
        elif character == "]":
            self.config = dataclasses.replace(
                self.config, sh_order=min(6, self.config.sh_order + 1)
            )
        elif character == ",":
            self.config = dataclasses.replace(
                self.config,
                compensation_strength=max(0.0, self.config.compensation_strength - 0.1),
            )
        elif character == ".":
            self.config = dataclasses.replace(
                self.config,
                compensation_strength=min(1.0, self.config.compensation_strength + 0.1),
            )
        elif character in ("o", "O"):
            self.blocker_enabled = not self.blocker_enabled
            existing = self.scene.occluder_by_id("viewer_blocker")
            if self.blocker_enabled and existing is None:
                center = (
                    (self.scene.bounds_min[0] + self.scene.bounds_max[0]) * 0.5,
                    (self.scene.bounds_min[1] + self.scene.bounds_max[1]) * 0.5,
                    (self.scene.bounds_min[2] + self.scene.bounds_max[2]) * 0.5,
                )
                blocker = BoxOccluder(
                    "viewer_blocker",
                    (center[0] - 0.8, center[1] - 0.6, center[2] - 0.8),
                    (center[0] + 0.8, center[1] + 0.6, center[2] + 0.8),
                )
                self.scene = dataclasses.replace(
                    self.scene, occluders=tuple(self.scene.occluders) + (blocker,)
                )
            elif not self.blocker_enabled and existing is not None:
                remaining = tuple(
                    item
                    for item in self.scene.occluders
                    if item.occluder_id != "viewer_blocker"
                )
                self.scene = dataclasses.replace(
                    self.scene,
                    occluders=remaining,
                    dynamic_mask=frozenset(
                        {item for item in self.scene.dynamic_mask if item != "viewer_blocker"}
                    ),
                )
        elif character in ("d", "D"):
            mask = set(self.scene.dynamic_mask)
            if "viewer_blocker" in mask:
                mask.remove("viewer_blocker")
            elif self.scene.occluder_by_id("viewer_blocker") is not None:
                mask.add("viewer_blocker")
            self.scene = dataclasses.replace(self.scene, dynamic_mask=frozenset(mask))
        elif character in ("b", "B"):
            self._rebuild()
            changed = False
        elif character in ("x", "X"):
            self.job.cancel()
            changed = False
        elif character in ("a", "A"):
            self.angle += 15.0
        else:
            changed = False
        if changed:
            self.GLUT.glutPostRedisplay()


def run_headless_preview(
    scene: Scene,
    config: BakeConfig,
    *,
    publisher: PublishedStore,
    job: BakeJobManager,
    snapshot_store: SnapshotStore | None,
) -> None:
    """A scriptable stand-in for CI/no-GPU hosts."""

    job.start(scene, config)
    outcome = job.wait()
    if outcome is None:
        raise ProbeError(ErrorCode.IO, "headless preview bake did not complete")
    snapshot = publisher.pin()
    result = snapshot.query_probes()
    print(
        f"headless-preview version={snapshot.version} probes={snapshot.grid.count} "
        f"heat_min={float(np.min(result['heat'])):.6f} "
        f"heat_max={float(np.max(result['heat'])):.6f}"
    )

