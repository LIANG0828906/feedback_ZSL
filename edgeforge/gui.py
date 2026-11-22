from __future__ import annotations

import csv
from datetime import datetime
import json
from pathlib import Path
import sys
import traceback
from typing import Any

import numpy as np
from PyQt5.QtCore import QThread, Qt, pyqtSignal
from PyQt5.QtGui import QColor, QFont, QFontDatabase, QPixmap
from PyQt5.QtWidgets import (
    QApplication,
    QCheckBox,
    QComboBox,
    QDoubleSpinBox,
    QFileDialog,
    QFormLayout,
    QGroupBox,
    QHBoxLayout,
    QHeaderView,
    QLabel,
    QMainWindow,
    QMessageBox,
    QProgressBar,
    QPushButton,
    QSpinBox,
    QSplitter,
    QStatusBar,
    QTabWidget,
    QTableWidget,
    QTableWidgetItem,
    QTextEdit,
    QToolBar,
    QVBoxLayout,
    QWidget,
)

from .config import PipelineConfig
from .errors import EdgeForgeError
from .image_io import array_to_qimage, write_png
from .loader import LoadedSample, load_sample
from .pipeline import Pipeline
from .samples import DEFAULT_SAMPLE_IDS, generate_samples
from .stages import STAGE_ORDER, BlockState, Stage


STAGE_COLORS = {
    BlockState.PENDING: "#2a3141",
    BlockState.PROCESSING: "#8a6a1f",
    BlockState.COMMITTED: "#1f6f4a",
    BlockState.FAILED: "#8e3038",
}


class PipelineWorker(QThread):
    event = pyqtSignal(dict)
    succeeded = pyqtSignal(object)
    failed = pyqtSignal(object, object)

    def __init__(
        self,
        pipeline: Pipeline,
        *,
        retry_failed: bool,
    ) -> None:
        super().__init__()
        self.pipeline = pipeline
        self.retry_failed = retry_failed
        self.pipeline.on_event = self.event.emit

    def run(self) -> None:
        try:
            self.pipeline.run(retry_failed=self.retry_failed)
        except EdgeForgeError as exc:
            self.failed.emit(exc.as_dict(), self.pipeline)
        except Exception as exc:
            self.failed.emit(
                {
                    "code": "E_INTERNAL",
                    "message": str(exc),
                    "exception": type(exc).__name__,
                    "traceback": traceback.format_exc(limit=12),
                },
                self.pipeline,
            )
        else:
            self.succeeded.emit(self.pipeline)


class ImagePanel(QWidget):
    def __init__(self, title: str, parent: QWidget | None = None) -> None:
        super().__init__(parent)
        layout = QVBoxLayout(self)
        layout.setContentsMargins(0, 0, 0, 0)
        header = QLabel(title)
        header.setObjectName("panelTitle")
        self.view = QLabel("尚未加载")
        self.view.setAlignment(Qt.AlignCenter)
        self.view.setMinimumSize(320, 240)
        self.view.setObjectName("imageView")
        layout.addWidget(header)
        layout.addWidget(self.view, 1)
        self._array: np.ndarray | None = None

    def set_array(self, array: np.ndarray | None, empty_text: str = "无数据") -> None:
        self._array = array
        if array is None:
            self.view.setPixmap(QPixmap())
            self.view.setText(empty_text)
            return
        image = array_to_qimage(array)
        pixmap = QPixmap.fromImage(image)
        target = self.view.size()
        if target.width() < 50 or target.height() < 50:
            target.setWidth(620)
            target.setHeight(420)
        self.view.setPixmap(
            pixmap.scaled(
                target,
                Qt.KeepAspectRatio,
                Qt.SmoothTransformation,
            )
        )

    def resizeEvent(self, event: Any) -> None:
        super().resizeEvent(event)
        if self._array is not None:
            self.set_array(self._array)


