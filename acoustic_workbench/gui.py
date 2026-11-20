from __future__ import annotations

import argparse
import copy
import os
import sys
from pathlib import Path
from typing import Any

from PySide6.QtCore import QObject, QPointF, QThread, QTimer, QUrl, Signal, Qt
from PySide6.QtGui import QColor, QFont, QPainter, QPen, QPolygonF, QBrush
from PySide6.QtMultimedia import QAudioOutput, QMediaPlayer
from PySide6.QtWidgets import (
    QApplication,
    QComboBox,
    QDoubleSpinBox,
    QFormLayout,
    QFrame,
    QHeaderView,
    QHBoxLayout,
    QLabel,
    QMainWindow,
    QMessageBox,
    QPlainTextEdit,
    QPushButton,
    QSplitter,
    QTreeWidget,
    QTreeWidgetItem,
    QVBoxLayout,
    QWidget,
)

from .cancellation import CancellationToken
from .errors import AcousticError
from .geometry import reflection_paths
from .graph import NODE_ORDER, NODE_TITLES, invalidation_plan
from .model import Point, Scene, apply_factor
from .service import AcousticEngine, RebuildReport


MATERIAL_COLORS = {
    "concrete": QColor("#7b8794"),
    "wood": QColor("#a66a3f"),
    "curtain": QColor("#7d4d7e"),
    "acoustic_panel": QColor("#3f8f7a"),
    "carpet": QColor("#8a6d58"),
}


class VenueView(QWidget):
    def __init__(self, parent: QWidget | None = None):
        super().__init__(parent)
        self.scene: Scene | None = None
        self.selected_listener = ""
        self.preview_source: tuple[float, float] | None = None
        self.setMinimumSize(520, 360)
        self.setAutoFillBackground(False)

    def set_scene(
        self,
        scene: Scene,
        selected_listener: str,
        preview_source: tuple[float, float] | None = None,
    ) -> None:
        self.scene = scene
        self.path_scene = scene
        if preview_source is not None and (
            abs(preview_source[0] - scene.source.x) > 1e-9
            or abs(preview_source[1] - scene.source.y) > 1e-9
        ):
            self.path_scene = copy.copy(scene)
            self.path_scene.source = Point(
                "source",
                preview_source[0],
                preview_source[1],
                scene.source.z,
            )
        self.selected_listener = selected_listener
        self.preview_source = preview_source
        self.update()

    def paintEvent(self, event: object) -> None:
        painter = QPainter(self)
        painter.setRenderHint(QPainter.RenderHint.Antialiasing)
        painter.fillRect(self.rect(), QColor("#eef2f3"))
        if self.scene is None:
            return
        width = max(
            [wall.x1 for wall in self.scene.walls]
            + [wall.x2 for wall in self.scene.walls]
            + [1.0]
        )
        height = max(
            [wall.y1 for wall in self.scene.walls]
            + [wall.y2 for wall in self.scene.walls]
            + [1.0]
        )
        margin = 28.0
        scale = min(
            (self.width() - 2.0 * margin) / max(width, 1.0),
            (self.height() - 2.0 * margin) / max(height, 1.0),
        )

        def mapped(x: float, y: float) -> tuple[float, float]:
            return margin + x * scale, margin + y * scale

        painter.setFont(QFont("Sans", 9))
        for wall in self.scene.walls:
            start = mapped(wall.x1, wall.y1)
            end = mapped(wall.x2, wall.y2)
            color = MATERIAL_COLORS.get(wall.material, QColor("#55606b"))
            painter.setPen(QPen(color, 8, Qt.PenStyle.SolidLine, Qt.PenCapStyle.RoundCap))
            painter.drawLine(*start, *end)
            middle = ((start[0] + end[0]) / 2.0, (start[1] + end[1]) / 2.0)
            painter.setPen(QPen(QColor("#33404a")))
            painter.drawText(
                int(middle[0] - 12),
                int(middle[1] - 10),
                f"{wall.id}:{wall.material}",
            )

        if self.selected_listener:
            for index, path in enumerate(
                reflection_paths(self.path_scene, self.selected_listener)
            ):
                points = [mapped(x, y) for x, y in path["points"]]
                painter.setPen(
                    QPen(
                        QColor(40 + index * 25, 120, 175, 135),
                        1.4,
                        Qt.PenStyle.DashLine,
                    )
                )
                painter.drawPolyline(QPolygonF([self._qpoint(x, y) for x, y in points]))

        source_x = (
            self.preview_source[0]
            if self.preview_source is not None
            else self.scene.source.x
        )
        source_y = (
            self.preview_source[1]
            if self.preview_source is not None
            else self.scene.source.y
        )
        sx, sy = mapped(source_x, source_y)
        painter.setBrush(QBrush(QColor("#d84b3f")))
        painter.setPen(QPen(QColor("#7f2019"), 2))
        painter.drawEllipse(QPointF(sx, sy), 8, 8)
        painter.setPen(QPen(QColor("#6b1712")))
        painter.drawText(int(sx + 11), int(sy + 4), "SOURCE")

        for listener in self.scene.listeners:
            lx, ly = mapped(listener.x, listener.y)
            selected = listener.id == self.selected_listener
            painter.setBrush(
                QBrush(QColor("#1f78b4") if selected else QColor("#8bb7d6"))
            )
            painter.setPen(
                QPen(QColor("#0b4c78") if selected else QColor("#4f7895"), 2)
            )
            radius = 8 if selected else 6
            painter.drawEllipse(QPointF(lx, ly), radius, radius)
            painter.setPen(QPen(QColor("#12384f")))
            painter.drawText(int(lx + 10), int(ly - 8), listener.id)

    @staticmethod
    def _qpoint(x: float, y: float) -> Any:
        return QPointF(x, y)


