"""PySide6 desktop workbench over the deterministic pipeline."""

from __future__ import annotations

import json
import threading
from datetime import datetime
from pathlib import Path
from typing import Any

import numpy as np
from PySide6.QtCore import QPointF, QRectF, Qt, QThread, Signal
from PySide6.QtGui import QColor, QFont, QImage, QPainter, QPen, QPixmap
from PySide6.QtWidgets import (
    QApplication,
    QComboBox,
    QDoubleSpinBox,
    QFrame,
    QGridLayout,
    QGroupBox,
    QHBoxLayout,
    QLabel,
    QMainWindow,
    QMessageBox,
    QProgressBar,
    QPushButton,
    QScrollArea,
    QSpinBox,
    QSplitter,
    QTextEdit,
    QVBoxLayout,
    QWidget,
)

from .compression import CompressionConfig
from .errors import WorkbenchError
from .metrics import heatmap_png_array, psnr_from_mse
from .pipeline import PipelineResult, run_pipeline
from .pngio import load_image
from .report import export_quality_report
from .samples import generate_samples, load_manifest


def _numpy_to_qimage(array: np.ndarray) -> QImage:
    contiguous = np.ascontiguousarray(array)
    if contiguous.ndim == 2:
        height, width = contiguous.shape
        return QImage(contiguous.data, width, height, contiguous.strides[0], QImage.Format.Format_Grayscale8).copy()
    height, width, channels = contiguous.shape
    if channels != 3:
        raise ValueError("QImage conversion expects grayscale or RGB")
    return QImage(contiguous.data, width, height, contiguous.strides[0], QImage.Format.Format_RGB888).copy()


class FitImageLabel(QLabel):
    def __init__(self, title: str, parent: QWidget | None = None) -> None:
        super().__init__(parent)
        self._image: QImage | None = None
        self._title = title
        self.setMinimumSize(260, 180)
        self.setAlignment(Qt.AlignmentFlag.AlignCenter)
        self.setFrameShape(QFrame.Shape.StyledPanel)
        self.setStyleSheet("QLabel { background:#151923; color:#9aa4b2; border:1px solid #343b49; }")
        self.setText(title)

    def set_array(self, array: np.ndarray | None) -> None:
        if array is None:
            self._image = None
            self.setPixmap(QPixmap())
            self.setText(self._title)
            return
        self._image = _numpy_to_qimage(array)
        self._refresh()

    def resizeEvent(self, event) -> None:  # noqa: N802 - Qt API
        super().resizeEvent(event)
        self._refresh()

    def _refresh(self) -> None:
        if self._image is None:
            return
        pixmap = QPixmap.fromImage(self._image)
        self.setPixmap(
            pixmap.scaled(
                self.size(),
                Qt.AspectRatioMode.KeepAspectRatio,
                Qt.TransformationMode.SmoothTransformation,
            )
        )


