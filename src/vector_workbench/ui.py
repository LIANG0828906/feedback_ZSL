from __future__ import annotations

import sys
import traceback
from pathlib import Path
from typing import Any

import numpy as np
from PySide6.QtCore import (
    QObject,
    QPointF,
    QRunnable,
    QSize,
    Qt,
    QThreadPool,
    Signal,
    Slot,
)
from PySide6.QtGui import QColor, QImage, QPainter, QPen, QPixmap, QPolygonF
from PySide6.QtWidgets import (
    QApplication,
    QComboBox,
    QDoubleSpinBox,
    QFileDialog,
    QFrame,
    QGridLayout,
    QGroupBox,
    QHBoxLayout,
    QLabel,
    QMainWindow,
    QMessageBox,
    QPlainTextEdit,
    QProgressBar,
    QPushButton,
    QSpinBox,
    QSplitter,
    QTableWidget,
    QTableWidgetItem,
    QVBoxLayout,
    QWidget,
)

from .diff import DiffResult, compare_pipelines, difference_heatmap
from .errors import WorkbenchError
from .export import save_coverage_png, save_rgba_png
from .model import RenderParams, RunResult, Scene
from .operations import list_samples, load_sample_scene, verify_three_paths
from .pipeline import Pipeline, RunControl

BACKGROUND = (0.018, 0.025, 0.05)


def _composite_qimage(image: np.ndarray) -> QImage:
    alpha = image[..., 3:4]
    rgb = image[..., :3] * alpha + np.asarray(BACKGROUND, dtype=np.float32) * (
        1.0 - alpha
    )
    rgba = np.empty((*rgb.shape[:2], 4), dtype=np.uint8)
    rgba[..., :3] = np.round(np.clip(rgb, 0.0, 1.0) * 255.0).astype(np.uint8)
    rgba[..., 3] = 255
    rgba = np.ascontiguousarray(rgba)
    height, width, _ = rgba.shape
    return QImage(
        rgba.data,
        width,
        height,
        int(rgba.strides[0]),
        QImage.Format.Format_RGBA8888,
    ).copy()


def _coverage_qimage(coverage: np.ndarray) -> QImage:
    gray = np.round(np.clip(coverage, 0.0, 1.0) * 255.0).astype(np.uint8)
    rgb = np.stack([gray, gray, gray], axis=-1)
    rgb = np.ascontiguousarray(rgb)
    height, width, _ = rgb.shape
    return QImage(
        rgb.data,
        width,
        height,
        int(rgb.strides[0]),
        QImage.Format.Format_RGB888,
    ).copy()


def _heatmap_qimage(heatmap: np.ndarray) -> QImage:
    intensity = np.clip(heatmap, 0.0, 1.0)
    rgba = np.zeros((*intensity.shape, 4), dtype=np.uint8)
    rgba[..., 0] = np.round(np.clip(intensity * 1.8, 0.0, 1.0) * 255.0).astype(np.uint8)
    rgba[..., 1] = np.round(np.clip(intensity * 0.45, 0.0, 1.0) * 255.0).astype(np.uint8)
    rgba[..., 2] = np.round((1.0 - intensity) * 10.0).astype(np.uint8)
    rgba[..., 3] = 255
    rgba = np.ascontiguousarray(rgba)
    height, width, _ = rgba.shape
    return QImage(
        rgba.data,
        width,
        height,
        int(rgba.strides[0]),
        QImage.Format.Format_RGBA8888,
    ).copy()


class ImagePanel(QFrame):
    def __init__(self, title: str) -> None:
        super().__init__()
        self.setObjectName("imagePanel")
        self._image: QImage | None = None
        self._title = QLabel(title)
        self._title.setObjectName("panelTitle")
        self._image_label = QLabel("No data")
        self._image_label.setAlignment(Qt.AlignmentFlag.AlignCenter)
        self._image_label.setMinimumSize(QSize(220, 140))
        self._image_label.setStyleSheet("color: #70809a;")
        layout = QVBoxLayout(self)
        layout.setContentsMargins(8, 6, 8, 8)
        layout.setSpacing(4)
        layout.addWidget(self._title)
        layout.addWidget(self._image_label, 1)

    def set_image(self, image: QImage | None) -> None:
        self._image = image
        self._refresh()

    def resizeEvent(self, event: Any) -> None:
        super().resizeEvent(event)
        self._refresh()

    def _refresh(self) -> None:
        if self._image is None:
            self._image_label.setPixmap(QPixmap())
            self._image_label.setText("No data")
            return
        pixmap = QPixmap.fromImage(self._image)
        size = self._image_label.size()
        if size.width() > 4 and size.height() > 4:
            pixmap = pixmap.scaled(
                size,
                Qt.AspectRatioMode.KeepAspectRatio,
                Qt.TransformationMode.SmoothTransformation,
            )
        self._image_label.setPixmap(pixmap)


