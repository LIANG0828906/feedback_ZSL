"""PySide6 desktop workbench entry point and window."""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path
from typing import Any, Mapping

import numpy as np
from PySide6.QtCore import QThread, QTimer, Qt
from PySide6.QtGui import QColor, QCloseEvent, QFont, QPalette
from PySide6.QtWidgets import (
    QApplication,
    QComboBox,
    QDoubleSpinBox,
    QFileDialog,
    QFormLayout,
    QGridLayout,
    QGroupBox,
    QHBoxLayout,
    QLabel,
    QMainWindow,
    QMessageBox,
    QPlainTextEdit,
    QPushButton,
    QScrollArea,
    QSpinBox,
    QSplitter,
    QTabWidget,
    QTableWidget,
    QTableWidgetItem,
    QVBoxLayout,
    QWidget,
)

from ..diagnostics import ComparisonReport
from ..errors import WorkbenchError
from ..imaging import DecodedImage, difference_heatmap, save_png
from ..pipeline import PipelineRun, PipelineSession, ResourceLimits, RunControl
from ..samples import generate_samples, load_manifest, load_truth
from ..snapshot import SnapshotStore
from ..stages import (
    DEFAULT_PARAMS,
    STAGE_IDS,
    STAGE_SPECS,
    normalize_pipeline_params,
)
from .widgets import (
    STATUS_COLORS,
    STATUS_LABELS,
    DependencyGraphWidget,
    ImageCanvas,
)
from .workers import SessionWorker


APP_STYLE = """
QMainWindow, QWidget {
    background: #0b0f19;
    color: #dde5f4;
}
QGroupBox {
    background: #111827;
    border: 1px solid #273148;
    border-radius: 9px;
    margin-top: 12px;
    padding: 12px 8px 8px 8px;
    font-weight: 600;
}
QGroupBox::title {
    subcontrol-origin: margin;
    left: 12px;
    padding: 0 6px;
    color: #b9c6e2;
}
QLabel#muted {
    color: #7d8aa4;
}
QLabel#sectionTitle {
    color: #eef3ff;
    font-size: 16px;
    font-weight: 700;
}
QPushButton {
    background: #1a2438;
    color: #dce6f8;
    border: 1px solid #33415f;
    border-radius: 7px;
    padding: 7px 12px;
}
QPushButton:hover {
    background: #23314d;
    border-color: #51688e;
}
QPushButton:pressed {
    background: #152034;
}
QPushButton:disabled {
    color: #59647a;
    background: #131a29;
    border-color: #242d40;
}
QPushButton#primary {
    background: #1c5bae;
    border-color: #3d82d8;
    color: white;
    font-weight: 700;
}
QPushButton#primary:hover {
    background: #246bc7;
}
QPushButton#danger {
    background: #4d1d2b;
    border-color: #8a3850;
}
QComboBox, QSpinBox, QDoubleSpinBox {
    background: #0d1422;
    border: 1px solid #2c3953;
    border-radius: 6px;
    padding: 5px 8px;
    selection-background-color: #265faa;
}
QComboBox::drop-down {
    border: none;
}
QComboBox QAbstractItemView {
    background: #111a2b;
    border: 1px solid #34435f;
    selection-background-color: #245a9e;
}
QTabWidget::pane {
    border: 1px solid #232d43;
    background: #0d1320;
    border-radius: 8px;
}
QTabBar::tab {
    background: #111827;
    color: #8491aa;
    padding: 8px 16px;
    margin-right: 2px;
    border-top-left-radius: 7px;
    border-top-right-radius: 7px;
}
QTabBar::tab:selected {
    background: #1b2740;
    color: #e7efff;
}
QTableWidget {
    background: #0d1320;
    alternate-background-color: #101827;
    gridline-color: #202a3e;
    border: none;
}
QHeaderView::section {
    background: #151f32;
    color: #a8b6d2;
    border: none;
    border-bottom: 1px solid #2a3650;
    padding: 7px;
}
QPlainTextEdit {
    background: #090e18;
    color: #c8d4e8;
    border: none;
    selection-background-color: #245797;
}
QSplitter::handle {
    background: #171f30;
    width: 5px;
    height: 5px;
}
QScrollArea {
    border: none;
    background: transparent;
}
QScrollBar:vertical {
    background: #0c1220;
    width: 10px;
}
QScrollBar::handle:vertical {
    background: #31405f;
    border-radius: 5px;
    min-height: 24px;
}
QStatusBar {
    background: #0a0f19;
    color: #9eabc4;
}
"""