class MetricsCurveWidget(QWidget):
    def __init__(self, parent: QWidget | None = None) -> None:
        super().__init__(parent)
        self.setMinimumSize(360, 220)
        self._result: PipelineResult | None = None
        self._series: list[tuple[int, float, float, float]] = []

    def set_result(self, result: PipelineResult | None) -> None:
        self._result = result
        self._series = []
        if result is not None:
            total_sse = 0.0
            total_pixels = 0
            total_ssim = 0.0
            total_edge = 0.0
            total_edge_count = 0
            for index in result.commit_order:
                summary = result.block_summaries.get(index)
                if summary is None or summary.get("evaluation") is None:
                    continue
                evaluation = summary["evaluation"]
                total_sse += float(evaluation.get("pixel_sse", 0.0))
                total_pixels += int(evaluation.get("pixel_count", 0))
                total_ssim += float(evaluation.get("ssim_sum", 0.0))
                total_edge += float(evaluation.get("edge_sum", 0.0))
                total_edge_count += int(evaluation.get("edge_count", 0))
                if total_pixels:
                    psnr = psnr_from_mse(total_sse / total_pixels)
                    ssim = total_ssim / total_pixels
                    edge = total_edge / total_edge_count if total_edge_count else 1.0
                    self._series.append((index, psnr, ssim, edge))
        self.update()

    def paintEvent(self, event) -> None:  # noqa: N802 - Qt API
        painter = QPainter(self)
        painter.setRenderHint(QPainter.RenderHint.Antialiasing)
        painter.fillRect(self.rect(), QColor("#11151d"))
        painter.setPen(QColor("#7f8998"))
        painter.setFont(QFont("Sans Serif", 8))
        painter.drawText(12, 20, "cumulative quality curve")
        if len(self._series) < 2:
            painter.setPen(QColor("#5d6675"))
            painter.drawText(self.rect(), Qt.AlignmentFlag.AlignCenter, "no committed block metrics")
            return
        left, top, right, bottom = 48, 34, self.width() - 18, self.height() - 30
        rect = QRectF(left, top, max(1, right - left), max(1, bottom - top))
        painter.setPen(QPen(QColor("#2a303b"), 1))
        for step in range(5):
            y = rect.top() + rect.height() * step / 4.0
            painter.drawLine(QPointF(rect.left(), y), QPointF(rect.right(), y))
        psnr_values = [row[1] for row in self._series]
        low = min(psnr_values) - 0.5
        high = max(psnr_values) + 0.5
        if high - low < 1.0:
            high = low + 1.0

        def point(position: int, psnr: float, metric: float) -> QPointF:
            x = rect.left() + rect.width() * position / max(1, len(self._series) - 1)
            y = rect.top() + rect.height() * (1.0 - (psnr - low) / (high - low))
            return QPointF(x, y)

        def point_metric(position: int, metric: float) -> QPointF:
            x = rect.left() + rect.width() * position / max(1, len(self._series) - 1)
            y = rect.bottom() - rect.height() * max(0.0, min(1.0, metric))
            return QPointF(x, y)

        painter.setPen(QPen(QColor("#48d597"), 2))
        for position in range(1, len(self._series)):
            painter.drawLine(
                point(position - 1, self._series[position - 1][1], 0),
                point(position, self._series[position][1], 0),
            )
        painter.setPen(QPen(QColor("#65b7ff"), 2))
        for position in range(1, len(self._series)):
            painter.drawLine(
                point_metric(position - 1, self._series[position - 1][2]),
                point_metric(position, self._series[position][2]),
            )
        painter.setPen(QPen(QColor("#ffbd66"), 2))
        for position in range(1, len(self._series)):
            painter.drawLine(
                point_metric(position - 1, self._series[position - 1][3]),
                point_metric(position, self._series[position][3]),
            )
        painter.setPen(QColor("#48d597"))
        painter.drawText(52, self.height() - 8, f"PSNR {self._series[-1][1]:.2f} dB")
        painter.setPen(QColor("#65b7ff"))
        painter.drawText(155, self.height() - 8, f"SSIM {self._series[-1][2]:.4f}")
        painter.setPen(QColor("#ffbd66"))
        painter.drawText(255, self.height() - 8, f"edge {self._series[-1][3]:.4f}")


