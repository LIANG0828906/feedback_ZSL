from __future__ import annotations

import json
import math
import os
import threading
import time
from dataclasses import dataclass
from pathlib import Path
from typing import Any

import numpy as np
from OpenGL.GL import (
    GL_AMBIENT_AND_DIFFUSE,
    GL_COLOR_BUFFER_BIT,
    GL_COLOR_MATERIAL,
    GL_DEPTH_BUFFER_BIT,
    GL_DEPTH_TEST,
    GL_DIFFUSE,
    GL_FRONT_AND_BACK,
    GL_LIGHT0,
    GL_LIGHTING,
    GL_MODELVIEW,
    GL_NORMALIZE,
    GL_POSITION,
    GL_PROJECTION,
    GL_QUADS,
    GL_TRIANGLES,
    glBegin,
    glClear,
    glClearColor,
    glColor3f,
    glColorMaterial,
    glDisable,
    glEnable,
    glEnd,
    glFrustum,
    glLightfv,
    glLoadIdentity,
    glMultMatrixd,
    glMatrixMode,
    glNormal3f,
    glVertex3f,
    glViewport,
)
from PyQt5.QtCore import QObject, QPoint, QThread, QTimer, Qt, pyqtSignal, pyqtSlot
from PyQt5.QtGui import QColor, QSurfaceFormat
from PyQt5.QtWidgets import (
    QAction,
    QApplication,
    QCheckBox,
    QComboBox,
    QDockWidget,
    QDoubleSpinBox,
    QFileDialog,
    QFormLayout,
    QGroupBox,
    QHBoxLayout,
    QLabel,
    QMainWindow,
    QMessageBox,
    QOpenGLWidget,
    QPlainTextEdit,
    QProgressDialog,
    QPushButton,
    QSpinBox,
    QVBoxLayout,
    QWidget,
)

from .editing import Selection, simplify_edge_collapse, transform_selection
from .errors import (
    EditCancelled,
    E_RECOVERY_INVALID,
    MeshEditorError,
    RecoveryError,
    StaleVersionError,
    VersionLossError,
)
from .formats import parse_mesh
from .model import Mesh
from .precision import compare_meshes, summarize_precision, write_precision_report
from .render_backend import BackendInfo, configure_render_backend
from .samplegen import generate_project_samples
from .topology import weld_vertices
from .versioning import VersionRepository


EDIT_PROGRESS_THRESHOLD_SECONDS = 0.75


@dataclass(frozen=True)
class RenderSnapshot:
    version: int
    vertices: np.ndarray
    triangles: np.ndarray
    face_ids: np.ndarray
    normals: np.ndarray
    face_colors: np.ndarray
    selected_faces: frozenset[int]
    bad_faces: frozenset[int]
    bbox_min: np.ndarray
    bbox_max: np.ndarray


def _make_snapshot(
    mesh: Mesh,
    version: int,
    selected: frozenset[int],
    bad: frozenset[int],
) -> RenderSnapshot:
    triangles, _, face_ids = mesh.triangle_arrays()
    normals = mesh.triangulated_normals()
    palette = np.asarray(
        [
            [0.47, 0.65, 0.78],
            [0.56, 0.73, 0.55],
            [0.77, 0.65, 0.47],
            [0.65, 0.56, 0.78],
            [0.47, 0.75, 0.75],
            [0.78, 0.53, 0.61],
        ],
        dtype=np.float32,
    )
    group_names = sorted({face.group for face in mesh.faces})
    group_index = {name: i for i, name in enumerate(group_names)}
    face_colors = np.asarray(
        [palette[group_index[face.group] % len(palette)] for face in mesh.faces],
        dtype=np.float32,
    )
    bbox_min, bbox_max = mesh.bounding_box()
    vertices = mesh.vertices.copy()
    vertices.setflags(write=False)
    triangles.setflags(write=False)
    face_ids.setflags(write=False)
    normals.setflags(write=False)
    face_colors.setflags(write=False)
    return RenderSnapshot(
        version=int(version),
        vertices=vertices,
        triangles=triangles,
        face_ids=face_ids,
        normals=normals,
        face_colors=face_colors,
        selected_faces=selected,
        bad_faces=bad,
        bbox_min=np.asarray(bbox_min, dtype=np.float64),
        bbox_max=np.asarray(bbox_max, dtype=np.float64),
    )


