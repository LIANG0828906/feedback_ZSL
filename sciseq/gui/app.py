"""Interactive PySide6 scientific imaging sequence workbench."""

from __future__ import annotations

import argparse
import sys
import threading
from pathlib import Path
from typing import Any, Iterable

import numpy as np
from PySide6.QtCore import QPointF, QRectF, QSize, Qt, QThread
from PySide6.QtGui import QColor, QFont, QImage, QPainter, QPen
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
    QPushButton,
    QProgressBar,
    QSlider,
    QSpinBox,
    QSplitter,
    QTableWidget,
    QTableWidgetItem,
    QVBoxLayout,
    QWidget,
)

from ..config import BudgetConfig, DenoiseConfig, PipelineConfig, RegistrationConfig
from ..engine import RunOutcome
from ..errors import SciSeqError
from ..loader import load_sequence
from ..models import ExtractedFeature, FrameResult, Sequence, SequenceResult
from ..state import FrameStatus
from .worker import JobSpec, ProcessorWorker


class ImageCanvas(QWidget):
    def __init__(self, title: str, parent: QWidget | None = None) -> None:
        super().__init__(parent)
        self.title = title
        self._image: QImage | None = None
        self._features: tuple[ExtractedFeature, ...] = ()
        self.setMinimumSize(360, 300)

    def sizeHint(self) -> QSize:
        return QSize(520, 420)

    def set_frame(
        self,
        image: np.ndarray | None,
        features: Iterable[ExtractedFeature] = (),
    ) -> None:
        self._features = tuple(features)
        if image is None:
            self._image = None
            self.update()
            return
        work = np.asarray(image, dtype=np.float32)
        low, high = np.percentile(work, (1.0, 99.5))
        if high <= low:
            high = low + 1.0
        normalized = np.clip((work - low) / (high - low), 0.0, 1.0)
        pixels = np.ascontiguousarray((normalized * 255.0).astype(np.uint8))
        height, width = pixels.shape
        qimage = QImage(
            pixels.data,
            width,
            height,
            int(pixels.strides[0]),
            QImage.Format.Format_Grayscale8,
        ).copy()
        self._image = qimage
        self.update()

    def _image_rect(self) -> QRectF:
        margin_top = 42.0
        margin = 12.0
        available_width = max(1.0, self.width() - 2 * margin)
        available_height = max(1.0, self.height() - margin_top - margin)
        if self._image is None:
            return QRectF(margin, margin_top, available_width, available_height)
        image_width = float(self._image.width())
        image_height = float(self._image.height())
        scale = min(available_width / image_width, available_height / image_height)
        draw_width = image_width * scale
        draw_height = image_height * scale
        x = margin + (available_width - draw_width) / 2.0
        y = margin_top + (available_height - draw_height) / 2.0
        return QRectF(x, y, draw_width, draw_height)

    def paintEvent(self, event: Any) -> None:
        painter = QPainter(self)
        painter.fillRect(self.rect(), QColor("#10151c"))
        painter.setPen(QColor("#dce7f3"))
        painter.setFont(QFont("Sans Serif", 11, QFont.Weight.DemiBold))
        painter.drawText(14, 26, self.title)
        rect = self._image_rect()
        if self._image is None:
            painter.setPen(QColor("#6d7d90"))
            painter.drawText(rect, Qt.AlignmentFlag.AlignCenter, "等待帧数据")
            return
        painter.drawImage(rect, self._image)
        painter.setPen(QPen(QColor("#334155"), 1))
        painter.drawRect(rect)
        if not self._features or rect.width() <= 1 or rect.height() <= 1:
            return
        image_width = float(self._image.width())
        image_height = float(self._image.height())
        scale_x = rect.width() / image_width
        scale_y = rect.height() / image_height
        for feature in self._features:
            center = QPointF(
                rect.left() + feature.center[0] * scale_x,
                rect.top() + feature.center[1] * scale_y,
            )
            color = QColor("#ffcf4a") if feature.kind == "spot" else QColor("#40d9ff")
            painter.setPen(QPen(color, 1.7))
            if feature.kind == "line":
                bbox_width = feature.bbox[2] - feature.bbox[0] + 1
                bbox_height = feature.bbox[3] - feature.bbox[1] + 1
                length = max(bbox_width, bbox_height) * 0.7
                angle = np.deg2rad(feature.orientation_deg)
                dx = 0.5 * length * np.cos(angle) * scale_x
                dy = 0.5 * length * np.sin(angle) * scale_y
                painter.drawLine(
                    QPointF(center.x() - dx, center.y() - dy),
                    QPointF(center.x() + dx, center.y() + dy),
                )
            else:
                radius = max(5.0, min(16.0, feature.elongation * 4.0))
                painter.drawEllipse(center, radius, radius)
            painter.setPen(color)
            painter.setFont(QFont("Sans Serif", 8))
            painter.drawText(QPointF(center.x() + 7, center.y() - 7), feature.feature_id)


