"""PyQt5 desktop workbench for mask diagnostics and block recovery."""

from __future__ import annotations

import argparse
import json
import sys
import threading
from pathlib import Path
from typing import Any

import numpy as np
from PyQt5.QtCore import QThread, Qt, pyqtSignal
from PyQt5.QtGui import QColor, QImage, QPixmap
from PyQt5.QtWidgets import (
    QApplication,
    QCheckBox,
    QComboBox,
    QDoubleSpinBox,
    QFileDialog,
    QFormLayout,
    QFrame,
    QGroupBox,
    QHBoxLayout,
    QLabel,
    QLineEdit,
    QListWidget,
    QMainWindow,
    QMessageBox,
    QPlainTextEdit,
    QProgressBar,
    QPushButton,
    QScrollArea,
    QSpinBox,
    QSplitter,
    QTabWidget,
    QTableWidget,
    QTableWidgetItem,
    QTextEdit,
    QVBoxLayout,
    QWidget,
)

from ..budget import choose_mode
from ..config import Budget, SegmentationParams
from ..errors import BusinessError
from ..geometry import compare_masks
from ..imageops import (
    deviation_heatmap,
    labels_to_rgb,
    overlay_labels,
)
from ..pipeline import PipelineResult, SegmentationPipeline, export_artifacts
from ..samplegen import generate_suite
from ..state import STAGES, Stage
from ..validation import load_sample, load_suite

STATE_COLORS = {
    "pending": QColor("#4b5563"),
    "processing": QColor("#d97706"),
    "committed": QColor("#15803d"),
    "failed": QColor("#b91c1c"),
}


def _array_to_qimage(array: np.ndarray) -> QImage:
    contiguous = np.ascontiguousarray(array)
    if contiguous.ndim != 3:
        raise ValueError("expected H x W x C image")
    height, width, channels = contiguous.shape
    if channels == 3:
        fmt = QImage.Format_RGB888
    elif channels == 4:
        fmt = QImage.Format_RGBA8888
    else:
        raise ValueError("expected 3 or 4 channels")
    return QImage(
        contiguous.data,
        width,
        height,
        int(contiguous.strides[0]),
        fmt,
    ).copy()


class ImageView(QLabel):
    def __init__(self) -> None:
        super().__init__("尚未加载图像")
        self.setAlignment(Qt.AlignCenter)
        self.setMinimumSize(320, 240)
        self.setStyleSheet(
            "background:#111827; color:#9ca3af; border:1px solid #374151;"
        )
        self._image: QImage | None = None

    def set_array(self, array: np.ndarray) -> None:
        self._image = _array_to_qimage(array)
        self._refresh()

    def clear_image(self, text: str) -> None:
        self._image = None
        self.setText(text)

    def _refresh(self) -> None:
        if self._image is None or self._image.isNull():
            return
        pixmap = QPixmap.fromImage(self._image)
        self.setPixmap(
            pixmap.scaled(
                max(200, self.width() - 8),
                max(160, self.height() - 8),
                Qt.KeepAspectRatio,
                Qt.SmoothTransformation,
            )
        )

    def resizeEvent(self, event: Any) -> None:
        super().resizeEvent(event)
        self._refresh()