class MainWindow(QMainWindow):
    def __init__(
        self,
        samples_dir: str | Path = "samples",
        cache_dir: str | Path = ".cache",
        *,
        auto_start: bool = True,
    ) -> None:
        super().__init__()
        self.samples_dir = Path(samples_dir)
        self.cache_dir = Path(cache_dir)
        self._ensure_samples()
        self.manifest = load_manifest(self.samples_dir)
        self.samples: list[dict[str, Any]] = list(self.manifest["samples"])
        self.current_sample_index = 0
        self.session: PipelineSession | None = None
        self._initializing = True
        self._busy = False
        self._thread: QThread | None = None
        self._worker: SessionWorker | None = None
        self._task_name: str | None = None
        self._task_control: RunControl | None = None
        self._last_run: PipelineRun | None = None
        self._last_comparison: ComparisonReport | None = None
        self._current_truth: dict[str, Any] | None = None
        self.param_widgets: dict[str, dict[str, QSpinBox | QDoubleSpinBox]] = {}
        self.stage_status_labels: dict[str, QLabel] = {}
        self.stage_key_labels: dict[str, QLabel] = {}

        self.setWindowTitle("Lumen Filter Studio — 游戏与多媒体图像滤波工作台")
        self.resize(1480, 940)
        self.setMinimumSize(1120, 760)
        self._build_ui()
        self._prime_from_snapshot()
        initial_params = self._collect_params()
        self.cache_dir.mkdir(parents=True, exist_ok=True)
        self.session = PipelineSession(
            self.cache_dir / "workbench.snapshot",
            params=initial_params,
            limits=ResourceLimits(),
        )
        self._initializing = False
        self._load_current_sample(auto_run=False)
        self._refresh_all()
        if auto_start:
            QTimer.singleShot(90, lambda: self._start_pipeline(force_full=False))

    def _ensure_samples(self) -> None:
        manifest_path = self.samples_dir / "manifest.json"
        if not manifest_path.is_file():
            generate_samples(self.samples_dir)

    def _build_ui(self) -> None:
        root = QWidget()
        root_layout = QVBoxLayout(root)
        root_layout.setContentsMargins(12, 10, 12, 10)
        root_layout.setSpacing(9)
        root_layout.addLayout(self._build_toolbar())

        splitter = QSplitter(Qt.Orientation.Horizontal)
        splitter.setChildrenCollapsible(False)
        splitter.addWidget(self._build_sidebar())
        splitter.addWidget(self._build_workspace())
        splitter.setStretchFactor(0, 0)
        splitter.setStretchFactor(1, 1)
        splitter.setSizes([320, 1120])
        root_layout.addWidget(splitter, 1)
        self.setCentralWidget(root)

    def _build_toolbar(self) -> QHBoxLayout:
        layout = QHBoxLayout()
        layout.setSpacing(8)
        title = QLabel("LUMEN")
        title.setObjectName("sectionTitle")
        title.setStyleSheet("letter-spacing: 2px; color: #77b8ff;")
        layout.addWidget(title)
        subtitle = QLabel("FILTER STUDIO")
        subtitle.setObjectName("muted")
        layout.addWidget(subtitle)
        layout.addSpacing(18)
        layout.addWidget(QLabel("样本"))
        self.sample_combo = QComboBox()
        for sample in self.samples:
            self.sample_combo.addItem(
                f"{sample['title']}  ·  {sample['mode']} {sample['width']}×{sample['height']}"
            )
        self.sample_combo.currentIndexChanged.connect(self._on_sample_changed)
        self.sample_combo.setMinimumWidth(310)
        layout.addWidget(self.sample_combo)
        self.truth_checkbox = QPushButton("几何真值轮廓")
        self.truth_checkbox.setCheckable(True)
        self.truth_checkbox.toggled.connect(self._on_truth_toggled)
        layout.addWidget(self.truth_checkbox)
        layout.addStretch(1)
        self.apply_button = QPushButton("应用参数 · 增量更新")
        self.apply_button.setObjectName("primary")
        self.apply_button.clicked.connect(lambda: self._start_pipeline(force_full=False))
        layout.addWidget(self.apply_button)
        self.rebuild_button = QPushButton("全量重算")
        self.rebuild_button.clicked.connect(lambda: self._start_pipeline(force_full=True))
        layout.addWidget(self.rebuild_button)
        self.verify_button = QPushButton("全量逐像素验算")
        self.verify_button.clicked.connect(self._start_compare)
        layout.addWidget(self.verify_button)
        return layout

    def _build_sidebar(self) -> QWidget:
        panel = QWidget()
        panel.setMinimumWidth(292)
        panel.setMaximumWidth(380)
        layout = QVBoxLayout(panel)
        layout.setContentsMargins(0, 0, 5, 0)
        layout.setSpacing(8)

        identity = QGroupBox("当前素材")
        identity_layout = QVBoxLayout(identity)
        self.asset_title = QLabel("—")
        self.asset_title.setWordWrap(True)
        self.asset_meta = QLabel("—")
        self.asset_meta.setObjectName("muted")
        self.asset_hash = QLabel("—")
        self.asset_hash.setObjectName("muted")
        self.asset_hash.setWordWrap(True)
        self.truth_summary = QLabel("几何真值: —")
        self.truth_summary.setObjectName("muted")
        self.truth_summary.setWordWrap(True)
        identity_layout.addWidget(self.asset_title)
        identity_layout.addWidget(self.asset_meta)
        identity_layout.addWidget(self.asset_hash)
        identity_layout.addWidget(self.truth_summary)
        layout.addWidget(identity)

        scroll = QScrollArea()
        scroll.setWidgetResizable(True)
        stage_host = QWidget()
        stage_layout = QVBoxLayout(stage_host)
        stage_layout.setContentsMargins(0, 0, 0, 0)
        stage_layout.setSpacing(4)
        for stage_id in STAGE_IDS:
            stage_layout.addWidget(self._build_stage_group(stage_id))
        stage_layout.addStretch(1)
        scroll.setWidget(stage_host)
        layout.addWidget(scroll, 1)

        controls = QGroupBox("运行控制")
        controls_layout = QGridLayout(controls)
        controls_layout.setSpacing(6)
        self.pause_button = QPushButton("暂停")
        self.pause_button.clicked.connect(self._toggle_pause)
        self.cancel_button = QPushButton("取消")
        self.cancel_button.setObjectName("danger")
        self.cancel_button.clicked.connect(self._cancel_task)
        self.retry_button = QPushButton("重试失败阶段")
        self.retry_button.clicked.connect(self._retry)
        self.export_button = QPushButton("导出增强图")
        self.export_button.clicked.connect(self._export_output)
        self.heatmap_export_button = QPushButton("导出热力图")
        self.heatmap_export_button.clicked.connect(self._export_heatmap)
        controls_layout.addWidget(self.pause_button, 0, 0)
        controls_layout.addWidget(self.cancel_button, 0, 1)
        controls_layout.addWidget(self.retry_button, 1, 0)
        controls_layout.addWidget(self.export_button, 1, 1)
        controls_layout.addWidget(self.heatmap_export_button, 2, 0, 1, 2)
        layout.addWidget(controls)
        return panel

    def _build_stage_group(self, stage_id: str) -> QGroupBox:
        spec = STAGE_SPECS[stage_id]
        group = QGroupBox(spec.display_name)
        layout = QFormLayout(group)
        layout.setLabelAlignment(Qt.AlignmentFlag.AlignRight)
        layout.setFormAlignment(Qt.AlignmentFlag.AlignLeft)
        layout.setHorizontalSpacing(10)
        layout.setVerticalSpacing(5)
        status = QLabel("● 空")
        status.setStyleSheet(f"color: {STATUS_COLORS['empty']}; font-weight: 700;")
        layout.addRow("状态", status)
        self.stage_status_labels[stage_id] = status
        dependency_text = "根阶段" if not spec.dependencies else f"← {', '.join(spec.dependencies)}"
        dependency = QLabel(dependency_text)
        dependency.setObjectName("muted")
        layout.addRow("依赖", dependency)
        stage_widgets: dict[str, QSpinBox | QDoubleSpinBox] = {}
        for param in spec.params:
            if param.kind == "int":
                widget: QSpinBox | QDoubleSpinBox = QSpinBox()
                widget.setRange(int(param.minimum), int(param.maximum))
                widget.setSingleStep(int(param.step))
                widget.setValue(int(param.default))
            else:
                widget = QDoubleSpinBox()
                widget.setDecimals(3)
                widget.setRange(float(param.minimum), float(param.maximum))
                widget.setSingleStep(float(param.step))
                widget.setValue(float(param.default))
            widget.setKeyboardTracking(False)
            widget.setToolTip(
                f"{param.name}: [{param.minimum}, {param.maximum}]，默认 {param.default}"
            )
            widget.valueChanged.connect(self._on_parameter_edited)
            layout.addRow(param.label, widget)
            stage_widgets[param.name] = widget
        self.param_widgets[stage_id] = stage_widgets
        key_label = QLabel("key —")
        key_label.setObjectName("muted")
        key_label.setWordWrap(True)
        layout.addRow("缓存", key_label)
        self.stage_key_labels[stage_id] = key_label
        return group

    def _build_workspace(self) -> QWidget:
        right = QWidget()
        layout = QVBoxLayout(right)
        layout.setContentsMargins(5, 0, 0, 0)
        layout.setSpacing(8)

        preview_splitter = QSplitter(Qt.Orientation.Horizontal)
        self.original_canvas = ImageCanvas("原图")
        self.result_canvas = ImageCanvas("增强结果")
        preview_splitter.addWidget(self.original_canvas)
        preview_splitter.addWidget(self.result_canvas)
        preview_splitter.setSizes([560, 560])
        layout.addWidget(preview_splitter, 3)

        tabs = QTabWidget()
        heatmap_host = QWidget()
        heatmap_layout = QVBoxLayout(heatmap_host)
        heatmap_layout.setContentsMargins(6, 6, 6, 6)
        self.heatmap_canvas = ImageCanvas("差异热力图 · 原图 ↔ 最终增强")
        heatmap_layout.addWidget(self.heatmap_canvas)
        heatmap_caption = QLabel(
            "颜色越亮表示跨通道最大绝对差异越大；深紫/黑表示接近无损。"
        )
        heatmap_caption.setObjectName("muted")
        heatmap_layout.addWidget(heatmap_caption)
        tabs.addTab(heatmap_host, "差异热力图")

        cache_host = QWidget()
        cache_layout = QVBoxLayout(cache_host)
        cache_layout.setContentsMargins(6, 6, 6, 6)
        self.graph_widget = DependencyGraphWidget()
        cache_layout.addWidget(self.graph_widget, 2)
        self.cache_table = QTableWidget(0, 6)
        self.cache_table.setHorizontalHeaderLabels(
            ["阶段", "状态", "有效", "缓存键", "上游缓存键", "阶段耗时"]
        )
        self.cache_table.verticalHeader().setVisible(False)
        self.cache_table.setAlternatingRowColors(True)
        self.cache_table.setEditTriggers(QTableWidget.EditTrigger.NoEditTriggers)
        self.cache_table.setSelectionBehavior(
            QTableWidget.SelectionBehavior.SelectRows
        )
        self.cache_table.horizontalHeader().setStretchLastSection(False)
        self.cache_table.setColumnWidth(0, 105)
        self.cache_table.setColumnWidth(1, 90)
        self.cache_table.setColumnWidth(2, 55)
        self.cache_table.setColumnWidth(3, 265)
        self.cache_table.setColumnWidth(4, 265)
        self.cache_table.setColumnWidth(5, 90)
        cache_layout.addWidget(self.cache_table, 3)
        tabs.addTab(cache_host, "依赖与缓存")

        diagnostics_host = QWidget()
        diagnostics_layout = QVBoxLayout(diagnostics_host)
        diagnostics_layout.setContentsMargins(6, 6, 6, 6)
        self.diagnostics_view = QPlainTextEdit()
        self.diagnostics_view.setReadOnly(True)
        font = QFont("Cascadia Mono")
        font.setStyleHint(QFont.StyleHint.Monospace)
        font.setPointSize(9)
        self.diagnostics_view.setFont(font)
        diagnostics_layout.addWidget(self.diagnostics_view)
        tabs.addTab(diagnostics_host, "诊断统计")

        layout.addWidget(tabs, 2)
        return right

    def _prime_from_snapshot(self) -> None:
        snapshot_path = self.cache_dir / "workbench.snapshot"
        manifest = SnapshotStore(snapshot_path).peek_manifest()
        if not manifest:
            return
        saved_name = Path(manifest.get("image", {}).get("path", "")).name
        for index, sample in enumerate(self.samples):
            if sample["file"] == saved_name:
                self.current_sample_index = index
                self.sample_combo.setCurrentIndex(index)
                break
        saved_params = manifest.get("params")
        if not isinstance(saved_params, dict):
            return
        try:
            normalized = normalize_pipeline_params(saved_params)
        except WorkbenchError:
            return
        if manifest.get("stage_versions") != {
            stage_id: STAGE_SPECS[stage_id].version for stage_id in STAGE_IDS
        }:
            return
        self._apply_params_to_widgets(normalized)

    def _apply_params_to_widgets(
        self,
        params: Mapping[str, Mapping[str, int | float]],
    ) -> None:
        for stage_id in STAGE_IDS:
            for name, widget in self.param_widgets[stage_id].items():
                value = params[stage_id][name]
                widget.blockSignals(True)
                if isinstance(widget, QSpinBox):
                    widget.setValue(int(value))
                else:
                    widget.setValue(float(value))
                widget.blockSignals(False)

    def _collect_params(self) -> dict[str, dict[str, int | float]]:
        result: dict[str, dict[str, int | float]] = {}
        for stage_id in STAGE_IDS:
            stage_params: dict[str, int | float] = {}
            for name, widget in self.param_widgets[stage_id].items():
                stage_params[name] = (
                    int(widget.value())
                    if isinstance(widget, QSpinBox)
                    else float(widget.value())
                )
            result[stage_id] = stage_params
        return normalize_pipeline_params(result)

    def _on_parameter_edited(self, value: Any) -> None:
        del value
        if self._initializing:
            return
        self.apply_button.setText("应用参数 · 增量更新 *")

    def _on_sample_changed(self, index: int) -> None:
        if self._initializing:
            return
        if self._busy:
            self.sample_combo.blockSignals(True)
            self.sample_combo.setCurrentIndex(self.current_sample_index)
            self.sample_combo.blockSignals(False)
            return
        self.current_sample_index = index
        self._last_run = None
        self._last_comparison = None
        self._load_current_sample(auto_run=True)

    def _load_current_sample(self, *, auto_run: bool) -> None:
        if self.session is None:
            return
        sample = self.samples[self.current_sample_index]
        image_path = self.samples_dir / sample["file"]
        try:
            self.session.set_pipeline_params(self._collect_params())
            restore = self.session.set_image(image_path)
        except WorkbenchError as exc:
            self._show_error(exc.to_dict())
            return
        self._current_truth = load_truth(self.samples_dir, sample)
        self.original_canvas.set_truth(self._current_truth)
        self.result_canvas.set_truth(self._current_truth)
        self.original_canvas.set_empty_message("输入图像")
        self.original_canvas.set_image(self.session.image.array if self.session.image else None)
        self.asset_title.setText(sample["title"])
        self.truth_summary.setText(
            f"几何真值: {len(self._current_truth.get('objects', []))} 个对象，"
            f"可切换轮廓叠加"
        )
        self._refresh_all()
        self.statusBar().showMessage(f"恢复状态: {restore.status} · {restore.reason}", 8000)
        self.apply_button.setText("应用参数 · 增量更新")
        if auto_run:
            self._start_pipeline(force_full=False)

    def _start_pipeline(self, *, force_full: bool) -> None:
        if self.session is None or self._busy:
            return
        try:
            invalidated = self.session.set_pipeline_params(self._collect_params())
        except WorkbenchError as exc:
            self._show_error(exc.to_dict())
            return
        if invalidated:
            self._refresh_status_panels()
            self._refresh_diagnostics()
        control = RunControl()
        self._task_control = control

        def action(emit: Any) -> PipelineRun:
            return self.session.run(
                control=control,
                listener=emit,
                force_full=force_full,
            )

        self._start_worker("run", action)

    def _start_compare(self) -> None:
        if self.session is None or self._busy:
            return
        control = RunControl()
        self._task_control = control

        def action(emit: Any) -> ComparisonReport:
            del emit
            return self.session.compare_with_full(control=control)

        self._start_worker("compare", action)

    def _start_worker(self, task_name: str, action: Any) -> None:
        self._busy = True
        self._task_name = task_name
        self._set_controls_busy()
        thread = QThread(self)
        worker = SessionWorker(task_name, action)
        worker.moveToThread(thread)
        thread.started.connect(worker.run)
        worker.event.connect(self._on_worker_event)
        worker.completed.connect(self._on_worker_completed)
        worker.failed.connect(self._on_worker_failed)
        worker.completed.connect(thread.quit)
        worker.failed.connect(thread.quit)
        thread.finished.connect(self._on_thread_finished)
        self._thread = thread
        self._worker = worker
        label = "全量验算" if task_name == "compare" else "滤波处理"
        self.statusBar().showMessage(f"{label}运行中…")
        thread.start()

    def _on_worker_event(self, event: dict[str, Any]) -> None:
        event_type = event.get("type")
        if event_type == "run_start":
            self.statusBar().showMessage(
                f"运行路径: {event.get('path_type')} / {event.get('source')}"
            )
        elif event_type == "stage_start":
            display = STAGE_SPECS[event["stage"]].display_name
            self.statusBar().showMessage(f"正在重算阶段: {display}")
        elif event_type == "cache_hit":
            display = STAGE_SPECS[event["stage"]].display_name
            self.statusBar().showMessage(f"缓存命中: {display}")
        elif event_type == "stage_done":
            display = STAGE_SPECS[event["stage"]].display_name
            self.statusBar().showMessage(
                f"阶段完成: {display} · {event.get('elapsed_sec', 0):.4f}s"
            )
        elif event_type == "invalidated":
            self.statusBar().showMessage(
                "缓存失效: " + ", ".join(event.get("stages", []))
            )
        self._refresh_status_panels()

    def _on_worker_completed(self, task_name: str, result: object) -> None:
        if task_name == "run" and isinstance(result, PipelineRun):
            self._last_run = result
            if result.ok:
                self.statusBar().showMessage(
                    f"处理完成 · {result.diagnostics.path_type} · "
                    f"重算 {result.diagnostics.recompute_count} / "
                    f"命中 {result.diagnostics.cache_hit_count}"
                )
            elif result.status == "cancelled":
                self.statusBar().showMessage(
                    "处理已取消；已完成阶段已持久化"
                )
            else:
                self.statusBar().showMessage(
                    f"处理失败: {(result.error or {}).get('code', 'E_STAGE')}"
                )
            self.apply_button.setText("应用参数 · 增量更新")
        elif task_name == "compare" and isinstance(result, ComparisonReport):
            self._last_comparison = result
            if result.equal:
                self.statusBar().showMessage(
                    "自动比对通过：增量与空缓存全量逐像素一致"
                )
            elif result.code == "E_DIFF":
                self.statusBar().showMessage(
                    f"E_DIFF · {result.message}", 20000
                )
            else:
                self.statusBar().showMessage(
                    f"验算未通过: {result.code} · {result.message}", 15000
                )
        self._refresh_all()

    def _on_worker_failed(self, task_name: str, error: object) -> None:
        del task_name
        if isinstance(error, dict):
            self._show_error(error)
        self.statusBar().showMessage(f"工作线程异常: {error}")
        self._refresh_all()

    def _on_thread_finished(self) -> None:
        worker = self._worker
        thread = self._thread
        self._worker = None
        self._thread = None
        self._task_name = None
        self._task_control = None
        self._busy = False
        self.pause_button.setText("暂停")
        self._set_controls_enabled(True)
        if worker is not None:
            worker.deleteLater()
        if thread is not None:
            thread.deleteLater()

    def _set_controls_busy(self) -> None:
        self._set_controls_enabled(False)
        self.pause_button.setEnabled(True)
        self.cancel_button.setEnabled(True)

    def _set_controls_enabled(self, enabled: bool) -> None:
        for widget in (
            self.sample_combo,
            self.apply_button,
            self.rebuild_button,
            self.verify_button,
            self.retry_button,
            self.export_button,
            self.heatmap_export_button,
            self.truth_checkbox,
        ):
            widget.setEnabled(enabled)
        for stage_widgets in self.param_widgets.values():
            for widget in stage_widgets.values():
                widget.setEnabled(enabled)
        if enabled:
            self.pause_button.setEnabled(False)
            self.cancel_button.setEnabled(False)

    def _toggle_pause(self) -> None:
        control = self._task_control
        if control is None:
            return
        if control.paused:
            control.resume()
            self.pause_button.setText("暂停")
            self.statusBar().showMessage("已继续处理")
        else:
            control.request_pause()
            self.pause_button.setText("继续")
            self.statusBar().showMessage("已请求暂停（阶段边界的协作式检查点）")

    def _cancel_task(self) -> None:
        control = self._task_control
        if control is None:
            return
        control.cancel()
        self.pause_button.setText("暂停")
        self.statusBar().showMessage("已请求取消；正在完成原子阶段提交")

    def _retry(self) -> None:
        if self._busy or self.session is None:
            return
        control = RunControl()
        self._task_control = control
        self._start_worker(
            "run",
            lambda emit: self.session.retry(control=control, listener=emit),
        )

    def _on_truth_toggled(self, checked: bool) -> None:
        self.original_canvas.set_show_truth(checked)
        self.result_canvas.set_show_truth(checked)

    def _refresh_all(self) -> None:
        self._refresh_asset()
        self._refresh_images()
        self._refresh_status_panels()
        self._refresh_diagnostics()

    def _refresh_asset(self) -> None:
        if self.session is None or self.session.image is None:
            return
        image: DecodedImage = self.session.image
        self.asset_meta.setText(
            f"{image.mode} · {image.width}×{image.height} · {image.channels} 通道 · uint8"
        )
        self.asset_hash.setText(
            f"输入哈希: {image.content_hash[:24]}…"
        )

    def _last_cached_output(self) -> np.ndarray | None:
        if self.session is None:
            return None
        entries = self.session.cache.valid_prefix()
        return entries[-1].output if entries else None

    def _refresh_images(self) -> None:
        if self.session is None or self.session.image is None:
            return
        original = self.session.image.array
        self.original_canvas.set_image(original)
        output = self._last_cached_output()
        self.result_canvas.set_image(output)
        if output is None:
            self.result_canvas.set_empty_message("等待阶段输出")
            self.heatmap_canvas.set_image(None)
            self.heatmap_canvas.set_empty_message("等待完成阶段输出")
            return
        valid = self.session.cache.valid_prefix()
        final_stage = valid[-1].stage
        if len(valid) == len(STAGE_IDS):
            self.result_canvas.set_title("增强结果 · local_contrast")
            self.result_canvas.set_empty_message("")
        else:
            display = STAGE_SPECS[final_stage].display_name
            self.result_canvas.set_title(f"中间结果 · {display}")
            self.result_canvas.set_empty_message("")
        if output.shape == original.shape:
            self.heatmap_canvas.set_image(difference_heatmap(original, output))
            self.heatmap_canvas.set_empty_message("")
        else:
            self.heatmap_canvas.set_image(None)
            self.heatmap_canvas.set_empty_message("输出尺寸不一致")

    def _refresh_status_panels(self) -> None:
        if self.session is None:
            return
        rows = self.session.cache_rows()
        errors = self.session.stage_errors
        for row in rows:
            stage_id = row["stage"]
            status = row["status"]
            label = self.stage_status_labels[stage_id]
            label.setText(f"● {STATUS_LABELS.get(status, status)}")
            label.setStyleSheet(
                f"color: {STATUS_COLORS.get(status, '#596174')}; font-weight: 700;"
            )
            key = row["cache_key"]
            self.stage_key_labels[stage_id].setText(
                f"key {str(key)[:16]}…" if key else "key —"
            )
            self.stage_key_labels[stage_id].setToolTip(str(key or ""))
        self.graph_widget.set_rows(rows)
        self.cache_table.setRowCount(len(rows))
        for row_index, row in enumerate(rows):
            status = row["status"]
            values = [
                row["display_name"],
                STATUS_LABELS.get(status, status),
                "是" if row["valid"] else "否",
                row["cache_key"] or "—",
                row["upstream_key"] or "—",
                (
                    f"{row['elapsed_sec']:.4f}s"
                    if row["elapsed_sec"] is not None
                    else "—"
                ),
            ]
            for column, value in enumerate(values):
                item = QTableWidgetItem(str(value))
                if column in {3, 4}:
                    item.setToolTip(str(value))
                if column == 1:
                    item.setForeground(QColor(STATUS_COLORS.get(status, "#ffffff")))
                    error = errors.get(row["stage"])
                    if error:
                        item.setToolTip(json.dumps(error, ensure_ascii=False, indent=2))
                self.cache_table.setItem(row_index, column, item)
        self.cache_table.resizeRowsToContents()

    def _refresh_diagnostics(self) -> None:
        if self.session is None:
            return
        sections: list[str] = []
        restore = self.session.restore_result
        sections.append(
            "== 快照恢复 ==\n"
            f"状态: {restore.status}\n"
            f"说明: {restore.reason}\n"
            f"已加载阶段: {', '.join(restore.loaded_stages) or '无'}"
        )
        if self._last_run is not None:
            sections.append("== 最近运行 ==\n" + self._last_run.diagnostics.summary_text())
        if self._last_comparison is not None:
            report = self._last_comparison
            comparison_lines = [
                "== 增量 / 全量审计 ==",
                f"结果: {'一致' if report.equal else '不一致'}",
                f"代码: {report.code or 'OK'}",
                f"说明: {report.message}",
                f"比较阶段: {', '.join(report.compared_stages) or '无'}",
                (
                    f"熵减缓存: {report.incremental_cache_entries} / "
                    f"全量阶段数: {report.full_stage_count}"
                ),
            ]
            if report.x is not None:
                comparison_lines.append(
                    f"首个差异: stage={report.stage}, "
                    f"x={report.x}, y={report.y}, channel={report.channel}"
                )
            if report.full_diagnostics:
                comparison_lines.append(
                    "全量路径: 重算="
                    f"{report.full_diagnostics.recompute_count}, 命中="
                    f"{report.full_diagnostics.cache_hit_count}, 耗时="
                    f"{report.full_diagnostics.total_elapsed_sec:.4f}s"
                )
            sections.append("\n".join(comparison_lines))
        snapshot = self.session.snapshot_summary()
        sections.append(
            "== 持久化快照 ==\n"
            + json.dumps(snapshot, ensure_ascii=False, indent=2)
        )
        self.diagnostics_view.setPlainText("\n\n".join(sections))

    def _export_output(self) -> None:
        output = self._last_cached_output()
        if self.session is None or self.session.image is None or output is None:
            QMessageBox.information(self, "导出", "当前没有可导出的阶段输出。")
            return
        suggestion = f"{Path(self.samples[self.current_sample_index]['file']).stem}_enhanced.png"
        path, _ = QFileDialog.getSaveFileName(
            self,
            "导出增强图像",
            str(self.cache_dir / suggestion),
            "PNG 图像 (*.png)",
        )
        if not path:
            return
        try:
            save_png(output, self.session.image.mode, path)
        except Exception as exc:
            self._show_error({"code": "E_ENCODE", "message": str(exc)})
            return
        self.statusBar().showMessage(f"已导出: {path}", 10000)

    def _export_heatmap(self) -> None:
        if self.session is None or self.session.image is None:
            QMessageBox.information(self, "导出", "当前没有可导出的差异热力图。")
            return
        output = self._last_cached_output()
        if output is None or output.shape != self.session.image.array.shape:
            QMessageBox.information(self, "导出", "当前没有可导出的差异热力图。")
            return
        suggestion = (
            f"{Path(self.samples[self.current_sample_index]['file']).stem}"
            "_heatmap.png"
        )
        path, _ = QFileDialog.getSaveFileName(
            self,
            "导出差异热力图",
            str(self.cache_dir / suggestion),
            "PNG 图像 (*.png)",
        )
        if not path:
            return
        try:
            heatmap = difference_heatmap(self.session.image.array, output)
            save_png(heatmap, "RGB", path)
        except Exception as exc:
            self._show_error({"code": "E_ENCODE", "message": str(exc)})
            return
        self.statusBar().showMessage(f"已导出热力图: {path}", 10000)

    def _show_error(self, error: Mapping[str, Any]) -> None:
        code = error.get("code", "E_UNKNOWN")
        stage = error.get("stage")
        param = error.get("param")
        details = []
        if stage:
            details.append(f"阶段: {stage}")
        if param:
            details.append(f"参数: {param}")
        text = f"{code}\n{error.get('message', '')}"
        if details:
            text += "\n" + " · ".join(details)
        QMessageBox.warning(self, "处理异常", text)

    def closeEvent(self, event: QCloseEvent) -> None:
        if self._busy and self._task_control is not None:
            self._task_control.cancel()
        if self._thread is not None:
            self._thread.quit()
            self._thread.wait(5000)
        event.accept()