class RebuildWorker(QObject):
    finished = Signal(object, object, object)
    failed = Signal(object)
    cancelled = Signal(object)

    def __init__(
        self,
        engine: AcousticEngine,
        scene: Scene,
        previous_scene: Scene,
        changes: list[dict[str, Any]],
        selected_listener: str,
    ):
        super().__init__()
        self.engine = engine
        self.scene = scene
        self.previous_scene = previous_scene
        self.changes = changes
        self.selected_listener = selected_listener
        self.token = CancellationToken()

    def run(self) -> None:
        try:
            report = self.engine.rebuild(
                self.scene,
                changes=self.changes,
                previous_scene=self.previous_scene,
                mode="incremental",
                token=self.token,
            )
            self.engine.save_scene(self.scene)
            renders = {
                listener.id: self.engine.render_point(self.scene, listener.id)
                for listener in self.scene.listeners
            }
            self.finished.emit(report, renders, self.scene)
        except AcousticError as exc:
            if exc.code == "E_CANCELLED":
                self.cancelled.emit(exc.to_dict())
            else:
                self.failed.emit(exc.to_dict())
        except Exception as exc:  # defensive GUI boundary
            self.failed.emit(
                {"code": "E_INTERNAL", "message": str(exc), "details": {}}
            )


class WorkbenchWindow(QMainWindow):
    def __init__(self, sample_dir: str | Path):
        super().__init__()
        self.sample_dir = Path(sample_dir)
        self.engine = AcousticEngine(self.sample_dir)
        self.scene = self.engine.scene
        self.materials = self.engine.materials
        self.thread: QThread | None = None
        self.worker: RebuildWorker | None = None
        self.latest_report: RebuildReport | None = None
        self.player: QMediaPlayer | None = None
        self.audio_output: QAudioOutput | None = None
        self._build_ui()
        self._load_scene_into_controls()

    def _build_ui(self) -> None:
        self.setWindowTitle("Spatial Acoustic Field Workbench")
        self.resize(1320, 820)
        root = QWidget()
        outer = QHBoxLayout(root)
        splitter = QSplitter(Qt.Orientation.Horizontal)
        outer.addWidget(splitter)
        self.setCentralWidget(root)

        left = QWidget()
        left_layout = QVBoxLayout(left)
        title = QLabel("Venue plan and first-order reflection paths")
        title.setFont(QFont("Sans", 14, QFont.Weight.DemiBold))
        left_layout.addWidget(title)
        self.venue_view = VenueView()
        left_layout.addWidget(self.venue_view, 1)
        self.scene_meta = QLabel()
        self.scene_meta.setWordWrap(True)
        left_layout.addWidget(self.scene_meta)
        splitter.addWidget(left)

        right = QWidget()
        right_layout = QVBoxLayout(right)
        controls = QFrame()
        controls.setFrameShape(QFrame.Shape.StyledPanel)
        form = QFormLayout(controls)
        self.listener_combo = QComboBox()
        self.listener_combo.currentTextChanged.connect(self._on_listener_changed)
        form.addRow("Listener", self.listener_combo)
        self.wall_combo = QComboBox()
        self.wall_combo.currentTextChanged.connect(self._on_wall_changed)
        form.addRow("Wall", self.wall_combo)
        self.material_combo = QComboBox()
        self.material_combo.addItems(sorted(self.materials))
        self.material_combo.currentTextChanged.connect(self._on_preview)
        form.addRow("Wall material", self.material_combo)
        self.source_x = self._coordinate_spin()
        self.source_y = self._coordinate_spin()
        self.source_x.valueChanged.connect(self._on_preview)
        self.source_y.valueChanged.connect(self._on_preview)
        form.addRow("Source X (m)", self.source_x)
        form.addRow("Source Y (m)", self.source_y)
        right_layout.addWidget(controls)

        buttons = QHBoxLayout()
        self.apply_button = QPushButton("Commit + incremental rebuild")
        self.apply_button.clicked.connect(self._apply_changes)
        self.cancel_button = QPushButton("Cancel")
        self.cancel_button.setEnabled(False)
        self.cancel_button.clicked.connect(self._cancel_rebuild)
        self.play_button = QPushButton("Audition latest valid clip")
        self.play_button.clicked.connect(self._play_latest)
        buttons.addWidget(self.apply_button)
        buttons.addWidget(self.cancel_button)
        buttons.addWidget(self.play_button)
        right_layout.addLayout(buttons)

        graph_title = QLabel("Dependency graph and invalidation")
        graph_title.setFont(QFont("Sans", 12, QFont.Weight.DemiBold))
        right_layout.addWidget(graph_title)
        self.graph = QTreeWidget()
        self.graph.setColumnCount(4)
        self.graph.setHeaderLabels(["Node", "Upstream", "Status", "Reason"])
        self.graph.setRootIsDecorated(False)
        self.graph.setAlternatingRowColors(True)
        header = self.graph.header()
        header.setSectionResizeMode(0, QHeaderView.ResizeMode.ResizeToContents)
        header.setSectionResizeMode(1, QHeaderView.ResizeMode.ResizeToContents)
        header.setSectionResizeMode(2, QHeaderView.ResizeMode.Fixed)
        header.resizeSection(2, 112)
        header.setSectionResizeMode(3, QHeaderView.ResizeMode.Stretch)
        right_layout.addWidget(self.graph, 2)

        log_title = QLabel("Invalidation range and recompute log")
        log_title.setFont(QFont("Sans", 12, QFont.Weight.DemiBold))
        right_layout.addWidget(log_title)
        self.log = QPlainTextEdit()
        self.log.setReadOnly(True)
        self.log.setMaximumBlockCount(3000)
        right_layout.addWidget(self.log, 2)
        splitter.addWidget(right)
        splitter.setSizes([760, 540])

    @staticmethod
    def _coordinate_spin() -> QDoubleSpinBox:
        spin = QDoubleSpinBox()
        spin.setRange(-1000.0, 1000.0)
        spin.setDecimals(2)
        spin.setSingleStep(0.25)
        return spin

    def _load_scene_into_controls(self) -> None:
        self.listener_combo.blockSignals(True)
        self.listener_combo.clear()
        self.listener_combo.addItems([point.id for point in self.scene.listeners])
        self.listener_combo.blockSignals(False)
        self.wall_combo.clear()
        for wall in self.scene.walls:
            self.wall_combo.addItem(wall.id)
        if self.wall_combo.currentText():
            self._sync_material_to_wall()
        self.source_x.setValue(self.scene.source.x)
        self.source_y.setValue(self.scene.source.y)
        selected = self.listener_combo.currentText()
        self._update_view(selected)
        self.scene_meta.setText(
            f"Venue {self.scene.scene_id} · {self.scene.sample_rate} Hz · "
            f"{len(self.scene.listeners)} listeners · 5.1 · content-hash cache keys"
        )
        self._refresh_play_button()
        self._render_graph(
            {"invalidated_nodes": [], "reasons": {}},
            report=None,
        )
        self.log.setPlainText(
            "No pending changes. Edit a wall material or source position to preview "
            "invalidation."
        )

    def _on_listener_changed(self, listener_id: str) -> None:
        if listener_id:
            self._update_view(listener_id)
            self._on_preview()

    def _sync_material_to_wall(self) -> None:
        wall_id = self.wall_combo.currentText()
        if not wall_id:
            return
        wall = self.scene.wall(wall_id)
        self.material_combo.blockSignals(True)
        index = self.material_combo.findText(wall.material)
        if index >= 0:
            self.material_combo.setCurrentIndex(index)
        self.material_combo.blockSignals(False)

    def _on_wall_changed(self, wall_id: str) -> None:
        if wall_id:
            self._sync_material_to_wall()
        self._on_preview()

    def _on_preview(self) -> None:
        changes = self._pending_changes()
        plan = (
            invalidation_plan(self.scene, changes)
            if changes
            else {"points": {}, "factors": []}
        )
        selected = self.listener_combo.currentText()
        point_plan = plan.get("points", {}).get(
            selected,
            {"invalidated_nodes": [], "reasons": {}},
        )
        self._render_graph(point_plan, report=None)
        self.venue_view.set_scene(
            self.scene,
            selected,
            preview_source=(self.source_x.value(), self.source_y.value()),
        )
        if changes:
            self.log.setPlainText(
                "Preview invalidation (not committed):\n"
                + "\n".join(
                    f"  {node}: {', '.join(point_plan.get('reasons', {}).get(node, []))}"
                    for node in point_plan.get("invalidated_nodes", [])
                )
            )

    def _update_view(self, selected: str) -> None:
        self.venue_view.set_scene(
            self.scene,
            selected,
            preview_source=(self.source_x.value(), self.source_y.value()),
        )

    def _pending_changes(self) -> list[dict[str, Any]]:
        changes: list[dict[str, Any]] = []
        wall_id = self.wall_combo.currentText()
        if wall_id:
            wall = self.scene.wall(wall_id)
            material = self.material_combo.currentText()
            if material and material != wall.material:
                changes.append(
                    {
                        "factor": "material",
                        "wall_id": wall_id,
                        "material": material,
                    }
                )
        if (
            abs(self.source_x.value() - self.scene.source.x) > 1e-9
            or abs(self.source_y.value() - self.scene.source.y) > 1e-9
        ):
            changes.append(
                {
                    "factor": "source_position",
                    "position": [
                        self.source_x.value(),
                        self.source_y.value(),
                        self.scene.source.z,
                    ],
                }
            )
        return changes

    def _render_graph(
        self,
        point_plan: dict[str, Any],
        report: RebuildReport | None,
    ) -> None:
        selected = self.listener_combo.currentText()
        report_nodes: dict[str, dict[str, Any]] = {}
        if report is not None:
            for point in report.points:
                if point.point_id == selected:
                    report_nodes = {
                        item.node: item.to_dict() for item in point.nodes
                    }
        invalidated = set(point_plan.get("invalidated_nodes", []))
        reasons = point_plan.get("reasons", {})
        self.graph.clear()
        for node in NODE_ORDER:
            node_index = NODE_ORDER.index(node)
            upstream = "—" if node_index == 0 else NODE_ORDER[node_index - 1]
            trace = report_nodes.get(node)
            if trace is not None:
                status = (
                    "recomputed" if trace["status"] == "recomputed" else "cache hit"
                )
                reason = ", ".join(trace.get("reason", []))
                if trace["status"] == "recomputed":
                    background = QColor("#dff3e4")
                else:
                    background = QColor("#edf1f3")
            elif node in invalidated:
                status = "preview invalid"
                reason = ", ".join(reasons.get(node, []))
                background = QColor("#ffe2d4")
            else:
                status = "valid cache"
                reason = "content_hash_match" if report is None else ""
                background = QColor("#f7f9fa")
            item = QTreeWidgetItem(
                [
                    NODE_TITLES.get(node, node),
                    upstream,
                    status,
                    reason or "—",
                ]
            )
            for column in range(4):
                item.setBackground(column, QBrush(background))
            self.graph.addTopLevelItem(item)

    def _apply_changes(self) -> None:
        if self.thread is not None:
            self._set_busy(False)
            return
        changes = self._pending_changes()
        if not changes:
            QMessageBox.information(
                self,
                "No changes",
                "There is no pending material or source-position change.",
            )
            return
        updated = self.scene
        try:
            for change in changes:
                updated, _, _ = apply_factor(updated, self.materials, change)
        except AcousticError as exc:
            QMessageBox.critical(self, exc.code, exc.message)
            return
        self._set_busy(True)
        self.thread = QThread(self)
        self.worker = RebuildWorker(
            self.engine,
            updated,
            self.scene,
            changes,
            self.listener_combo.currentText(),
        )
        self.worker.moveToThread(self.thread)
        self.thread.started.connect(self.worker.run)
        self.worker.finished.connect(self._on_rebuild_finished)
        self.worker.failed.connect(self._on_rebuild_failed)
        self.worker.cancelled.connect(self._on_rebuild_cancelled)
        self.worker.finished.connect(self.thread.quit)
        self.worker.failed.connect(self.thread.quit)
        self.worker.cancelled.connect(self.thread.quit)
        self.thread.finished.connect(self.thread.deleteLater)
        self.thread.finished.connect(self._on_thread_finished)
        self.thread.start()

    def _on_thread_finished(self) -> None:
        self.thread = None
        self.worker = None

    def _set_busy(self, busy: bool) -> None:
        self.apply_button.setEnabled(not busy)
        self.cancel_button.setEnabled(busy)
        self.play_button.setEnabled(not busy and self._has_valid_render())

    def _cancel_rebuild(self) -> None:
        if self.worker is not None:
            self.worker.token.cancel()
            self.log.appendPlainText(
                "Cancel requested; stopping at the next node checkpoint."
            )

    def _on_rebuild_finished(
        self,
        report: RebuildReport,
        renders: dict[str, dict[str, Any]],
        scene: Scene,
    ) -> None:
        self.latest_report = report
        self.scene = scene
        self.engine.scene = scene
        self._set_busy(False)
        selected = self.listener_combo.currentText()
        point_plan = {"invalidated_nodes": [], "reasons": {}}
        lines = [
            f"Commit complete: {len(report.points)} listeners",
            f"Invalidated: {', '.join(report.to_dict()['invalidated_nodes']) or 'none'}",
            f"Recomputed: {', '.join(report.to_dict()['recomputed_nodes']) or 'none'}",
            f"Cache hits: {', '.join(report.to_dict()['cache_hit_nodes']) or 'none'}",
        ]
        for point in report.points:
            lines.append(
                f"[{point.point_id}] invalid={len(point.invalidated_nodes)} "
                f"recomputed={len([n for n in point.nodes if n.status == 'recomputed'])} "
                f"hits={len([n for n in point.nodes if n.status == 'cache_hit'])} "
                f"reflection-events={len(point.affected_reflection_events)}"
            )
            if point.point_id == selected:
                point_plan = {
                    "invalidated_nodes": point.invalidated_nodes,
                    "reasons": point.reasons,
                }
        for point_id, render in sorted(renders.items()):
            lines.append(
                f"[{point_id}] render SHA-256: {render.get('wav_sha256', '')}"
            )
        self.log.setPlainText("\n".join(lines))
        self._render_graph(point_plan, report=report)
        self._refresh_play_button()
        self._update_view(selected)

    def _on_rebuild_failed(self, error: dict[str, Any]) -> None:
        self._set_busy(False)
        self.log.appendPlainText(
            f"Rebuild failed; last committed cache retained: {error.get('code')} "
            f"{error.get('message')}"
        )
        QMessageBox.critical(
            self,
            str(error.get("code", "ERROR")),
            str(error.get("message", "")),
        )

    def _on_rebuild_cancelled(self, error: dict[str, Any]) -> None:
        self._set_busy(False)
        self.log.appendPlainText(
            "Incremental rebuild cancelled. Committed nodes remain on disk and the "
            "workbench keeps the latest valid field coefficients."
        )
        self._refresh_play_button()

    def _has_valid_render(self) -> bool:
        selected = self.listener_combo.currentText()
        if not selected:
            return False
        try:
            return self.engine.latest_render(selected) is not None
        except AcousticError:
            return False

    def _refresh_play_button(self) -> None:
        self.play_button.setEnabled(self._has_valid_render())

    def _play_latest(self) -> None:
        selected = self.listener_combo.currentText()
        record = self.engine.latest_render(selected)
        if record is None:
            QMessageBox.warning(
                self,
                "No valid clip",
                "No committed render exists for this listener.",
            )
            return
        try:
            field_result, _ = self.engine.validate_field(self.scene, selected)
        except AcousticError as exc:
            QMessageBox.warning(self, exc.code, f"Playback refused: {exc.message}")
            self._refresh_play_button()
            return
        if record.get("field_version") != field_result.cache_key:
            QMessageBox.warning(
                "E_FIELD_STALE",
                "The latest render does not match the current field version.",
            )
            self._refresh_play_button()
            return
        path = Path(record["render_path"])
        if not path.is_file():
            QMessageBox.warning(
                self,
                "Missing clip",
                "The latest valid render file does not exist.",
            )
            return
        if (
            os.environ.get("ACOUSTIC_DISABLE_AUDIO") == "1"
            or os.environ.get("QT_QPA_PLATFORM") == "offscreen"
            or (
                sys.platform.startswith("linux")
                and not os.environ.get("DISPLAY")
                and not os.environ.get("WAYLAND_DISPLAY")
            )
        ):
            QMessageBox.information(
                self,
                "No audio backend",
                f"This session has no playable desktop audio backend.\nClip: {path}",
            )
            return
        if self.player is None:
            self.player = QMediaPlayer(self)
            self.audio_output = QAudioOutput(self)
            self.player.setAudioOutput(self.audio_output)
            self.audio_output.setVolume(0.75)
        self.player.stop()
        self.player.setSource(QUrl.fromLocalFile(str(path)))
        self.player.play()
        self.log.appendPlainText(
            f"Audition {selected}: {path.name} · field={record.get('field_version', '')[:12]}"
        )

    def closeEvent(self, event: object) -> None:
        if self.worker is not None:
            self.worker.token.cancel()
        if self.thread is not None:
            self.thread.quit()
            self.thread.wait(3000)
        super().closeEvent(event)


def _default_sample_dir() -> Path:
    preferred = Path("artifacts/samples/venue_hall")
    if preferred.is_dir():
        return preferred
    root = Path("artifacts/samples")
    if root.is_dir():
        candidates = sorted(
            path for path in root.glob("venue_*") if (path / "scene.json").is_file()
        )
        if candidates:
            return candidates[0]
    return preferred


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description="Spatial acoustic field workbench GUI")
    parser.add_argument(
        "--samples",
        default=str(_default_sample_dir()),
        help="venue sample directory containing scene.json",
    )
    parser.add_argument(
        "--smoke-test",
        action="store_true",
        help="construct the window and exit after one event-loop tick",
    )
    args = parser.parse_args(argv)
    if not (Path(args.samples) / "scene.json").is_file():
        print(
            f"missing scene.json under {args.samples}; run acoustic-samples first",
            file=sys.stderr,
        )
        return 2
    app = QApplication.instance() or QApplication(sys.argv[:1])
    app.setApplicationName("Spatial Acoustic Field Workbench")
    window = WorkbenchWindow(args.samples)
    window.show()
    if args.smoke_test:
        QTimer.singleShot(250, app.quit)
    return int(app.exec())


if __name__ == "__main__":
    raise SystemExit(main())