class PipelineWorker(QThread):
    event = pyqtSignal(dict)
    completed = pyqtSignal(object)
    failed = pyqtSignal(object)

    def __init__(
        self,
        pipeline: SegmentationPipeline,
        sample: Any,
        params: SegmentationParams,
        budget: Budget,
        *,
        force_mode: str | None,
        force_factor: int | None,
        fail_injections: set[Any],
        pause_event: threading.Event,
        cancel_event: threading.Event,
    ) -> None:
        super().__init__()
        self.pipeline = pipeline
        self.sample = sample
        self.params = params
        self.budget = budget
        self.force_mode = force_mode
        self.force_factor = force_factor
        self.fail_injections = fail_injections
        self.pause_event = pause_event
        self.cancel_event = cancel_event
        self.state_machine = None

    def run(self) -> None:
        try:
            result = self.pipeline.run(
                self.sample,
                self.params,
                self.budget,
                force_mode=self.force_mode,
                force_factor=self.force_factor,
                fail_injections=self.fail_injections,
                cancel_event=self.cancel_event,
                pause_event=self.pause_event,
                status_callback=lambda payload: self.event.emit(payload),
                state_callback=self._bind_state_machine,
            )
            self.completed.emit(result)
        except BusinessError as exc:
            self.failed.emit(exc.to_dict())
        except Exception as exc:  # pragma: no cover - desktop guard
            self.failed.emit(
                {
                    "code": "E_INTERNAL",
                    "message": str(exc),
                    "details": {},
                }
            )

    def _bind_state_machine(self, state_machine: Any) -> None:
        self.state_machine = state_machine


