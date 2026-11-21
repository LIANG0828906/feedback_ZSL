"""Optional PyQt5 workbench.

The deterministic core never imports Qt.  This module is loaded lazily by the
``gui`` subcommand and can be smoke-tested with ``QT_QPA_PLATFORM=offscreen``.
"""

from __future__ import annotations

import json
import os
import threading
from pathlib import Path
from typing import Any, Sequence

from .camera import Camera
from .errors import E_CANCELLED, E_GUI_UNAVAILABLE, PickError
from .generator import build_dataset
from .model import Scene
from .picking import PickResult, pick_from_store
from .placement import PlacementResult, check_placement
from .scene_io import decode_scene
from .versioning import RegionSnapshot, VersionStore


def _qt_modules() -> tuple[Any, Any, Any]:
    try:
        from PyQt5 import QtCore, QtGui, QtWidgets
    except ImportError as exc:
        raise PickError(
            code=E_GUI_UNAVAILABLE,
            message="PyQt5 is not installed; the scripted core remains available",
            location={"field": "PyQt5", "error": str(exc)},
        ) from exc
    return QtCore, QtGui, QtWidgets


def _load_workbench_sample(
    samples_dir: str | Path,
) -> tuple[Scene, Camera, dict[str, Any], str]:
    samples_path = Path(samples_dir)
    manifest_path = samples_path / "manifest.json"
    if manifest_path.exists():
        try:
            manifest = json.loads(manifest_path.read_text("utf-8"))
            first = manifest["samples"][0]
            scene_bytes = (samples_path / first["scene_file"]).read_bytes()
            scenario = json.loads(
                (samples_path / first["scenario_file"]).read_text("utf-8")
            )
            return (
                decode_scene(scene_bytes, source=str(first["scene_file"])),
                Camera.from_dict(scenario["camera"]).validate(),
                scenario,
                str(scenario.get("sample_id", "sample")),
            )
        except (OSError, KeyError, TypeError, json.JSONDecodeError):
            pass
    bundle = build_dataset(7)
    scene_name = next(name for name in bundle.sample_files if name.endswith(".tpscene"))
    scenario_name = next(
        name for name in bundle.sample_files if name.endswith(".scenario.json")
    )
    scenario = json.loads(bundle.sample_files[scenario_name].decode("utf-8"))
    return (
        decode_scene(bundle.sample_files[scene_name], source="<memory>"),
        Camera.from_dict(scenario["camera"]).validate(),
        scenario,
        str(scenario.get("sample_id", "sample")),
    )