class BlockMatrixWidget(QWidget):
    blockSelected = Signal(int)

    STATE_COLORS = {
        "pending": QColor("#4a5261"),
        "processing": QColor("#4aa8ff"),
        "committed": QColor("#38bd7a"),
        "failed": QColor("#e45b63"),
    }

    def __init__(self, parent: QWidget | None = None) -> None:
        super().__init__(parent)
        self._grid = (1, 1)
        self._states = ["pending"]
        self._selected = -1
        self._cell = 8
        self.setMouseTracking(True)
        self.setMinimumSize(360, 220)

    def set_result(self, result: PipelineResult | None) -> None:
        if result is None:
            self._grid = (1, 1)
            self._states = ["pending"]
            self._selected = -1
        else:
            self._grid = result.grid
            self._states = list(result.block_states)
        self._update_geometry()

    def set_progress(self, total: int, index: int, state: str) -> None:
        if index < 0 or total <= 0:
            return
        if len(self._states) != total:
            self._states = ["pending"] * total
        if 0 <= index < len(self._states):
            self._states[index] = state
        self.update()

    def _update_geometry(self) -> None:
        rows, columns = self._grid
        self._cell = max(3, min(12, 760 // max(columns, 1), 500 // max(rows, 1)))
        self.setMinimumSize(max(360, columns * self._cell + 16), max(220, rows * self._cell + 16))
        self.update()

    def _cell_rect(self, index: int) -> QRectF:
        rows, columns = self._grid
        row, column = divmod(index, max(columns, 1))
        offset = 8 if self._cell < 6 else 20
        return QRectF(
            8 + column * self._cell,
            offset + row * self._cell,
            max(1, self._cell - 1),
            max(1, self._cell - 1),
        )

    def paintEvent(self, event) -> None:  # noqa: N802 - Qt API
        painter = QPainter(self)
        painter.fillRect(self.rect(), QColor("#11151d"))
        rows, columns = self._grid
        painter.setFont(QFont("Sans Serif", 8))
        painter.setPen(QColor("#7f8998"))
        painter.drawText(8, 16, f"block state matrix  {rows}x{columns}")
        offset = 8 if self._cell < 6 else 20
        for index, state in enumerate(self._states):
            row, column = divmod(index, max(columns, 1))
            rect = QRectF(
                8 + column * self._cell,
                offset + row * self._cell,
                max(1, self._cell - 1),
                max(1, self._cell - 1),
            )
            painter.fillRect(rect, self.STATE_COLORS.get(state, QColor("#888888")))
        if 0 <= self._selected < len(self._states):
            painter.setPen(QPen(QColor("#ffffff"), 2))
            painter.drawRect(self._cell_rect(self._selected).adjusted(-1, -1, 1, 1))

    def mousePressEvent(self, event) -> None:  # noqa: N802 - Qt API
        rows, columns = self._grid
        cell = self._cell
        column = int((event.position().x() - 8) // cell)
        row = int((event.position().y() - (20 if cell >= 6 else 8)) // cell)
        if 0 <= row < rows and 0 <= column < columns:
            index = row * columns + column
            if 0 <= index < len(self._states):
                self._selected = index
                self.blockSelected.emit(index)
                self.update()


class PipelineWorker(QThread):
    resultReady = Signal(object)
    errorReady = Signal(object)
    progress = Signal(int, int, int, str)

    def __init__(
        self,
        *,
        image: np.ndarray,
        truth: np.ndarray,
        config: CompressionConfig,
        checkpoint: Path,
        image_key: str,
        mode: str,
        only_indices: set[int] | None = None,
    ) -> None:
        super().__init__()
        self.image = image
        self.truth = truth
        self.config = config
        self.checkpoint = checkpoint
        self.image_key = image_key
        self.mode = mode
        self.only_indices = only_indices
        self.cancel_event = threading.Event()

    def run(self) -> None:
        try:
            result = run_pipeline(
                self.image,
                self.truth,
                self.config,
                checkpoint_path=self.checkpoint,
                image_key=self.image_key,
                mode=self.mode,
                cancel_event=self.cancel_event,
                progress_callback=lambda done, total, index, state: self.progress.emit(done, total, index, state),
                only_indices=self.only_indices,
            )
            self.resultReady.emit(result)
        except WorkbenchError as exc:
            self.errorReady.emit(exc.to_dict())
        except Exception as exc:
            self.errorReady.emit({"status": "error", "code": "E_UI_WORKER", "message": str(exc), "details": {}})


class WorkbenchWindow(QMainWindow):
    def __init__(self, samples_dir: Path, checkpoint: Path, runs_dir: Path) -> None:
        super().__init__()
        self.samples_dir = samples_dir
        self.checkpoint = checkpoint
        self.runs_dir = runs_dir
        self.manifest: dict[str, Any] | None = None
        self.entry: dict[str, Any] | None = None
        self.image: np.ndarray | None = None
        self.truth: np.ndarray | None = None
        self.result: PipelineResult | None = None
        self.worker: PipelineWorker | None = None
        self._retry_index: int | None = None
        self.setWindowTitle("IMQ Workbench - 图像压缩质量评估工作台")
        self.resize(1480, 960)
        self._build_ui()
        self._load_manifest()

    def _build_ui(self) -> None:
        central = QWidget()
        root = QVBoxLayout(central)
        root.setContentsMargins(10, 10, 10, 10)
        root.setSpacing(8)

        controls = QGroupBox("Pipeline controls")
        control_layout = QGridLayout(controls)
        self.sample_combo = QComboBox()
        self.block_combo = QComboBox()
        self.block_combo.addItems(["4", "8", "16", "32", "64"])
        self.block_combo.setCurrentText("16")
        self.quant_spin = QDoubleSpinBox()
        self.quant_spin.setRange(0.1, 512.0)
        self.quant_spin.setDecimals(3)
        self.quant_spin.setValue(8.0)
        self.window_combo = QComboBox()
        self.window_combo.addItems([str(value) for value in range(3, 64, 2)])
        self.window_combo.setCurrentText("7")
        self.workers_spin = QSpinBox()
        self.workers_spin.setRange(1, 8)
        self.workers_spin.setValue(4)
        self.mode_combo = QComboBox()
        self.mode_combo.addItems(["realtime", "resume", "recompute"])
        self.generate_button = QPushButton("Generate samples")
        self.start_button = QPushButton("Start")
        self.pause_button = QPushButton("Pause")
        self.resume_button = QPushButton("Resume")
        self.recompute_button = QPushButton("Recompute")
        self.retry_button = QPushButton("Retry selected")
        self.export_button = QPushButton("Export report")
        self.pause_button.setEnabled(False)
        self.retry_button.setEnabled(False)
        self.export_button.setEnabled(False)
        for column, (label, widget) in enumerate(
            (
                ("Sample", self.sample_combo),
                ("Block", self.block_combo),
                ("Quant", self.quant_spin),
                ("Window", self.window_combo),
                ("Workers", self.workers_spin),
                ("Mode", self.mode_combo),
            )
        ):
            wrapper = QWidget()
            layout = QVBoxLayout(wrapper)
            layout.setContentsMargins(0, 0, 0, 0)
            layout.addWidget(QLabel(label))
            layout.addWidget(widget)
            control_layout.addWidget(wrapper, 0, column)
        button_row = QWidget()
        button_layout = QHBoxLayout(button_row)
        button_layout.setContentsMargins(0, 0, 0, 0)
        for button in (
            self.generate_button,
            self.start_button,
            self.pause_button,
            self.resume_button,
            self.recompute_button,
            self.retry_button,
            self.export_button,
        ):
            button_layout.addWidget(button)
        control_layout.addWidget(button_row, 1, 0, 1, 6)
        root.addWidget(controls)

        top_splitter = QSplitter(Qt.Orientation.Horizontal)
        compare_group = QGroupBox("Original / reconstruction")
        compare_layout = QHBoxLayout(compare_group)
        self.original_label = FitImageLabel("original")
        self.reconstruction_label = FitImageLabel("reconstruction")
        compare_layout.addWidget(self.original_label)
        compare_layout.addWidget(self.reconstruction_label)
        top_splitter.addWidget(compare_group)
        heat_group = QGroupBox("Block error heatmap")
        heat_layout = QVBoxLayout(heat_group)
        self.heatmap_label = FitImageLabel("heatmap")
        self.metrics_label = QLabel("metrics: not complete")
        self.metrics_label.setWordWrap(True)
        self.metrics_label.setStyleSheet(
            "QLabel { color:#d7deea; background:#151923; border:1px solid #343b49; padding:7px; }"
        )
        heat_layout.addWidget(self.heatmap_label, 1)
        heat_layout.addWidget(self.metrics_label)
        top_splitter.addWidget(heat_group)
        top_splitter.setSizes([920, 520])
        root.addWidget(top_splitter, 3)

        bottom_splitter = QSplitter(Qt.Orientation.Horizontal)
        curve_group = QGroupBox("Quality metric curve")
        curve_layout = QVBoxLayout(curve_group)
        self.curve_widget = MetricsCurveWidget()
        curve_layout.addWidget(self.curve_widget)
        bottom_splitter.addWidget(curve_group)
        matrix_group = QGroupBox("Block state matrix")
        matrix_layout = QVBoxLayout(matrix_group)
        self.matrix_scroll = QScrollArea()
        self.matrix_scroll.setWidgetResizable(False)
        self.matrix_widget = BlockMatrixWidget()
        self.matrix_scroll.setWidget(self.matrix_widget)
        matrix_layout.addWidget(self.matrix_scroll)
        bottom_splitter.addWidget(matrix_group)
        detail_group = QGroupBox("Selected block")
        detail_layout = QVBoxLayout(detail_group)
        self.detail_text = QTextEdit()
        self.detail_text.setReadOnly(True)
        self.detail_text.setFont(QFont("Monospace", 9))
        self.detail_text.setPlaceholderText("Click a cell in the block state matrix to inspect its frozen parameters and errors.")
        detail_layout.addWidget(self.detail_text)
        bottom_splitter.addWidget(detail_group)
        bottom_splitter.setSizes([520, 520, 360])
        root.addWidget(bottom_splitter, 2)
        self.setCentralWidget(central)

        self.progress_label = QLabel("ready")
        self.progress_bar = QProgressBar()
        self.progress_bar.setRange(0, 1)
        self.progress_bar.setValue(0)
        self.progress_bar.setFixedWidth(240)
        self.statusBar().addWidget(self.progress_bar)
        self.statusBar().addWidget(self.progress_label, 1)

        self.generate_button.clicked.connect(self._generate)
        self.sample_combo.currentIndexChanged.connect(self._sample_changed)
        self.start_button.clicked.connect(lambda: self._start("realtime"))
        self.pause_button.clicked.connect(self._pause)
        self.resume_button.clicked.connect(lambda: self._start("resume"))
        self.recompute_button.clicked.connect(lambda: self._start("recompute"))
        self.retry_button.clicked.connect(self._retry_selected)
        self.export_button.clicked.connect(self._export)
        self.matrix_widget.blockSelected.connect(self._select_block)

    def _load_manifest(self) -> None:
        try:
            self.manifest = load_manifest(self.samples_dir)
        except WorkbenchError:
            self.manifest = None
            self.progress_label.setText("no samples found; click Generate samples")
            self.start_button.setEnabled(False)
            self.resume_button.setEnabled(False)
            self.recompute_button.setEnabled(False)
            return
        self.sample_combo.blockSignals(True)
        self.sample_combo.clear()
        for item in self.manifest["samples"]:
            self.sample_combo.addItem(
                f"{item['name']}  ({item['width']}x{item['height']}x{item['channels']})",
                item["name"],
            )
        self.sample_combo.blockSignals(False)
        self.start_button.setEnabled(True)
        self.resume_button.setEnabled(True)
        self.recompute_button.setEnabled(True)
        self._sample_changed()

    def _sample_changed(self) -> None:
        if self.manifest is None:
            return
        name = self.sample_combo.currentData()
        self.entry = next((item for item in self.manifest["samples"] if item["name"] == name), None)
        if self.entry is None:
            return
        try:
            self.image = load_image(self.samples_dir / self.entry["image_path"])
            with np.load(self.samples_dir / self.entry["truth_path"], allow_pickle=False) as truth:
                self.truth = truth["edge_mask"].astype(bool, copy=False)
            self.original_label.set_array(self.image)
            self.reconstruction_label.set_array(None)
            self.heatmap_label.set_array(None)
            self.result = None
            self.matrix_widget.set_result(None)
            self.curve_widget.set_result(None)
            self.detail_text.clear()
            self.export_button.setEnabled(False)
            self.progress_label.setText(f"loaded {name}: {self.image.shape}")
        except Exception as exc:
            self._show_error({"code": "E_DECODE", "message": str(exc), "details": {}})

    def _generate(self) -> None:
        try:
            QApplication.setOverrideCursor(Qt.CursorShape.WaitCursor)
            generate_samples(self.samples_dir, preset="full")
        except Exception as exc:
            QApplication.restoreOverrideCursor()
            self._show_error({"code": "E_IO", "message": str(exc), "details": {}})
            return
        QApplication.restoreOverrideCursor()
        self._load_manifest()

    def _config(self) -> CompressionConfig:
        return CompressionConfig(
            block_size=int(self.block_combo.currentText()),
            quant_step=float(self.quant_spin.value()),
            evaluation_window=int(self.window_combo.currentText()),
            max_workers=int(self.workers_spin.value()),
            max_buffer_pixels=8_000_000,
        )

    def _start(self, mode: str, only_indices: set[int] | None = None) -> None:
        if self.image is None or self.truth is None or self.entry is None or self.worker is not None:
            return
        try:
            config = self._config()
            config.validate_for_image(self.image)
        except WorkbenchError as exc:
            self._show_error(exc.to_dict())
            return
        self.result = None
        self.export_button.setEnabled(False)
        self.retry_button.setEnabled(False)
        self.curve_widget.set_result(None)
        self.matrix_widget.set_result(None)
        total = ((self.image.shape[0] + config.block_size - 1) // config.block_size) * (
            (self.image.shape[1] + config.block_size - 1) // config.block_size
        )
        self.matrix_widget._grid = (
            (self.image.shape[0] + config.block_size - 1) // config.block_size,
            (self.image.shape[1] + config.block_size - 1) // config.block_size,
        )
        self.matrix_widget._states = ["pending"] * total
        self.matrix_widget._update_geometry()
        self.worker = PipelineWorker(
            image=self.image,
            truth=self.truth,
            config=config,
            checkpoint=self.checkpoint,
            image_key=self.entry["name"],
            mode=mode,
            only_indices=only_indices,
        )
        self.worker.progress.connect(self._on_progress)
        self.worker.resultReady.connect(self._on_result)
        self.worker.errorReady.connect(self._on_error)
        self.worker.finished.connect(self._worker_finished)
        self._set_running(True)
        self.progress_label.setText(f"{mode}: starting")
        self.worker.start()

    def _pause(self) -> None:
        if self.worker is not None:
            self.worker.cancel_event.set()
            self.progress_label.setText("pause requested; completed blocks remain checkpointed")
            self.pause_button.setEnabled(False)

    def _retry_selected(self) -> None:
        if self.result is None or self._retry_index is None:
            return
        if self.result.block_states[self._retry_index] != "failed":
            self._show_error({"code": "E_STATE", "message": "selected block is not failed", "details": {"block_index": self._retry_index}})
            return
        self._start("resume", only_indices={self._retry_index})

    def _set_running(self, running: bool) -> None:
        for widget in (
            self.sample_combo,
            self.block_combo,
            self.quant_spin,
            self.window_combo,
            self.workers_spin,
            self.mode_combo,
            self.generate_button,
            self.start_button,
            self.resume_button,
            self.recompute_button,
            self.retry_button,
            self.export_button,
        ):
            widget.setEnabled(not running)
        self.pause_button.setEnabled(running)

    def _on_progress(self, done: int, total: int, index: int, state: str) -> None:
        self.progress_label.setText(f"{state}: {done}/{total} committed")
        self.progress_bar.setRange(0, max(1, total))
        self.progress_bar.setValue(done)
        if index >= 0 and state in {"committed", "failed", "processing"}:
            self.matrix_widget.set_progress(total, index, state)

    def _on_result(self, result: PipelineResult) -> None:
        self.result = result
        self.reconstruction_label.set_array(result.reconstructed if result.progress["committed"] else None)
        self.heatmap_label.set_array(heatmap_png_array(result.heatmap) if result.progress["committed"] else None)
        self.matrix_widget.set_result(result)
        self.curve_widget.set_result(result)
        if result.metrics:
            self.metrics_label.setText(
                f"PSNR {result.metrics['psnr_db']:.4f} dB | MSE {result.metrics['mse']:.6f} | "
                f"SSIM {result.metrics['ssim']:.6f} | edge {result.metrics['edge_fidelity']:.6f}"
            )
            self.export_button.setEnabled(True)
        else:
            self.metrics_label.setText(
                f"incomplete: {result.progress['committed']}/{result.progress['total']} blocks; no final report claimed"
            )
        self.retry_button.setEnabled(bool(result.failed_blocks))
        self.progress_label.setText(
            f"{result.mode}: committed {result.progress['committed']}/{result.progress['total']}, "
            f"processed {result.processed_blocks}, resumed {result.resumed_blocks}"
        )

    def _on_error(self, payload: dict[str, Any]) -> None:
        self._show_error(payload)

    def _worker_finished(self) -> None:
        self.worker = None
        self._set_running(False)
        if self.result is not None:
            self.export_button.setEnabled(self.result.complete)
            self.retry_button.setEnabled(bool(self.result.failed_blocks))

    def _show_error(self, payload: dict[str, Any]) -> None:
        message = f"{payload.get('code', 'ERROR')}: {payload.get('message', '')}"
        self.progress_label.setText(message)
        QMessageBox.warning(self, "Workbench error", message + "\n\n" + json.dumps(payload.get("details", {}), ensure_ascii=False))

    def _select_block(self, index: int) -> None:
        self._retry_index = index
        if self.result is None:
            return
        summary = self.result.block_summaries.get(index)
        if summary is None:
            self.detail_text.setPlainText(
                json.dumps(
                    {
                        "block_index": index,
                        "state": self.result.block_states[index] if index < len(self.result.block_states) else "pending",
                    },
                    indent=2,
                )
            )
            return
        self.detail_text.setPlainText(json.dumps(summary, indent=2, ensure_ascii=False, default=str))
        self.retry_button.setEnabled(
            bool(self.result.failed_blocks) and self.result.block_states[index] == "failed"
        )

    def _export(self) -> None:
        if self.result is None or not self.result.complete:
            self._show_error({"code": "E_STATE", "message": "only a complete snapshot can export a quality report", "details": {}})
            return
        timestamp = datetime.now().strftime("%Y%m%d_%H%M%S")
        output = self.runs_dir / f"{self.result.image_key}_{self.result.mode}_{timestamp}"
        try:
            exported = export_quality_report(self.result, output, sample_metadata=self.entry)
        except WorkbenchError as exc:
            self._show_error(exc.to_dict())
            return
        self.progress_label.setText(f"report exported: {exported['report_path']}")
        QMessageBox.information(self, "Report exported", exported["report_path"])

    def closeEvent(self, event) -> None:  # noqa: N802 - Qt API
        if self.worker is not None:
            self.worker.cancel_event.set()
            self.worker.wait(2500)
        event.accept()


def launch(*, samples_dir: Path, checkpoint: Path, runs_dir: Path) -> int:
    app = QApplication.instance() or QApplication([])
    app.setApplicationName("IMQ Workbench")
    app.setStyle("Fusion")
    window = WorkbenchWindow(samples_dir, checkpoint, runs_dir)
    window.show()
    return int(app.exec())
