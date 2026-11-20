"""PyQt5 desktop workbench."""

from __future__ import annotations

import math
import sys
from dataclasses import replace
from typing import Any

import numpy as np
from PyQt5.QtCore import Qt, QTimer, pyqtSignal
from PyQt5.QtGui import QColor, QPainter, QPen
from PyQt5.QtWidgets import (
    QApplication,
    QComboBox,
    QDoubleSpinBox,
    QFileDialog,
    QFormLayout,
    QGroupBox,
    QHBoxLayout,
    QHeaderView,
    QLabel,
    QLineEdit,
    QMainWindow,
    QMessageBox,
    QPushButton,
    QScrollArea,
    QSlider,
    QSpinBox,
    QSplitter,
    QTableWidget,
    QTableWidgetItem,
    QVBoxLayout,
    QWidget,
)

from ..bvh import BvhDocument, parse_bvh
from ..constraints import ConstraintSet, load_constraints
from ..errors import BusinessError
from ..pipeline import RunReport, SolvePipeline
from ..solver import FrameResult, SolveCache, SolverConfig
from .viewport import SkeletonViewport
from .worker import SolveWorker


class ResidualHeatmap(QWidget):
    clicked_frame = pyqtSignal(int)

    def __init__(self, parent=None):
        super().__init__(parent)
        self.setMinimumHeight(46)
        self.results: dict[int, FrameResult] = {}
        self.frame_count = 0
        self.current = 0
        self.constraint_id: str | None = None

    def set_results(self, results: dict[int, FrameResult], frame_count: int) -> None:
        self.results = results
        self.frame_count = max(frame_count, 0)
        self.update()

    def set_metric(self, constraint_id: str | None) -> None:
        self.constraint_id = constraint_id
        self.update()

    def set_current(self, frame: int) -> None:
        self.current = int(frame)
        self.update()

    def _ratio(self, result: FrameResult) -> float:
        if self.constraint_id is None:
            ratios = [
                item.residual / max(item.tolerance, 1.0e-12)
                for item in result.residuals
                if item.applicable and item.weight > 0.0
            ]
            return max(ratios, default=0.0)
        for item in result.residuals:
            if item.constraint_id == self.constraint_id:
                if not item.applicable or item.weight <= 0.0:
                    return 0.0
                return item.residual / max(item.tolerance, 1.0e-12)
        return 0.0

    def paintEvent(self, event) -> None:
        painter = QPainter(self)
        painter.fillRect(self.rect(), QColor(18, 22, 30))
        width = max(self.width(), 1)
        if self.frame_count <= 0:
            painter.setPen(QColor(140, 150, 165))
            painter.drawText(self.rect(), Qt.AlignCenter, "constraint residual heatmap")
            return
        for frame in range(self.frame_count):
            x0 = int(frame * width / self.frame_count)
            x1 = max(x0 + 1, int((frame + 1) * width / self.frame_count))
            result = self.results.get(frame)
            if result is None:
                color = QColor(36, 42, 52)
            else:
                ratio = self._ratio(result)
                if ratio <= 0.6:
                    color = QColor(62, 180, 110)
                elif ratio <= 1.0:
                    t = (ratio - 0.6) / 0.4
                    color = QColor(int(62 + 205 * t), int(180 + 30 * t), 70)
                else:
                    color = QColor(230, 64, 64)
            painter.fillRect(x0, 8, max(1, x1 - x0), self.height() - 16, color)
        x = int((self.current + 0.5) * width / self.frame_count)
        painter.setPen(QPen(QColor(240, 245, 255), 1.5))
        painter.drawLine(x, 2, x, self.height() - 2)

    def mousePressEvent(self, event) -> None:
        if self.frame_count <= 0:
            return
        frame = int(event.x() * self.frame_count / max(self.width(), 1))
        self.clicked_frame.emit(max(0, min(frame, self.frame_count - 1)))