def run_gui(
    samples_dir: str | Path,
    baselines_dir: str | Path,
    *,
    smoke_ms: int | None = None,
) -> int:
    if "QT_QPA_PLATFORM" not in os.environ and os.environ.get("WAYLAND_DISPLAY"):
        os.environ["QT_QPA_PLATFORM"] = "wayland"
    QtCore, QtGui, QtWidgets = _qt_modules()

    class BattlefieldCanvas(QtWidgets.QWidget):
        picked = QtCore.pyqtSignal(object, object)

        def __init__(self) -> None:
            super().__init__()
            self.setMinimumSize(720, 520)
            self.snapshot: RegionSnapshot | None = None
            self.pick_result: PickResult | None = None
            self.placement_result: PlacementResult | None = None
            self.selected_unit = 10
            self.store: VersionStore | None = None
            self.camera: Camera | None = None
            self._scale = 1.0
            self._offset = (0.0, 0.0)

        def set_state(
            self,
            snapshot: RegionSnapshot,
            store: VersionStore,
            camera: Camera,
        ) -> None:
            self.snapshot = snapshot
            self.store = store
            self.camera = camera
            self.update()

        def _compute_transform(self) -> None:
            assert self.snapshot is not None
            grid = self.snapshot.scene.grid
            margin = 34.0
            available_w = max(1.0, self.width() - margin * 2)
            available_h = max(1.0, self.height() - margin * 2)
            world_w = max(grid.cell_size, grid.maximum_x - grid.origin_x)
            world_h = max(grid.cell_size, grid.maximum_y - grid.origin_y)
            self._scale = min(available_w / world_w, available_h / world_h)
            self._offset = (
                margin + (available_w - world_w * self._scale) * 0.5,
                margin + (available_h - world_h * self._scale) * 0.5,
            )

        def _to_canvas(self, x: float, y: float) -> tuple[float, float]:
            assert self.snapshot is not None
            grid = self.snapshot.scene.grid
            return (
                self._offset[0] + (x - grid.origin_x) * self._scale,
                self._offset[1] + (y - grid.origin_y) * self._scale,
            )

        def _to_world(self, x: float, y: float) -> tuple[float, float]:
            assert self.snapshot is not None
            grid = self.snapshot.scene.grid
            return (
                grid.origin_x + (x - self._offset[0]) / self._scale,
                grid.origin_y + (y - self._offset[1]) / self._scale,
            )

        def paintEvent(self, event: Any) -> None:
            del event
            painter = QtGui.QPainter(self)
            try:
                painter.fillRect(self.rect(), QtGui.QColor("#10151d"))
                if self.snapshot is None:
                    return
                self._compute_transform()
                scene = self.snapshot.scene
                grid = scene.grid
                for row in range(grid.rows):
                    for col in range(grid.cols):
                        x0, y0 = self._to_canvas(
                            grid.origin_x + col * grid.cell_size,
                            grid.origin_y + row * grid.cell_size,
                        )
                        size = grid.cell_size * self._scale
                        if grid.is_walkable(row, col):
                            color = QtGui.QColor("#263442")
                        else:
                            color = QtGui.QColor("#4b2730")
                        painter.fillRect(
                            QtCore.QRectF(x0, y0, size, size),
                            color,
                        )
                        painter.setPen(QtGui.QPen(QtGui.QColor("#33475a"), 0.7))
                        painter.drawRect(QtCore.QRectF(x0, y0, size, size))

                for cover in scene.covers:
                    triangle = scene.triangle(cover)
                    points = [
                        QtCore.QPointF(*self._to_canvas(point[0], point[1]))
                        for point in triangle
                    ]
                    polygon = QtGui.QPolygonF(points)
                    painter.setBrush(QtGui.QColor(55, 112, 162, 105))
                    painter.setPen(QtGui.QPen(QtGui.QColor("#69a6d8"), 2.0))
                    painter.drawPolygon(polygon)
                    center_x, center_y = self._to_canvas(
                        sum(point[0] for point in triangle) / 3.0,
                        sum(point[1] for point in triangle) / 3.0,
                    )
                    painter.setPen(QtGui.QColor("#d9edff"))
                    painter.drawText(
                        QtCore.QRectF(center_x - 55, center_y - 10, 110, 20),
                        QtCore.Qt.AlignCenter,
                        f"cover {cover.cover_id}",
                    )

                for unit in scene.units:
                    x0, y0 = self._to_canvas(unit.minimum[0], unit.minimum[1])
                    x1, y1 = self._to_canvas(unit.maximum[0], unit.maximum[1])
                    selected = unit.unit_id == self.selected_unit
                    if unit.bounds_kind == "capsule" and unit.capsule_a is not None and unit.capsule_b is not None:
                        painter.setPen(
                            QtGui.QPen(
                                QtGui.QColor("#f4d35e" if selected else "#8ad6a0"),
                                max(2.0, unit.radius * 2.0 * self._scale),
                                QtCore.Qt.SolidLine,
                                QtCore.Qt.RoundCap,
                            )
                        )
                        ax, ay = self._to_canvas(unit.capsule_a[0], unit.capsule_a[1])
                        bx, by = self._to_canvas(unit.capsule_b[0], unit.capsule_b[1])
                        painter.drawLine(QtCore.QPointF(ax, ay), QtCore.QPointF(bx, by))
                        painter.setPen(QtGui.QPen(QtGui.QColor("#8ad6a0"), 1.4))
                        painter.drawRect(QtCore.QRectF(x0, y0, x1 - x0, y1 - y0))
                    else:
                        painter.setBrush(
                            QtGui.QColor("#d8b84a" if selected else "#66b37f")
                        )
                        painter.setPen(QtGui.QPen(QtGui.QColor("#f7f2d0"), 2.0))
                        painter.drawRect(QtCore.QRectF(x0, y0, x1 - x0, y1 - y0))
                    painter.setPen(QtGui.QColor("#f6fbff"))
                    painter.drawText(
                        QtCore.QRectF(x0 - 30, y0 - 22, 150, 20),
                        QtCore.Qt.AlignLeft,
                        f"{unit.unit_id} {unit.name}",
                    )

                if self.pick_result is not None and self.pick_result.hit.point is not None:
                    hit = self.pick_result.hit
                    px, py = self._to_canvas(hit.point[0], hit.point[1])
                    painter.setPen(QtGui.QPen(QtGui.QColor("#ff5d73"), 2.5))
                    painter.setBrush(QtGui.QColor(255, 93, 115, 55))
                    painter.drawEllipse(QtCore.QPointF(px, py), 13.0, 13.0)
                    painter.drawLine(px - 18, py, px + 18, py)
                    painter.drawLine(px, py - 18, px, py + 18)
                    painter.setPen(QtGui.QColor("#ffd4db"))
                    painter.drawText(
                        QtCore.QRectF(px + 16, py + 10, 280, 24),
                        QtCore.Qt.AlignLeft,
                        f"{hit.kind}:{hit.entity_id} v{self.pick_result.version}",
                    )

                if self.placement_result is not None:
                    placement = self.placement_result
                    color = "#4cd58a" if placement.valid else "#ff6b6b"
                    polygon_points: list[QtCore.QPointF] = []
                    # The checked vertex list is retained by the workbench
                    # through this attribute set by check_draft.
                    for x, y in getattr(self, "_draft_vertices", []):
                        polygon_points.append(QtCore.QPointF(*self._to_canvas(x, y)))
                    if polygon_points:
                        painter.setBrush(
                            QtGui.QColor(76, 213, 138, 48)
                            if placement.valid
                            else QtGui.QColor(255, 107, 107, 52)
                        )
                        painter.setPen(QtGui.QPen(QtGui.QColor(color), 2.5))
                        painter.drawPolygon(QtGui.QPolygonF(polygon_points))
                    painter.setPen(QtGui.QColor(color))
                    painter.drawText(
                        QtCore.QRectF(16, 14, self.width() - 32, 24),
                        QtCore.Qt.AlignLeft,
                        f"placement: {placement.message}",
                    )
            finally:
                painter.end()

        def mousePressEvent(self, event: Any) -> None:
            if self.snapshot is None or self.store is None or self.camera is None:
                return
            if event.button() != QtCore.Qt.LeftButton:
                return
            sx = event.x() / max(1.0, float(self.width())) * self.camera.viewport_width
            sy = event.y() / max(1.0, float(self.height())) * self.camera.viewport_height
            try:
                result = pick_from_store(self.store, self.camera, [sx, sy])
            except PickError:
                return
            self.pick_result = result
            if result.hit.kind == "unit" and result.hit.entity_id is not None:
                self.selected_unit = result.hit.entity_id
            self.picked.emit(result, self.selected_unit)
            self.update()

    class ReplayWorker(QtCore.QThread):
        progress = QtCore.pyqtSignal(object)
        finished_ok = QtCore.pyqtSignal(object)
        failed = QtCore.pyqtSignal(object)
        cancelled = QtCore.pyqtSignal()

        def __init__(
            self,
            store: VersionStore,
            camera: Camera,
            screens: Sequence[Sequence[float]],
        ) -> None:
            super().__init__()
            self.store = store
            self.camera = camera
            self.screens = list(screens)
            self.cancel_event = threading.Event()

        def cancel(self) -> None:
            self.cancel_event.set()

        def run(self) -> None:
            versions: set[int] = set()
            hashes: set[str] = set()
            try:
                for index, screen in enumerate(self.screens):
                    if self.cancel_event.is_set():
                        self.cancelled.emit()
                        return
                    result = pick_from_store(self.store, self.camera, screen)
                    versions.add(result.version)
                    hashes.add(result.content_hash)
                    self.progress.emit(
                        {
                            "index": index,
                            "total": len(self.screens),
                            "version": result.version,
                            "kind": result.hit.kind,
                            "entity_id": result.hit.entity_id,
                        }
                    )
                self.finished_ok.emit(
                    {
                        "records": len(self.screens),
                        "versions": sorted(versions),
                        "hashes": sorted(hashes),
                        "cancelled": False,
                    }
                )
            except PickError as exc:
                if exc.code == E_CANCELLED:
                    self.cancelled.emit()
                else:
                    self.failed.emit(exc.to_dict())
            except Exception as exc:  # pragma: no cover - GUI defensive boundary
                self.failed.emit(
                    {
                        "ok": False,
                        "code": "E_GUI_WORKER",
                        "message": str(exc),
                    }
                )

    class Workbench(QtWidgets.QMainWindow):
        def __init__(self) -> None:
            super().__init__()
            self.setWindowTitle("Tactical Pick Workbench")
            self.resize(1120, 760)
            scene, camera, scenario, sample_id = _load_workbench_sample(samples_dir)
            self.store = VersionStore(scene)
            self.camera = camera
            self.scenario = scenario
            self.sample_id = sample_id
            self.canvas = BattlefieldCanvas()
            self.canvas.set_state(self.store.current_snapshot(), self.store, camera)
            self.canvas.picked.connect(self._on_picked)
            self.worker: ReplayWorker | None = None
            self.draft_vertices = self._find_draft_vertices()
            self.canvas._draft_vertices = self.draft_vertices

            central = QtWidgets.QWidget()
            layout = QtWidgets.QVBoxLayout(central)
            controls = QtWidgets.QHBoxLayout()
            self.status = QtWidgets.QLabel()
            self.status.setTextInteractionFlags(QtCore.Qt.TextSelectableByMouse)
            controls.addWidget(self.status, 1)

            self.move_button = QtWidgets.QPushButton("提交批量修改")
            self.move_button.clicked.connect(self.submit_batch)
            controls.addWidget(self.move_button)

            self.placement_button = QtWidgets.QPushButton("检查草拟放置")
            self.placement_button.clicked.connect(self.check_draft)
            controls.addWidget(self.placement_button)

            self.start_button = QtWidgets.QPushButton("启动回放")
            self.start_button.clicked.connect(self.start_replay)
            controls.addWidget(self.start_button)

            self.cancel_button = QtWidgets.QPushButton("协作取消")
            self.cancel_button.clicked.connect(self.cancel_replay)
            self.cancel_button.setEnabled(False)
            controls.addWidget(self.cancel_button)

            layout.addLayout(controls)
            layout.addWidget(self.canvas, 1)
            self.setCentralWidget(central)
            self._refresh_status("ready")

        def _find_draft_vertices(self) -> list[list[float]]:
            for event in self.scenario.get("events", []):
                if event.get("type") == "placement" and event.get("label") == "placement-valid":
                    return [list(vertex) for vertex in event["vertices"]]
            grid = self.store.current_snapshot().scene.grid
            row = max(0, grid.rows - 3)
            col = max(0, grid.cols - 3)
            return [
                [grid.origin_x + col, grid.origin_y + row],
                [grid.origin_x + col + 2, grid.origin_y + row],
                [grid.origin_x + col + 2, grid.origin_y + row + 2],
                [grid.origin_x + col, grid.origin_y + row + 2],
            ]

        def _refresh_status(self, message: str) -> None:
            snapshot = self.store.current_snapshot()
            self.status.setText(
                f"{self.sample_id} | {message} | v{snapshot.version} "
                f"{snapshot.content_hash[:12]}"
            )

        def _refresh_canvas(self) -> None:
            self.canvas.selected_unit = self.canvas.selected_unit
            self.canvas.set_state(
                self.store.current_snapshot(),
                self.store,
                self.camera,
            )

        def _on_picked(self, result: PickResult, selected_unit: int) -> None:
            hit = result.hit
            self._refresh_status(
                f"pick {hit.kind}:{hit.entity_id} unit={selected_unit} "
                f"d={hit.distance if hit.distance is not None else 'none'}"
            )

        def submit_batch(self) -> None:
            unit_id = int(self.canvas.selected_unit)
            try:
                snapshot = self.store.apply_batch(
                    [
                        {
                            "op": "translate_unit",
                            "unit_id": unit_id,
                            "delta": [0.18, 0.10, 0.0],
                       
                        }
                    ]
                )
            except PickError as exc:
                self._refresh_status(f"batch failed {exc.code}")
                return
            self._refresh_canvas()
            self.canvas.pick_result = None
            self.canvas.update()
            self._refresh_status(f"batch published v{snapshot.version}")

        def check_draft(self) -> None:
            try:
                result = check_placement(self.store, self.draft_vertices)
            except PickError as exc:
                self._refresh_status(f"placement rejected {exc.code}")
                return
            self.canvas.placement_result = result
            self.canvas.update()
            self._refresh_status(
                f"placement {'valid' if result.valid else result.failure_code}"
            )

        def start_replay(self) -> None:
            if self.worker is not None and self.worker.isRunning():
                return
            screens = [
                event["screen"]
                for event in self.scenario.get("events", [])
                if event.get("type") == "pick"
            ]
            if not screens:
                screens = [[self.camera.viewport_width * 0.5, self.camera.viewport_height * 0.5]]
            self.worker = ReplayWorker(self.store, self.camera, screens)
            self.worker.progress.connect(
                lambda value: self._refresh_status(
                    f"replay {value['index'] + 1}/{value['total']} "
                    f"v{value['version']} {value['kind']}:{value['entity_id']}"
                )
            )
            self.worker.finished_ok.connect(self._replay_finished)
            self.worker.failed.connect(
                lambda value: self._refresh_status(f"replay failed {value['code']}")
            )
            self.worker.cancelled.connect(
                lambda: self._refresh_status("replay cancelled cooperatively")
            )
            self.worker.finished.connect(self._replay_thread_finished)
            self.start_button.setEnabled(False)
            self.cancel_button.setEnabled(True)
            self.worker.start()

        def cancel_replay(self) -> None:
            if self.worker is not None:
                self.worker.cancel()
                self._refresh_status("cancellation requested")

        def _replay_finished(self, value: dict[str, Any]) -> None:
            versions = ",".join(str(item) for item in value["versions"])
            self._refresh_status(
                f"replay complete {value['records']} records versions=[{versions}]"
            )

        def _replay_thread_finished(self) -> None:
            self.start_button.setEnabled(True)
            self.cancel_button.setEnabled(False)

        def closeEvent(self, event: Any) -> None:
            if self.worker is not None and self.worker.isRunning():
                self.worker.cancel()
                self.worker.wait(3000)
            event.accept()

    app = QtWidgets.QApplication.instance() or QtWidgets.QApplication([])
    window = Workbench()
    window.show()
    if smoke_ms is not None:
        QtCore.QTimer.singleShot(int(smoke_ms), app.quit)
    return int(app.exec_())