class OffsetPlot(QWidget):
    def __init__(self, parent: QWidget | None = None) -> None:
        super().__init__(parent)
        self._offsets: dict[int, tuple[float, float]] = {}
        self._total = 1
        self._current = 0
        self.setMinimumHeight(170)

    def set_data(
        self,
        offsets: dict[int, tuple[float, float]],
        total: int,
        current: int,
    ) -> None:
        self._offsets = offsets
        self._total = max(total, 1)
        self._current = current
        self.update()

    def paintEvent(self, event: Any) -> None:
        painter = QPainter(self)
        painter.fillRect(self.rect(), QColor("#10151c"))
        plot = QRectF(54, 20, max(1.0, self.width() - 74), max(1.0, self.height() - 46))
        painter.setPen(QColor("#dce7f3"))
        painter.setFont(QFont("Sans Serif", 10, QFont.Weight.DemiBold))
        painter.drawText(14, 16, "配准偏移曲线")
        painter.setPen(QPen(QColor("#2f3d4f"), 1))
        painter.drawRect(plot)
        painter.drawLine(
            QPointF(plot.left(), plot.center().y()),
            QPointF(plot.right(), plot.center().y()),
        )
        if not self._offsets:
            painter.setPen(QColor("#6d7d90"))
            painter.drawText(plot, Qt.AlignmentFlag.AlignCenter, "暂无已提交偏移")
            return
        values = np.asarray(list(self._offsets.values()), dtype=np.float64)
        extent = max(0.25, float(np.max(np.abs(values))) * 1.15)

        def point(index: int, value: float) -> QPointF:
            x = plot.left() + (index / max(self._total - 1, 1)) * plot.width()
            y = plot.center().y() - (value / extent) * 0.46 * plot.height()
            return QPointF(x, y)

        for component, color in ((0, QColor("#40d9ff")), (1, QColor("#ffb454"))):
            painter.setPen(QPen(color, 1.6))
            previous: QPointF | None = None
            for index in sorted(self._offsets):
                current = point(index, self._offsets[index][component])
                if previous is not None:
                    painter.drawLine(previous, current)
                previous = current
        painter.setPen(QPen(QColor("#ff5d73"), 1.0, Qt.PenStyle.DashLine))
        cursor_x = plot.left() + (self._current / max(self._total - 1, 1)) * plot.width()
        painter.drawLine(QPointF(cursor_x, plot.top()), QPointF(cursor_x, plot.bottom()))
        painter.setPen(QColor("#9fb0c4"))
        painter.setFont(QFont("Sans Serif", 8))
        painter.drawText(8, 68, "x")
        painter.drawText(8, 86, "y")
        painter.drawText(
            QRectF(plot.left(), plot.bottom() + 6, 90, 20),
            Qt.AlignmentFlag.AlignLeft,
            f"+{extent:.2f}px",
        )