class MainWindow(QMainWindow):
    def __init__(
        self,
        *,
        samples_dir: str | Path,
        run_dir: str | Path,
    ) -> None:
        super().__init__()
        self.samples_dir = Path(samples_dir)
        self.run_root = Path(run_dir)
        self.sample: LoadedSample | None = None
        self.pipeline: Pipeline | None = None
        self.worker: PipelineWorker | None = None
        self.control = None
        self.block_index_by_id: dict[int, int] = {}
        self.column_by_stage = {
            stage: index + 1 for index, stage in enumerate(STAGE_ORDER)
        }
        self.stage_by_column = {
            index + 1: stage for index, stage in enumerate(STAGE_ORDER)
        }
        self.diff_stage = Stage.PRUNE
        self.setWindowTitle("EdgeForge · 边缘检测与形态学诊断台")
        self.resize(1480, 940)
        _install_bundled_font()
        self._build_ui()
        self._ensure_samples()
        self._load_selected_sample()

    def _build_ui(self) -> None:
        self.setStyleSheet(
            """
            QMainWindow, QWidget { background: #141821; color: #d8dee9; }
            QToolBar { background: #1b2130; border-bottom: 1px solid #313a4d; spacing: 6px; }
            QGroupBox { border: 1px solid #313a4d; border-radius: 4px; margin-top: 10px; padding-top: 8px; }
            QGroupBox::title { subcontrol-origin: margin; left: 8px; color: #8fd3ff; }
            QLabel#panelTitle { color: #8fd3ff; font-weight: 600; padding: 4px; }
            QLabel#imageView { background: #0c0f15; border: 1px solid #30384a; }
            QTableWidget, QTextEdit, QComboBox, QSpinBox, QDoubleSpinBox {
                background: #0f131b; color: #d8dee9; border: 1px solid #30384a;
                selection-background-color: #2f5f8a;
            }
            QHeaderView::section { background: #222a39; color: #c7d3e3; padding: 4px; border: 1px solid #30384a; }
            QPushButton { background: #253047; border: 1px solid #40506c; padding: 5px 10px; border-radius: 3px; }
            QPushButton:hover { background: #30405f; }
            QPushButton:disabled { color: #687181; background: #1a1f2a; }
            QProgressBar { background: #0f131b; border: 1px solid #30384a; text-align: center; }
            QProgressBar::chunk { background: #2c7d63; }
            QTabWidget::pane { border: 1px solid #30384a; }
            QTabBar::tab { background: #1d2432; padding: 6px 10px; }
            QTabBar::tab:selected { background: #2b3951; color: #8fd3ff; }
            """
        )
        toolbar = QToolBar("主导航")
        toolbar.setMovable(False)
        self.addToolBar(toolbar)
        self.sample_combo = QComboBox()
        self.sample_combo.setMinimumWidth(290)
        self.sample_combo.currentIndexChanged.connect(self._load_selected_sample)
        toolbar.addWidget(QLabel(" 样本 "))
        toolbar.addWidget(self.sample_combo)
        toolbar.addSeparator()
        self.run_button = QPushButton("运行 / 续跑")
        self.run_button.clicked.connect(lambda: self._start_run(False, False))
        toolbar.addWidget(self.run_button)
        self.recompute_button = QPushButton("全量重算")
        self.recompute_button.clicked.connect(lambda: self._start_run(False, True))
        toolbar.addWidget(self.recompute_button)
        self.pause_button = QPushButton("暂停")
        self.pause_button.setEnabled(False)
        self.pause_button.clicked.connect(self._toggle_pause)
        toolbar.addWidget(self.pause_button)
        self.cancel_button = QPushButton("取消")
        self.cancel_button.setEnabled(False)
        self.cancel_button.clicked.connect(self._cancel_run)
        toolbar.addWidget(self.cancel_button)
        self.retry_button = QPushButton("重试失败块")
        self.retry_button.setEnabled(False)
        self.retry_button.clicked.connect(lambda: self._start_run(True, False))
        toolbar.addWidget(self.retry_button)
        toolbar.addSeparator()
        self.export_button = QPushButton("导出")
        self.export_button.clicked.connect(self._export)
        toolbar.addWidget(self.export_button)

        central = QWidget()
        outer = QVBoxLayout(central)
        outer.setContentsMargins(8, 6, 8, 6)
        vertical = QSplitter(Qt.Vertical)
        outer.addWidget(vertical, 1)
        top = QSplitter(Qt.Horizontal)
        self.source_panel = ImagePanel("原图")
        self.stage_tabs = QTabWidget()
        self.stage_views: dict[str, ImagePanel] = {}
        for stage in STAGE_ORDER:
            panel = ImagePanel(stage.label)
            self.stage_views[stage.value] = panel
            self.stage_tabs.addTab(panel, stage.label)
        truth_panel = ImagePanel("真值边缘")
        self.stage_views["truth"] = truth_panel
        self.stage_tabs.addTab(truth_panel, "真值")
        diff_panel = ImagePanel("差异热力图")
        self.stage_views["diff"] = diff_panel
        self.stage_tabs.addTab(diff_panel, "差异热力")
        self.stage_tabs.currentChanged.connect(self._refresh_stage_tab)
        top.addWidget(self.source_panel)
        top.addWidget(self.stage_tabs)
        top.setStretchFactor(0, 1)
        top.setStretchFactor(1, 1)
        vertical.addWidget(top)

        bottom = QSplitter(Qt.Horizontal)
        config_group = QGroupBox("处理参数")
        config_layout = QFormLayout(config_group)
        self.operator_combo = QComboBox()
        self.operator_combo.addItems(["sobel", "prewitt", "scharr"])
        self.structure_combo = QComboBox()
        self.structure_combo.addItems(["cross", "square"])
        self.low_spin = _double_spin(0, 255, 8.0)
        self.high_spin = _double_spin(0, 255, 18.0)
        self.open_spin = _odd_spin(3)
        self.close_spin = _odd_spin(3)
        self.thin_spin = _int_spin(1, 64, 8)
        self.prune_spin = _int_spin(1, 256, 6)
        self.tile_spin = _int_spin(16, 4096, 192)
        self.parallel_spin = _int_spin(1, 64, 4)
        self.memory_spin = _int_spin(1, 4096, 64)
        self.thread_pixels_spin = _int_spin(4096, 50_000_000, 1_500_000)
        self.band_check = QCheckBox("强制边带")
        self.band_spin = _int_spin(0, 1024, 0)
        self.band_spin.setEnabled(False)
        self.band_check.toggled.connect(self.band_spin.setEnabled)
        config_layout.addRow("梯度算子", self.operator_combo)
        config_layout.addRow("结构元素", self.structure_combo)
        config_layout.addRow("低阈值", self.low_spin)
        config_layout.addRow("高阈值", self.high_spin)
        config_layout.addRow("开运算尺寸", self.open_spin)
        config_layout.addRow("闭运算尺寸", self.close_spin)
        config_layout.addRow("细化迭代", self.thin_spin)
        config_layout.addRow("剪枝长度", self.prune_spin)
        config_layout.addRow("分块尺寸", self.tile_spin)
        config_layout.addRow("最大并行度", self.parallel_spin)
        config_layout.addRow("内存预算 MiB", self.memory_spin)
        config_layout.addRow("每线程像素", self.thread_pixels_spin)
        band_row = QHBoxLayout()
        band_row.addWidget(self.band_check)
        band_row.addWidget(self.band_spin, 1)
        band_container = QWidget()
        band_container.setLayout(band_row)
        config_layout.addRow("边带覆盖", band_container)

        self.state_table = QTableWidget(0, len(STAGE_ORDER) + 1)
        self.state_table.setHorizontalHeaderLabels(
            ["分块"] + [stage.value for stage in STAGE_ORDER]
        )
        self.state_table.horizontalHeader().setSectionResizeMode(
            QHeaderView.ResizeToContents
        )
        self.state_table.verticalHeader().setVisible(False)
        self.state_table.cellClicked.connect(self._cell_selected)

        right = QWidget()
        right_layout = QVBoxLayout(right)
        right_layout.setContentsMargins(0, 0, 0, 0)
        self.metrics_table = QTableWidget(0, 8)
        self.metrics_table.setHorizontalHeaderLabels(
            [
                "阶段",
                "覆盖率",
                "精确率",
                "F1",
                "端点均差",
                "端点最大差",
                "缺失端点",
                "圆覆盖率",
            ]
        )
        self.metrics_table.horizontalHeader().setSectionResizeMode(
            QHeaderView.Stretch
        )
        self.metrics_table.verticalHeader().setVisible(False)
        self.log_view = QTextEdit()
        self.log_view.setReadOnly(True)
        self.log_view.document().setMaximumBlockCount(3000)
        right_layout.addWidget(QLabel("几何真值比对"))
        right_layout.addWidget(self.metrics_table, 2)
        right_layout.addWidget(QLabel("运行 / 检查点日志"))
        right_layout.addWidget(self.log_view, 3)
        bottom.addWidget(config_group)
        bottom.addWidget(self.state_table)
        bottom.addWidget(right)
        bottom.setStretchFactor(0, 0)
        bottom.setStretchFactor(1, 2)
        bottom.setStretchFactor(2, 2)
        vertical.addWidget(bottom)
        self.setCentralWidget(central)

        status = QStatusBar()
        self.progress_bar = QProgressBar()
        self.progress_bar.setRange(0, 1)
        self.progress_bar.setValue(0)
        self.progress_bar.setMaximumWidth(360)
        self.status_label = QLabel("就绪")
        status.addWidget(self.status_label, 1)
        status.addPermanentWidget(self.progress_bar)
        self.setStatusBar(status)

    def _ensure_samples(self) -> None:
        try:
            generate_samples(self.samples_dir, DEFAULT_SAMPLE_IDS)
        except Exception as exc:
            QMessageBox.critical(self, "样本生成失败", str(exc))
            return
        manifest_path = self.samples_dir / "manifest.json"
        manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
        self.sample_combo.blockSignals(True)
        self.sample_combo.clear()
        for entry in manifest["samples"]:
            self.sample_combo.addItem(
                f"{entry['sample_id']}  ({entry['width']}×{entry['height']}×{entry['channels']})",
                entry["sample_id"],
            )
        self.sample_combo.blockSignals(False)
        if self.sample_combo.count() == 0:
            raise RuntimeError("manifest 中没有样本")
        self.sample_combo.setCurrentIndex(
            max(0, self.sample_combo.findData("low_contrast_gray_320x240"))
        )

    def _load_selected_sample(self) -> None:
        sample_id = self.sample_combo.currentData()
        if not sample_id:
            return
        try:
            self.sample = load_sample(self.samples_dir, str(sample_id))
        except EdgeForgeError as exc:
            QMessageBox.critical(self, "样本加载失败", str(exc))
            self._log(json.dumps(exc.as_dict(), ensure_ascii=False))
            return
        self.pipeline = None
        self.control = None
        self.source_panel.set_array(self.sample.image)
        self.stage_views["truth"].set_array(
            np.where(self.sample.truth_mask, 255, 0).astype(np.uint8)
        )
        self.stage_views["diff"].set_array(None, "运行后生成差异热力图")
        for stage in STAGE_ORDER:
            self.stage_views[stage.value].set_array(None, "尚未运行")
        self._build_state_table(None)
        self.metrics_table.setRowCount(0)
        self.status_label.setText(
            f"{self.sample.sample_id} · {self.sample.width}×{self.sample.height}×{self.sample.channels}"
        )
        self._log(
            f"加载样本 {self.sample.sample_id}; image_sha256={self.sample.image_sha256[:12]}…"
        )

    def _config_from_ui(self) -> PipelineConfig:
        return PipelineConfig(
            gradient_operator=self.operator_combo.currentText(),
            low_threshold=self.low_spin.value(),
            high_threshold=self.high_spin.value(),
            open_size=self.open_spin.value(),
            close_size=self.close_spin.value(),
            thin_iterations=self.thin_spin.value(),
            prune_length=self.prune_spin.value(),
            structure=self.structure_combo.currentText(),
            tile_size=self.tile_spin.value(),
            max_parallel=self.parallel_spin.value(),
            thread_buffer_pixels=self.thread_pixels_spin.value(),
            memory_budget_bytes=self.memory_spin.value() * 1024 * 1024,
            band_override=(
                self.band_spin.value() if self.band_check.isChecked() else None
            ),
        ).validate()

    def _start_run(self, retry_failed: bool, reset_checkpoint: bool) -> None:
        if self.sample is None:
            return
        if self.worker is not None and self.worker.isRunning():
            return
        try:
            config = self._config_from_ui()
        except EdgeForgeError as exc:
            QMessageBox.warning(self, "参数错误", str(exc))
            self._log(str(exc))
            return
        run_dir = self.run_root / self.sample.sample_id
        if (
            retry_failed
            and self.pipeline is not None
            and not self.pipeline.snapshot_invalidated
            and self.pipeline.config.params_version() == config.params_version()
            and self.pipeline.sample.sample_id == self.sample.sample_id
        ):
            pipeline = self.pipeline
            pipeline.on_event = None
        else:
            try:
                pipeline = Pipeline(
                    self.sample,
                    config,
                    run_dir=run_dir,
                    execution_mode="parallel",
                    reset_checkpoint=reset_checkpoint,
                )
            except EdgeForgeError as exc:
                QMessageBox.critical(self, "检查点初始化失败", str(exc))
                self._log(json.dumps(exc.as_dict(), ensure_ascii=False))
                return
            self.pipeline = pipeline
            self.control = pipeline.control
            self._build_state_table(pipeline)
            self._refresh_metrics()
            self._refresh_stage_views()
            if pipeline.snapshot_invalidated:
                self._log(
                    f"检查点整体作废: {pipeline.snapshot_invalidation_reason}"
                )
            if pipeline.resumed_commits:
                self._log(f"从检查点恢复 {pipeline.resumed_commits} 个块阶段")
        self.worker = PipelineWorker(pipeline, retry_failed=retry_failed)
        self.worker.event.connect(self._on_pipeline_event)
        self.worker.succeeded.connect(self._on_run_succeeded)
        self.worker.failed.connect(self._on_run_failed)
        self.worker.finished.connect(self._on_worker_finished)
        self.worker.start()
        self.run_button.setEnabled(False)
        self.recompute_button.setEnabled(False)
        self.retry_button.setEnabled(False)
        self.export_button.setEnabled(False)
        self.pause_button.setEnabled(True)
        self.pause_button.setText("暂停")
        self.cancel_button.setEnabled(True)
        self.status_label.setText("运行中…")
        self._log(
            f"启动 {pipeline.execution_mode} 管线 tile={config.tile_size} "
            f"parallel={config.max_parallel} retry={retry_failed} reset={reset_checkpoint}"
        )

    def _toggle_pause(self) -> None:
        if self.pipeline is None:
            return
        if self.pipeline.control.paused:
            self.pipeline.control.resume()
            self.pause_button.setText("暂停")
            self._log("继续运行")
        else:
            self.pipeline.control.pause()
            self.pause_button.setText("继续")
            self._log("已请求暂停：活动块完成后停在阶段边界")

    def _cancel_run(self) -> None:
        if self.pipeline is None:
            return
        self.pipeline.control.cancel()
        self._log("已请求取消；仅保留已原子提交的分块阶段")
        self.status_label.setText("正在取消…")

    def _on_pipeline_event(self, event: dict[str, Any]) -> None:
        event_type = event.get("type")
        progress = event.get("progress", {})
        if progress:
            self.progress_bar.setMaximum(max(1, int(progress.get("total", 1))))
            self.progress_bar.setValue(int(progress.get("committed", 0)))
        if event_type in {"block_processing", "block_committed", "block_failed"}:
            target_state = {
                "block_processing": BlockState.PROCESSING,
                "block_committed": BlockState.COMMITTED,
                "block_failed": BlockState.FAILED,
            }[event_type]
            self._update_state_cell(
                Stage(event["stage"]),
                int(event["block_id"]),
                target_state,
                event.get("message"),
            )
        if event_type == "block_committed":
            self._log(
                f"提交 {event['stage']}/block-{event['block_id']} "
                f"hash={(event.get('content_hash') or '')[:12]}"
            )
        elif event_type == "block_failed":
            self.retry_button.setEnabled(True)
            self._log(
                f"失败 {event['stage']}/block-{event['block_id']}: "
                f"{event.get('message')}"
            )
        elif event_type == "budget_queue":
            self._log(
                f"预算排队 {event['stage']}/block-{event['block_id']} "
                f"reason={event.get('reason')} "
                f"required={event.get('required_bytes')} "
                f"reserved={event.get('reserved_bytes')}"
            )
        elif event_type == "stage_complete":
            self._refresh_stage_views()
            self._refresh_metrics()
            self._log(f"阶段完成: {event.get('stage')}")
        elif event_type == "snapshot_invalidated":
            self._log(
                f"检查点哈希/版本不匹配，整体作废并全量重算: "
                f"{event.get('reason')}"
            )
            self._build_state_table(self.pipeline)
        elif event_type == "complete":
            self._log(
                f"运行完成: progress={progress.get('committed')}/"
                f"{progress.get('total')} queue={event.get('queue_events')}"
            )
        elif event_type == "stage_start":
            self._log(
                f"阶段开始 {event.get('stage')} blocks={event.get('blocks')} "
                f"band={event.get('band_width')}"
            )

    def _on_run_succeeded(self, pipeline: Pipeline) -> None:
        self.pipeline = pipeline
        self._refresh_stage_views()
        self._refresh_metrics()
        self._build_state_table(pipeline)
        failed = any(
            cell.state is BlockState.FAILED
            for cell in pipeline.state_machine.snapshot().cells.values()
        )
        self.retry_button.setEnabled(failed)
        self.status_label.setText(
            f"完成 · 已提交 {pipeline.progress['committed']}/{pipeline.progress['total']}"
        )
        self._log(
            f"完成: resumed={pipeline.resumed_commits} "
            f"queue_events={len(pipeline.queue_events)} "
            f"peak={pipeline.budget_peak_bytes}B"
        )

    def _on_run_failed(self, error: dict[str, Any], pipeline: Pipeline) -> None:
        self.pipeline = pipeline
        self._refresh_stage_views()
        self._refresh_metrics()
        self._build_state_table(pipeline)
        self.retry_button.setEnabled(
            any(
                cell.state is BlockState.FAILED
                for cell in pipeline.state_machine.snapshot().cells.values()
            )
        )
        code = error.get("code", "E_INTERNAL")
        self.status_label.setText(f"{code}: {error.get('message', '')}")
        self._log(json.dumps(error, indent=2, ensure_ascii=False))

    def _on_worker_finished(self) -> None:
        self.run_button.setEnabled(True)
        self.recompute_button.setEnabled(True)
        self.export_button.setEnabled(True)
        self.pause_button.setEnabled(False)
        self.cancel_button.setEnabled(False)
        if self.worker is not None:
            self.worker.deleteLater()
        self.worker = None

    def _build_state_table(self, pipeline: Pipeline | None) -> None:
        self.state_table.clearContents()
        if pipeline is None:
            self.state_table.setRowCount(0)
            return
        self.block_index_by_id = {
            block.block_id: row for row, block in enumerate(pipeline.blocks)
        }
        snapshot = pipeline.state_machine.snapshot()
        self.state_table.setRowCount(len(pipeline.blocks))
        for row, block in enumerate(pipeline.blocks):
            item = QTableWidgetItem(
                f"#{block.block_id} [{block.y0}:{block.y1},{block.x0}:{block.x1}]"
            )
            item.setFlags(item.flags() & ~Qt.ItemIsEditable)
            self.state_table.setItem(row, 0, item)
            for stage in STAGE_ORDER:
                cell = snapshot.cells[(stage, block.block_id)]
                self._set_state_item(
                    row,
                    self.column_by_stage[stage],
                    cell.state,
                    cell.error,
                )

    def _update_state_cell(
        self,
        stage: Stage,
        block_id: int,
        state: BlockState,
        error: str | None = None,
    ) -> None:
        row = self.block_index_by_id.get(block_id)
        column = self.column_by_stage.get(stage)
        if row is None or column is None:
            return
        self._set_state_item(row, column, state, error)

    def _set_state_item(
        self,
        row: int,
        column: int,
        state: BlockState,
        error: str | None,
    ) -> None:
        item = QTableWidgetItem(state.label)
        item.setFlags(item.flags() & ~Qt.ItemIsEditable)
        item.setBackground(QColor(STAGE_COLORS[state]))
        if error:
            item.setToolTip(error)
        self.state_table.setItem(row, column, item)

    def _cell_selected(self, row: int, column: int) -> None:
        stage = self.stage_by_column.get(column)
        if stage is None or self.pipeline is None or row >= len(self.pipeline.blocks):
            return
        block = self.pipeline.blocks[row]
        cell = self.pipeline.state_machine.cell(stage, block.block_id)
        text = (
            f"block-{block.block_id} {stage.value} state={cell.state.label} "
            f"attempts={cell.attempts}"
        )
        if cell.error:
            text += f" error={cell.error}"
        self.status_label.setText(text)

    def _refresh_stage_views(self) -> None:
        pipeline = self.pipeline
        if pipeline is None:
            return
        for stage in STAGE_ORDER:
            array = pipeline.stage_arrays.get(stage)
            self.stage_views[stage.value].set_array(
                _display_array(stage, array),
                "尚未提交",
            )
        self._refresh_diff_heatmap()

    def _refresh_diff_heatmap(self) -> None:
        if self.pipeline is None or self.sample is None:
            return
        stage = self.diff_stage
        if stage not in self.pipeline.stage_arrays:
            self.stage_views["diff"].set_array(None, "该阶段尚未提交")
            return
        self.stage_views["diff"].set_array(
            _diff_heatmap(
                stage,
                self.pipeline.stage_arrays[stage],
                self.pipeline.config,
                self.sample.truth_mask,
            )
        )

    def _refresh_stage_tab(self, _index: int) -> None:
        current = self.stage_tabs.currentIndex()
        if current < len(STAGE_ORDER):
            self.diff_stage = STAGE_ORDER[current]
        if self.stage_tabs.tabText(current) == "差异热力":
            self._refresh_diff_heatmap()

    def _refresh_metrics(self) -> None:
        if self.pipeline is None:
            self.metrics_table.setRowCount(0)
            return
        metrics = self.pipeline.stage_metrics
        self.metrics_table.setRowCount(len(metrics))
        for row, stage in enumerate(STAGE_ORDER):
            data = metrics.get(stage.value)
            if data is None:
                continue
            endpoint = data["endpoint"]
            values = [
                stage.value,
                _pct(data["coverage"]),
                _pct(data["precision"]),
                _pct(data["f1"]),
                _num(endpoint["mean_distance"], 2),
                _num(endpoint["max_distance"], 2),
                f"{endpoint['missing_endpoints']}/{endpoint['total_endpoints']}",
                _pct(data["circle"]["mean_coverage"]),
            ]
            for column, value in enumerate(values):
                item = QTableWidgetItem(value)
                item.setFlags(item.flags() & ~Qt.ItemIsEditable)
                self.metrics_table.setItem(row, column, item)

    def _log(self, message: str) -> None:
        self.log_view.append(message)

    def _export(self) -> None:
        if self.pipeline is None or self.sample is None:
            QMessageBox.information(self, "导出", "尚无运行结果")
            return
        default = (
            self.run_root
            / "exports"
            / f"{self.sample.sample_id}-{datetime.now().strftime('%Y%m%d-%H%M%S')}"
        )
        default.parent.mkdir(parents=True, exist_ok=True)
        target = QFileDialog.getExistingDirectory(
            self,
            "选择导出目录",
            str(default.parent),
        )
        if not target:
            return
        destination = Path(target) / default.name
        destination.mkdir(parents=True, exist_ok=True)
        write_png(destination / "original.png", self.sample.image)
        write_png(
            destination / "truth_mask.png",
            np.where(self.sample.truth_mask, 255, 0).astype(np.uint8),
        )
        for stage in STAGE_ORDER:
            if stage in self.pipeline.stage_arrays:
                write_png(
                    destination / f"{stage.value}.png",
                    _display_array(stage, self.pipeline.stage_arrays[stage]),
                )
        if Stage.PRUNE in self.pipeline.stage_arrays:
            write_png(
                destination / "final_diff.png",
                _diff_heatmap(
                    Stage.PRUNE,
                    self.pipeline.stage_arrays[Stage.PRUNE],
                    self.pipeline.config,
                    self.sample.truth_mask,
                ),
            )
        (destination / "config.json").write_text(
            json.dumps(
                self.pipeline.config.describe(),
                indent=2,
                ensure_ascii=False,
            )
            + "\n",
            encoding="utf-8",
        )
        (destination / "metrics.json").write_text(
            json.dumps(
                self.pipeline.stage_metrics,
                indent=2,
                ensure_ascii=False,
            )
            + "\n",
            encoding="utf-8",
        )
        (destination / "queue_events.json").write_text(
            json.dumps(self.pipeline.queue_events, indent=2, ensure_ascii=False)
            + "\n",
            encoding="utf-8",
        )
        snapshot = self.pipeline.state_machine.snapshot()
        with (destination / "block_states.csv").open(
            "w",
            newline="",
            encoding="utf-8-sig",
        ) as handle:
            writer = csv.writer(handle)
            writer.writerow(["block_id", *[stage.value for stage in STAGE_ORDER]])
            for block in self.pipeline.blocks:
                writer.writerow(
                    [
                        block.block_id,
                        *[
                            snapshot.cells[(stage, block.block_id)].state.value
                            for stage in STAGE_ORDER
                        ],
                    ]
                )
        checkpoint_payload: dict[str, Any] = {
            "run_dir": str(self.pipeline.run_dir),
            "invalidated": self.pipeline.snapshot_invalidated,
            "invalidation_reason": self.pipeline.snapshot_invalidation_reason,
            "resumed_commits": self.pipeline.resumed_commits,
            "progress": self.pipeline.progress,
        }
        if self.pipeline.checkpoint is not None:
            checkpoint_payload["next_seq"] = self.pipeline.checkpoint.next_seq
            checkpoint_payload["commit_order"] = self.pipeline.checkpoint.commit_order
        (destination / "checkpoint_info.json").write_text(
            json.dumps(checkpoint_payload, indent=2, ensure_ascii=False) + "\n",
            encoding="utf-8",
        )
        self._log(f"已导出到 {destination}")
        QMessageBox.information(self, "导出完成", str(destination))

    def closeEvent(self, event: Any) -> None:
        if self.worker is not None and self.worker.isRunning():
            answer = QMessageBox.question(
                self,
                "退出",
                "处理仍在运行。是否取消未提交块并退出？\n"
                "已原子提交的检查点会保留，下次可续跑。",
                QMessageBox.Yes | QMessageBox.No,
                QMessageBox.No,
            )
            if answer != QMessageBox.Yes:
                event.ignore()
                return
            if self.pipeline is not None:
                self.pipeline.control.cancel()
            self.worker.wait(8000)
        event.accept()