class WorkbenchWindow(QMainWindow):
    def __init__(self, workspace: Path) -> None:
        super().__init__()
        self.workspace = workspace.resolve()
        self.samples: list[Any] = []
        self.current_sample = None
        self.last_result: PipelineResult | None = None
        self.worker: PipelineWorker | None = None
        self.pause_event = threading.Event()
        self.cancel_event = threading.Event()
        self.selected_block = 0
        self.current_state_snapshot: dict[str, Any] = {}
        self._build_ui()
        self._apply_style()
        self._load_samples()

    def _build_ui(self) -> None:
        self.setWindowTitle("ArtMask Workbench · 游戏美术掩码诊断")
        self.resize(1500, 960)
        central = QWidget()
        self.setCentralWidget(central)
        root_layout = QHBoxLayout(central)
        splitter = QSplitter(Qt.Horizontal)
        root_layout.addWidget(splitter)

        controls = QWidget()
        controls.setMinimumWidth(350)
        controls.setMaximumWidth(430)
        controls_layout = QVBoxLayout(controls)
        title = QLabel("ARTMASK / DIAGNOSTICS")
        title.setObjectName("Title")
        subtitle = QLabel("确定性分割 · 规模自适应 · 可恢复提交")
        subtitle.setObjectName("Subtitle")
        controls_layout.addWidget(title)
        controls_layout.addWidget(subtitle)

        sample_group = QGroupBox("本地样本")
        sample_layout = QVBoxLayout(sample_group)
        self.sample_list = QListWidget()
        self.sample_list.setMaximumHeight(148)
        self.sample_list.currentRowChanged.connect(self._sample_changed)
        sample_layout.addWidget(self.sample_list)
        sample_buttons = QHBoxLayout()
        self.reload_button = QPushButton("重载")
        self.generate_button = QPushButton("生成样本")
        self.reload_button.clicked.connect(self._load_samples)
        self.generate_button.clicked.connect(self._generate_samples)
        sample_buttons.addWidget(self.reload_button)
        sample_buttons.addWidget(self.generate_button)
        sample_layout.addLayout(sample_buttons)
        controls_layout.addWidget(sample_group)

        param_group = QGroupBox("生长与后处理参数")
        param_layout = QFormLayout(param_group)
        self.color_tolerance = QDoubleSpinBox()
        self.color_tolerance.setRange(0.1, 441.673)
        self.color_tolerance.setDecimals(2)
        self.gradient_threshold = QDoubleSpinBox()
        self.gradient_threshold.setRange(0.0, 1024.0)
        self.gradient_threshold.setDecimals(2)
        self.confidence_threshold = QDoubleSpinBox()
        self.confidence_threshold.setRange(0.0, 1.0)
        self.confidence_threshold.setDecimals(3)
        self.confidence_threshold.setSingleStep(0.01)
        self.morph_size = QSpinBox()
        self.morph_size.setRange(1, 31)
        self.morph_size.setSingleStep(2)
        self.min_area = QSpinBox()
        self.min_area.setRange(0, 10_000_000)
        self.auto_seeds = QCheckBox("自动种子（按颜色簇）")
        self.auto_seeds.setChecked(True)
        self.auto_seeds.toggled.connect(self._auto_seed_toggled)
        self.auto_seeds.toggled.connect(self._parameters_changed)
        self.manual_seeds = QLineEdit()
        self.manual_seeds.setPlaceholderText("x,y; x,y")
        self.manual_seeds.setEnabled(False)
        param_layout.addRow("颜色容差", self.color_tolerance)
        param_layout.addRow("梯度阈值", self.gradient_threshold)
        param_layout.addRow("置信度阈值", self.confidence_threshold)
        param_layout.addRow("结构元素", self.morph_size)
        param_layout.addRow("最小面积", self.min_area)
        param_layout.addRow(self.auto_seeds)
        param_layout.addRow("手动种子", self.manual_seeds)
        for widget in (
            self.color_tolerance,
            self.gradient_threshold,
            self.confidence_threshold,
            self.min_area,
        ):
            widget.valueChanged.connect(self._parameters_changed)
        self.morph_size.valueChanged.connect(self._parameters_changed)
        self.manual_seeds.textChanged.connect(self._parameters_changed)
        controls_layout.addWidget(param_group)

        budget_group = QGroupBox("规模预算")
        budget_layout = QFormLayout(budget_group)
        self.time_budget = QDoubleSpinBox()
        self.time_budget.setRange(0.1, 3600.0)
        self.time_budget.setValue(20.0)
        self.memory_budget = QDoubleSpinBox()
        self.memory_budget.setRange(1.0, 65536.0)
        self.memory_budget.setValue(512.0)
        self.workers = QSpinBox()
        self.workers.setRange(1, 32)
        self.workers.setValue(4)
        self.tile_size = QSpinBox()
        self.tile_size.setRange(32, 2048)
        self.tile_size.setValue(256)
        self.block_buffer = QDoubleSpinBox()
        self.block_buffer.setRange(0.1, 256.0)
        self.block_buffer.setValue(4.0)
        self.force_mode = QComboBox()
        self.force_mode.addItems(["自动", "精细 fine", "降采样 downsampled"])
        self.force_factor = QComboBox()
        self.force_factor.addItems(["2", "4", "8", "16"])
        for widget in (
            self.time_budget,
            self.memory_budget,
            self.workers,
            self.tile_size,
            self.block_buffer,
        ):
            if hasattr(widget, "valueChanged"):
                widget.valueChanged.connect(self._update_mode_preview)
                widget.valueChanged.connect(self._parameters_changed)
        budget_layout.addRow("时间预算(s)", self.time_budget)
        budget_layout.addRow("内存预算(MB)", self.memory_budget)
        budget_layout.addRow("最大并行度", self.workers)
        budget_layout.addRow("分块尺寸", self.tile_size)
        budget_layout.addRow("单块缓冲(MB)", self.block_buffer)
        budget_layout.addRow("模式", self.force_mode)
        budget_layout.addRow("实验倍率", self.force_factor)
        controls_layout.addWidget(budget_group)

        self.mode_label = QLabel("选择样本后显示规模取舍")
        self.mode_label.setWordWrap(True)
        self.mode_label.setObjectName("ModeSummary")
        controls_layout.addWidget(self.mode_label)

        run_buttons = QHBoxLayout()
        self.run_button = QPushButton("运行")
        self.run_button.setObjectName("Primary")
        self.pause_button = QPushButton("暂停")
        self.pause_button.setEnabled(False)
        self.cancel_button = QPushButton("取消")
        self.cancel_button.setEnabled(False)
        self.retry_button = QPushButton("重试未完成")
        self.export_button = QPushButton("导出掩码与报告")
        self.retry_button.setEnabled(False)
        self.export_button.setEnabled(False)
        self.run_button.clicked.connect(self._run_pipeline)
        self.pause_button.clicked.connect(self._toggle_pause)
        self.cancel_button.clicked.connect(self._cancel_pipeline)
        self.retry_button.clicked.connect(self._retry_pipeline)
        self.export_button.clicked.connect(self._export_results)
        run_buttons.addWidget(self.run_button)
        run_buttons.addWidget(self.pause_button)
        run_buttons.addWidget(self.cancel_button)
        controls_layout.addLayout(run_buttons)
        retry_export = QHBoxLayout()
        retry_export.addWidget(self.retry_button)
        retry_export.addWidget(self.export_button)
        controls_layout.addLayout(retry_export)

        fault_group = QGroupBox("失败隔离演练")
        fault_layout = QFormLayout(fault_group)
        self.fault_enabled = QCheckBox("注入所选块故障")
        self.fault_stage = QComboBox()
        self.fault_stage.addItems([stage.value for stage in STAGES])
        fault_layout.addRow(self.fault_enabled)
        fault_layout.addRow("阶段", self.fault_stage)
        controls_layout.addWidget(fault_group)

        self.progress = QProgressBar()
        self.progress.setFormat("提交阶段 %v / %m")
        self.progress_label = QLabel("0/0")
        self.progress_label.setAlignment(Qt.AlignCenter)
        controls_layout.addWidget(self.progress)
        controls_layout.addWidget(self.progress_label)
        controls_layout.addStretch(1)
        splitter.addWidget(controls)

        right = QWidget()
        right_layout = QVBoxLayout(right)
        self.tabs = QTabWidget()
        self.original_view = ImageView()
        self.initial_view = ImageView()
        self.final_view = ImageView()
        self.heatmap_view = ImageView()
        self.tabs.addTab(self._scrollable(self.original_view), "原图")
        self.tabs.addTab(self._scrollable(self.initial_view), "生长初始掩码")
        self.tabs.addTab(self._scrollable(self.final_view), "最终掩码叠加")
        self.tabs.addTab(self._scrollable(self.heatmap_view), "逐区域偏差热力图")
        self.tabs.addTab(self._build_block_tab(), "分块阶段矩阵")
        right_layout.addWidget(self.tabs, 5)
        self.log = QPlainTextEdit()
        self.log.setReadOnly(True)
        self.log.setMaximumBlockCount(1200)
        self.log.setPlaceholderText("运行日志")
        right_layout.addWidget(self.log, 2)
        splitter.addWidget(right)
        splitter.setSizes([380, 1120])

    def _scrollable(self, widget: QWidget) -> QScrollArea:
        scroll = QScrollArea()
        scroll.setWidgetResizable(True)
        scroll.setWidget(widget)
        return scroll

    def _build_block_tab(self) -> QWidget:
        widget = QWidget()
        layout = QVBoxLayout(widget)
        self.block_table = QTableWidget(0, 6)
        self.block_table.setHorizontalHeaderLabels(
            ["块", "像素范围", "生长", "清理", "统计", "失效"]
        )
        self.block_table.setSelectionBehavior(QTableWidget.SelectRows)
        self.block_table.setEditTriggers(QTableWidget.NoEditTriggers)
        self.block_table.cellClicked.connect(self._block_clicked)
        self.block_details = QTextEdit()
        self.block_details.setReadOnly(True)
        self.block_details.setPlaceholderText("点击块行查看阶段状态")
        layout.addWidget(self.block_table, 3)
        layout.addWidget(self.block_details, 1)
        return widget

    def _apply_style(self) -> None:
        self.setStyleSheet(
            """
            QMainWindow, QWidget { background: #0b1120; color: #e5e7eb;
                font-family: "Noto Sans CJK SC", "Microsoft YaHei", sans-serif;
                font-size: 13px; }
            QGroupBox { border: 1px solid #334155; border-radius: 6px;
                margin-top: 12px; padding-top: 10px; font-weight: 600; }
            QGroupBox::title { subcontrol-origin: margin; left: 10px;
                padding: 0 5px; color: #93c5fd; }
            QListWidget, QTableWidget, QPlainTextEdit, QTextEdit, QLineEdit,
            QSpinBox, QDoubleSpinBox, QComboBox {
                background: #111827; color: #e5e7eb; border: 1px solid #334155;
                border-radius: 4px; padding: 4px; }
            QListWidget::item:selected, QTableWidget::item:selected {
                background: #1d4ed8; color: white; }
            QPushButton { background: #1f2937; border: 1px solid #475569;
                border-radius: 4px; padding: 6px 10px; }
            QPushButton:hover { background: #334155; }
            QPushButton:disabled { color: #64748b; background: #111827; }
            QPushButton#Primary { background: #1d4ed8; border-color: #3b82f6;
                font-weight: 700; }
            QLabel#Title { font-size: 19px; font-weight: 800; color: #f8fafc;
                letter-spacing: 2px; }
            QLabel#Subtitle { color: #94a3b8; margin-bottom: 6px; }
            QLabel#ModeSummary { background: #111827; border-left: 3px solid
                #38bdf8; padding: 8px; color: #cbd5e1; }
            QTabBar::tab { background: #111827; padding: 8px 14px;
                border: 1px solid #334155; }
            QTabBar::tab:selected { background: #1e3a8a; color: white; }
            QHeaderView::section { background: #1f2937; color: #cbd5e1;
                border: 1px solid #334155; padding: 4px; }
            QProgressBar { border: 1px solid #334155; border-radius: 4px;
                text-align: center; background: #111827; }
            QProgressBar::chunk { background: #2563eb; }
            """
        )

    def _load_samples(self) -> None:
        self.samples = []
        self.sample_list.clear()
        try:
            self.samples = load_suite(self.workspace / "samples")
        except BusinessError as exc:
            self._log(f"样本加载失败: {exc}")
            return
        for sample in self.samples:
            self.sample_list.addItem(
                f"{sample.sample_id}  ·  {sample.width}×{sample.height}  ·  r{sample.region_count}"
            )
        if self.samples:
            self.sample_list.setCurrentRow(0)
        self._log(f"已加载 {len(self.samples)} 个本地样本")

    def _generate_samples(self) -> None:
        try:
            index = generate_suite(self.workspace / "samples")
            self._log(f"已生成 {index['sample_count']} 个固定种子样本")
            self._load_samples()
        except BusinessError as exc:
            self._show_error(exc.to_dict())
        except Exception as exc:  # pragma: no cover - desktop guard
            self._show_error({"code": "E_INTERNAL", "message": str(exc), "details": {}})

    def _sample_changed(self, row: int) -> None:
        if not 0 <= row < len(self.samples):
            return
        try:
            self.current_sample = load_sample(self.samples[row].directory)
        except BusinessError as exc:
            self._show_error(exc.to_dict())
            return
        sample = self.current_sample
        recommended = sample.recommended_params
        self.color_tolerance.setValue(
            float(recommended.get("color_tolerance", 30.0))
        )
        self.gradient_threshold.setValue(
            float(recommended.get("gradient_threshold", 80.0))
        )
        self.confidence_threshold.setValue(
            float(recommended.get("confidence_threshold", 0.02))
        )
        self.morph_size.setValue(int(recommended.get("morph_size", 3)))
        self.min_area.setValue(int(recommended.get("min_area", 12)))
        self.original_view.set_array(sample.image)
        self.initial_view.clear_image("尚未运行")
        self.final_view.clear_image("尚未运行")
        self.heatmap_view.clear_image("尚未运行")
        self.last_result = None
        self.retry_button.setEnabled(False)
        self.export_button.setEnabled(False)
        self.block_table.setRowCount(0)
        self.block_details.clear()
        self._log(
            f"加载 {sample.sample_id}: {sample.width}×{sample.height}, "
            f"{sample.region_count} 个真值区域"
        )
        self._update_mode_preview()

    def _auto_seed_toggled(self, checked: bool) -> None:
        self.manual_seeds.setEnabled(not checked)

    def _current_budget(self) -> Budget:
        return Budget(
            time_seconds=self.time_budget.value(),
            memory_mb=self.memory_budget.value(),
            max_workers=self.workers.value(),
            tile_size=self.tile_size.value(),
            block_buffer_mb=self.block_buffer.value(),
        )

    def _current_params(self) -> SegmentationParams:
        sample = self.current_sample
        if sample is None:
            raise BusinessError("E_PARA", "尚未选择样本", field="sample")
        seeds = ()
        if not self.auto_seeds.isChecked():
            parsed = []
            text = self.manual_seeds.text().strip()
            if not text:
                raise BusinessError(
                    "E_PARA", "请输入至少一个种子点", field="seeds"
                )
            for part in text.replace(";", ",").split(","):
                if not part.strip():
                    continue
                parsed.append(part.strip())
            if len(parsed) % 2 != 0:
                raise BusinessError(
                    "E_PARA",
                    "种子点必须按 x,y 成对输入",
                    field="seeds",
                )
            seeds = tuple(
                (int(parsed[index]), int(parsed[index + 1]))
                for index in range(0, len(parsed), 2)
            )
        return SegmentationParams(
            color_tolerance=self.color_tolerance.value(),
            gradient_threshold=self.gradient_threshold.value(),
            confidence_threshold=self.confidence_threshold.value(),
            morph_size=self.morph_size.value(),
            min_area=self.min_area.value(),
            auto_seeds=self.auto_seeds.isChecked(),
            seeds=seeds,
            expected_regions=sample.region_count if self.auto_seeds.isChecked() else None,
        )

    def _update_mode_preview(self) -> None:
        if self.current_sample is None:
            return
        try:
            decision = choose_mode(
                self.current_sample.width,
                self.current_sample.height,
                self.current_sample.region_count,
                self._current_budget(),
            )
        except BusinessError as exc:
            self.mode_label.setText(f"{exc.code}\n{exc.message}")
            return
        self.mode_label.setText(
            f"判定：{decision.mode} · 倍率 {decision.downsample_factor} · "
            f"块 {decision.block_size}px\n"
            f"预计：{decision.estimated_seconds:.2f}s / {decision.estimated_mb:.1f}MB\n"
            f"{decision.reason}"
        )

    def _run_pipeline(self) -> None:
        if self.worker is not None and self.worker.isRunning():
            return
        try:
            params = self._current_params()
            budget = self._current_budget()
            params.validate(image_shape=self.current_sample.image.shape[:2])
            budget.validate()
        except BusinessError as exc:
            self._show_error(exc.to_dict())
            return
        force_mode = None
        if self.force_mode.currentIndex() == 1:
            force_mode = "fine"
        elif self.force_mode.currentIndex() == 2:
            force_mode = "downsampled"
        failures: set[Any] = set()
        if self.fault_enabled.isChecked():
            failures.add((self.selected_block, Stage(self.fault_stage.currentText())))
        self.pause_event.clear()
        self.cancel_event.clear()
        pipeline = SegmentationPipeline(
            self.workspace / "checkpoints"
        )
        self.worker = PipelineWorker(
            pipeline,
            self.current_sample,
            params,
            budget,
            force_mode=force_mode,
            force_factor=int(self.force_factor.currentText()),
            fail_injections=failures,
            pause_event=self.pause_event,
            cancel_event=self.cancel_event,
        )
        self.worker.event.connect(self._on_pipeline_event)
        self.worker.completed.connect(self._on_pipeline_completed)
        self.worker.failed.connect(self._on_pipeline_failed)
        self.run_button.setEnabled(False)
        self.pause_button.setEnabled(True)
        self.cancel_button.setEnabled(True)
        self.retry_button.setEnabled(False)
        self.export_button.setEnabled(False)
        self.pause_button.setText("暂停")
        self._log(
            f"启动流水线：{params.color_tolerance:.2f} / "
            f"{params.gradient_threshold:.2f} / k={params.morph_size}"
        )
        self.worker.start()

    def _toggle_pause(self) -> None:
        if self.pause_event.is_set():
            self.pause_event.clear()
            self.pause_button.setText("暂停")
            self._log("继续处理")
        else:
            self.pause_event.set()
            self.pause_button.setText("继续")
            self._log("已暂停：新提交边界前等待")

    def _parameters_changed(self, *_args: Any) -> None:
        if self.worker is None or not self.worker.isRunning():
            return
        state_machine = getattr(self.worker, "state_machine", None)
        if state_machine is None:
            return
        invalidated = state_machine.invalidate_uncommitted()
        if invalidated:
            self._log(
                "参数已调整：已提交阶段保持冻结；失效块清单 "
                + ", ".join(str(index) for index in invalidated)
            )

    def _cancel_pipeline(self) -> None:
        self.cancel_event.set()
        self.pause_event.clear()
        self._log("已请求取消；仅保留已持久化且版本匹配的阶段")

    def _on_pipeline_event(self, event: dict[str, Any]) -> None:
        self.current_state_snapshot = event.get("state", {})
        grid = self.current_state_snapshot.get("grid", {})
        total = int(grid.get("stage_count", 0))
        committed = int(self.current_state_snapshot.get("committed_stages", 0))
        self.progress.setMaximum(max(1, total))
        self.progress.setValue(committed)
        self.progress_label.setText(
            f"已提交块阶段数 / 总块阶段数：{committed} / {total}"
        )
        self._refresh_block_table()
        kind = event.get("event")
        if kind == "snapshot_opened":
            if event.get("invalidated"):
                self._log(f"快照整体作废：{event.get('reason')}")
            elif event.get("reused"):
                self._log(
                    f"检查点快照匹配，已发现 {event.get('record_count')} 个已提交阶段"
                )
        elif kind == "segmentation_complete":
            self._log(
                f"掩码生长完成：{event.get('mode')} ×{event.get('factor')}，"
                f"{event.get('seconds', 0):.3f}s"
            )
        elif kind == "stage_committed":
            self._log(
                f"提交块 {event.get('block_index')} / {event.get('stage')} "
                f"@seq {event.get('sequence')}"
            )
        elif kind == "stage_failed":
            self._log(
                f"失败块 {event.get('block_index')} / {event.get('stage')}: "
                f"{event.get('error')}"
            )
        elif kind == "snapshot_invalidated":
            self._log(f"快照失效并全量重算：{event.get('reason')}")

    def _refresh_block_table(self) -> None:
        blocks = self.current_state_snapshot.get("blocks", [])
        self.block_table.setRowCount(len(blocks))
        for row, block in enumerate(blocks):
            self.block_table.setItem(
                row, 0, QTableWidgetItem(str(block["block_index"]))
            )
            x0, y0, x1, y1 = block["rect"]
            self.block_table.setItem(
                row, 1, QTableWidgetItem(f"({x0},{y0})-({x1},{y1})")
            )
            for offset, stage in enumerate(STAGES, start=2):
                stage_info = block["stages"][stage.value]
                state = stage_info["state"]
                item = QTableWidgetItem(state)
                item.setBackground(STATE_COLORS.get(state, QColor("#374151")))
                item.setForeground(QColor("#f8fafc"))
                if stage_info.get("error"):
                    item.setToolTip(stage_info["error"])
                self.block_table.setItem(row, offset, item)
            self.block_table.setItem(
                row, 5, QTableWidgetItem("是" if block.get("invalidated") else "")
            )
        self.block_table.resizeColumnsToContents()

    def _block_clicked(self, row: int, _column: int) -> None:
        self.selected_block = row
        blocks = self.current_state_snapshot.get("blocks", [])
        if row >= len(blocks):
            return
        block = blocks[row]
        lines = [
            f"块 {block['block_index']}  范围 {block['rect']}",
            f"失效标记: {block.get('invalidated')}",
        ]
        for stage in STAGES:
            info = block["stages"][stage.value]
            lines.append(
                f"{stage.value}: {info['state']}  sequence={info.get('sequence')}"
                + (f"  error={info.get('error')}" if info.get("error") else "")
            )
        self.block_details.setPlainText("\n".join(lines))

    def _on_pipeline_completed(self, result: PipelineResult) -> None:
        self.last_result = result
        self.run_button.setEnabled(True)
        self.pause_button.setEnabled(False)
        self.cancel_button.setEnabled(False)
        self.retry_button.setEnabled(not result.complete)
        self.export_button.setEnabled(True)
        self.current_state_snapshot = result.state_snapshot
        self._refresh_block_table()
        self.initial_view.set_array(labels_to_rgb(result.initial_labels))
        self.final_view.set_array(overlay_labels(self.current_sample.image, result.final_labels))
        errors = {
            item.predicted_region_id: item.centroid_error_px
            for item in result.comparison.regions
        }
        heatmap = deviation_heatmap(
            result.final_labels,
            self.current_sample.truth_mask,
            errors,
        )
        self.heatmap_view.set_array(heatmap)
        self.mode_label.setText(
            f"实际模式：{result.mode} · 倍率 {result.factor} · "
            f"块 {result.decision.block_size}px\n"
            f"耗时 {result.elapsed_seconds:.3f}s · "
            f"检查点恢复 {result.recovered_stages} 阶段\n"
            f"{result.decision.reason}"
        )
        summary = result.comparison
        self._log(
            "完成："
            f"complete={result.complete}, "
            f"重心={summary.mean_centroid_error_px:.3f}px, "
            f"面积相对误差={summary.mean_area_relative_error:.4f}, "
            f"bboxIoU={summary.mean_bbox_iou:.4f}, "
            f"maskIoU={summary.mean_mask_iou:.4f}"
        )
        self._log(f"检查点目录：{result.checkpoint_dir}")
        if result.cancelled:
            self._log("运行已取消：不承诺完整报告，仅保留版本匹配的已提交阶段")

    def _on_pipeline_failed(self, payload: dict[str, Any]) -> None:
        self.run_button.setEnabled(True)
        self.pause_button.setEnabled(False)
        self.cancel_button.setEnabled(False)
        self.retry_button.setEnabled(True)
        self.export_button.setEnabled(self.last_result is not None)
        self._show_error(payload)

    def _retry_pipeline(self) -> None:
        self.fault_enabled.setChecked(False)
        self._log("重试未完成块阶段；已提交阶段保持冻结")
        self._run_pipeline()

    def _export_results(self) -> None:
        if self.last_result is None or self.current_sample is None:
            return
        try:
            paths = export_artifacts(
                self.last_result,
                self.workspace / "exports",
                image=self.current_sample.image,
                truth_mask=self.current_sample.truth_mask,
            )
        except Exception as exc:  # pragma: no cover - filesystem guard
            self._show_error(
                {"code": "E_EXPORT", "message": str(exc), "details": {}}
            )
            return
        for name, path in paths.items():
            self._log(f"导出 {name}: {path}")
        QMessageBox.information(
            self,
            "导出完成",
            "\n".join(f"{name}: {path}" for name, path in paths.items()),
        )

    def _show_error(self, payload: dict[str, Any]) -> None:
        self._log(json.dumps(payload, ensure_ascii=False, indent=2))
        QMessageBox.critical(
            self,
            str(payload.get("code", "ERROR")),
            json.dumps(payload, ensure_ascii=False, indent=2),
        )

    def _log(self, message: str) -> None:
        self.log.appendPlainText(message)

    def closeEvent(self, event: Any) -> None:
        if self.worker is not None and self.worker.isRunning():
            self.cancel_event.set()
            self.pause_event.clear()
            self.worker.wait(3000)
        super().closeEvent(event)


def build_window(workspace: str | Path) -> WorkbenchWindow:
    return WorkbenchWindow(Path(workspace))


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(prog="artmask-gui")
    parser.add_argument(
        "--workspace",
        default=str((Path.cwd() / "workspace_data").resolve()),
    )
    args = parser.parse_args(argv)
    app = QApplication.instance() or QApplication(sys.argv[:1])
    window = WorkbenchWindow(Path(args.workspace))
    window.show()
    return int(app.exec_())


if __name__ == "__main__":
    raise SystemExit(main())