class WorkbenchWindow(QMainWindow):
    def __init__(
        self,
        *,
        bvh_path: str | None = None,
        constraints_path: str | None = None,
        snapshot_dir: str = "workbench_snapshots",
    ):
        super().__init__()
        self.setWindowTitle("Mocap Constraint Workbench")
        self.resize(1380, 880)
        self.document: BvhDocument | None = None
        self.constraints: ConstraintSet | None = None
        self.report: RunReport | None = None
        self.results: dict[int, FrameResult] = {}
        self.worker: SolveWorker | None = None
        self.cache = SolveCache()
        self.current_frame = 0
        self.play_timer = QTimer(self)
        self.play_timer.setTimerType(Qt.PreciseTimer)
        self.play_timer.timeout.connect(self._advance_playback)
        self._build_ui()
        self._apply_styles()
        if bvh_path:
            self.bvh_edit.setText(bvh_path)
        if constraints_path:
            self.constraints_edit.setText(constraints_path)
        self.snapshot_edit.setText(snapshot_dir)
        if bvh_path and constraints_path:
            self.load_inputs()

    def _build_ui(self) -> None:
        central = QWidget()
        self.setCentralWidget(central)
        root = QVBoxLayout(central)
        root.setContentsMargins(8, 8, 8, 8)
        root.setSpacing(6)

        path_box = QGroupBox("Inputs")
        path_form = QFormLayout(path_box)
        self.bvh_edit = QLineEdit()
        self.constraints_edit = QLineEdit()
        self.snapshot_edit = QLineEdit()
        self.bvh_edit.setPlaceholderText("local .bvh file")
        self.constraints_edit.setPlaceholderText("local constraints .json")
        self.snapshot_edit.setPlaceholderText("snapshot directory")
        path_form.addRow("BVH", self._browse_row(self.bvh_edit, "BVH (*.bvh);;All files (*)"))
        path_form.addRow(
            "Constraints",
            self._browse_row(
                self.constraints_edit,
                "Constraints (*.json);;All files (*)",
            ),
        )
        path_form.addRow(
            "Snapshot dir",
            self._browse_row(self.snapshot_edit, "", directory=True),
        )
        root.addWidget(path_box)

        toolbar = QHBoxLayout()
        self.load_btn = QPushButton("Load")
        self.start_btn = QPushButton("Start solve")
        self.pause_btn = QPushButton("Pause")
        self.cancel_btn = QPushButton("Cancel")
        self.recover_btn = QPushButton("Recover snapshot")
        self.play_btn = QPushButton("Play")
        for button in (
            self.load_btn,
            self.start_btn,
            self.pause_btn,
            self.cancel_btn,
            self.recover_btn,
            self.play_btn,
        ):
            toolbar.addWidget(button)
        toolbar.addStretch(1)
        self.status_label = QLabel("Ready")
        toolbar.addWidget(self.status_label)
        root.addLayout(toolbar)

        splitter = QSplitter(Qt.Horizontal)
        root.addWidget(splitter, 1)
        self.viewport = SkeletonViewport()
        splitter.addWidget(self.viewport)

        side = QScrollArea()
        side.setWidgetResizable(True)
        side.setMinimumWidth(360)
        side_content = QWidget()
        side_content.setMinimumWidth(340)
        side_layout = QVBoxLayout(side_content)
        side_layout.setContentsMargins(4, 4, 4, 4)
        side.setWidget(side_content)
        splitter.addWidget(side)
        splitter.setStretchFactor(0, 5)
        splitter.setStretchFactor(1, 2)
        splitter.setSizes([960, 400])

        solver_box = QGroupBox("Solver configuration")
        solver_form = QFormLayout(solver_box)
        self.strategy_combo = QComboBox()
        self.strategy_combo.addItems(["auto", "batch", "window"])
        self.iterations_spin = QSpinBox()
        self.iterations_spin.setRange(1, 500)
        self.iterations_spin.setValue(24)
        self.chunk_spin = QSpinBox()
        self.chunk_spin.setRange(1, 100000)
        self.chunk_spin.setValue(240)
        self.window_spin = QSpinBox()
        self.window_spin.setRange(2, 100000)
        self.window_spin.setValue(96)
        self.overlap_spin = QSpinBox()
        self.overlap_spin.setRange(0, 99999)
        self.overlap_spin.setValue(12)
        self.workers_spin = QSpinBox()
        self.workers_spin.setRange(1, 64)
        self.workers_spin.setValue(1)
        self.memory_spin = QSpinBox()
        self.memory_spin.setRange(1, 32768)
        self.memory_spin.setValue(512)
        self.memory_spin.setSuffix(" MiB")
        solver_form.addRow("Strategy", self.strategy_combo)
        solver_form.addRow("Max iterations", self.iterations_spin)
        solver_form.addRow("Snapshot interval", self.chunk_spin)
        solver_form.addRow("Window size", self.window_spin)
        solver_form.addRow("Window overlap", self.overlap_spin)
        solver_form.addRow("Batch workers", self.workers_spin)
        solver_form.addRow("Memory budget", self.memory_spin)
        side_layout.addWidget(solver_box)

        constraint_box = QGroupBox("Constraints")
        constraint_layout = QVBoxLayout(constraint_box)
        self.constraint_table = QTableWidget(0, 5)
        self.constraint_table.setHorizontalHeaderLabels(
            ["Constraint", "Type", "Joint", "Weight", "Tolerance"]
        )
        self.constraint_table.horizontalHeader().setSectionResizeMode(
            QHeaderView.ResizeToContents
        )
        self.constraint_table.verticalHeader().setVisible(False)
        constraint_layout.addWidget(self.constraint_table)
        self.apply_constraints_btn = QPushButton("Apply weights / tolerances")
        constraint_layout.addWidget(self.apply_constraints_btn)
        side_layout.addWidget(constraint_box)

        residual_box = QGroupBox("Current frame residuals")
        residual_layout = QVBoxLayout(residual_box)
        self.heat_metric_combo = QComboBox()
        self.heat_metric_combo.addItem("max constraint", None)
        residual_layout.addWidget(self.heat_metric_combo)
        self.residual_table = QTableWidget(0, 6)
        self.residual_table.setHorizontalHeaderLabels(
            ["Constraint", "Joint", "Residual", "Tolerance", "Ratio", "State"]
        )
        self.residual_table.horizontalHeader().setSectionResizeMode(
            QHeaderView.ResizeToContents
        )
        self.residual_table.verticalHeader().setVisible(False)
        residual_layout.addWidget(self.residual_table)
        side_layout.addWidget(residual_box)
        side_layout.addStretch(1)

        timeline = QHBoxLayout()
        self.frame_label = QLabel("frame -")
        self.range_label = QLabel("committed: -")
        self.cache_label = QLabel("cache: -")
        self.slider = QSlider(Qt.Horizontal)
        self.slider.setRange(0, 0)
        timeline.addWidget(self.frame_label)
        timeline.addWidget(self.slider, 1)
        timeline.addWidget(self.range_label)
        timeline.addWidget(self.cache_label)
        root.addLayout(timeline)
        self.heatmap = ResidualHeatmap()
        root.addWidget(self.heatmap)

        self.load_btn.clicked.connect(self.load_inputs)
        self.start_btn.clicked.connect(self.start_solve)
        self.pause_btn.clicked.connect(self.toggle_pause)
        self.cancel_btn.clicked.connect(self.cancel_solve)
        self.recover_btn.clicked.connect(self.recover_solve)
        self.play_btn.clicked.connect(self.toggle_play)
        self.slider.valueChanged.connect(self.show_frame)
        self.heatmap.clicked_frame.connect(self._seek_frame)
        self.apply_constraints_btn.clicked.connect(self.apply_constraints)
        self.heat_metric_combo.currentIndexChanged.connect(self._metric_changed)

    def _browse_row(self, edit: QLineEdit, filter_text: str, directory: bool = False):
        widget = QWidget()
        layout = QHBoxLayout(widget)
        layout.setContentsMargins(0, 0, 0, 0)
        layout.addWidget(edit, 1)
        button = QPushButton("Browse")

        def choose():
            if directory:
                path = QFileDialog.getExistingDirectory(self, "Select directory", edit.text())
            else:
                path, _ = QFileDialog.getOpenFileName(
                    self, "Select file", edit.text(), filter_text
                )
            if path:
                edit.setText(path)

        button.clicked.connect(choose)
        layout.addWidget(button)
        return widget

    def _apply_styles(self) -> None:
        self.setStyleSheet(
            """
            QMainWindow, QWidget { background: #11151c; color: #dce3ee; }
            QGroupBox { border: 1px solid #2a3342; border-radius: 6px;
                        margin-top: 8px; padding-top: 8px; font-weight: 600; }
            QGroupBox::title { subcontrol-origin: margin; left: 10px;
                               padding: 0 4px; color: #8fb7e8; }
            QLineEdit, QSpinBox, QDoubleSpinBox, QComboBox, QTableWidget {
                background: #18202b; border: 1px solid #2e3a4c; padding: 3px;
                selection-background-color: #2e6da4;
            }
            QPushButton { background: #223247; border: 1px solid #365171;
                          padding: 6px 10px; border-radius: 4px; }
            QPushButton:hover { background: #2b405b; }
            QPushButton:disabled { color: #707b89; background: #181d25; }
            QSlider::groove:horizontal { height: 6px; background: #263142;
                                         border-radius: 3px; }
            QSlider::handle:horizontal { width: 13px; margin: -4px 0;
                                         background: #78aee8; border-radius: 6px; }
            QHeaderView::section { background: #1b2634; padding: 4px; border: 0; }
            """
        )

    def _error(self, title: str, message: str) -> None:
        QMessageBox.critical(self, title, message)

    def load_inputs(self) -> None:
        bvh_path = self.bvh_edit.text().strip()
        constraints_path = self.constraints_edit.text().strip()
        if not bvh_path or not constraints_path:
            self._error("Inputs", "Select both a BVH and a constraint JSON file.")
            return
        try:
            document = parse_bvh(bvh_path)
            constraints = load_constraints(constraints_path, document)
        except BusinessError as exc:
            self._error(exc.code, exc.message)
            return
        self.document = document
        self.constraints = constraints
        self.cache.clear()
        self.results.clear()
        self.report = None
        self.viewport.set_skeleton(document.joint_names, tuple(j.parent for j in document.joints))
        self._populate_constraints()
        self.heatmap.set_results({}, document.frame_count)
        self.slider.setRange(0, max(0, document.frame_count - 1))
        self.slider.setValue(0)
        self.frame_label.setText(f"frame 0 / {document.frame_count - 1}")
        self.range_label.setText("committed: -")
        self.cache_label.setText("cache: -")
        self.status_label.setText(
            f"Loaded {document.frame_count} frames, {document.joint_count} joints"
        )
        self._update_heat_metrics()
        self._show_base_frame(0)

    def _populate_constraints(self) -> None:
        assert self.constraints is not None
        self.constraint_table.setRowCount(len(self.constraints.constraints))
        for row, spec in enumerate(self.constraints.constraints):
            self.constraint_table.setItem(row, 0, QTableWidgetItem(spec.id))
            self.constraint_table.setItem(row, 1, QTableWidgetItem(spec.kind))
            self.constraint_table.setItem(row, 2, QTableWidgetItem(spec.joint))
            weight = QDoubleSpinBox()
            weight.setRange(0.0, 10000.0)
            weight.setDecimals(4)
            weight.setSingleStep(0.05)
            weight.setValue(spec.weight)
            tolerance = QDoubleSpinBox()
            if spec.kind == "joint_limit":
                tolerance.setRange(1.0e-6, 180.0)
                tolerance.setDecimals(5)
                tolerance.setValue(math.degrees(spec.tolerance))
            else:
                tolerance.setRange(1.0e-8, 10000.0)
                tolerance.setDecimals(7)
                tolerance.setValue(spec.tolerance)
            self.constraint_table.setCellWidget(row, 3, weight)
            self.constraint_table.setCellWidget(row, 4, tolerance)

    def _update_heat_metrics(self) -> None:
        current = self.heat_metric_combo.currentData()
        self.heat_metric_combo.blockSignals(True)
        self.heat_metric_combo.clear()
        self.heat_metric_combo.addItem("max constraint", None)
        if self.constraints is not None:
            for spec in self.constraints.constraints:
                self.heat_metric_combo.addItem(spec.id, spec.id)
        index = self.heat_metric_combo.findData(current)
        self.heat_metric_combo.setCurrentIndex(max(index, 0))
        self.heat_metric_combo.blockSignals(False)
        self.heatmap.set_metric(self.heat_metric_combo.currentData())

    def _metric_changed(self) -> None:
        self.heatmap.set_metric(self.heat_metric_combo.currentData())

    def apply_constraints(self) -> None:
        if self.constraints is None:
            return
        updated = []
        for row, spec in enumerate(self.constraints.constraints):
            weight_widget = self.constraint_table.cellWidget(row, 3)
            tolerance_widget = self.constraint_table.cellWidget(row, 4)
            weight = float(weight_widget.value())
            tolerance = float(tolerance_widget.value())
            if spec.kind == "joint_limit":
                tolerance = math.radians(tolerance)
            updated.append(replace(spec, weight=weight, tolerance=tolerance))
        candidate = ConstraintSet(
            constraints=tuple(updated),
            revision=self.constraints.revision + 1,
            name=self.constraints.name,
        )
        assert self.document is not None
        try:
            candidate.validate(self.document)
        except BusinessError as exc:
            self._error(exc.code, exc.message)
            return
        self.constraints = candidate
        self.status_label.setText(
            f"Constraint weights/tolerances applied, revision {candidate.revision}"
        )
        self._update_heat_metrics()

    def _solver_config(self) -> SolverConfig:
        return SolverConfig(
            max_iterations=self.iterations_spin.value(),
            strategy=self.strategy_combo.currentText(),
            chunk_size=self.chunk_spin.value(),
            window_size=self.window_spin.value(),
            window_overlap=min(
                self.overlap_spin.value(), self.window_spin.value() - 1
            ),
            workers=self.workers_spin.value(),
            memory_budget_bytes=self.memory_spin.value() * 1024 * 1024,
        )

    def _ensure_inputs(self) -> bool:
        if self.document is None or self.constraints is None:
            self.load_inputs()
        return self.document is not None and self.constraints is not None

    def _pipeline(self, config: SolverConfig) -> SolvePipeline:
        assert self.document is not None and self.constraints is not None
        return SolvePipeline(
            self.bvh_edit.text().strip(),
            self.constraints_edit.text().strip(),
            self.snapshot_edit.text().strip() or "workbench_snapshots",
            config,
            document=self.document,
            constraints=self.constraints,
        )

    def _begin_worker(self, *, resume: bool) -> None:
        if self.worker is not None and self.worker.isRunning():
            self.status_label.setText("A solve task is already running")
            return
        if not self._ensure_inputs():
            return
        try:
            config = self._solver_config()
            config.validate()
            pipeline = self._pipeline(config)
        except BusinessError as exc:
            self._error(exc.code, exc.message)
            return
        self.results.clear()
        self.report = None
        self.heatmap.set_results({}, self.document.frame_count if self.document else 0)
        self.worker = SolveWorker(
            pipeline,
            resume=resume,
            cache=self.cache,
            parent=self,
        )
        self.worker.progress.connect(self._on_progress)
        self.worker.completed.connect(self._on_completed)
        self.worker.failed.connect(self._on_failed)
        self.worker.finished.connect(self._on_worker_finished)
        self.play_timer.stop()
        self.play_btn.setText("Play")
        self.status_label.setText("Recovering..." if resume else "Solving...")
        self.start_btn.setEnabled(False)
        self.recover_btn.setEnabled(False)
        self.pause_btn.setEnabled(True)
        self.cancel_btn.setEnabled(True)
        self.worker.start()

    def start_solve(self) -> None:
        self._begin_worker(resume=False)

    def recover_solve(self) -> None:
        self._begin_worker(resume=True)

    def toggle_pause(self) -> None:
        if self.worker is None or not self.worker.isRunning():
            return
        paused = not self.worker.pause_event.is_set()
        self.worker.request_pause(paused)
        self.pause_btn.setText("Resume" if paused else "Pause")
        self.status_label.setText(
            "Pause requested; stopping at next interval boundary"
            if paused
            else "Resuming..."
        )

    def cancel_solve(self) -> None:
        if self.worker is None or not self.worker.isRunning():
            return
        self.worker.request_cancel()
        self.status_label.setText(
            "Cancel requested; retaining the last committed snapshot interval"
        )

    def _on_progress(self, event) -> None:
        for result in event.frame_results:
            self.results[result.frame] = result
        self.heatmap.set_results(self.results, event.frame_count)
        self.slider.setRange(0, max(0, event.frame_count - 1))
        self.range_label.setText(
            "committed: " + self._format_ranges(event.committed_ranges)
        )
        self.cache_label.setText(
            f"cache hit {event.cache_hits} / miss {event.cache_misses}"
        )
        self.status_label.setText(
            f"{event.status}: interval {event.start_frame}-{event.end_frame}, "
            f"{event.committed_frames} committed, {event.conflicts} conflicts"
        )
        if event.frame_results:
            self._seek_frame(event.frame_results[-1].frame)

    def _on_completed(self, report: RunReport) -> None:
        self.report = report
        for result in report.results:
            self.results[result.frame] = result
        self.heatmap.set_results(self.results, report.frame_count)
        self.slider.setRange(0, max(0, report.frame_count - 1))
        self.range_label.setText("committed: " + self._format_ranges(report.committed_ranges))
        self.cache_label.setText(
            f"cache hit {report.cache_stats.get('hits', 0)} / "
            f"miss {report.cache_stats.get('misses', 0)}"
        )
        status = report.status
        if report.conflict_reports:
            status += f", {len(report.conflict_reports)} conflict frame(s)"
        if report.message:
            status += f" - {report.message}"
        self.status_label.setText(status)
        if report.results:
            self._seek_frame(report.results[-1].frame)
        elif self.document is not None:
            self._show_base_frame(0)

    def _on_failed(self, payload: dict[str, Any]) -> None:
        self.status_label.setText(f"{payload.get('code')}: {payload.get('message')}")

    def _on_worker_finished(self) -> None:
        self.start_btn.setEnabled(True)
        self.recover_btn.setEnabled(True)
        self.pause_btn.setEnabled(False)
        self.cancel_btn.setEnabled(False)
        self.pause_btn.setText("Pause")

    def _format_ranges(self, ranges) -> str:
        if not ranges:
            return "none"
        return ", ".join(
            f"{start}-{end}" if start != end else str(start) for start, end in ranges
        )

    def _show_base_frame(self, frame: int) -> None:
        if self.document is None:
            return
        pose = self.document.evaluate(frame)
        self.viewport.set_frame(pose.world)
        self.current_frame = frame
        self.heatmap.set_current(frame)
        self.frame_label.setText(f"frame {frame} / {self.document.frame_count - 1}")

    def _seek_frame(self, frame: int) -> None:
        frame = max(0, min(int(frame), self.slider.maximum()))
        self.slider.setValue(frame)

    def show_frame(self, frame: int) -> None:
        if self.document is None:
            return
        self.current_frame = int(frame)
        self.heatmap.set_current(frame)
        self.frame_label.setText(f"frame {frame} / {self.document.frame_count - 1}")
        result = self.results.get(frame)
        if result is None:
            self._show_base_frame(frame)
            self._populate_residuals(None, frame)
            return
        ratios: dict[int, float] = {}
        targets: list[tuple[str, np.ndarray, float]] = []
        for item in result.residuals:
            if not item.applicable or item.weight <= 0.0:
                continue
            try:
                joint_index = self.document.joint_names.index(item.joint)
            except ValueError:
                continue
            ratio = item.residual / max(item.tolerance, 1.0e-12)
            ratios[joint_index] = max(ratios.get(joint_index, 0.0), ratio)
        if self.constraints is not None:
            for spec in self.constraints.constraints:
                if spec.kind != "effector":
                    continue
                target = spec.target_for_frame(frame)
                if target is None:
                    continue
                matching = next(
                    (item for item in result.residuals if item.constraint_id == spec.id),
                    None,
                )
                ratio = (
                    matching.residual / max(matching.tolerance, 1.0e-12)
                    if matching is not None
                    else 0.0
                )
                targets.append((spec.id, np.asarray(target), ratio))
        self.viewport.set_frame(result.pose.world, ratios, targets)
        self._populate_residuals(result, frame)

    def _populate_residuals(self, result: FrameResult | None, frame: int) -> None:
        if result is None:
            self.residual_table.setRowCount(0)
            return
        self.residual_table.setRowCount(len(result.residuals))
        for row, item in enumerate(result.residuals):
            ratio = item.residual / max(item.tolerance, 1.0e-12)
            values = [
                item.constraint_id,
                item.joint,
                f"{item.residual:.8g}",
                f"{item.tolerance:.8g}",
                f"{ratio:.4f}",
                "ok" if item.satisfied else "conflict",
            ]
            for column, value in enumerate(values):
                cell = QTableWidgetItem(value)
                if column == 5 and not item.satisfied:
                    cell.setForeground(QColor(255, 100, 100))
                self.residual_table.setItem(row, column, cell)

    def toggle_play(self) -> None:
        if self.play_timer.isActive():
            self.play_timer.stop()
            self.play_btn.setText("Play")
            return
        if self.document is None:
            return
        interval = max(10, int(self.document.frame_time * 1000.0))
        self.play_timer.start(interval)
        self.play_btn.setText("Pause playback")

    def _advance_playback(self) -> None:
        if self.slider.maximum() <= 0:
            return
        frame = self.slider.value() + 1
        if frame > self.slider.maximum():
            frame = 0
        self.slider.setValue(frame)

    def closeEvent(self, event) -> None:
        if self.worker is not None and self.worker.isRunning():
            self.worker.request_cancel()
            self.worker.wait(3000)
        event.accept()


def launch(
    *,
    bvh_path: str | None = None,
    constraints_path: str | None = None,
    snapshot_dir: str = "workbench_snapshots",
    software_gl: bool = False,
) -> int:
    if software_gl:
        QApplication.setAttribute(Qt.AA_UseSoftwareOpenGL, True)
    QApplication.setAttribute(Qt.AA_EnableHighDpiScaling, True)
    app = QApplication.instance() or QApplication(sys.argv)
    window = WorkbenchWindow(
        bvh_path=bvh_path,
        constraints_path=constraints_path,
        snapshot_dir=snapshot_dir,
    )
    window.show()
    return int(app.exec_())