class UiSignals(QObject):
    progress = Signal(object)
    job_finished = Signal(str, object)
    job_failed = Signal(str, object)
    compare_finished = Signal(object, object, object)


class RenderJob(QRunnable):
    def __init__(
        self,
        signals: UiSignals,
        pipeline: Pipeline,
        mode: str,
        label: str,
    ) -> None:
        super().__init__()
        self.signals = signals
        self.pipeline = pipeline
        self.mode = mode
        self.label = label

    @Slot()
    def run(self) -> None:
        try:
            result = self.pipeline.run(self.mode, raise_on_error=False)
            self.signals.job_finished.emit(self.label, result)
        except Exception:
            self.signals.job_failed.emit(self.label, traceback.format_exc())


class RetryJob(QRunnable):
    def __init__(
        self, signals: UiSignals, pipeline: Pipeline, path_id: str
    ) -> None:
        super().__init__()
        self.signals = signals
        self.pipeline = pipeline
        self.path_id = path_id

    @Slot()
    def run(self) -> None:
        try:
            result = self.pipeline.run_path(self.path_id, raise_on_error=False)
            self.signals.job_finished.emit(f"retry:{self.path_id}", result)
        except Exception:
            self.signals.job_failed.emit("retry", traceback.format_exc())


class CompareJob(QRunnable):
    def __init__(
        self,
        signals: UiSignals,
        scene: Scene,
        params: RenderParams,
        root: Path,
    ) -> None:
        super().__init__()
        self.signals = signals
        self.scene = scene
        self.params = params
        self.root = root

    @Slot()
    def run(self) -> None:
        try:
            report, pipelines = verify_three_paths(
                self.scene, self.params, self.root / self.scene.scene_id
            )
            diff = compare_pipelines(
                pipelines["full"], pipelines["resume"]
            )
            heatmap = difference_heatmap(
                pipelines["full"].canvas, pipelines["resume"].canvas
            )
            self.signals.compare_finished.emit(report, diff, heatmap)
        except Exception:
            self.signals.job_failed.emit("compare", traceback.format_exc())