class MeshViewport(QOpenGLWidget):
    selectionChanged = pyqtSignal(object)

    def __init__(self, parent: QWidget | None = None) -> None:
        super().__init__(parent)
        self._snapshot: RenderSnapshot | None = None
        self._selected: set[int] = set()
        self._bad: set[int] = set()
        self._yaw = math.radians(35.0)
        self._pitch = math.radians(22.0)
        self._distance = 4.0
        self._center = np.zeros(3, dtype=np.float64)
        self._radius = 1.0
        self._last_mouse = QPoint()
        self._mouse_moved = 0
        self.setMinimumSize(640, 420)
        self.setFocusPolicy(Qt.StrongFocus)

    def initializeGL(self) -> None:
        glClearColor(0.08, 0.09, 0.11, 1.0)
        glEnable(GL_DEPTH_TEST)
        glEnable(GL_NORMALIZE)
        glEnable(GL_LIGHTING)
        glEnable(GL_LIGHT0)
        glEnable(GL_COLOR_MATERIAL)
        glColorMaterial(GL_FRONT_AND_BACK, GL_AMBIENT_AND_DIFFUSE)
        glLightfv(GL_LIGHT0, GL_POSITION, (0.4, 0.8, 1.0, 0.0))
        glLightfv(GL_LIGHT0, GL_DIFFUSE, (1.0, 1.0, 1.0, 1.0))

    def set_mesh(self, mesh: Mesh, version: int, bad_faces: set[int] | None = None) -> None:
        self._selected = {i for i in self._selected if i < len(mesh.faces)}
        self._bad = {i for i in (bad_faces or set()) if i < len(mesh.faces)}
        snapshot = _make_snapshot(
            mesh, version, frozenset(self._selected), frozenset(self._bad)
        )
        self._apply_camera_bounds(snapshot)
        self._snapshot = snapshot
        self.update()

    def set_selection(self, faces: set[int]) -> None:
        snapshot = self._snapshot
        if snapshot is None:
            self._selected = set(faces)
            return
        self._selected = {i for i in faces if 0 <= i < len(snapshot.face_colors)}
        self._snapshot = RenderSnapshot(
            version=snapshot.version,
            vertices=snapshot.vertices,
            triangles=snapshot.triangles,
            face_ids=snapshot.face_ids,
            normals=snapshot.normals,
            face_colors=snapshot.face_colors,
            selected_faces=frozenset(self._selected),
            bad_faces=snapshot.bad_faces,
            bbox_min=snapshot.bbox_min,
            bbox_max=snapshot.bbox_max,
        )
        self.selectionChanged.emit(sorted(self._selected))
        self.update()

    def set_bad_faces(self, faces: set[int]) -> None:
        snapshot = self._snapshot
        if snapshot is None:
            return
        self._bad = {i for i in faces if 0 <= i < len(snapshot.face_colors)}
        self._snapshot = RenderSnapshot(
            version=snapshot.version,
            vertices=snapshot.vertices,
            triangles=snapshot.triangles,
            face_ids=snapshot.face_ids,
            normals=snapshot.normals,
            face_colors=snapshot.face_colors,
            selected_faces=snapshot.selected_faces,
            bad_faces=frozenset(self._bad),
            bbox_min=snapshot.bbox_min,
            bbox_max=snapshot.bbox_max,
        )
        self.update()

    def selected_faces(self) -> set[int]:
        return set(self._selected)

    def fit_view(self) -> None:
        if self._snapshot is not None:
            self._apply_camera_bounds(self._snapshot, force=True)
            self.update()

    def _apply_camera_bounds(self, snapshot: RenderSnapshot, *, force: bool = False) -> None:
        center = (snapshot.bbox_min + snapshot.bbox_max) * 0.5
        radius = max(0.05, float(np.linalg.norm(snapshot.bbox_max - snapshot.bbox_min)) * 0.5)
        if force or self._distance <= 0.0:
            self._center = center
            self._radius = radius
            self._distance = radius * 3.0
        elif not np.allclose(center, self._center):
            self._center = center
            self._radius = radius

    def _eye(self) -> np.ndarray:
        cp = math.cos(self._pitch)
        direction = np.asarray(
            [cp * math.sin(self._yaw), math.sin(self._pitch), cp * math.cos(self._yaw)]
        )
        return self._center + direction * self._distance

    def paintGL(self) -> None:
        width = max(1, self.width())
        height = max(1, self.height())
        glViewport(0, 0, width, height)
        glClear(GL_COLOR_BUFFER_BIT | GL_DEPTH_BUFFER_BIT)
        snapshot = self._snapshot
        if snapshot is None or len(snapshot.triangles) == 0:
            return
        eye = self._eye()
        glMatrixMode(GL_PROJECTION)
        glLoadIdentity()
        near = max(0.001, self._radius * 0.001)
        far = self._radius * 1000.0
        top = near * math.tan(math.radians(45.0) * 0.5)
        right = top * (width / height)
        glFrustum(-right, right, -top, top, near, far)
        glMatrixMode(GL_MODELVIEW)
        glLoadIdentity()
        forward = self._center - eye
        forward /= np.linalg.norm(forward)
        side = np.cross(forward, np.asarray([0.0, 1.0, 0.0]))
        if np.linalg.norm(side) < 1e-8:
            side = np.asarray([1.0, 0.0, 0.0])
        side /= np.linalg.norm(side)
        up = np.cross(side, forward)
        view = np.asarray(
            [
                [side[0], side[1], side[2], 0.0],
                [up[0], up[1], up[2], 0.0],
                [-forward[0], -forward[1], -forward[2], 0.0],
                [
                    -float(np.dot(side, eye)),
                    -float(np.dot(up, eye)),
                    float(np.dot(forward, eye)),
                    1.0,
                ],
            ],
            dtype=np.float64,
        )
        glMultMatrixd(view.reshape(-1))
        glBegin(GL_TRIANGLES)
        for triangle_id, (a, b, c) in enumerate(snapshot.triangles.tolist()):
            face_id = int(snapshot.face_ids[triangle_id])
            if face_id in snapshot.bad_faces:
                color = (0.95, 0.16, 0.12)
            elif face_id in snapshot.selected_faces:
                color = (1.0, 0.58, 0.08)
            else:
                color = tuple(float(x) for x in snapshot.face_colors[face_id])
            glColor3f(*color)
            n = snapshot.normals[triangle_id]
            glNormal3f(float(n[0]), float(n[1]), float(n[2]))
            for index in (a, b, c):
                vertex = snapshot.vertices[index]
                glVertex3f(float(vertex[0]), float(vertex[1]), float(vertex[2]))
        glEnd()

    def resizeGL(self, width: int, height: int) -> None:
        glViewport(0, 0, max(1, width), max(1, height))

    def mousePressEvent(self, event) -> None:  # type: ignore[no-untyped-def]
        self._last_mouse = event.pos()
        self._mouse_moved = 0
        event.accept()

    def mouseMoveEvent(self, event) -> None:  # type: ignore[no-untyped-def]
        if event.buttons() & Qt.LeftButton:
            dx = event.x() - self._last_mouse.x()
            dy = event.y() - self._last_mouse.y()
            self._mouse_moved += abs(dx) + abs(dy)
            self._yaw += dx * 0.007
            self._pitch = max(-1.45, min(1.45, self._pitch + dy * 0.007))
            self._last_mouse = event.pos()
            self.update()
        event.accept()

    def mouseReleaseEvent(self, event) -> None:  # type: ignore[no-untyped-def]
        if event.button() == Qt.LeftButton and self._mouse_moved < 5:
            face_id = self._pick_face(event.x(), event.y())
            if face_id is not None:
                modifiers = event.modifiers()
                if modifiers & Qt.ControlModifier and self._snapshot is not None:
                    # Group selection is resolved by the main window from face id.
                    self.selectionChanged.emit([face_id, "group"])
                elif modifiers & Qt.ShiftModifier:
                    selected = set(self._selected)
                    if face_id in selected:
                        selected.remove(face_id)
                    else:
                        selected.add(face_id)
                    self.set_selection(selected)
                else:
                    self.set_selection({face_id})
        event.accept()

    def wheelEvent(self, event) -> None:  # type: ignore[no-untyped-def]
        steps = event.angleDelta().y() / 120.0
        self._distance *= math.exp(-steps * 0.12)
        self._distance = max(self._radius * 0.05, min(self._radius * 200.0, self._distance))
        self.update()
        event.accept()

    def _pick_face(self, x: int, y: int) -> int | None:
        snapshot = self._snapshot
        if snapshot is None or len(snapshot.triangles) == 0:
            return None
        width = max(1, self.width())
        height = max(1, self.height())
        ndc_x = (2.0 * x / width) - 1.0
        ndc_y = 1.0 - (2.0 * y / height)
        aspect = width / height
        eye = self._eye()
        forward = self._center - eye
        forward /= np.linalg.norm(forward)
        right = np.cross(forward, np.asarray([0.0, 1.0, 0.0]))
        if np.linalg.norm(right) < 1e-8:
            right = np.asarray([1.0, 0.0, 0.0])
        right /= np.linalg.norm(right)
        up = np.cross(right, forward)
        tan_half = math.tan(math.radians(45.0) * 0.5)
        direction = forward + right * (ndc_x * tan_half * aspect) + up * (ndc_y * tan_half)
        direction /= np.linalg.norm(direction)
        vertices = snapshot.vertices[snapshot.triangles]
        v0 = vertices[:, 0, :]
        v1 = vertices[:, 1, :]
        v2 = vertices[:, 2, :]
        e1 = v1 - v0
        e2 = v2 - v0
        h = np.cross(direction, e2)
        det = np.sum(e1 * h, axis=1)
        valid = np.abs(det) > 1e-12
        inv_det = np.zeros_like(det)
        inv_det[valid] = 1.0 / det[valid]
        s = eye - v0
        u = inv_det * np.sum(s * h, axis=1)
        q = np.cross(s, e1)
        v = inv_det * np.sum(direction * q, axis=1)
        t = inv_det * np.sum(e2 * q, axis=1)
        valid &= (u >= 0.0) & (v >= 0.0) & (u + v <= 1.0) & (t > 0.0)
        if not np.any(valid):
            return None
        candidates = np.where(valid, t, np.inf)
        triangle_id = int(np.argmin(candidates))
        return int(snapshot.face_ids[triangle_id])