def _configure_application(app: QApplication) -> None:
    app.setStyle("Fusion")
    font = QFont("Microsoft YaHei UI")
    font.setStyleHint(QFont.StyleHint.SansSerif)
    font.setPointSize(9)
    app.setFont(font)
    palette = QPalette()
    palette.setColor(QPalette.ColorRole.Window, QColor("#0b0f19"))
    palette.setColor(QPalette.ColorRole.WindowText, QColor("#dde5f4"))
    palette.setColor(QPalette.ColorRole.Base, QColor("#0d1422"))
    palette.setColor(QPalette.ColorRole.Text, QColor("#dde5f4"))
    palette.setColor(QPalette.ColorRole.Button, QColor("#1a2438"))
    palette.setColor(QPalette.ColorRole.ButtonText, QColor("#dce6f8"))
    palette.setColor(QPalette.ColorRole.Highlight, QColor("#245a9e"))
    palette.setColor(QPalette.ColorRole.HighlightedText, QColor("#ffffff"))
    app.setPalette(palette)
    app.setStyleSheet(APP_STYLE)


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description="Lumen Filter Studio")
    parser.add_argument("--samples", default="samples", help="样本目录")
    parser.add_argument("--cache", default=".cache", help="缓存目录")
    parser.add_argument(
        "--no-auto-run",
        action="store_true",
        help="启动窗口但不自动开始处理",
    )
    args = parser.parse_args(argv)
    app = QApplication(sys.argv[:1])
    app.setApplicationName("Lumen Filter Studio")
    app.setOrganizationName("Lumen")
    _configure_application(app)
    window = MainWindow(
        args.samples,
        args.cache,
        auto_start=not args.no_auto_run,
    )
    window.show()
    return app.exec()


if __name__ == "__main__":
    raise SystemExit(main())

