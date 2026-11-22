"""PySide6 operator interface for the deterministic virtual-production pipeline."""

from __future__ import annotations

import sys
from pathlib import Path
from typing import Any

import numpy as np
from PySide6.QtCore import QRectF, Qt, QThread, Signal
from PySide6.QtGui import QColor, QFont, QImage, QPainter, QPen
from PySide6.QtWidgets import (
    QApplication,
    QCheckBox,
    QColorDialog,
    QComboBox,
    QDoubleSpinBox,
    QFileDialog,
    QFrame,
    QGridLayout,
    QGroupBox,
    QHBoxLayout,
    QHeaderView,
    QLabel,
    QMainWindow,
    QMessageBox,
    QPushButton,
    QScrollArea,
    QSizePolicy,
    QSlider,
    QSpinBox,
    QSplitter,
    QTableWidget,
    QTableWidgetItem,
    QTextEdit,
    QVBoxLayout,
    QWidget,
)

from .checkpoint import CheckpointStore
from .color import linear_to_srgb, srgb_to_linear
from .errors import PipelineError
from .export import ExportResult, export_bundle
from .frame import FrameResult
from .loader import load_scene
from .model import Layer
from .session import (
    CompositorSession,
    InvalidationRecord,
    RecomputeResult,
)
from .state import STATE_LABELS_ZH, WindowState

VERSION_LABELS = {
    "vpfc/1": "vpfc/1 · legacy flat / global premultiplied",
    "vpfc/2": "vpfc/2 · ticks / parent refs / per-layer alpha",
    "vpfc/3": "vpfc/3 · seconds / nested hierarchy / linear",
}

STATE_COLORS = {
    WindowState.PENDING: "#626b7b",
    WindowState.COMPUTING: "#c8862f",
    WindowState.COMMITTED: "#2f8f63",
    WindowState.FAILED: "#b44a4a",
}


class FrameCanvas(QWidget):
    def __init__(self) -> None:
        super().__init__()
        self.setObjectName("frameCanvas")
        self.setMinimumSize(420, 260)
        self.setSizePolicy(
            QSizePolicy.Policy.Expanding, QSizePolicy.Policy.Expanding
        )
        self._image: QImage | None = None
        self._heatmap: QImage | None = None
        self._label = "尚未提交帧"

    def clear(self, label: str = "尚未提交帧") -> None:
        self._image = None
        self._heatmap = None
        self._label = label
        self.update()

    def set_frame(
        self,
        frame: FrameResult,
        heatmap: np.ndarray | None = None,
    ) -> None:
        pixels = np.ascontiguousarray(frame.pixels)
        self._image = QImage(
            pixels.data,
            frame.width,
            frame.height,
            int(pixels.strides[0]),
            QImage.Format.Format_RGBA8888,
        ).copy()
        if heatmap is None:
            self._heatmap = None
        else:
            normalized = np.clip(heatmap.astype(np.float64) / 255.0, 0.0, 1.0)
            rgba = np.zeros((*normalized.shape, 4), dtype=np.uint8)
            rgba[..., 0] = np.clip(normalized * 3.0, 0.0, 1.0) * 255
            rgba[..., 1] = np.clip(normalized * 3.0 - 1.0, 0.0, 1.0) * 255
            rgba[..., 2] = np.clip(normalized * 3.0 - 2.0, 0.0, 1.0) * 255
            rgba[..., 3] = np.where(normalized > 0.0, 190, 0).astype(np.uint8)
            rgba = np.ascontiguousarray(rgba)
            self._heatmap = QImage(
                rgba.data,
                frame.width,
                frame.height,
                int(rgba.strides[0]),
                QImage.Format.Format_RGBA8888,
            ).copy()
        self._label = ""
        self.update()

    def paintEvent(self, event: Any) -> None:
        del event
        painter = QPainter(self)
        painter.fillRect(self.rect(), QColor("#080b10"))
        if self._image is None:
            painter.setPen(QColor("#9aa5b5"))
            painter.setFont(QFont("Sans Serif", 11))
            painter.drawText(
                self.rect(),
                Qt.AlignmentFlag.AlignCenter,
                self._label,
            )
            return
        margin = 18
        available = self.rect().adjusted(margin, margin, -margin, -margin)
        scaled = self._image.size().scaled(
            available.size(), Qt.AspectRatioMode.KeepAspectRatio
        )
        target = QRectF(
            available.x() + (available.width() - scaled.width()) / 2,
            available.y() + (available.height() - scaled.height()) / 2,
            scaled.width(),
            scaled.height(),
        )
        painter.setRenderHint(QPainter.RenderHint.SmoothPixmapTransform)
        painter.drawImage(target, self._image)
        if self._heatmap is not None:
            painter.drawImage(target, self._heatmap)
        painter.setPen(QPen(QColor("#4b5565"), 1))
        painter.drawRect(target)