class EditWorker(QObject):
    progress = pyqtSignal(float)
    committed = pyqtSignal(int, object)
    cancelled = pyqtSignal()
    failed = pyqtSignal(object)
    finished = pyqtSignal()

    def __init__(
        self,
        repo: VersionRepository,
        *,
        base_version: int,
        operation: str,
        parameters: dict[str, Any],
        weld_tolerance: float,
    ) -> None:
        super().__init__()
        self.repo = repo
        self.base_version = int(base_version)
        self.operation_name = operation
        self.parameters = parameters
        self.weld_tolerance = float(weld_tolerance)
        self.cancel_event = threading.Event()

    def cancel(self) -> None:
        self.cancel_event.set()

    @pyqtSlot()
    def run(self) -> None:
        try:
            mesh, entry = self.repo.load_current()
            if int(entry["version"]) != self.base_version:
                raise StaleVersionError(
                    code="E_VERSION_STALE",
                    message="the committed head moved before the background edit started",
                )
            if self.cancel_event.is_set():
                raise EditCancelled()
            self.progress.emit(0.03)
            if self.operation_name == "transform":
                result = transform_selection(
                    mesh,
                    Selection(face_indices=tuple(self.parameters["faces"])),
                    translation=self.parameters.get("translation", (0.0, 0.0, 0.0)),
                    rotation_degrees=self.parameters.get("rotation", (0.0, 0.0, 0.0)),
                    scale=self.parameters.get("scale", 1.0),
                )
                self.progress.emit(0.9)
            elif self.operation_name == "simplify":
                result = simplify_edge_collapse(
                    mesh,
                    target_ratio=float(self.parameters.get("ratio", 0.8)),
                    progress=lambda value: self.progress.emit(0.05 + value * 0.9),
                    cancelled=self.cancel_event.is_set,
                )
            else:
                raise ValueError(f"unknown operation {self.operation_name!r}")
            if self.cancel_event.is_set():
                raise EditCancelled()
            self.progress.emit(0.96)
            committed = self.repo.commit_edit(
                self.base_version,
                result.mesh,
                metrics_before=result.metrics_before,
                metrics_after=result.metrics_after,
                operation=result.operation,
                weld_tolerance=self.weld_tolerance,
            )
            self.committed.emit(int(committed["version"]), committed)
        except EditCancelled:
            self.cancelled.emit()
        except MeshEditorError as exc:
            self.failed.emit(exc.to_dict())
        except Exception as exc:
            self.failed.emit({"code": "E_INTERNAL", "message": f"{type(exc).__name__}: {exc}"})
        finally:
            self.finished.emit()