def _display_array(stage: Stage, array: np.ndarray | None) -> np.ndarray | None:
    if array is None:
        return None
    values = np.asarray(array)
    if stage is Stage.GRADIENT:
        scalar = values if values.ndim == 2 else np.max(values, axis=2)
        return np.clip(scalar, 0, 255).astype(np.uint8)
    if values.ndim == 3:
        scalar = np.max(values, axis=2)
    else:
        scalar = values
    return np.where(scalar > 0, 255, 0).astype(np.uint8)


def _diff_heatmap(
    stage: Stage,
    array: np.ndarray,
    config: PipelineConfig,
    truth: np.ndarray,
) -> np.ndarray:
    from .metrics import stage_mask

    predicted = stage_mask(stage, array, config)
    heat = np.zeros((*truth.shape, 3), dtype=np.uint8)
    heat[truth & ~predicted] = (50, 130, 255)
    heat[predicted & ~truth] = (255, 75, 75)
    heat[predicted & truth] = (240, 240, 240)
    return heat


def _double_spin(minimum: float, maximum: float, value: float) -> QDoubleSpinBox:
    spin = QDoubleSpinBox()
    spin.setRange(minimum, maximum)
    spin.setDecimals(2)
    spin.setSingleStep(0.5)
    spin.setValue(value)
    return spin