class RecomputeWorker(QThread):
    completed = Signal(object)
    failed = Signal(object)

    def __init__(
        self,
        session: CompositorSession,
        *,
        parallel: bool,
        full: bool,
        max_workers: int,
    ) -> None:
        super().__init__()
        self.session = session
        self.parallel = parallel
        self.full = full
        self.max_workers = max_workers

    def run(self) -> None:
        try:
            result = self.session.run(
                parallel=self.parallel,
                max_workers=self.max_workers,
                full=self.full,
            )
            self.completed.emit(result)
        except Exception as exc:
            if isinstance(exc, PipelineError):
                payload = exc.to_dict()
            else:
                payload = {
                    "code": "E_INTERNAL",
                    "message": f"{type(exc).__name__}: {exc}",
                }
            self.failed.emit(payload)


class MainWindow(QMainWindow):
    def __init__(
        self,
        samples_directory: str | Path,
        checkpoint_directory: str | Path,
        *,
        source_version: int = 3,
        auto_run: bool = True,
    ) -> None:
        super().__init__()
        self.samples_directory = Path(samples_directory)
        self.checkpoint_directory = Path(checkpoint_directory)
        self.checkpoint_directory.mkdir(parents=True, exist_ok=True)
        self.source_version_number = source_version
        self.session: CompositorSession | None = None
        self.current_window = 0
        self.current_frame_index: int | None = None
        self.invalidated_windows: set[int] = set()
        self.reused_windows: set[int] = set()
        self.heatmaps: dict[int, np.ndarray] = {}
        self._worker: RecomputeWorker | None = None
        self.last_ui_result: RecomputeResult | None = None
        self.last_ui_error: dict[str, Any] | None = None
        self._pending_color_linear: tuple[float, float, float] | None = None
        self._window_buttons: dict[int, QPushButton] = {}
        self.setWindowTitle("VP Composite Inspector")
        self.resize(1460, 880)
        self._build_ui()
        self._apply_style()
        self._load_session(source_version)
        if auto_run and self.session is not None:
            result = self.session.run(parallel=False)
            self.reused_windows = set(result.reused_windows)
            self._refresh_all()
            self.statusBar().showMessage(
                f"初始合成完成：{result.progress_label} 窗口已提交"
            )

    def _build_ui(self) -> None:
        central = QWidget()
        self.setCentralWidget(central)
        root = QVBoxLayout(central)
        root.setContentsMargins(12, 10, 12, 10)
        root.setSpacing(8)

        toolbar = QHBoxLayout()
        toolbar.addWidget(QLabel("源版本"))
        self.source_version_combo = QComboBox()
        self.source_version_combo.setObjectName("sourceVersionCombo")
        for version in (1, 2, 3):
            self.source_version_combo.addItem(
                VERSION_LABELS[f"vpfc/{version}"], version
            )
        self.source_version_combo.setCurrentIndex(self.source_version_number - 1)
        self.source_version_combo.currentIndexChanged.connect(
            self._on_source_version_changed
        )
        toolbar.addWidget(self.source_version_combo, 2)

        toolbar.addSpacing(16)
        self.source_badge = QLabel()
        self.source_badge.setObjectName("sourceBadge")
        toolbar.addWidget(self.source_badge)
        toolbar.addStretch(1)

        toolbar.addWidget(QLabel("目标版本"))
        self.target_version_combo = QComboBox()
        self.target_version_combo.setObjectName("targetVersionCombo")
        for version in (1, 2, 3):
            self.target_version_combo.addItem(f"vpfc/{version}", f"vpfc/{version}")
        self.target_version_combo.setCurrentIndex(0)
        toolbar.addWidget(self.target_version_combo)

        self.parallel_checkbox = QCheckBox("并行窗口")
        self.parallel_checkbox.setChecked(True)
        self.parallel_checkbox.setObjectName("parallelCheckbox")
        toolbar.addWidget(self.parallel_checkbox)
        self.worker_count = QSpinBox()
        self.worker_count.setRange(1, 8)
        self.worker_count.setValue(2)
        self.worker_count.setPrefix("线程 ")
        self.worker_count.setObjectName("workerCount")
        toolbar.addWidget(self.worker_count)
        root.addLayout(toolbar)

        splitter = QSplitter(Qt.Orientation.Horizontal)
        splitter.setChildrenCollapsible(False)
        root.addWidget(splitter, 1)

        splitter.addWidget(self._build_layer_panel())
        splitter.addWidget(self._build_center_panel())
        splitter.addWidget(self._build_inspector_panel())
        splitter.setStretchFactor(0, 2)
        splitter.setStretchFactor(1, 5)
        splitter.setStretchFactor(2, 3)

    def _build_layer_panel(self) -> QWidget:
        group = QGroupBox("场景层栈")
        layout = QVBoxLayout(group)
        self.layer_source_label = QLabel()
        self.layer_source_label.setObjectName("layerSourceLabel")
        layout.addWidget(self.layer_source_label)
        self.layer_table = QTableWidget(0, 6)
        self.layer_table.setObjectName("layerTable")
        self.layer_table.setHorizontalHeaderLabels(
            ["#", "图层", "类型", "父层", "可见", "Alpha"]
        )
        self.layer_table.verticalHeader().setVisible(False)
        self.layer_table.setSelectionBehavior(
            QTableWidget.SelectionBehavior.SelectRows
        )
        self.layer_table.setSelectionMode(
            QTableWidget.SelectionMode.SingleSelection
        )
        self.layer_table.setEditTriggers(
            QTableWidget.EditTrigger.NoEditTriggers
        )
        header = self.layer_table.horizontalHeader()
        header.setSectionResizeMode(0, QHeaderView.ResizeMode.ResizeToContents)
        header.setSectionResizeMode(1, QHeaderView.ResizeMode.Stretch)
        header.setSectionResizeMode(2, QHeaderView.ResizeMode.ResizeToContents)
        header.setSectionResizeMode(3, QHeaderView.ResizeMode.ResizeToContents)
        header.setSectionResizeMode(4, QHeaderView.ResizeMode.ResizeToContents)
        header.setSectionResizeMode(5, QHeaderView.ResizeMode.ResizeToContents)
        self.layer_table.itemSelectionChanged.connect(self._on_layer_selected)
        layout.addWidget(self.layer_table, 1)

        editor = QGroupBox("图层参数")
        form = QGridLayout(editor)
        self.selected_layer_label = QLabel("未选择图层")
        self.selected_layer_label.setObjectName("selectedLayerLabel")
        self.selected_layer_label.setWordWrap(True)
        form.addWidget(self.selected_layer_label, 0, 0, 1, 2)
        form.addWidget(QLabel("Opacity"), 1, 0)
        self.opacity_spin = QDoubleSpinBox()
        self.opacity_spin.setObjectName("opacitySpin")
        self.opacity_spin.setRange(0.0, 1.0)
        self.opacity_spin.setSingleStep(0.01)
        self.opacity_spin.setDecimals(3)
        form.addWidget(self.opacity_spin, 1, 1)
        form.addWidget(QLabel("Alpha"), 2, 0)
        self.alpha_spin = QDoubleSpinBox()
        self.alpha_spin.setObjectName("alphaSpin")
        self.alpha_spin.setRange(0.0, 1.0)
        self.alpha_spin.setSingleStep(0.01)
        self.alpha_spin.setDecimals(3)
        form.addWidget(self.alpha_spin, 2, 1)
        form.addWidget(QLabel("可见"), 3, 0)
        self.visible_checkbox = QCheckBox("显示该层")
        self.visible_checkbox.setObjectName("visibleCheckbox")
        form.addWidget(self.visible_checkbox, 3, 1)
        form.addWidget(QLabel("颜色"), 4, 0)
        self.color_button = QPushButton("选择颜色")
        self.color_button.setObjectName("colorButton")
        self.color_button.clicked.connect(self._choose_color)
        form.addWidget(self.color_button, 4, 1)
        self.apply_layer_button = QPushButton("应用图层编辑")
        self.apply_layer_button.setObjectName("applyLayerButton")
        self.apply_layer_button.clicked.connect(self._apply_layer_edits)
        form.addWidget(self.apply_layer_button, 5, 0, 1, 2)
        layout.addWidget(editor)

        timebase = QGroupBox("时间基准")
        timebase_layout = QHBoxLayout(timebase)
        self.fps_spin = QSpinBox()
        self.fps_spin.setObjectName("fpsSpin")
        self.fps_spin.setRange(1, 240)
        self.fps_spin.setSuffix(" fps")
        self.apply_fps_button = QPushButton("应用")
        self.apply_fps_button.setObjectName("applyFpsButton")
        self.apply_fps_button.clicked.connect(self._apply_frame_rate)
        timebase_layout.addWidget(self.fps_spin, 1)
        timebase_layout.addWidget(self.apply_fps_button)
        layout.addWidget(timebase)
        return group

    def _build_center_panel(self) -> QWidget:
        panel = QWidget()
        layout = QVBoxLayout(panel)
        layout.setContentsMargins(0, 0, 0, 0)
        self.frame_canvas = FrameCanvas()
        layout.addWidget(self.frame_canvas, 1)
        control_row = QHBoxLayout()
        self.window_label = QLabel("窗口 -")
        self.window_label.setObjectName("windowLabel")
        control_row.addWidget(self.window_label)
        self.frame_slider = QSlider(Qt.Orientation.Horizontal)
        self.frame_slider.setObjectName("frameSlider")
        self.frame_slider.valueChanged.connect(self._on_frame_changed)
        control_row.addWidget(self.frame_slider, 1)
        self.frame_label = QLabel("帧 -")
        self.frame_label.setObjectName("frameLabel")
        control_row.addWidget(self.frame_label)
        self.heatmap_checkbox = QCheckBox("差异热力图")
        self.heatmap_checkbox.setObjectName("heatmapCheckbox")
        self.heatmap_checkbox.toggled.connect(self._refresh_frame_display)
        control_row.addWidget(self.heatmap_checkbox)
        layout.addLayout(control_row)

        matrix = QGroupBox("帧窗口状态矩阵")
        self.window_grid = QGridLayout(matrix)
        self.window_grid.setSpacing(6)
        layout.addWidget(matrix)
        return panel

    def _build_inspector_panel(self) -> QWidget:
        panel = QWidget()
        layout = QVBoxLayout(panel)
        layout.setContentsMargins(0, 0, 0, 0)

        progress_group = QGroupBox("进度与状态")
        progress_layout = QVBoxLayout(progress_group)
        self.progress_label = QLabel("0/0")
        self.progress_label.setObjectName("progressLabel")
        self.progress_label.setFont(QFont("Sans Serif", 18, QFont.Weight.DemiBold))
        progress_layout.addWidget(self.progress_label)
        self.status_detail = QLabel("尚未运行")
        self.status_detail.setWordWrap(True)
        self.status_detail.setObjectName("statusDetail")
        progress_layout.addWidget(self.status_detail)
        layout.addWidget(progress_group)

        stats_group = QGroupBox("逐帧统计")
        stats_layout = QVBoxLayout(stats_group)
        self.stats_view = QTextEdit()
        self.stats_view.setObjectName("statsView")
        self.stats_view.setReadOnly(True)
        self.stats_view.setMinimumHeight(170)
        stats_layout.addWidget(self.stats_view)
        layout.addWidget(stats_group, 1)

        invalidation_group = QGroupBox("失效与复用清单")
        invalidation_layout = QVBoxLayout(invalidation_group)
        self.invalidation_view = QTextEdit()
        self.invalidation_view.setObjectName("invalidationView")
        self.invalidation_view.setReadOnly(True)
        self.invalidation_view.setMinimumHeight(110)
        invalidation_layout.addWidget(self.invalidation_view)
        layout.addWidget(invalidation_group, 1)

        buttons = QGridLayout()
        self.recompute_button = QPushButton("增量重算")
        self.recompute_button.setObjectName("recomputeButton")
        self.recompute_button.clicked.connect(lambda: self._start_recompute(False))
        buttons.addWidget(self.recompute_button, 0, 0)
        self.full_recompute_button = QPushButton("全量重算")
        self.full_recompute_button.setObjectName("fullRecomputeButton")
        self.full_recompute_button.clicked.connect(lambda: self._start_recompute(True))
        buttons.addWidget(self.full_recompute_button, 0, 1)
        self.verify_button = QPushButton("校验并热力图")
        self.verify_button.setObjectName("verifyButton")
        self.verify_button.clicked.connect(self._verify_current_window)
        buttons.addWidget(self.verify_button, 1, 0)
        self.export_button = QPushButton("导出场景")
        self.export_button.setObjectName("exportButton")
        self.export_button.clicked.connect(self._export_dialog)
        buttons.addWidget(self.export_button, 1, 1)
        self.export_window_button = QPushButton("导出当前窗口")
        self.export_window_button.setObjectName("exportWindowButton")
        self.export_window_button.clicked.connect(self._export_window_dialog)
        buttons.addWidget(self.export_window_button, 2, 0, 1, 2)
        layout.addLayout(buttons)
        return panel

    def _apply_style(self) -> None:
        self.setStyleSheet(
            """
            QMainWindow { background: #11151c; color: #dce3ee; }
            QWidget { color: #dce3ee; font-size: 12px; }
            QGroupBox {
                border: 1px solid #2b3442; border-radius: 7px;
                margin-top: 12px; padding-top: 9px; background: #151b24;
                font-weight: 600;
            }
            QGroupBox::title { subcontrol-origin: margin; left: 10px; padding: 0 4px; }
            QTableWidget, QTextEdit, QComboBox, QSpinBox, QDoubleSpinBox {
                background: #0d1117; border: 1px solid #2b3442;
                border-radius: 5px; selection-background-color: #2c5c80;
            }
            QTableWidget::item { padding: 4px; }
            QHeaderView::section {
                background: #1c2430; border: 0; border-right: 1px solid #2b3442;
                padding: 5px; font-weight: 600;
            }
            QPushButton {
                background: #253247; border: 1px solid #3a4d68;
                border-radius: 5px; padding: 6px 10px;
            }
            QPushButton:hover { background: #30415a; }
            QPushButton:pressed { background: #1d293a; }
            QPushButton:disabled { color: #727b88; background: #171d26; }
            QCheckBox { spacing: 7px; }
            QSlider::groove:horizontal {
                height: 5px; background: #2b3442; border-radius: 2px;
            }
            QSlider::handle:horizontal {
                background: #6ca7d8; width: 15px; margin: -5px 0; border-radius: 7px;
            }
            #sourceBadge {
                background: #21384b; border: 1px solid #3c6686;
                border-radius: 10px; padding: 4px 9px; color: #a9d8ff;
            }
            #progressLabel { color: #8fd4b0; }
            #statusDetail { color: #9aa5b5; }
            """
        )

    def _checkpoint_subdirectory(self, version: int) -> Path:
        return self.checkpoint_directory / f"vpfc_{version}"

    def _load_session(self, version: int) -> None:
        self.source_version_number = version
        model = load_scene(self.samples_directory / f"scene_vpfc{version}.json")
        store = CheckpointStore(self._checkpoint_subdirectory(version))
        self.session = CompositorSession(
            model,
            store,
            max_workers=self.worker_count.value(),
            max_frame_buffers_per_thread=1,
        )
        self.invalidated_windows.clear()
        self.reused_windows = set(self.session.recovery.restored)
        self.heatmaps.clear()
        self.current_window = 0
        self.fps_spin.blockSignals(True)
        self.fps_spin.setValue(model.timeline.fps_num)
        self.fps_spin.blockSignals(False)
        self._refresh_all()
        if self.session.recovery.discarded:
            message = "检查点已整体作废：" + "; ".join(
                self.session.recovery.reasons
            )
            self.statusBar().showMessage(message)

    def _on_source_version_changed(self) -> None:
        if self._worker is not None and self._worker.isRunning():
            self.statusBar().showMessage("合成运行中，暂不能切换版本")
            return
        version = int(self.source_version_combo.currentData())
        try:
            self._load_session(version)
            if self.session is not None:
                result = self.session.run(parallel=False)
                self.reused_windows = set(result.reused_windows)
                self._refresh_all()
        except Exception as exc:
            self._show_error(exc)

    def _refresh_all(self) -> None:
        if self.session is None:
            return
        model = self.session.model
        self.setWindowTitle(
            f"VP Composite Inspector — {model.scene_id} — {model.source_version}"
        )
        self.source_badge.setText(f"源版本 {model.source_version}")
        self.layer_source_label.setText(
            f"源版本 {model.source_version} · {len(model.layers)} 层 · "
            f"尺寸 {model.canvas.width}×{model.canvas.height}×{model.canvas.channels} · "
            f"{model.timeline.fps_num}/{model.timeline.fps_den} fps"
        )
        self._refresh_layer_table()
        self._refresh_window_matrix()
        self._refresh_progress()
        self._select_window(self.current_window)
        self._refresh_invalidation_view()

    def _refresh_layer_table(self) -> None:
        assert self.session is not None
        model = self.session.model
        selected = self._selected_layer_id()
        self.layer_table.blockSignals(True)
        self.layer_table.setRowCount(len(model.layers))
        for row, layer in enumerate(model.layers):
            values = [
                str(layer.stack_order),
                layer.layer_id,
                layer.kind,
                layer.parent_id or "—",
                "是" if layer.visible else "否",
                f"{layer.alpha:.3f}",
            ]
            for column, value in enumerate(values):
                item = QTableWidgetItem(value)
                item.setData(Qt.ItemDataRole.UserRole, layer.layer_id)
                if column == 4 and not layer.visible:
                    item.setForeground(QColor("#c38b6b"))
                self.layer_table.setItem(row, column, item)
        self.layer_table.blockSignals(False)
        if selected is not None:
            for row in range(self.layer_table.rowCount()):
                item = self.layer_table.item(row, 1)
                if item is not None and item.text() == selected:
                    self.layer_table.selectRow(row)
                    break
        elif self.layer_table.rowCount():
            self.layer_table.selectRow(0)

    def _refresh_window_matrix(self) -> None:
        assert self.session is not None
        while self.window_grid.count():
            item = self.window_grid.takeAt(0)
            widget = item.widget()
            if widget is not None:
                widget.deleteLater()
        self._window_buttons.clear()
        for slot in self.session.windows:
            button = QPushButton()
            button.setObjectName(f"windowButton_{slot.index}")
            button.setCheckable(True)
            button.setMinimumHeight(54)
            lines = [
                f"窗口 {slot.index} · {slot.start}–{slot.end_exclusive - 1}",
                STATE_LABELS_ZH[slot.state],
            ]
            if slot.index in self.invalidated_windows:
                lines.append("已失效")
            elif slot.index in self.reused_windows:
                lines.append("复用缓存")
            button.setText("\n".join(lines))
            color = STATE_COLORS[slot.state]
            border = "#d9a34a" if slot.index in self.invalidated_windows else color
            button.setStyleSheet(
                f"QPushButton {{ background: {color}; border: 2px solid {border}; "
                f"border-radius: 6px; padding: 4px; color: #f4f7fb; }}"
                "QPushButton:checked { border: 2px solid #8fd4ff; }"
            )
            tooltip = [
                f"状态：{STATE_LABELS_ZH[slot.state]}",
                f"帧：{slot.start}..{slot.end_exclusive - 1}",
            ]
            if slot.invalidation_reason:
                tooltip.append(f"失效原因：{slot.invalidation_reason}")
            if slot.last_error:
                tooltip.append(f"错误：{slot.last_error.get('message')}")
            button.setToolTip("\n".join(tooltip))
            button.clicked.connect(
                lambda checked=False, index=slot.index: self._select_window(index)
            )
            self.window_grid.addWidget(button, slot.index // 3, slot.index % 3)
            self._window_buttons[slot.index] = button
        selected_button = self._window_buttons.get(self.current_window)
        if selected_button is not None:
            selected_button.setChecked(True)

    def _refresh_progress(self) -> None:
        assert self.session is not None
        committed, total = self.session.progress
        self.progress_label.setText(f"{committed}/{total} 窗口已提交")
        self.status_detail.setText(
            "已失效窗口：" + (", ".join(map(str, sorted(self.invalidated_windows))) or "无")
            + " ｜ 复用窗口："
            + (", ".join(map(str, sorted(self.reused_windows))) or "无")
        )

    def _selected_layer_id(self) -> str | None:
        row = self.layer_table.currentRow()
        if row < 0:
            return None
        item = self.layer_table.item(row, 1)
        return item.text() if item is not None else None

    def _selected_layer(self) -> Layer | None:
        if self.session is None:
            return None
        layer_id = self._selected_layer_id()
        if layer_id is None:
            return None
        return self.session.model.layer(layer_id)

    def _on_layer_selected(self) -> None:
        layer = self._selected_layer()
        if layer is None:
            self.selected_layer_label.setText("未选择图层")
            return
        self.selected_layer_label.setText(
            f"{layer.name}\n{layer.layer_id} · {layer.kind} · "
            f"源 {layer.source_version}\n"
            f"alpha_mode={layer.alpha_mode} · color={layer.source_color_space}"
        )
        self.opacity_spin.blockSignals(True)
        self.alpha_spin.blockSignals(True)
        self.visible_checkbox.blockSignals(True)
        self.opacity_spin.setValue(layer.opacity)
        self.alpha_spin.setValue(layer.alpha)
        self.visible_checkbox.setChecked(layer.visible)
        self.opacity_spin.blockSignals(False)
        self.alpha_spin.blockSignals(False)
        self.visible_checkbox.blockSignals(False)
        self._pending_color_linear = layer.color_linear
        self._update_color_button(layer.color_linear)

    def _update_color_button(self, linear: tuple[float, float, float]) -> None:
        srgb = linear_to_srgb(np.asarray(linear, dtype=np.float64))
        rgb = [int(round(float(channel) * 255.0)) for channel in srgb]
        self.color_button.setStyleSheet(
            f"background: rgb({rgb[0]},{rgb[1]},{rgb[2]}); "
            f"color: {'#111' if sum(rgb) > 380 else '#fff'};"
        )

    def _choose_color(self) -> None:
        layer = self._selected_layer()
        if layer is None:
            return
        srgb = linear_to_srgb(np.asarray(layer.color_linear, dtype=np.float64))
        initial = QColor.fromRgbF(*[float(channel) for channel in srgb])
        selected = QColorDialog.getColor(initial, self, "选择线性化源颜色")
        if not selected.isValid():
            return
        linear = tuple(
            float(value)
            for value in srgb_to_linear(
                np.asarray(
                    [selected.redF(), selected.greenF(), selected.blueF()],
                    dtype=np.float64,
                )
            )
        )
        self._pending_color_linear = linear
        self._update_color_button(linear)

    def _apply_layer_edits(self) -> None:
        if self.session is None:
            return
        layer = self._selected_layer()
        if layer is None:
            return
        changes: dict[str, Any] = {}
        if abs(self.opacity_spin.value() - layer.opacity) > 1e-9:
            changes["opacity"] = self.opacity_spin.value()
        if abs(self.alpha_spin.value() - layer.alpha) > 1e-9:
            changes["alpha"] = self.alpha_spin.value()
        if self.visible_checkbox.isChecked() != layer.visible:
            changes["visible"] = self.visible_checkbox.isChecked()
        if (
            self._pending_color_linear is not None
            and self._pending_color_linear != layer.color_linear
        ):
            changes["color_linear"] = self._pending_color_linear
        if not changes:
            self.statusBar().showMessage("没有待应用的图层参数变化")
            return
        try:
            result = self.session.apply_edits(layer.layer_id, changes)
        except Exception as exc:
            self._show_error(exc)
            return
        self.invalidated_windows.update(
            item.window_index
            for item in result.invalidations
            if item.window_index is not None
        )
        self.reused_windows.clear()
        self.heatmaps.clear()
        self._refresh_all()
        self.statusBar().showMessage(
            f"已应用 {layer.layer_id} 参数；{len(self.invalidated_windows)} 个窗口失效"
        )

    def _apply_frame_rate(self) -> None:
        if self.session is None:
            return
        try:
            result = self.session.set_frame_rate(self.fps_spin.value(), 1)
        except Exception as exc:
            self._show_error(exc)
            return
        self.invalidated_windows.update(
            item.window_index
            for item in result.invalidations
            if item.window_index is not None
        )
        self.reused_windows.clear()
        self.heatmaps.clear()
        self._refresh_all()
        self.statusBar().showMessage("时间基准已调整，所有帧窗口失效")

    def _select_window(self, window_index: int) -> None:
        if self.session is None:
            return
        slots = self.session.windows
        if window_index < 0 or window_index >= len(slots):
            return
        slot = slots[window_index]
        self.current_window = window_index
        for index, button in self._window_buttons.items():
            button.setChecked(index == window_index)
        self.window_label.setText(
            f"窗口 {slot.index} · 帧 {slot.start}–{slot.end_exclusive - 1} · "
            f"{STATE_LABELS_ZH[slot.state]}"
        )
        self.frame_slider.blockSignals(True)
        self.frame_slider.setRange(slot.start, slot.end_exclusive - 1)
        if self.current_frame_index is None or not (
            slot.start <= self.current_frame_index < slot.end_exclusive
        ):
            self.current_frame_index = slot.start
        self.frame_slider.setValue(self.current_frame_index)
        self.frame_slider.blockSignals(False)
        self._refresh_frame_display()

    def _on_frame_changed(self, frame_index: int) -> None:
        self.current_frame_index = frame_index
        self._refresh_frame_display()

    def _current_frame(self) -> FrameResult | None:
        if self.session is None or self.current_frame_index is None:
            return None
        for frame in self.session.frames_for_window(self.current_window):
            if frame.frame_index == self.current_frame_index:
                return frame
        return None

    def _refresh_frame_display(self) -> None:
        frame = self._current_frame()
        if frame is None:
            self.frame_canvas.clear("该窗口尚未提交；运行重算后可查看")
            self.frame_label.setText(f"帧 {self.current_frame_index if self.current_frame_index is not None else '-'}")
            self.stats_view.setPlainText("无已提交帧")
            return
        heatmap = None
        if self.heatmap_checkbox.isChecked():
            heatmap = self.heatmaps.get(frame.frame_index)
        self.frame_canvas.set_frame(frame, heatmap)
        self.frame_label.setText(f"帧 {frame.frame_index}")
        seconds = frame.frame_index * frame.fps_den / frame.fps_num
        lines = [
            f"frame: {frame.frame_index}",
            f"time: {seconds:.6f} s",
            f"size: {frame.width}×{frame.height}×{frame.channels}",
            f"fps: {frame.fps_num}/{frame.fps_den}",
            f"mean alpha: {frame.mean_alpha:.6f}",
            f"mean linear luminance: {frame.mean_linear_luminance:.6f}",
            f"opaque pixels: {frame.opaque_pixels}",
            "visible layers:",
            *[f"  · {layer_id}" for layer_id in frame.visible_layer_ids],
            f"content hash: {frame.content_hash}",
        ]
        if self.heatmap_checkbox.isChecked() and heatmap is None:
            lines.append("heatmap: 尚未校验该帧")
        self.stats_view.setPlainText("\n".join(lines))

    def _refresh_invalidation_view(self) -> None:
        if self.session is None:
            return
        lines = []
        for slot in self.session.windows:
            if slot.invalidation_reason:
                lines.append(
                    f"窗口 {slot.index} 失效 · {slot.invalidation_reason} · "
                    f"状态 {STATE_LABELS_ZH[slot.state]}"
                )
        if self.reused_windows:
            lines.append(
                "复用持久化缓存：" + ", ".join(map(str, sorted(self.reused_windows)))
            )
        if self.session.last_recompute and self.session.last_recompute.invalidations:
            for item in self.session.last_recompute.invalidations:
                stage = "导出阶段" if item.stage == "export" else f"窗口 {item.window_index}"
                lines.append(
                    f"{stage} · 层 {', '.join(item.layer_ids) or '全场景'} · "
                    f"{item.reason}"
                )
        self.invalidation_view.setPlainText("\n".join(lines) or "无失效窗口")

    def _start_recompute(self, full: bool) -> None:
        if self.session is None:
            return
        if self._worker is not None and self._worker.isRunning():
            self.statusBar().showMessage("已有合成任务运行中")
            return
        self._set_busy(True)
        self.last_ui_result = None
        self.last_ui_error = None
        worker = RecomputeWorker(
            self.session,
            parallel=self.parallel_checkbox.isChecked(),
            full=full,
            max_workers=self.worker_count.value(),
        )
        self._worker = worker
        worker.completed.connect(self._on_recompute_complete)
        worker.failed.connect(self._on_worker_failed)
        worker.finished.connect(lambda: self._set_busy(False))
        worker.start()
        self.statusBar().showMessage(
            "正在" + ("全量" if full else "增量") + "合成…"
        )

    def _on_recompute_complete(self, result: RecomputeResult) -> None:
        self.last_ui_result = result
        self.invalidated_windows.difference_update(result.recomputed_windows)
        self.reused_windows = set(result.reused_windows)
        self.heatmaps.clear()
        self._refresh_all()
        message = (
            f"合成完成：{result.progress_label}；提交 "
            f"{list(result.commit_order)}；复用 {list(result.reused_windows)}"
        )
        if result.failed_windows:
            message += f"；失败 {list(result.failed_windows)}"
        self.statusBar().showMessage(message)

    def _on_worker_failed(self, payload: dict[str, Any]) -> None:
        self.last_ui_error = payload
        self._set_busy(False)
        self.statusBar().showMessage(
            f"{payload.get('code')}: {payload.get('message')}"
        )
        self._refresh_all()

    def _set_busy(self, busy: bool) -> None:
        for widget in (
            self.recompute_button,
            self.full_recompute_button,
            self.verify_button,
            self.export_button,
            self.export_window_button,
            self.apply_layer_button,
            self.apply_fps_button,
            self.source_version_combo,
        ):
            widget.setEnabled(not busy)
        if busy:
            self.progress_label.setText("合成进行中…")

    def _verify_current_window(self) -> None:
        if self.session is None:
            return
        try:
            comparison = self.session.compare_with_full(
                self.current_window, raise_on_diff=False
            )
        except Exception as exc:
            self._show_error(exc)
            return
        self.heatmaps = comparison.heatmaps
        self.heatmap_checkbox.setChecked(True)
        self._refresh_frame_display()
        if comparison.ok:
            self.statusBar().showMessage(
                f"窗口 {self.current_window} 与全量重算逐像素一致"
            )
        else:
            report = comparison.report
            assert report is not None
            message = (
                f"E_DIFF 首个不一致：窗口 {report.window_index}，"
                f"帧 {report.frame_index}，像素 ({report.x},{report.y})，"
                f"层 {report.layer_id or '无'}"
            )
            self.statusBar().showMessage(message)
            QMessageBox.warning(self, "像素不一致", message)

    def export_to(
        self,
        output_directory: str | Path,
        *,
        window_indices: list[int] | None = None,
    ) -> ExportResult:
        if self.session is None:
            raise PipelineError("E_STATE", "No session", "session")
        frames = self.session.current_frames()
        target = str(self.target_version_combo.currentData())
        result = export_bundle(
            self.session.model,
            frames,
            target,
            output_directory,
            window_indices=window_indices,
        )
        self.session.mark_exported()
        self.statusBar().showMessage(
            f"已导出 {target} 到 {output_directory}；"
            f"E_LOSSY 警告 {len(result.warnings)} 条"
        )
        return result

    def _export_dialog(self) -> None:
        if self.session is None:
            return
        default = (
            Path.cwd()
            / "artifacts"
            / "exports"
            / f"{self.session.model.source_version.replace('/', '_')}_"
            f"{self.target_version_combo.currentData().replace('/', '_')}"
        )
        selected = QFileDialog.getExistingDirectory(
            self, "选择导出目录", str(default)
        )
        if not selected:
            return
        try:
            self.export_to(selected)
        except Exception as exc:
            self._show_error(exc)

    def _export_window_dialog(self) -> None:
        default = (
            Path.cwd()
            / "artifacts"
            / "exports"
            / f"window_{self.current_window}_"
            f"{self.target_version_combo.currentData().replace('/', '_')}"
        )
        selected = QFileDialog.getExistingDirectory(
            self, "选择窗口导出目录", str(default)
        )
        if not selected:
            return
        try:
            self.export_to(selected, window_indices=[self.current_window])
        except Exception as exc:
            self._show_error(exc)

    def _show_error(self, exc: Exception) -> None:
        if isinstance(exc, PipelineError):
            message = f"{exc.code}: {exc.message}"
            if exc.field_path:
                message += f"\n字段路径: {exc.field_path}"
            if exc.details:
                message += f"\n细节: {exc.details}"
            self.statusBar().showMessage(message.replace("\n", " · "))
            self.stats_view.setPlainText(message)
        else:
            self.statusBar().showMessage(f"{type(exc).__name__}: {exc}")
            self.stats_view.setPlainText(f"{type(exc).__name__}: {exc}")

    def closeEvent(self, event: Any) -> None:
        if self._worker is not None and self._worker.isRunning():
            self._worker.wait(3000)
        event.accept()


def run_ui(
    samples_directory: str | Path,
    checkpoint_directory: str | Path,
    *,
    source_version: int = 3,
) -> int:
    app = QApplication.instance()
    owns_application = app is None
    if app is None:
        app = QApplication(sys.argv)
    app.setApplicationName("VP Composite Inspector")
    window = MainWindow(
        samples_directory,
        checkpoint_directory,
        source_version=source_version,
    )
    window.show()
    if owns_application:
        return app.exec()
    return 0