class MainWindow(QMainWindow):
    def __init__(
        self,
        repo: VersionRepository,
        data_dir: Path,
        backend: BackendInfo,
        *,
        smoke_test: bool = False,
    ) -> None:
        super().__init__()
        self.repo = repo
        self.data_dir = data_dir
        self.backend = backend
        self.smoke_test = smoke_test
        self.current_entry: dict[str, Any] | None = None
        self.current_mesh: Mesh | None = None
        self.truth_mesh: Mesh | None = None
        self.thread: QThread | None = None
        self.worker: EditWorker | None = None
        self.progress_dialog: QProgressDialog | None = None
        self.edit_started = 0.0
        self.close_requested = False
        self.edit_timer = QTimer(self)
        self.edit_timer.setInterval(50)
        self.edit_timer.timeout.connect(self._check_progress_threshold)

        self.setWindowTitle(f"Mesh Editor | renderer: {backend.name}")
        self.resize(1180, 760)
        self.viewport = MeshViewport(self)
        self.viewport.selectionChanged.connect(self._on_selection_changed)
        self.setCentralWidget(self.viewport)
        self._build_actions()
        self._build_dock()
        self.statusBar().showMessage(f"Renderer: {backend.label()}")
        self._recover_and_load()

    def _build_actions(self) -> None:
        toolbar = self.addToolBar("Main toolbar")
        toolbar.setMovable(False)
        import_action = QAction("Import mesh", self)
        import_action.triggered.connect(self._import_mesh)
        toolbar.addAction(import_action)
        fit_action = QAction("Fit view", self)
        fit_action.triggered.connect(self.viewport.fit_view)
        toolbar.addAction(fit_action)
        undo_action = QAction("Undo last commit", self)
        undo_action.triggered.connect(self._undo)
        toolbar.addAction(undo_action)
        recover_action = QAction("Verify & recover", self)
        recover_action.triggered.connect(self._recover_and_load)
        toolbar.addAction(recover_action)

    def _build_dock(self) -> None:
        dock = QDockWidget("Edit & versions", self)
        dock.setAllowedAreas(Qt.LeftDockWidgetArea | Qt.RightDockWidgetArea)
        panel = QWidget()
        layout = QVBoxLayout(panel)

        self.version_label = QLabel("Version: -")
        self.version_label.setTextInteractionFlags(Qt.TextSelectableByMouse)
        layout.addWidget(self.version_label)
        self.metrics_view = QPlainTextEdit()
        self.metrics_view.setReadOnly(True)
        self.metrics_view.setMaximumHeight(180)
        layout.addWidget(self.metrics_view)

        selection_group = QGroupBox("Selection")
        selection_layout = QVBoxLayout(selection_group)
        self.selection_label = QLabel("Selected faces: 0")
        selection_layout.addWidget(self.selection_label)
        row = QHBoxLayout()
        clear_button = QPushButton("Clear selection")
        clear_button.clicked.connect(lambda: self.viewport.set_selection(set()))
        all_button = QPushButton("Select all")
        all_button.clicked.connect(self._select_all)
        row.addWidget(clear_button)
        row.addWidget(all_button)
        selection_layout.addLayout(row)
        layout.addWidget(selection_group)

        transform_group = QGroupBox("Transform selection")
        transform_form = QFormLayout(transform_group)
        self.translation = [QDoubleSpinBox() for _ in range(3)]
        self.rotation = [QDoubleSpinBox() for _ in range(3)]
        for label, widgets in (("Translate", self.translation), ("Rotate deg", self.rotation)):
            row = QHBoxLayout()
            for widget in widgets:
                widget.setRange(-10000.0, 10000.0)
                widget.setDecimals(4)
                row.addWidget(widget)
            transform_form.addRow(label, row)
        self.scale_spin = QDoubleSpinBox()
        self.scale_spin.setRange(0.0001, 10000.0)
        self.scale_spin.setValue(1.0)
        self.scale_spin.setDecimals(5)
        transform_form.addRow("Scale", self.scale_spin)
        self.transform_button = QPushButton("Apply transform in background")
        self.transform_button.clicked.connect(self._apply_transform)
        transform_form.addRow(self.transform_button)
        layout.addWidget(transform_group)

        simplify_group = QGroupBox("Edge-collapse simplification")
        simplify_form = QFormLayout(simplify_group)
        self.ratio_spin = QDoubleSpinBox()
        self.ratio_spin.setRange(0.05, 0.99)
        self.ratio_spin.setValue(0.8)
        self.ratio_spin.setSingleStep(0.05)
        simplify_form.addRow("Target face ratio", self.ratio_spin)
        self.simplify_button = QPushButton("Simplify in background")
        self.simplify_button.clicked.connect(self._apply_simplify)
        simplify_form.addRow(self.simplify_button)
        layout.addWidget(simplify_group)

        version_group = QGroupBox("Versions")
        version_layout = QFormLayout(version_group)
        self.rollback_combo = QComboBox()
        version_layout.addRow("Roll back to", self.rollback_combo)
        rollback_button = QPushButton("Rollback and recompute dependencies")
        rollback_button.clicked.connect(self._rollback)
        version_layout.addRow(rollback_button)
        layout.addWidget(version_group)

        export_group = QGroupBox("Export")
        export_form = QFormLayout(export_group)
        self.format_combo = QComboBox()
        self.format_combo.addItems(["obj", "stl"])
        export_form.addRow("Format", self.format_combo)
        self.format_version_combo = QComboBox()
        self.format_version_combo.addItems(["v2", "v1"])
        export_form.addRow("Format version", self.format_version_combo)
        self.ascii_stl_check = QCheckBox("ASCII STL")
        export_form.addRow("", self.ascii_stl_check)
        self.allow_lossy_check = QCheckBox("Allow loss and report E_VERSION_LOSS")
        export_form.addRow("", self.allow_lossy_check)
        self.verify_export_check = QCheckBox("Bidirectional precision check and highlight")
        self.verify_export_check.setChecked(True)
        export_form.addRow("", self.verify_export_check)
        self.export_button = QPushButton("Export current committed version")
        self.export_button.clicked.connect(self._export)
        export_form.addRow(self.export_button)
        layout.addWidget(export_group)

        self.weld_spin = QDoubleSpinBox()
        self.weld_spin.setDecimals(8)
        self.weld_spin.setRange(0.0, 1000.0)
        self.weld_spin.setValue(1e-6)
        layout.addWidget(QLabel("Import weld tolerance"))
        layout.addWidget(self.weld_spin)
        layout.addStretch(1)
        dock.setWidget(panel)
        self.addDockWidget(Qt.LeftDockWidgetArea, dock)

    def _ensure_workspace(self) -> None:
        if self.repo.manifest_exists():
            return
        sample = self.data_dir / "samples/part_v2.obj"
        if not sample.exists():
            generate_project_samples(self.data_dir.parent)
        mesh = parse_mesh(sample)
        self.truth_mesh = mesh
        welded = weld_vertices(mesh, 1e-6)
        self.repo.create_baseline(
            welded.mesh,
            weld_tolerance=1e-6,
            operation={"type": "baseline", "source": str(sample)},
        )

    def _recover_and_load(self) -> None:
        try:
            self._ensure_workspace()
            report = self.repo.recover()
            if self.truth_mesh is None:
                self.truth_mesh = self._initial_truth_mesh()
            self._refresh_current()
            self.statusBar().showMessage(
                f"Renderer: {self.backend.label()} | recovered committed v{report.recovered_version}; "
                f"discarded {report.discarded_staging} staging snapshot(s)"
            )
        except RecoveryError as exc:
            QMessageBox.critical(
                self,
                exc.code,
                f"{exc.message}\nRecovery aborted. No exit report is promised for an interrupted edit.",
            )
            self.statusBar().showMessage(f"{exc.code}: {exc.message}")
            self._set_edit_enabled(False)
        except Exception as exc:
            QMessageBox.critical(self, "E_RECOVERY_INVALID", str(exc))
            self._set_edit_enabled(False)

    def _initial_truth_mesh(self) -> Mesh | None:
        try:
            manifest = self.repo.read_manifest()
            source = manifest["versions"][0].get("operation", {}).get("source")
            if not source:
                return self.repo.load_version(1)
            source_path = Path(str(source))
            if not source_path.is_absolute():
                source_path = self.data_dir.parent / source_path
            if source_path.is_file():
                return parse_mesh(source_path)
        except Exception:
            pass
        return self.repo.load_version(1)

    def _refresh_current(self) -> None:
        mesh, entry = self.repo.load_current()
        self.current_mesh = mesh
        self.current_entry = entry
        if self.truth_mesh is None:
            self.truth_mesh = self.repo.load_version(1)
        self.viewport.set_selection(set())
        self.viewport.set_mesh(mesh, int(entry["version"]))
        self.version_label.setText(
            f"Version: v{entry['version']} | Next: v{self.repo.read_manifest()['next_version']}\n"
            f"{entry['content_hash']}"
        )
        self.metrics_view.setPlainText(
            json.dumps(entry["metrics_after"], sort_keys=True, indent=2, ensure_ascii=False)
        )
        self._populate_versions()
        self._set_edit_enabled(True)

    def _populate_versions(self) -> None:
        manifest = self.repo.read_manifest()
        current = int(manifest["current_version"])
        self.rollback_combo.blockSignals(True)
        self.rollback_combo.clear()
        for entry in manifest["versions"]:
            label = f"v{entry['version']} ({entry['content_hash'][7:17]})"
            self.rollback_combo.addItem(label, int(entry["version"]))
            if int(entry["version"]) == current:
                self.rollback_combo.setCurrentIndex(self.rollback_combo.count() - 1)
        self.rollback_combo.blockSignals(False)

    def _set_edit_enabled(self, enabled: bool) -> None:
        for widget in (
            self.transform_button,
            self.simplify_button,
            self.export_button,
        ):
            widget.setEnabled(enabled)
        self.rollback_combo.setEnabled(enabled)

    def _select_all(self) -> None:
        if self.current_mesh is not None:
            self.viewport.set_selection(set(range(len(self.current_mesh.faces))))

    def _on_selection_changed(self, value: object) -> None:
        if isinstance(value, list) and value and value[-1] == "group" and self.current_mesh is not None:
            face_id = int(value[0])
            group = self.current_mesh.faces[face_id].group
            selected = {
                index
                for index, face in enumerate(self.current_mesh.faces)
                if face.group == group
            }
            self.viewport.set_selection(selected)
            return
        faces = set(int(i) for i in value)
        self.selection_label.setText(f"Selected faces: {len(faces)}")

    def _apply_transform(self) -> None:
        faces = sorted(self.viewport.selected_faces())
        if not faces:
            QMessageBox.information(self, "Empty selection", "Click a face first; use Shift to add faces.")
            return
        parameters = {
            "faces": faces,
            "translation": [spin.value() for spin in self.translation],
            "rotation": [spin.value() for spin in self.rotation],
            "scale": self.scale_spin.value(),
        }
        self._start_background_edit("transform", parameters)

    def _apply_simplify(self) -> None:
        self._start_background_edit("simplify", {"ratio": self.ratio_spin.value()})

    def _start_background_edit(self, operation: str, parameters: dict[str, Any]) -> None:
        if self.current_entry is None or self.thread is not None:
            return
        base_version = int(self.current_entry["version"])
        self.worker = EditWorker(
            self.repo,
            base_version=base_version,
            operation=operation,
            parameters=parameters,
            weld_tolerance=self.weld_spin.value(),
        )
        self.thread = QThread(self)
        self.worker.moveToThread(self.thread)
        self.thread.started.connect(self.worker.run)
        self.worker.progress.connect(self._on_worker_progress)
        self.worker.committed.connect(self._on_worker_committed)
        self.worker.cancelled.connect(self._on_worker_cancelled)
        self.worker.failed.connect(self._on_worker_failed)
        self.worker.finished.connect(self.thread.quit)
        self.worker.finished.connect(self.worker.deleteLater)
        self.thread.finished.connect(self._on_thread_finished)
        self.thread.finished.connect(self.thread.deleteLater)
        self._set_edit_enabled(False)
        self.edit_started = time.monotonic()
        self.edit_timer.start()
        self.statusBar().showMessage(
            f"Background edit running; viewport keeps committed v{base_version}. "
            f"Progress appears after {EDIT_PROGRESS_THRESHOLD_SECONDS:.2f}s"
        )
        self.thread.start()

    def _on_worker_progress(self, value: float) -> None:
        if self.progress_dialog is not None:
            self.progress_dialog.setValue(max(0, min(100, int(value * 100))))

    def _check_progress_threshold(self) -> None:
        if self.worker is None or self.progress_dialog is not None:
            return
        if time.monotonic() - self.edit_started < EDIT_PROGRESS_THRESHOLD_SECONDS:
            return
        dialog = QProgressDialog(
            "Editing in background; the viewport still shows the last committed version.",
            "Cancel",
            0,
            100,
            self,
        )
        dialog.setWindowTitle("Mesh edit progress")
        dialog.setAutoClose(False)
        dialog.setAutoReset(False)
        dialog.setMinimumDuration(0)
        dialog.canceled.connect(self.worker.cancel)
        self.progress_dialog = dialog
        dialog.show()

    def _close_progress_dialog(self) -> None:
        self.edit_timer.stop()
        if self.progress_dialog is not None:
            self.progress_dialog.close()
            self.progress_dialog.deleteLater()
            self.progress_dialog = None

    def _on_worker_committed(self, version: int, entry: object) -> None:
        self._close_progress_dialog()
        self._refresh_current()
        self.selection_label.setText("Selected faces: 0")
        self.statusBar().showMessage(f"Atomically committed v{version}; render snapshot replaced as a whole")

    def _on_worker_cancelled(self) -> None:
        self._close_progress_dialog()
        current = self.current_entry["version"] if self.current_entry else "?"
        self.statusBar().showMessage(
            f"Edit cancelled; temporary buffers released. Committed v{current} remains readable"
        )

    def _on_worker_failed(self, error: object) -> None:
        self._close_progress_dialog()
        details = error if isinstance(error, dict) else {"message": str(error)}
        QMessageBox.warning(
            self,
            str(details.get("code", "E_EDIT_FAILED")),
            str(details.get("message", details)),
        )
        self.statusBar().showMessage(
            f"{details.get('code', 'E_EDIT_FAILED')}: {details.get('message', '')}"
        )

    def _on_thread_finished(self) -> None:
        self.worker = None
        self.thread = None
        self._close_progress_dialog()
        self._set_edit_enabled(True)
        if self.close_requested:
            QTimer.singleShot(0, self.close)

    def _undo(self) -> None:
        if self.thread is not None:
            return
        try:
            entry = self.repo.undo_last_commit()
            self._refresh_current()
            self.viewport.set_selection(set())
            self.statusBar().showMessage(
                f"Undo moved the head to existing v{entry['version']}; snapshot IDs unchanged, dependency cache cleared"
            )
        except MeshEditorError as exc:
            QMessageBox.warning(self, exc.code, exc.message)

    def _rollback(self) -> None:
        if self.thread is not None or self.rollback_combo.currentData() is None:
            return
        version = int(self.rollback_combo.currentData())
        try:
            entry = self.repo.rollback_to(version)
            self._refresh_current()
            self.viewport.set_selection(set())
            self.statusBar().showMessage(
                f"Rolled back to v{entry['version']}; the next edit recomputes dependencies from this commit"
            )
        except MeshEditorError as exc:
            QMessageBox.warning(self, exc.code, exc.message)

    def _import_mesh(self) -> None:
        if self.thread is not None:
            return
        path, _ = QFileDialog.getOpenFileName(
            self, "Import surface mesh", str(self.data_dir / "samples"), "Mesh files (*.obj *.stl)"
        )
        if not path:
            return
        try:
            mesh = parse_mesh(path)
            self.truth_mesh = mesh
            welded = weld_vertices(mesh, self.weld_spin.value())
            current_mesh, current = self.repo.load_current()
            imported = welded.mesh
            entry = self.repo.commit_edit(
                current["version"],
                imported,
                metrics_before=current_mesh.metrics(),
                metrics_after=imported.metrics(),
                operation={
                    "type": "import_mesh",
                    "source": Path(path).name,
                    "source_format": mesh.metadata.get("source_format"),
                    "source_format_version": mesh.metadata.get("format_version"),
                    "weld_tolerance": self.weld_spin.value(),
                    "weld_report": welded.report(),
                },
                weld_tolerance=self.weld_spin.value(),
            )
            self._refresh_current()
            self.statusBar().showMessage(f"Import committed as v{entry['version']}")
        except MeshEditorError as exc:
            location = ""
            if exc.line is not None:
                location = f" line {exc.line}"
            if exc.offset is not None:
                location += f" byte {exc.offset}"
            QMessageBox.critical(self, exc.code, f"{exc.message}{location}")
        except Exception as exc:
            QMessageBox.critical(self, "E_PARSE_INVALID", str(exc))

    def _export(self) -> None:
        if self.current_entry is None:
            return
        fmt = self.format_combo.currentText()
        version = 1 if self.format_version_combo.currentText() == "v1" else 2
        suffix = f".{fmt}"
        path, _ = QFileDialog.getSaveFileName(
            self,
            "Export committed version",
            str(self.data_dir / f"export_v{self.current_entry['version']}{suffix}"),
            f"{fmt.upper()} (*{suffix})",
        )
        if not path:
            return
        try:
            report = self.repo.export_version(
                int(self.current_entry["version"]),
                path,
                fmt,
                version,
                ascii_stl=self.ascii_stl_check.isChecked(),
                allow_lossy=self.allow_lossy_check.isChecked(),
            )
        except VersionLossError as exc:
            QMessageBox.warning(
                self,
                exc.code,
                f"{exc.message}\nAffected faces: {exc.faces[:80]}"
                + (" ..." if len(exc.faces) > 80 else "")
                + "\nEnable 'Allow loss' to export and record this error code.",
            )
            return
        except MeshEditorError as exc:
            QMessageBox.critical(self, exc.code, exc.message)
            return
        self.statusBar().showMessage(
            f"Exported {path}; {report.code}; {report.bytes_written} bytes"
        )
        if not self.verify_export_check.isChecked():
            return
        try:
            truth = self.truth_mesh or self.repo.load_version(1)
            candidate = parse_mesh(path)
            precision = compare_meshes(
                truth, candidate, tolerance=1e-4, normal_tolerance_deg=2.0
            )
            report_path = (
                self.data_dir.parent
                / f"logs/precision_v{self.current_entry['version']}_{fmt}_v{version}.json"
            )
            write_precision_report(precision, report_path)
            self.viewport.set_bad_faces(set(precision["candidate_bad_faces"]))
            QMessageBox.information(
                self,
                precision["code"],
                summarize_precision(precision) + f"\nReport: {report_path}",
            )
        except Exception as exc:
            QMessageBox.warning(self, "E_PRECISION_CHECK", str(exc))

    def closeEvent(self, event) -> None:  # type: ignore[no-untyped-def]
        if self.thread is None:
            event.accept()
            return
        self.close_requested = True
        if self.worker is not None:
            self.worker.cancel()
        self.statusBar().showMessage("Cancelling background edit before closing...")
        event.ignore()


def run_editor(
    *,
    workspace: str | Path,
    data_dir: str | Path,
    backend: str = "auto",
    smoke_test: bool = False,
) -> int:
    if smoke_test:
        os.environ.setdefault("QT_QPA_PLATFORM", "offscreen")
    backend_info = configure_render_backend(backend)
    print(f"RENDER_BACKEND={backend_info.name}; {backend_info.detail}", flush=True)
    fmt = QSurfaceFormat.defaultFormat()
    fmt.setVersion(2, 1)
    fmt.setProfile(QSurfaceFormat.CompatibilityProfile)
    fmt.setDepthBufferSize(24)
    QSurfaceFormat.setDefaultFormat(fmt)
    app = QApplication.instance() or QApplication([])
    app.setApplicationName("Versioned Mesh Editor")
    window = MainWindow(
        VersionRepository(Path(workspace)),
        Path(data_dir),
        backend_info,
        smoke_test=smoke_test,
    )
    window.show()
    if smoke_test:
        print("UI_SMOKE_READY", flush=True)
        QTimer.singleShot(1200, app.quit)
    return int(app.exec_())