def _odd_spin(value: int) -> QSpinBox:
    spin = QSpinBox()
    spin.setRange(1, 31)
    spin.setSingleStep(2)
    spin.setValue(value)
    return spin


def _int_spin(minimum: int, maximum: int, value: int) -> QSpinBox:
    spin = QSpinBox()
    spin.setRange(minimum, maximum)
    spin.setValue(value)
    return spin


def _pct(value: float | None) -> str:
    return "—" if value is None else f"{value * 100:.2f}%"


def _num(value: float | None, digits: int) -> str:
    return "—" if value is None else f"{value:.{digits}f}"


def run_gui(
    *,
    samples_dir: str | Path = "samples",
    run_dir: str | Path = "runs/gui",
) -> int:
    app = QApplication.instance()
    owns_app = app is None
    if app is None:
        app = QApplication(sys.argv)
    app.setApplicationName("EdgeForge")
    app.setStyle("Fusion")
    _install_bundled_font()
    window = MainWindow(samples_dir=samples_dir, run_dir=run_dir)
    window.show()
    if owns_app:
        return int(app.exec_())
    return 0


_FONT_INSTALLED = False


def _install_bundled_font() -> None:
    global _FONT_INSTALLED
    if _FONT_INSTALLED:
        return
    font_path = (
        Path(__file__).resolve().parent.parent
        / "assets"
        / "fonts"
        / "wqy-microhei.ttc"
    )
    if not font_path.is_file():
        _FONT_INSTALLED = True
        return
    font_id = QFontDatabase.addApplicationFont(str(font_path))
    if font_id >= 0:
        families = QFontDatabase.applicationFontFamilies(font_id)
        if families:
            QApplication.setFont(QFont(families[0], 9))
    _FONT_INSTALLED = True


if __name__ == "__main__":
    raise SystemExit(run_gui())