class WorkbenchWindow(QMainWindow):
    def __init__(
        self,
        data_dir: Path,
        checkpoint_root: Path,
        initial_scene_id: str | None = None,
    ) -> None:
        super().__init__()
        self.data_dir = data_dir
        self.checkpoint_root = checkpoint_root
        self.setWindowTitle("Vector Path Compositing Diagnostics Workbench")
        self.resize(1540, 960)
        self.setMinimumSize(1180, 760)
        self._samples = list_samples(data_dir)
        self._scene: Scene | None = None
        self._pipeline: Pipeline | None = None
        self._selected_path_id: str | None = None
        self._last_status = "idle"
        self._compare_running = False
        self._heatmap: np.ndarray | None = None
        self._injection_overrides: dict[str, Any] = {}
        self._signals = UiSignals()
        self._signals.progress.connect(self._on_progress)
        self._signals.job_finished.connect(self._on_job_finished)
        self._signals.job_failed.connect(self._on_job_failed)
        self._signals.compare_finished.connect(self._on_compare_finished)
        self._thread_pool = QThreadPool.globalInstance()
        self._thread_pool.setMaxThreadCount(1)
        self._build_ui()
        self._apply_style()
        self._populate_scenes(initial_scene_id)

    def _build_ui(self) -> None:
        central = QWidget()
        self.setCentralWidget(central)
        outer = QHBoxLayout(central)
        outer.setContentsMargins(12, 12, 12, 10)
        outer.setSpacing(10)
        splitter = QSplitter(Qt.Orientation.Horizontal)
        outer.addWidget(splitter)

        left = QWidget()
        left_layout = QVBoxLayout(left)
        left_layout.setContentsMargins(0, 0, 0, 0)
        left_layout.setSpacing(8)
        splitter.addWidget(left)

        scene_group = QGroupBox("Scene")
        scene_layout = QHBoxLayout(scene_group)
        self.scene_combo = QComboBox()
        self.scene_combo.setMinimumWidth(260)
        self.scene_combo.currentIndexChanged.connect(self._scene_changed)
        self.scene_info = QLabel("")
        self.scene_info.setStyleSheet("color: #8fa5c2;")
        scene_layout.addWidget(self.scene_combo)
        scene_layout.addWidget(self.scene_info, 1)
        left_layout.addWidget(scene_group)

        parameter_group = QGroupBox("Render parameters")
        parameter_layout = QGridLayout(parameter_group)
        parameter_layout.setHorizontalSpacing(8)
        parameter_layout.setVerticalSpacing(5)

        self.scale_spin = QDoubleSpinBox()
        self.scale_spin.setRange(0.05, 8.0)
        self.scale_spin.setDecimals(3)
        self.scale_spin.setSingleStep(0.1)
        self.scale_spin.setValue(1.0)
        self.tolerance_spin = QDoubleSpinBox()
        self.tolerance_spin.setRange(0.001, 8.0)
        self.tolerance_spin.setDecimals(4)
        self.tolerance_spin.setSingleStep(0.05)
        self.tolerance_spin.setValue(0.25)
        self.subsamples_spin = QSpinBox()
        self.subsamples_spin.setRange(1, 16)
        self.subsamples_spin.setValue(4)
        self.workers_spin = QSpinBox()
        self.workers_spin.setRange(1, 16)
        self.workers_spin.setValue(2)
        self.buffer_spin = QSpinBox()
        self.buffer_spin.setRange(1, 512)
        self.buffer_spin.setValue(32)
        self.buffer_spin.setSuffix(" MiB")
        self.max_stroke_spin = QSpinBox()
        self.max_stroke_spin.setRange(16, 100000)
        self.max_stroke_spin.setValue(4096)
        self.max_stroke_spin.setSuffix(" px")

        labels = (
            ("Scale", self.scale_spin),
            ("Tolerance", self.tolerance_spin),
            ("Subsamples", self.subsamples_spin),
            ("Max workers", self.workers_spin),
            ("Buffer / thread", self.buffer_spin),
            ("Stroke clip", self.max_stroke_spin),
        )
        for index, (label, widget) in enumerate(labels):
            row, column = divmod(index, 3)
            parameter_layout.addWidget(QLabel(label), row * 2, column)
            parameter_layout.addWidget(widget, row * 2 + 1, column)
        left_layout.addWidget(parameter_group)

        self.path_table = QTableWidget(0, 7)
        self.path_table.setHorizontalHeaderLabels(
            ["Path", "Kind", "Flatten", "Coverage", "Compose", "Error", "Name"]
        )
        self.path_table.verticalHeader().setVisible(False)
        self.path_table.setSelectionBehavior(
            QTableWidget.SelectionBehavior.SelectRows
        )
        self.path_table.setSelectionMode(
            QTableWidget.SelectionMode.SingleSelection
        )
        self.path_table.setEditTriggers(QTableWidget.EditTrigger.NoEditTriggers)
        self.path_table.setAlternatingRowColors(True)
        self.path_table.horizontalHeader().setStretchLastSection(True)
        self.path_table.cellClicked.connect(self._table_clicked)
        self.path_table.setMinimumHeight(220)
        left_layout.addWidget(self.path_table, 1)

        injection_group = QGroupBox("Failure injection / retry")
        injection_layout = QGridLayout(injection_group)
        self.injection_kind = QComboBox()
        self.injection_kind.addItems(
            [
                "follow input",
                "disable injection",
                "self intersection",
                "zero area",
                "stroke overflow",
                "stage failure",
            ]
        )
        self.injection_stage = QComboBox()
        self.injection_stage.addItems(["flatten", "coverage", "compose"])
        self.injection_stage.setCurrentText("coverage")
        self.btn_apply_injection = QPushButton("Apply to selected")
        self.btn_apply_injection.clicked.connect(self._apply_injection)
        injection_layout.addWidget(QLabel("Mode"), 0, 0)
        injection_layout.addWidget(self.injection_kind, 0, 1)
        injection_layout.addWidget(QLabel("Stage"), 0, 2)
        injection_layout.addWidget(self.injection_stage, 0, 3)
        injection_layout.addWidget(self.btn_apply_injection, 1, 0, 1, 4)
        left_layout.addWidget(injection_group)

        action_layout = QGridLayout()
        self.btn_run = QPushButton("Run / Continue")
        self.btn_full = QPushButton("Full recompute")
        self.btn_pause = QPushButton("Pause")
        self.btn_cancel = QPushButton("Cancel (recoverable)")
        self.btn_terminate = QPushButton("Hard stop")
        self.btn_retry = QPushButton("Retry selected")
        self.btn_compare = QPushButton("Auto compare 3 paths")
        self.btn_export = QPushButton("Export bitmap")
        self.btn_run.clicked.connect(lambda: self._start_run("resume"))
        self.btn_full.clicked.connect(lambda: self._start_run("full"))
        self.btn_pause.clicked.connect(self._pause_or_continue)
        self.btn_cancel.clicked.connect(self._cancel)
        self.btn_terminate.clicked.connect(self._terminate)
        self.btn_retry.clicked.connect(self._retry_selected)
        self.btn_compare.clicked.connect(self._auto_compare)
        self.btn_export.clicked.connect(self._export)
        action_layout.addWidget(self.btn_run, 0, 0)
        action_layout.addWidget(self.btn_full, 0, 1)
        action_layout.addWidget(self.btn_pause, 0, 2)
        action_layout.addWidget(self.btn_cancel, 1, 0)
        action_layout.addWidget(self.btn_terminate, 1, 1)
        action_layout.addWidget(self.btn_retry, 1, 2)
        action_layout.addWidget(self.btn_compare, 2, 0, 1, 2)
        action_layout.addWidget(self.btn_export, 2, 2)
        left_layout.addLayout(action_layout)

        right = QWidget()
        right_layout = QVBoxLayout(right)
        right_layout.setContentsMargins(0, 0, 0, 0)
        right_layout.setSpacing(8)
        splitter.addWidget(right)
        preview_grid = QGridLayout()
        preview_grid.setSpacing(8)
        self.composite_panel = ImagePanel("Composite result")
        self.flatten_panel = ImagePanel("Flattened polyline overlay")
        self.coverage_panel = ImagePanel("Coverage bitmap / anti-aliased gray")
        self.diff_panel = ImagePanel("Difference heatmap")
        preview_grid.addWidget(self.composite_panel, 0, 0)
        preview_grid.addWidget(self.flatten_panel, 0, 1)
        preview_grid.addWidget(self.coverage_panel, 1, 0)
        preview_grid.addWidget(self.diff_panel, 1, 1)
        right_layout.addLayout(preview_grid, 1)

        progress_row = QHBoxLayout()
        self.progress = QProgressBar()
        self.progress.setRange(0, 1)
        self.progress.setValue(0)
        self.progress.setTextVisible(True)
        self.progress_label = QLabel("0 / 0 committed")
        progress_row.addWidget(self.progress, 1)
        progress_row.addWidget(self.progress_label)
        right_layout.addLayout(progress_row)

        self.log = QPlainTextEdit()
        self.log.setReadOnly(True)
        self.log.setMaximumBlockCount(1200)
        self.log.setFixedHeight(145)
        right_layout.addWidget(self.log)
        splitter.setSizes([680, 840])
        self.statusBar().showMessage("Ready")

    def _apply_style(self) -> None:
        self.setStyleSheet(
            """
            QMainWindow, QWidget {
                background: #0c1220;
                color: #dce6f5;
                font-family: "Segoe UI", "Noto Sans CJK SC", sans-serif;
                font-size: 10pt;
            }
            QGroupBox {
                border: 1px solid #263550;
                border-radius: 7px;
                margin-top: 8px;
                padding-top: 9px;
                background: #101a2d;
            }
            QGroupBox::title {
                subcontrol-origin: margin;
                left: 10px;
                color: #8fb8e8;
                padding: 0 5px;
            }
            QPushButton {
                background: #182944;
                border: 1px solid #31527f;
                border-radius: 5px;
                padding: 6px 10px;
            }
            QPushButton:hover { background: #22395e; }
            QPushButton:disabled { color: #617088; border-color: #263247; }
            QComboBox, QSpinBox, QDoubleSpinBox, QPlainTextEdit {
                background: #09101d;
                border: 1px solid #2c405f;
                border-radius: 4px;
                padding: 4px;
            }
            QTableWidget {
                background: #0b1423;
                alternate-background-color: #0e192a;
                gridline-color: #1f2e44;
                border: 1px solid #263550;
                border-radius: 5px;
            }
            QHeaderView::section {
                background: #17243a;
                color: #9cb8dc;
                border: 0;
                border-right: 1px solid #263550;
                padding: 5px;
            }
            QProgressBar {
                background: #09101d;
                border: 1px solid #2c405f;
                border-radius: 5px;
                text-align: center;
                height: 18px;
            }
            QProgressBar::chunk { background: #1a9fd4; border-radius: 4px; }
            QFrame#imagePanel {
                background: #09111f;
                border: 1px solid #243653;
                border-radius: 7px;
            }
            QLabel#panelTitle {
                color: #9cc4ec;
                font-weight: 600;
            }
            QSplitter::handle { background: #101a2b; width: 6px; }
            """
        )

    def _populate_scenes(self, initial_scene_id: str | None) -> None:
        self.scene_combo.blockSignals(True)
        self.scene_combo.clear()
        for entry in self._samples:
            label = f"{entry['scene_id']}  [{entry.get('category', 'normal')}]"
            self.scene_combo.addItem(label, entry["scene_id"])
        self.scene_combo.blockSignals(False)
        target = initial_scene_id
        if target is None and self._samples:
            target = str(self._samples[0]["scene_id"])
        if target:
            index = self.scene_combo.findData(target)
            if index >= 0:
                self.scene_combo.setCurrentIndex(index)
        self._scene_changed(self.scene_combo.currentIndex())

    def _scene_changed(self, index: int) -> None:
        if index < 0:
            return
        scene_id = self.scene_combo.itemData(index)
        if not scene_id:
            return
        try:
            scene = load_sample_scene(self.data_dir, str(scene_id))
        except WorkbenchError as exc:
            self._log(f"Scene load failed: {exc}")
            QMessageBox.critical(self, "Scene load failed", str(exc))
            return
        self._scene = scene
        self._injection_overrides.clear()
        self._heatmap = None
        self._last_status = "idle"
        self.scene_info.setText(
            f"{len(scene.paths)} paths · bound {scene.render_bounds[2]:.0f}×"
            f"{scene.render_bounds[3]:.0f} · coord 1/{scene.coord_scale}"
        )
        self._create_pipeline()
        self._refresh_table()
        self._refresh_previews()
        self._log(f"Loaded scene {scene.scene_id} from {scene.source_path}")

    def _current_params(self) -> RenderParams:
        return RenderParams(
            scale=self.scale_spin.value(),
            tolerance=self.tolerance_spin.value(),
            subsamples=self.subsamples_spin.value(),
            max_parallelism=self.workers_spin.value(),
            coverage_buffer_limit=max(
                1, int(self.buffer_spin.value() * 1024 * 1024)
            ),
            max_stroke_pixels=float(self.max_stroke_spin.value()),
        )

    def _create_pipeline(self) -> None:
        if self._scene is None:
            return
        try:
            params = self._current_params()
            params.validate()
            root = self.checkpoint_root / self._scene.scene_id
            self._pipeline = Pipeline(
                self._scene,
                params,
                root,
                injections=dict(self._injection_overrides),
                event_callback=self._signals.progress.emit,
            )
            if self._pipeline.invalidated_snapshot:
                self._log(
                    "Snapshot invalidated: "
                    f"{self._pipeline.invalidation_reason}; full recompute required"
                )
        except WorkbenchError as exc:
            self._pipeline = None
            self._log(f"Pipeline creation failed: {exc}")

    def _refresh_table(self) -> None:
        pipeline = self._pipeline
        if pipeline is None:
            self.path_table.setRowCount(0)
            return
        rows = pipeline.rows()
        self.path_table.setRowCount(len(rows))
        for row_index, row in enumerate(rows):
            values = (
                row["path_id"],
                row["kind"],
                row["flatten"],
                row["coverage"],
                row["compose"],
                row.get("flatten_error")
                or row.get("coverage_error")
                or row.get("compose_error")
                or "",
                row["name"],
            )
            for column, value in enumerate(values):
                item = QTableWidgetItem(str(value))
                item.setData(Qt.ItemDataRole.UserRole, row["path_id"])
                if column >= 2 and column <= 4:
                    color = {
                        "pending": "#66758b",
                        "processing": "#e6b84a",
                        "committed": "#52c78a",
                        "failed": "#f06464",
                    }.get(str(value), "#dce6f5")
                    item.setForeground(QColor(color))
                self.path_table.setItem(row_index, column, item)
        self.path_table.resizeColumnsToContents()
        self.path_table.setColumnWidth(6, 150)
        progress = pipeline.progress_committed
        total = pipeline.progress_total
        self.progress.setRange(0, max(1, total))
        self.progress.setValue(progress)
        self.progress_label.setText(f"{progress} / {total} committed")
        if self._selected_path_id is None and rows:
            self._selected_path_id = rows[0]["path_id"]
            self.path_table.selectRow(0)

    def _table_clicked(self, row: int, column: int) -> None:
        item = self.path_table.item(row, 0)
        if item is not None:
            self._selected_path_id = str(item.data(Qt.ItemDataRole.UserRole))
            self._refresh_previews()

    def _selected_row(self) -> dict[str, Any] | None:
        if self._pipeline is None or self._selected_path_id is None:
            return None
        return next(
            (
                row
                for row in self._pipeline.rows()
                if row["path_id"] == self._selected_path_id
            ),
            None,
        )

    def _flatten_overlay(self) -> QImage | None:
        pipeline = self._pipeline
        if pipeline is None or self._scene is None:
            return None
        base = _composite_qimage(pipeline.canvas)
        painter = QPainter(base)
        painter.setRenderHint(QPainter.RenderHint.Antialiasing, True)
        origin_x = self._scene.render_bounds[0] * pipeline.params.scale
        origin_y = self._scene.render_bounds[1] * pipeline.params.scale
        for path_index, path in enumerate(self._scene.paths):
            points = pipeline.flatten_cache.get(path.id)
            if points is None:
                continue
            selected = path.id == self._selected_path_id
            color = QColor("#ffd43b") if selected else QColor(80, 160, 220, 95)
            width = 1.8 if selected else 0.8
            painter.setPen(QPen(color, width))
            polygon = QPolygonF(
                [
                    QPointF(
                        float(point[0]) - origin_x,
                        float(point[1]) - origin_y,
                    )
                    for point in points
                ]
            )
            if len(polygon) >= 2:
                painter.drawPolygon(polygon)
        painter.end()
        return base

    def _refresh_previews(self) -> None:
        pipeline = self._pipeline
        if pipeline is None:
            for panel in (
                self.composite_panel,
                self.flatten_panel,
                self.coverage_panel,
                self.diff_panel,
            ):
                panel.set_image(None)
            return
        self.composite_panel.set_image(_composite_qimage(pipeline.canvas))
        self.flatten_panel.set_image(self._flatten_overlay())
        coverage = (
            pipeline.coverage_cache.get(self._selected_path_id)
            if self._selected_path_id
            else None
        )
        self.coverage_panel.set_image(
            _coverage_qimage(coverage) if coverage is not None else None
        )
        if self._heatmap is not None:
            self.diff_panel.set_image(_heatmap_qimage(self._heatmap))
        else:
            zeros = np.zeros(pipeline.image_size[::-1], dtype=np.float32)
            self.diff_panel.set_image(_heatmap_qimage(zeros))

    def _set_running(self, running: bool) -> None:
        for button in (
            self.btn_run,
            self.btn_full,
            self.btn_retry,
            self.btn_compare,
            self.btn_apply_injection,
        ):
            button.setEnabled(not running)
        for widget in (
            self.scene_combo,
            self.scale_spin,
            self.tolerance_spin,
            self.subsamples_spin,
            self.workers_spin,
            self.buffer_spin,
            self.max_stroke_spin,
            self.injection_kind,
            self.injection_stage,
        ):
            widget.setEnabled(not running)
        self.btn_cancel.setEnabled(running)
        self.btn_terminate.setEnabled(running)
        self.btn_pause.setEnabled(running or self._last_status == "paused")
        self.btn_export.setEnabled(not running and self._pipeline is not None)

    def _ensure_pipeline(self, *, restart: bool = False) -> bool:
        if self._scene is None:
            return False
        params = self._current_params()
        try:
            params.validate()
        except WorkbenchError as exc:
            QMessageBox.warning(self, "Invalid parameters", str(exc))
            return False
        if (
            restart
            or self._pipeline is None
            or self._pipeline.params != params
            or self._pipeline.scene.scene_id != self._scene.scene_id
        ):
            self._create_pipeline()
        if self._pipeline is None:
            return False
        self._pipeline.control.clear_stop()
        self._pipeline.set_injections(dict(self._injection_overrides))
        return True

    def _start_run(self, mode: str) -> None:
        if self._last_status in {"terminated"}:
            restart = True
        else:
            restart = False
        if not self._ensure_pipeline(restart=restart):
            return
        assert self._pipeline is not None
        self._last_status = "running"
        self._set_running(True)
        self._log(f"Queued {mode} run")
        job = RenderJob(self._signals, self._pipeline, mode, mode)
        self._thread_pool.start(job)

    def _pause_or_continue(self) -> None:
        if self._compare_running:
            return
        if self._last_status == "paused":
            self._start_run("resume")
            return
        if self._pipeline is not None:
            self._pipeline.control.request_pause()
            self._log("Pause requested; stops at the next stage boundary")

    def _cancel(self) -> None:
        if self._compare_running:
            return
        if self._pipeline is not None:
            self._pipeline.control.request_cancel()
            self._log("Recoverable cancel requested")

    def _terminate(self) -> None:
        if self._compare_running:
            return
        if self._pipeline is not None:
            self._pipeline.control.request_terminate()
            self._log("Hard stop requested; only persisted stages will survive")

    def _retry_selected(self) -> None:
        if not self._selected_path_id:
            return
        if not self._ensure_pipeline(restart=False):
            return
        assert self._pipeline is not None
        self._last_status = "running"
        self._set_running(True)
        self._log(
            f"Retrying path {self._selected_path_id} with current injection settings"
        )
        self._thread_pool.start(
            RetryJob(self._signals, self._pipeline, self._selected_path_id)
        )

    def _apply_injection(self) -> None:
        if not self._selected_path_id or self._pipeline is None:
            return
        kind = self.injection_kind.currentText()
        if kind == "follow input":
            self._injection_overrides.pop(self._selected_path_id, None)
        elif kind == "disable injection":
            self._injection_overrides[self._selected_path_id] = None
        else:
            spec: dict[str, Any] = {
                "kind": {
                    "self intersection": "self_intersection",
                    "zero area": "degenerate",
                    "stroke overflow": "clip",
                    "stage failure": "stage_failure",
                }[kind]
            }
            if kind == "stage failure":
                spec["stage"] = self.injection_stage.currentText()
            self._injection_overrides[self._selected_path_id] = spec
        try:
            self._pipeline.set_injections(dict(self._injection_overrides))
            self._log(
                f"Injection for {self._selected_path_id}: {kind}; use Retry selected"
            )
        except WorkbenchError as exc:
            QMessageBox.warning(self, "Injection rejected", str(exc))

    def _auto_compare(self) -> None:
        if self._scene is None:
            return
        params = self._current_params()
        try:
            params.validate()
        except WorkbenchError as exc:
            QMessageBox.warning(self, "Invalid parameters", str(exc))
            return
        self._last_status = "compare"
        self._compare_running = True
        self._set_running(True)
        self.btn_pause.setEnabled(False)
        self.btn_cancel.setEnabled(False)
        self.btn_terminate.setEnabled(False)
        self._log(
            "Running full, incremental, and hard-stop resume paths into an isolated snapshot root"
        )
        root = self.checkpoint_root / "_compare"
        self._thread_pool.start(
            CompareJob(self._signals, self._scene, params, root)
        )

    def _on_progress(self, event: Any) -> None:
        self.progress.setRange(0, max(1, int(event.total)))
        self.progress.setValue(int(event.committed))
        self.progress_label.setText(f"{event.committed} / {event.total} committed")
        if event.event in {"committed", "failed", "processing"}:
            self._refresh_table()

    def _on_job_finished(self, label: str, result: RunResult) -> None:
        self._last_status = result.status
        self._set_running(False)
        self._refresh_table()
        self._refresh_previews()
        if result.status == "completed":
            self.statusBar().showMessage("Completed")
            self._log(
                f"{label}: completed {result.progress_committed}/{result.progress_total} "
                f"stages; sha256={self._image_hash(result.image)[:16]}"
            )
        elif result.status == "failed":
            self.statusBar().showMessage("Failed")
            for error in result.errors:
                self._log(
                    "FAIL {code} path={path_id} stage={stage}: {message}".format(
                        **error
                    )
                )
        elif result.status in {"paused", "cancelled"}:
            self.statusBar().showMessage(
                f"{result.status} (recoverable at committed boundary)"
            )
            self._log(
                f"{label}: {result.status}; "
                f"{result.progress_committed}/{result.progress_total} committed"
            )
        elif result.status == "terminated":
            self.statusBar().showMessage("Hard stopped (restart from persisted prefix)")
            self._log(
                f"{label}: hard terminated; persisted prefix has "
                f"{result.progress_committed}/{result.progress_total} stages"
            )
        else:
            self.statusBar().showMessage(result.status)
            self._log(f"{label}: {result.status}")
        if result.invalidated_snapshot:
            self._log(
                "Snapshot invalidation: "
                f"{result.invalidation_reason or 'new run'}"
            )
        self.btn_pause.setText("Continue" if result.status == "paused" else "Pause")
        self._refresh_previews()

    def _on_job_failed(self, label: str, message: str) -> None:
        self._compare_running = False
        self._last_status = "error"
        self._set_running(False)
        self.statusBar().showMessage("Worker error")
        self._log(f"{label} worker error:\n{message}")
        QMessageBox.critical(self, "Worker error", message[-2000:])

    def _on_compare_finished(
        self,
        report: dict[str, Any],
        diff: DiffResult,
        heatmap: np.ndarray,
    ) -> None:
        self._last_status = "idle"
        self._compare_running = False
        self._set_running(False)
        self._heatmap = heatmap
        self._refresh_previews()
        if report.get("passed") and diff.equal:
            self.statusBar().showMessage("Three-path comparison matched")
            self._log(
                "Three-path comparison: exact match; commit sequence and bitmap SHA-256 agree"
            )
        else:
            self.statusBar().showMessage("Three-path comparison failed")
            self._log(
                "THREE-PATH DIFF {code}: first pixel={pixel} path={path} stage={stage} "
                "reason={reason}".format(
                    code="E_DIFF",
                    pixel=diff.first_pixel,
                    path=diff.path_id,
                    stage=diff.stage,
                    reason=diff.reason,
                )
            )
            QMessageBox.warning(
                self,
                "E_DIFF",
                f"First mismatch pixel: {diff.first_pixel}\n"
                f"Path: {diff.path_id}\nStage: {diff.stage}\n"
                f"Reason: {diff.reason}",
            )

    def _export(self) -> None:
        if self._pipeline is None:
            return
        default = str(
            Path.cwd() / "artifacts" / f"{self._pipeline.scene.scene_id}.png"
        )
        filename, _ = QFileDialog.getSaveFileName(
            self, "Export bitmap", default, "PNG image (*.png)"
        )
        if not filename:
            return
        try:
            if self._selected_path_id in self._pipeline.coverage_cache:
                answer = QMessageBox.question(
                    self,
                    "Export",
                    "Export selected coverage grayscale instead of composite?",
                    QMessageBox.StandardButton.Yes
                    | QMessageBox.StandardButton.No
                    | QMessageBox.StandardButton.Cancel,
                )
                if answer == QMessageBox.StandardButton.Cancel:
                    return
                if answer == QMessageBox.StandardButton.Yes:
                    save_coverage_png(
                        self._pipeline.coverage_cache[self._selected_path_id],
                        filename,
                    )
                else:
                    save_rgba_png(self._pipeline.canvas, filename)
            else:
                save_rgba_png(self._pipeline.canvas, filename)
            self._log(f"Exported {filename}")
            self.statusBar().showMessage("Bitmap exported")
        except WorkbenchError as exc:
            QMessageBox.critical(self, "Export failed", str(exc))

    def _image_hash(self, image: np.ndarray) -> str:
        import hashlib

        return hashlib.sha256(np.ascontiguousarray(image).tobytes()).hexdigest()

    def _log(self, message: str) -> None:
        self.log.appendPlainText(message)
        self.log.verticalScrollBar().setValue(
            self.log.verticalScrollBar().maximum()
        )

    def closeEvent(self, event: Any) -> None:
        if self._pipeline is not None:
            self._pipeline.control.request_terminate()
        self._thread_pool.waitForDone(2000)
        super().closeEvent(event)


def run_gui(
    data_dir: Path,
    checkpoint_root: Path,
    initial_scene_id: str | None = None,
) -> int:
    app = QApplication.instance() or QApplication(sys.argv)
    app.setApplicationName("Vector Path Compositing Diagnostics Workbench")
    window = WorkbenchWindow(data_dir, checkpoint_root, initial_scene_id)
    window.show()
    return int(app.exec())