class MainWindow(QMainWindow):
    def __init__(
        self,
        sequence_path: Path,
        checkpoint_dir: Path,
    ) -> None:
        super().__init__()
        self.sequence_path = Path(sequence_path)
        self.checkpoint_dir = Path(checkpoint_dir)
        self.sequence: Sequence = load_sequence(self.sequence_path)
        self.frames: dict[int, FrameResult] = {}
        self.states = [FrameStatus.PENDING for _ in range(self.sequence.frame_count)]
        self.latest_result: SequenceResult | None = None
        self.latest_preview: SequenceResult | None = None
        self._thread: QThread | None = None
        self._worker: ProcessorWorker | None = None
        self._pause_event = threading.Event()
        self._cancel_event = threading.Event()
        self._current_frame = 0
        self.setWindowTitle(
            f"SciSeq Workbench - {self.sequence.sequence_id} "
            f"({self.sequence.shape[0]}x{self.sequence.shape[1]}, "
            f"{self.sequence.frame_count} frames)"
        )
        self.resize(1480, 980)
        self._build_ui()
        self._populate_table()
        self._show_frame(0)
        self._set_idle_controls()

    def _build_ui(self) -> None:
        central = QWidget()
        self.setCentralWidget(central)
        root = QVBoxLayout(central)
        root.setContentsMargins(12, 10, 12, 10)

        header = QHBoxLayout()
        title = QLabel("科学成像序列处理工作台")
        title.setObjectName("title")
        header.addWidget(title)
        header.addStretch(1)
        self.mode_label = QLabel("模式: 未运行")
        self.mode_label.setObjectName("badge")
        header.addWidget(self.mode_label)
        root.addLayout(header)

        controls = QGroupBox("处理参数")
        controls_layout = QGridLayout(controls)
        self.window_combo = QComboBox()
        self.window_combo.addItems(["none", "hann", "hamming", "blackman"])
        self.upsample_spin = QSpinBox()
        self.upsample_spin.setRange(1, 100)
        self.upsample_spin.setValue(20)
        self.max_shift_spin = QDoubleSpinBox()
        self.max_shift_spin.setRange(0.1, 1000.0)
        self.max_shift_spin.setValue(8.0)
        self.filter_combo = QComboBox()
        self.filter_combo.addItems(["gaussian", "median", "guided"])
        self.block_spin = QSpinBox()
        self.block_spin.setRange(4, 512)
        self.block_spin.setValue(32)
        self.strength_spin = QDoubleSpinBox()
        self.strength_spin.setRange(0.1, 5.0)
        self.strength_spin.setSingleStep(0.1)
        self.strength_spin.setValue(1.0)
        self.reference_spin = QSpinBox()
        self.reference_spin.setRange(0, self.sequence.frame_count - 1)
        self.reference_spin.setValue(self.sequence.reference_frame)
        self.workers_spin = QSpinBox()
        self.workers_spin.setRange(1, 64)
        self.workers_spin.setValue(4)
        self.buffer_spin = QSpinBox()
        self.buffer_spin.setRange(1, 8)
        self.buffer_spin.setValue(2)
        self.time_budget_spin = QDoubleSpinBox()
        self.time_budget_spin.setRange(0.1, 86400.0)
        self.time_budget_spin.setValue(600.0)
        self.memory_budget_spin = QDoubleSpinBox()
        self.memory_budget_spin.setRange(16.0, 1_000_000.0)
        self.memory_budget_spin.setValue(2048.0)
        self.preview_fraction_spin = QDoubleSpinBox()
        self.preview_fraction_spin.setRange(0.05, 1.0)
        self.preview_fraction_spin.setSingleStep(0.05)
        self.preview_fraction_spin.setValue(0.5)
        widgets = [
            ("窗函数", self.window_combo),
            ("上采样", self.upsample_spin),
            ("最大位移", self.max_shift_spin),
            ("滤波器", self.filter_combo),
            ("噪声块", self.block_spin),
            ("强度", self.strength_spin),
            ("参考帧", self.reference_spin),
            ("线程", self.workers_spin),
            ("缓冲/线程", self.buffer_spin),
            ("时间预算(s)", self.time_budget_spin),
            ("内存预算(MB)", self.memory_budget_spin),
            ("预览去噪比例", self.preview_fraction_spin),
        ]
        for index, (label, widget) in enumerate(widgets):
            controls_layout.addWidget(QLabel(label), index // 6, (index % 6) * 2)
            controls_layout.addWidget(widget, index // 6, (index % 6) * 2 + 1)
        root.addWidget(controls)

        button_row = QHBoxLayout()
        self.run_button = QPushButton("应用参数并增量处理")
        self.full_button = QPushButton("一次性全量")
        self.preview_button = QPushButton("预算预览")
        self.pause_button = QPushButton("暂停")
        self.cancel_button = QPushButton("取消")
        self.retry_button = QPushButton("重试失败帧")
        self.export_button = QPushButton("导出高精度")
        for button in (
            self.run_button,
            self.full_button,
            self.preview_button,
            self.pause_button,
            self.cancel_button,
            self.retry_button,
            self.export_button,
        ):
            button_row.addWidget(button)
        button_row.addStretch(1)
        root.addLayout(button_row)
        self.run_button.clicked.connect(self._run_incremental)
        self.full_button.clicked.connect(self._run_full)
        self.preview_button.clicked.connect(self._run_preview)
        self.pause_button.clicked.connect(self._pause)
        self.cancel_button.clicked.connect(self._cancel)
        self.retry_button.clicked.connect(self._retry)
        self.export_button.clicked.connect(self._export)

        self.progress_bar = QProgressBar()
        self.progress_bar.setRange(0, self.sequence.frame_count)
        self.status_label = QLabel("就绪")
        progress_row = QHBoxLayout()
        progress_row.addWidget(self.progress_bar, 1)
        progress_row.addWidget(self.status_label)
        root.addLayout(progress_row)

        splitter = QSplitter(Qt.Orientation.Vertical)
        image_container = QWidget()
        image_layout = QHBoxLayout(image_container)
        image_layout.setContentsMargins(0, 0, 0, 0)
        self.raw_canvas = ImageCanvas("原始帧 / 噪声")
        self.denoised_canvas = ImageCanvas("去噪帧 / 特征标注")
        image_layout.addWidget(self.raw_canvas, 1)
        image_layout.addWidget(self.denoised_canvas, 1)
        splitter.addWidget(image_container)

        bottom = QWidget()
        bottom_layout = QHBoxLayout(bottom)
        bottom_layout.setContentsMargins(0, 0, 0, 0)
        self.table = QTableWidget(0, 7)
        self.table.setHorizontalHeaderLabels(
            ["帧", "状态", "dx", "dy", "噪声σ", "特征", "去噪摘要"]
        )
        self.table.setMinimumWidth(520)
        self.offset_plot = OffsetPlot()
        bottom_layout.addWidget(self.table, 1)
        bottom_layout.addWidget(self.offset_plot, 1)
        splitter.addWidget(bottom)
        splitter.setSizes([520, 260])
        root.addWidget(splitter, 1)

        timeline_row = QHBoxLayout()
        timeline_row.addWidget(QLabel("时间轴"))
        self.timeline = QSlider(Qt.Orientation.Horizontal)
        self.timeline.setRange(0, self.sequence.frame_count - 1)
        self.timeline.valueChanged.connect(self._show_frame)
        timeline_row.addWidget(self.timeline, 1)
        self.frame_label = QLabel("0 / 0")
        timeline_row.addWidget(self.frame_label)
        root.addLayout(timeline_row)

        self.setStyleSheet(
            """
            QWidget { background: #0b1016; color: #dce7f3; font-size: 12px; }
            QLabel#title { font-size: 20px; font-weight: 700; color: #f4f8ff; }
            QLabel#badge { background: #163247; color: #72d7ff; padding: 5px 10px; border-radius: 5px; }
            QGroupBox { border: 1px solid #243447; border-radius: 5px; margin-top: 9px; padding-top: 12px; }
            QGroupBox::title { subcontrol-origin: margin; left: 9px; color: #8fa7bf; }
            QPushButton { background: #17324a; border: 1px solid #2e5575; padding: 7px 11px; border-radius: 4px; }
            QPushButton:hover { background: #204765; }
            QPushButton:disabled { color: #566779; background: #121b25; border-color: #202d3b; }
            QComboBox, QSpinBox, QDoubleSpinBox { background: #111b26; border: 1px solid #2a4055; padding: 4px; min-width: 88px; }
            QTableWidget { background: #0e151e; alternate-background-color: #111c27; gridline-color: #223142; }
            QHeaderView::section { background: #172534; color: #a9bdd1; border: 0; padding: 4px; }
            QProgressBar { border: 1px solid #294057; background: #0e151e; text-align: center; }
            QProgressBar::chunk { background: #1d88b8; }
            QSlider::groove:horizontal { height: 5px; background: #24374b; }
            QSlider::handle:horizontal { width: 14px; margin: -5px 0; border-radius: 7px; background: #45c7ea; }
            """
        )

    def _populate_table(self) -> None:
        self.table.setRowCount(self.sequence.frame_count)
        for row in range(self.sequence.frame_count):
            for column in range(7):
                self.table.setItem(row, column, QTableWidgetItem(""))
            self.table.item(row, 0).setText(str(row))
            self.table.item(row, 1).setText(FrameStatus.PENDING.value)
        self.table.resizeColumnsToContents()

    def _config(self) -> PipelineConfig:
        config = PipelineConfig(
            reference_frame=self.reference_spin.value(),
            registration=RegistrationConfig(
                window=self.window_combo.currentText(),
                upsample_factor=self.upsample_spin.value(),
                max_shift=self.max_shift_spin.value(),
            ),
            denoise=DenoiseConfig(
                filter=self.filter_combo.currentText(),
                block_size=self.block_spin.value(),
                strength=self.strength_spin.value(),
            ),
            budget=BudgetConfig(
                time_budget_s=self.time_budget_spin.value(),
                memory_budget_mb=self.memory_budget_spin.value(),
                max_workers=self.workers_spin.value(),
                frames_per_worker=self.buffer_spin.value(),
                preview_denoise_fraction=self.preview_fraction_spin.value(),
            ),
        )
        config.validate(self.sequence.shape, self.sequence.frame_count)
        return config

    def _set_busy_controls(self) -> None:
        for button in (
            self.run_button,
            self.full_button,
            self.preview_button,
            self.retry_button,
            self.export_button,
        ):
            button.setEnabled(False)
        self.pause_button.setEnabled(True)
        self.cancel_button.setEnabled(True)

    def _set_idle_controls(self) -> None:
        for button in (
            self.run_button,
            self.full_button,
            self.preview_button,
            self.retry_button,
            self.export_button,
        ):
            button.setEnabled(True)
        self.pause_button.setEnabled(False)
        self.cancel_button.setEnabled(False)

    def _start_job(
        self,
        kind: str,
        *,
        preview_result: SequenceResult | None = None,
        export_dir: Path | None = None,
        reset: bool = False,
    ) -> None:
        if self._thread is not None and self._thread.isRunning():
            self.status_label.setText("已有任务在运行")
            return
        self._pause_event = threading.Event()
        self._cancel_event = threading.Event()
        spec = JobSpec(
            kind=kind,
            sequence_path=self.sequence_path,
            checkpoint_dir=self.checkpoint_dir,
            config=self._config(),
            export_dir=export_dir,
            preview_result=preview_result,
            reset=reset,
        )
        self._worker = ProcessorWorker(spec, self._pause_event, self._cancel_event)
        self._thread = QThread(self)
        self._worker.moveToThread(self._thread)
        self._thread.started.connect(self._worker.run)
        self._worker.progress.connect(self._on_progress)
        self._worker.state_changed.connect(self._on_state_changed)
        self._worker.finished.connect(self._on_finished)
        self._worker.failed.connect(self._on_failed)
        self._worker.done.connect(self._thread.quit)
        self._thread.finished.connect(self._on_thread_finished)
        self._set_busy_controls()
        self.status_label.setText(f"运行中: {kind}")
        self._thread.start()

    def _on_progress(self, committed: int, total: int, result: FrameResult | None) -> None:
        self.progress_bar.setMaximum(total)
        self.progress_bar.setValue(committed)
        self.status_label.setText(f"已提交 {committed} / {total}")
        if result is not None:
            self.frames[result.index] = result
            self.states[result.index] = FrameStatus.COMMITTED
            self._update_table_row(result.index)
            self.offset_plot.set_data(
                {
                    index: (float(frame.offset[0]), float(frame.offset[1]))
                    for index, frame in self.frames.items()
                },
                total,
                self._current_frame,
            )
            if result.index == self._current_frame:
                self._show_frame(result.index)

    def _on_state_changed(self, index: int, state: str) -> None:
        try:
            self.states[index] = FrameStatus(state)
        except ValueError:
            return
        self._update_table_row(index)

    def _on_finished(self, payload: Any) -> None:
        if not isinstance(payload, dict):
            return
        kind = payload.get("kind")
        if kind in ("incremental",):
            outcome: RunOutcome = payload["outcome"]
            for frame in outcome.records:
                self.frames[frame.index] = frame
                self.states[frame.index] = FrameStatus.COMMITTED
                self._update_table_row(frame.index)
            if outcome.result is not None:
                self.latest_result = outcome.result
                self.mode_label.setText(f"模式: {outcome.result.mode}")
            if outcome.invalidated:
                self.status_label.setText(
                    f"快照整体作废并重算: {outcome.snapshot_reason}"
                )
            elif outcome.complete:
                self.status_label.setText("增量处理完成")
            elif outcome.paused:
                self.status_label.setText(
                    f"已暂停于 {outcome.committed_count}/{self.sequence.frame_count}"
                )
            elif outcome.cancelled:
                self.status_label.setText("任务已取消")
            self._refresh_overlay()
        elif kind in ("full", "preview"):
            result: SequenceResult = payload["result"]
            self._adopt_result(result)
            if kind == "preview":
                self.latest_preview = result
                self.status_label.setText(
                    f"预览完成: x{result.downsample_factor} 降采样"
                )
            else:
                self.status_label.setText("全量处理完成")
        elif kind == "export":
            self.status_label.setText(f"导出完成: {payload['path']}")
            self.mode_label.setText("模式: 高精度导出")

    def _adopt_result(self, result: SequenceResult) -> None:
        frame_shape = result.shape
        if frame_shape != self.sequence.shape and result.mode != "preview":
            return
        self.latest_result = result
        for index in range(min(result.frame_count, self.sequence.frame_count)):
            denoised = result.denoised[index]
            features = result.features[index]
            self.frames[index] = FrameResult(
                index=index,
                offset=result.offsets[index],
                denoised=denoised,
                noise=self._noise_from_stats(result, index),
                background=float(result.backgrounds[index]),
                features=features,
                stats=result.stats["frames"][index] if "frames" in result.stats else {},
                content_sha256="",
                denoised_sha256="",
            )
            self.states[index] = FrameStatus.COMMITTED
            self._update_table_row(index)
        self.progress_bar.setMaximum(result.frame_count)
        self.progress_bar.setValue(result.frame_count)
        self.mode_label.setText(f"模式: {result.mode}")
        self._refresh_overlay()
        self._show_frame(self._current_frame)

    def _noise_from_stats(self, result: SequenceResult, index: int) -> Any:
        from ..models import NoiseEstimate

        stats = result.stats.get("frames", [])
        background = float(result.backgrounds[index])
        sigma = float(result.noise_sigmas[index])
        block_size = int(stats[index].get("block_size", 32)) if index < len(stats) else 32
        return NoiseEstimate(sigma, block_size, np.zeros((0, 0), dtype=np.float32), background)

    def _on_failed(self, error: SciSeqError) -> None:
        if error.frame is not None and 0 <= error.frame < len(self.states):
            self.states[error.frame] = FrameStatus.FAILED
            self._update_table_row(error.frame)
        self.status_label.setText(str(error))

    def _on_thread_finished(self) -> None:
        self._set_idle_controls()
        if self._worker is not None:
            self._worker.deleteLater()
        if self._thread is not None:
            self._thread.deleteLater()
        self._worker = None
        self._thread = None

    def _update_table_row(self, index: int) -> None:
        result = self.frames.get(index)
        state = self.states[index]
        self.table.item(index, 1).setText(state.value)
        self.table.item(index, 1).setForeground(
            QColor("#63e6a4") if state == FrameStatus.COMMITTED else QColor("#ff7b8a")
        )
        if result is None:
            for column in range(2, 7):
                self.table.item(index, column).setText("")
            return
        self.table.item(index, 2).setText(f"{result.offset[0]:+.4f}")
        self.table.item(index, 3).setText(f"{result.offset[1]:+.4f}")
        self.table.item(index, 4).setText(f"{result.noise.sigma:.3f}")
        self.table.item(index, 5).setText(str(len(result.features)))
        digest = getattr(result, "denoised_sha256", "")
        self.table.item(index, 6).setText(digest[:12])

    def _refresh_overlay(self) -> None:
        self.offset_plot.set_data(
            {
                index: (float(frame.offset[0]), float(frame.offset[1]))
                for index, frame in self.frames.items()
            },
            self.sequence.frame_count,
            self._current_frame,
        )
        committed = sum(1 for state in self.states if state == FrameStatus.COMMITTED)
        self.status_label.setText(
            f"进度: {committed}/{self.sequence.frame_count} 已提交"
        )

    def _show_frame(self, index: int) -> None:
        self._current_frame = int(index)
        raw = self.sequence.frame(index)
        result = self.frames.get(index)
        if result is not None:
            aligned = result.denoised
            features = result.features
            info = (
                f"帧 {index} | 偏移 ({result.offset[0]:+.3f}, {result.offset[1]:+.3f}) "
                f"| σ={result.noise.sigma:.2f} | 特征 {len(features)}"
            )
        else:
            aligned = None
            features = ()
            info = f"帧 {index} | 待处理"
        self.raw_canvas.set_frame(raw)
        self.denoised_canvas.set_frame(aligned, features)
        self.frame_label.setText(f"{index} / {self.sequence.frame_count - 1} | {info}")
        self.offset_plot.set_data(
            {
                frame_index: (float(frame.offset[0]), float(frame.offset[1]))
                for frame_index, frame in self.frames.items()
            },
            self.sequence.frame_count,
            index,
        )

    def _run_incremental(self, *_args: Any) -> None:
        self._start_job("incremental", reset=False)

    def _run_full(self, *_args: Any) -> None:
        self._start_job("full")

    def _run_preview(self, *_args: Any) -> None:
        self._start_job("preview")

    def _pause(self, *_args: Any) -> None:
        self._pause_event.set()
        self.status_label.setText("正在暂停于提交边界...")

    def _cancel(self, *_args: Any) -> None:
        self._cancel_event.set()
        self._pause_event.set()
        self.status_label.setText("正在取消...")

    def _retry(self, *_args: Any) -> None:
        self._start_job("incremental", reset=False)

    def _export(self, *_args: Any) -> None:
        directory = QFileDialog.getExistingDirectory(
            self,
            "选择高精度导出目录",
            str(Path("exports") / self.sequence.sequence_id),
        )
        if not directory:
            return
        preview = self.latest_preview
        if preview is None and self.latest_result is not None and self.latest_result.mode == "preview":
            preview = self.latest_result
        self._start_job("export", preview_result=preview, export_dir=Path(directory))

    def closeEvent(self, event: Any) -> None:
        if self._thread is not None and self._thread.isRunning():
            self._cancel_event.set()
            self._pause_event.set()
            self._thread.quit()
            self._thread.wait(5000)
        event.accept()


def _build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(prog="sciseq-gui")
    parser.add_argument("--sequence", default="samples/small")
    parser.add_argument("--checkpoint", default="artifacts/checkpoints/gui")
    parser.add_argument("--smoke-test", action="store_true")
    return parser


def main(argv: list[str] | None = None) -> int:
    args = _build_parser().parse_args(argv)
    app = QApplication.instance() or QApplication(sys.argv[:1])
    app.setApplicationName("SciSeq Workbench")
    window = MainWindow(Path(args.sequence), Path(args.checkpoint))
    window.show()
    if args.smoke_test:
        app.processEvents()
        window.close()
        return 0
    return app.exec()


if __name__ == "__main__":
    raise SystemExit(main())

