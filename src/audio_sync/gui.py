from __future__ import annotations

import threading
from pathlib import Path
from typing import Any

import numpy as np
from PySide6 import QtCore, QtGui, QtWidgets

from .config import AlignmentConfig
from .engine import AlignmentEngine, AlignmentResult
from .errors import SyncError
from .export import export_alignment
from .generator import generate_session
from .manifest import LoadedSession, load_session


_COLORS = (
    QtGui.QColor("#38bdf8"),
    QtGui.QColor("#f59e0b"),
    QtGui.QColor("#a78bfa"),
    QtGui.QColor("#34d399"),
    QtGui.QColor("#fb7185"),
)


def _nice_range(values: list[np.ndarray], fallback: tuple[float, float] = (-1.0, 1.0)) -> tuple[float, float]:
    finite = [array[np.isfinite(array)] for array in values if array.size]
    if not finite:
        return fallback
    merged = np.concatenate(finite)
    if merged.size == 0:
        return fallback
    low, high = float(np.min(merged)), float(np.max(merged))
    if low == high:
        pad = max(1.0, abs(low) * 0.1)
    else:
        pad = 0.08 * (high - low)
    return low - pad, high + pad


class VisualizationCanvas(QtWidgets.QWidget):
    def __init__(self, parent: QtWidgets.QWidget | None = None) -> None:
        super().__init__(parent)
        self.setMinimumSize(900, 620)
        self.result: AlignmentResult | None = None
        self.session: LoadedSession | None = None

    def set_data(self, result: AlignmentResult, session: LoadedSession) -> None:
        self.result = result
        self.session = session
        self.update()

    def _panel_rect(self, index: int) -> QtCore.QRectF:
        margin = 14.0
        gap = 12.0
        width = (self.width() - 2.0 * margin - gap) / 2.0
        height = (self.height() - 2.0 * margin - gap) / 2.0
        col = index % 2
        row = index // 2
        return QtCore.QRectF(
            margin + col * (width + gap),
            margin + row * (height + gap),
            width,
            height,
        )

    def paintEvent(self, event: QtGui.QPaintEvent) -> None:
        painter = QtGui.QPainter(self)
        painter.setRenderHint(QtGui.QPainter.RenderHint.Antialiasing)
        painter.fillRect(self.rect(), QtGui.QColor("#0b1020"))
        if self.result is None or self.session is None:
            painter.setPen(QtGui.QColor("#94a3b8"))
            painter.drawText(self.rect(), QtCore.Qt.AlignmentFlag.AlignCenter, "Load or generate a session to visualize")
            return
        self._draw_drift(painter, self._panel_rect(0))
        self._draw_residuals(painter, self._panel_rect(1))
        self._draw_waveform(painter, self._panel_rect(2))
        self._draw_heatmap(painter, self._panel_rect(3))

    def _frame(self, painter: QtGui.QPainter, rect: QtCore.QRectF, title: str) -> QtCore.QRectF:
        painter.setPen(QtGui.QPen(QtGui.QColor("#24314d"), 1.0))
        painter.setBrush(QtGui.QColor("#111a2e"))
        painter.drawRoundedRect(rect, 8.0, 8.0)
        painter.setPen(QtGui.QColor("#cbd5e1"))
        font = painter.font()
        font.setBold(True)
        painter.setFont(font)
        painter.drawText(rect.adjusted(14, 8, -14, -8), QtCore.Qt.AlignmentFlag.AlignLeft, title)
        font.setBold(False)
        painter.setFont(font)
        return rect.adjusted(54, 34, -14, -34)

    def _map(
        self,
        x: np.ndarray,
        y: np.ndarray,
        area: QtCore.QRectF,
        x_range: tuple[float, float],
        y_range: tuple[float, float],
    ) -> QtGui.QPolygonF:
        x0, x1 = x_range
        y0, y1 = y_range
        x_scale = area.width() / max(1.0e-12, x1 - x0)
        y_scale = area.height() / max(1.0e-12, y1 - y0)
        return QtGui.QPolygonF(
            [
                QtCore.QPointF(
                    area.left() + (float(xi) - x0) * x_scale,
                    area.bottom() - (float(yi) - y0) * y_scale,
                )
                for xi, yi in zip(x, y)
            ]
        )

    def _axes(
        self,
        painter: QtGui.QPainter,
        area: QtCore.QRectF,
        x_range: tuple[float, float],
        y_range: tuple[float, float],
        x_label: str,
        y_label: str,
    ) -> None:
        painter.setPen(QtGui.QPen(QtGui.QColor("#334155"), 1.0))
        painter.drawRect(area)
        for step in range(1, 4):
            x = area.left() + area.width() * step / 4.0
            y = area.bottom() - area.height() * step / 4.0
            painter.setPen(QtGui.QPen(QtGui.QColor("#1e293b"), 1.0, QtCore.Qt.PenStyle.DotLine))
            painter.drawLine(QtCore.QPointF(x, area.top()), QtCore.QPointF(x, area.bottom()))
            painter.drawLine(QtCore.QPointF(area.left(), y), QtCore.QPointF(area.right(), y))
        painter.setPen(QtGui.QColor("#7c8cab"))
        font = painter.font()
        font.setPointSizeF(8.5)
        painter.setFont(font)
        painter.drawText(
            QtCore.QRectF(area.left(), area.bottom() + 2, area.width(), 18),
            QtCore.Qt.AlignmentFlag.AlignCenter,
            x_label,
        )
        painter.save()
        painter.translate(area.left() - 34, area.center().y())
        painter.rotate(-90)
        painter.drawText(
            QtCore.QRectF(-area.height() / 2, -10, area.height(), 18),
            QtCore.Qt.AlignmentFlag.AlignCenter,
            y_label,
        )
        painter.restore()
        painter.drawText(
            QtCore.QRectF(area.left(), area.bottom() + 15, area.width(), 18),
            QtCore.Qt.AlignmentFlag.AlignLeft,
            f"{x_range[0]:.2f}",
        )
        painter.drawText(
            QtCore.QRectF(area.left(), area.bottom() + 15, area.width(), 18),
            QtCore.Qt.AlignmentFlag.AlignRight,
            f"{x_range[1]:.2f}",
        )
        painter.drawText(
            QtCore.QRectF(area.left() - 50, area.top() - 16, 48, 16),
            QtCore.Qt.AlignmentFlag.AlignRight,
            f"{y_range[1]:.2f}",
        )
        painter.drawText(
            QtCore.QRectF(area.left() - 50, area.bottom() - 2, 48, 16),
            QtCore.Qt.AlignmentFlag.AlignRight,
            f"{y_range[0]:.2f}",
        )

    def _draw_drift(self, painter: QtGui.QPainter, panel: QtCore.QRectF) -> None:
        area = self._frame(painter, panel, "Estimated relative drift and truth")
        assert self.result is not None and self.session is not None
        references = list(self.result.channels)
        curves: list[np.ndarray] = []
        x_seconds = np.linspace(0.0, self.session.manifest.session_samples / self.result.sample_rate, 1200)
        x_samples = x_seconds * self.result.sample_rate
        for channel in references[1:]:
            model = self.result.models[channel]
            curves.append(model.delay_at(x_samples) - model.initial_offset_samples)
        y_range = _nice_range(curves)
        x_range = (0.0, float(x_seconds[-1]))
        self._axes(painter, area, x_range, y_range, "time (s)", "relative delay")
        for index, channel in enumerate(references[1:]):
            color = _COLORS[(index + 1) % len(_COLORS)]
            model = self.result.models[channel]
            estimated = model.delay_at(x_samples) - model.initial_offset_samples
            polygon = self._map(x_seconds, estimated, area, x_range, y_range)
            painter.setPen(QtGui.QPen(color, 1.8))
            painter.drawPolyline(polygon)
            truth = self.session.truth["channels"][channel]
            knots = np.asarray(truth["knots_ref_samples"], dtype=np.float64)
            rates = np.asarray(truth["rates_ppm"], dtype=np.float64) * 1.0e-6
            delay = np.zeros_like(x_samples)
            for piece, rate in enumerate(rates):
                delay += rate * (np.clip(x_samples, knots[piece], knots[piece + 1]) - knots[piece])
            truth_polygon = self._map(x_seconds, delay, area, x_range, y_range)
            pen = QtGui.QPen(color, 1.0, QtCore.Qt.PenStyle.DashLine)
            painter.setPen(pen)
            painter.drawPolyline(truth_polygon)

    def _draw_residuals(self, painter: QtGui.QPainter, panel: QtCore.QRectF) -> None:
        area = self._frame(painter, panel, "Marker residual and observed-data bound")
        assert self.result is not None
        values = []
        for channel, report in self.result.reports.items():
            values.append(np.asarray(report["marker_residual_offsets_samples"], dtype=np.float64))
            values.append(np.asarray(report["marker_residual_bounds_samples"], dtype=np.float64))
        y_range = _nice_range(values, (-0.2, 0.2))
        y_range = (min(-0.05, y_range[0]), max(0.05, y_range[1]))
        marker_count = len(self.result.reports[next(iter(self.result.reports))]["marker_ids"])
        x_range = (0.0, float(max(1, marker_count - 1)))
        self._axes(painter, area, x_range, y_range, "marker ordinal", "samples")
        y_zero = area.bottom() - (0.0 - y_range[0]) / (y_range[1] - y_range[0]) * area.height()
        painter.setPen(QtGui.QPen(QtGui.QColor("#475569"), 1.0))
        painter.drawLine(QtCore.QPointF(area.left(), y_zero), QtCore.QPointF(area.right(), y_zero))
        indexes = np.arange(marker_count, dtype=np.float64)
        for index, (channel, report) in enumerate(self.result.reports.items()):
            if channel == self.result.reference_channel:
                continue
            color = _COLORS[(index + 1) % len(_COLORS)]
            residuals = np.asarray(report["marker_residual_offsets_samples"])
            bounds = np.asarray(report["marker_residual_bounds_samples"])
            painter.setPen(QtGui.QPen(color, 1.2))
            painter.drawPolyline(self._map(indexes, bounds, area, x_range, y_range))
            painter.drawPolyline(self._map(indexes, -bounds, area, x_range, y_range))
            painter.setBrush(color)
            for x_value, y_value in zip(indexes, residuals):
                point = self._map(
                    np.array([x_value]),
                    np.array([y_value]),
                    area,
                    x_range,
                    y_range,
                )[0]
                painter.drawEllipse(point, 2.8, 2.8)

    def _draw_waveform(self, painter: QtGui.QPainter, panel: QtCore.QRectF) -> None:
        area = self._frame(painter, panel, "Before / after waveform around marker")
        assert self.result is not None and self.session is not None
        reference = self.result.reference_channel
        channel = next((item for item in self.result.channels if item != reference), reference)
        marker = self.session.manifest.markers[min(2, len(self.session.manifest.markers) - 1)]
        radius = 92
        report = self.result.reports[channel]
        estimated = float(np.asarray(report["marker_estimated_arrival_offsets_samples"])[2])
        arrival = marker.ref_sample + estimated
        guard_before = next(
            spec.guard_before for spec in self.session.manifest.channels if spec.id == channel
        )
        before_start = int(round(arrival)) - radius
        before_stop = int(round(arrival)) + radius
        after_start = marker.ref_sample - radius
        after_stop = marker.ref_sample + radius
        source = np.asarray(
            self.session.channels[channel][
                guard_before + before_start : guard_before + before_stop
            ],
            dtype=np.float64,
        )
        aligned = np.asarray(self.result.channels[channel][after_start:after_stop], dtype=np.float64)
        x = np.arange(-radius, radius, dtype=np.float64)
        y_range = _nice_range([source, aligned], (-1.0, 1.0))
        self._axes(painter, area, (-radius, radius), y_range, "offset from marker (samples)", "amplitude")
        painter.setPen(QtGui.QPen(QtGui.QColor("#64748b"), 1.3))
        painter.drawPolyline(self._map(x, source, area, (-radius, radius), y_range))
        painter.setPen(QtGui.QPen(QtGui.QColor("#22d3ee"), 1.5))
        painter.drawPolyline(self._map(x, aligned, area, (-radius, radius), y_range))
        painter.setPen(QtGui.QColor("#94a3b8"))
        painter.drawText(area.adjusted(8, 4, -8, -4), QtCore.Qt.AlignmentFlag.AlignTop | QtCore.Qt.AlignmentFlag.AlignRight, "gray: before   cyan: after")

    def _draw_heatmap(self, painter: QtGui.QPainter, panel: QtCore.QRectF) -> None:
        area = self._frame(painter, panel, "Residual / bound ratio heatmap")
        label_width = 78.0
        grid_area = area.adjusted(label_width, 0.0, 0.0, 0.0)
        assert self.result is not None
        channels = [channel for channel in self.result.channels if channel != self.result.reference_channel]
        marker_count = len(self.result.reports[next(iter(self.result.reports))]["marker_ids"])
        if not channels or marker_count == 0:
            return
        cell_width = grid_area.width() / marker_count
        cell_height = grid_area.height() / len(channels)
        ratios = []
        for channel in channels:
            report = self.result.reports[channel]
            residual = np.abs(np.asarray(report["marker_residual_offsets_samples"], dtype=np.float64))
            bound = np.maximum(np.asarray(report["marker_residual_bounds_samples"], dtype=np.float64), 1.0e-9)
            ratios.append(residual / bound)
        maximum = max(0.05, max(float(np.max(item)) for item in ratios))
        for row, channel in enumerate(channels):
            for col in range(marker_count):
                ratio = ratios[row][col]
                normalized = min(1.0, ratio / max(maximum, 0.05))
                color = QtGui.QColor.fromRgbF(
                    0.08 + 0.82 * normalized,
                    0.68 - 0.45 * normalized,
                    0.35 - 0.25 * normalized,
                )
                cell = QtCore.QRectF(
                    grid_area.left() + col * cell_width,
                    grid_area.top() + row * cell_height,
                    cell_width + 0.5,
                    cell_height + 0.5,
                )
                painter.fillRect(cell, color)
            label_rect = QtCore.QRectF(
                area.left(),
                grid_area.top() + row * cell_height,
                label_width - 6.0,
                cell_height,
            )
            painter.setPen(QtGui.QColor("#f8fafc"))
            painter.drawText(
                label_rect,
                QtCore.Qt.AlignmentFlag.AlignVCenter | QtCore.Qt.AlignmentFlag.AlignRight,
                channel,
            )
        painter.setPen(QtGui.QColor("#cbd5e1"))
        painter.drawText(
            grid_area.adjusted(0, 2, 0, 0),
            QtCore.Qt.AlignmentFlag.AlignTop | QtCore.Qt.AlignmentFlag.AlignRight,
            f"max ratio {maximum:.3f}",
        )


class AlignmentWorker(QtCore.QThread):
    success = QtCore.Signal(object)
    failure = QtCore.Signal(object)
    progress = QtCore.Signal(int, int, str)

    def __init__(
        self,
        session: LoadedSession,
        config: AlignmentConfig,
        checkpoint_path: Path,
        path: str,
        cancel_event: threading.Event,
        parent: QtCore.QObject | None = None,
    ) -> None:
        super().__init__(parent)
        self.session = session
        self.config = config
        self.checkpoint_path = checkpoint_path
        self.path = path
        self.cancel_event = cancel_event

    def run(self) -> None:
        try:
            engine = AlignmentEngine(
                self.session,
                self.config,
                checkpoint_path=self.checkpoint_path,
                cancel_event=self.cancel_event,
                progress=lambda done, total, message: self.progress.emit(done, total, message),
            )
            result = engine.run(self.path)
            self.success.emit(result)
        except Exception as exc:
            payload = exc.to_dict() if isinstance(exc, SyncError) else {"code": "E_INTERNAL", "message": str(exc)}
            self.failure.emit(payload)


class MainWindow(QtWidgets.QMainWindow):
    def __init__(self, session_dir: str | Path | None = None) -> None:
        super().__init__()
        self.setWindowTitle("Interactive Audio Clock Synchronization Workbench")
        self.resize(1460, 920)
        self.session: LoadedSession | None = None
        self.result: AlignmentResult | None = None
        self.worker: AlignmentWorker | None = None
        self.cancel_event = threading.Event()
        self._build_ui()
        if session_dir is not None:
            self.session_path.setText(str(session_dir))
            QtCore.QTimer.singleShot(0, self.load_path)

    def _build_ui(self) -> None:
        central = QtWidgets.QWidget()
        self.setCentralWidget(central)
        root = QtWidgets.QVBoxLayout(central)
        root.setContentsMargins(10, 10, 10, 10)
        root.setSpacing(8)

        toolbar = QtWidgets.QHBoxLayout()
        self.session_path = QtWidgets.QLineEdit("artifacts/demo_session")
        self.browse_button = QtWidgets.QPushButton("Browse")
        self.load_button = QtWidgets.QPushButton("Load")
        self.generate_button = QtWidgets.QPushButton("Generate sample")
        toolbar.addWidget(QtWidgets.QLabel("Session"))
        toolbar.addWidget(self.session_path, 1)
        toolbar.addWidget(self.browse_button)
        toolbar.addWidget(self.load_button)
        toolbar.addWidget(self.generate_button)
        root.addLayout(toolbar)

        controls = QtWidgets.QHBoxLayout()
        self.path_combo = QtWidgets.QComboBox()
        self.path_combo.addItems(["full", "realtime", "resume"])
        self.order_spin = QtWidgets.QSpinBox()
        self.order_spin.setRange(1, 15)
        self.order_spin.setValue(7)
        self.window_spin = QtWidgets.QSpinBox()
        self.window_spin.setRange(0, 32)
        self.window_spin.setSpecialValueText("auto")
        self.window_spin.setValue(0)
        self.segment_spin = QtWidgets.QSpinBox()
        self.segment_spin.setRange(256, 2_000_000)
        self.segment_spin.setSingleStep(1000)
        self.segment_spin.setValue(64_000)
        self.tolerance_spin = QtWidgets.QDoubleSpinBox()
        self.tolerance_spin.setRange(0.01, 2.0)
        self.tolerance_spin.setDecimals(3)
        self.tolerance_spin.setSingleStep(0.01)
        self.tolerance_spin.setValue(0.25)
        self.workers_spin = QtWidgets.QSpinBox()
        self.workers_spin.setRange(1, 4)
        self.workers_spin.setValue(2)
        self.buffer_spin = QtWidgets.QSpinBox()
        self.buffer_spin.setRange(1, 256)
        self.buffer_spin.setValue(4)
        self.buffer_spin.setSuffix(" MiB")
        self.preserve_check = QtWidgets.QCheckBox("preserve committed")
        self.preserve_check.setChecked(True)
        for label, widget in (
            ("path", self.path_combo),
            ("order", self.order_spin),
            ("window half", self.window_spin),
            ("segment", self.segment_spin),
            ("tolerance", self.tolerance_spin),
            ("workers", self.workers_spin),
            ("thread buffer", self.buffer_spin),
        ):
            controls.addWidget(QtWidgets.QLabel(label))
            controls.addWidget(widget)
        controls.addWidget(self.preserve_check)
        self.run_button = QtWidgets.QPushButton("Recalculate")
        self.cancel_button = QtWidgets.QPushButton("Cancel")
        self.cancel_button.setEnabled(False)
        self.export_button = QtWidgets.QPushButton("Export")
        self.export_button.setEnabled(False)
        controls.addWidget(self.run_button)
        controls.addWidget(self.cancel_button)
        controls.addWidget(self.export_button)
        controls.addStretch(1)
        root.addLayout(controls)

        splitter = QtWidgets.QSplitter(QtCore.Qt.Orientation.Vertical)
        self.canvas = VisualizationCanvas()
        splitter.addWidget(self.canvas)
        lower = QtWidgets.QSplitter(QtCore.Qt.Orientation.Horizontal)
        self.table = QtWidgets.QTableWidget(0, 7)
        self.table.setHorizontalHeaderLabels(
            ["Channel", "Initial offset", "CI95", "Rates ppm", "Max residual", "Max bound", "Low confidence"]
        )
        self.table.horizontalHeader().setStretchLastSection(True)
        self.table.verticalHeader().setVisible(False)
        lower.addWidget(self.table)
        self.log = QtWidgets.QPlainTextEdit()
        self.log.setReadOnly(True)
        self.log.setMaximumBlockCount(2000)
        lower.addWidget(self.log)
        lower.setSizes([920, 420])
        splitter.addWidget(lower)
        splitter.setSizes([650, 230])
        root.addWidget(splitter, 1)

        self.progress_bar = QtWidgets.QProgressBar()
        self.progress_bar.setTextVisible(True)
        self.status_label = QtWidgets.QLabel("Ready")
        status_layout = QtWidgets.QHBoxLayout()
        status_layout.addWidget(self.status_label, 1)
        status_layout.addWidget(self.progress_bar)
        root.addLayout(status_layout)

        self.browse_button.clicked.connect(self.browse)
        self.load_button.clicked.connect(self.load_path)
        self.generate_button.clicked.connect(self.generate_sample)
        self.run_button.clicked.connect(self.run_alignment)
        self.cancel_button.clicked.connect(self.cancel_run)
        self.export_button.clicked.connect(self.export_current)

    def _append_log(self, text: str) -> None:
        self.log.appendPlainText(text)

    def browse(self) -> None:
        directory = QtWidgets.QFileDialog.getExistingDirectory(self, "Select session directory", self.session_path.text())
        if directory:
            self.session_path.setText(directory)
            self.load_path()

    def generate_sample(self) -> None:
        target = Path("artifacts") / "demo_session"
        try:
            info = generate_session(target, overwrite=True)
            self.session_path.setText(str(target))
            self._append_log(f"Generated sample: {info}")
            self.load_path()
        except Exception as exc:
            self._show_error({"code": "E_INTERNAL", "message": str(exc)})

    def load_path(self) -> None:
        try:
            self.session = load_session(self.session_path.text())
            self.result = None
            self.canvas.result = None
            self.canvas.session = self.session
            self.canvas.update()
            self.status_label.setText(
                f"Loaded {self.session.manifest.session_id}: "
                f"{self.session.manifest.channel_count} channels, "
                f"{len(self.session.manifest.markers)} markers"
            )
            self._append_log(f"Validated session {self.session_path.text()}")
        except SyncError as exc:
            self._show_error(exc.to_dict())
        except Exception as exc:
            self._show_error({"code": "E_INTERNAL", "message": str(exc)})

    def _config(self) -> AlignmentConfig:
        window = self.window_spin.value()
        return AlignmentConfig(
            interpolation_order=self.order_spin.value(),
            window_half_width=None if window == 0 else window,
            segment_length=self.segment_spin.value(),
            tolerance_samples=self.tolerance_spin.value(),
            max_workers=self.workers_spin.value(),
            max_buffer_bytes=self.buffer_spin.value() * 1024 * 1024,
            preserve_committed=self.preserve_check.isChecked(),
        )

    def run_alignment(self) -> None:
        if self.worker is not None and self.worker.isRunning():
            return
        if self.session is None:
            self.load_path()
        if self.session is None:
            return
        try:
            config = self._config()
            config.validate()
        except SyncError as exc:
            self._show_error(exc.to_dict())
            return
        checkpoint = Path("artifacts") / "runs" / self.session.manifest.session_id / "checkpoints.sqlite"
        self.cancel_event = threading.Event()
        self.worker = AlignmentWorker(
            self.session,
            config,
            checkpoint,
            self.path_combo.currentText(),
            self.cancel_event,
            self,
        )
        self.worker.progress.connect(self._on_progress)
        self.worker.success.connect(self._on_success)
        self.worker.failure.connect(self._on_failure)
        self.worker.finished.connect(self._on_worker_finished)
        self.run_button.setEnabled(False)
        self.load_button.setEnabled(False)
        self.cancel_button.setEnabled(True)
        self.export_button.setEnabled(False)
        self.status_label.setText("Aligning")
        self.worker.start()

    def cancel_run(self) -> None:
        self.cancel_event.set()
        self.status_label.setText("Cancellation requested; committed segments will remain durable")

    def _on_progress(self, done: int, total: int, message: str) -> None:
        self.progress_bar.setMaximum(max(1, total))
        self.progress_bar.setValue(done)
        self.status_label.setText(message)

    def _on_success(self, result: AlignmentResult) -> None:
        self.result = result
        self.canvas.set_data(result, self.session)  # type: ignore[arg-type]
        self._fill_table(result)
        self.export_button.setEnabled(result.complete)
        self.status_label.setText(
            f"Complete: {result.path}, reused={result.diagnostics.get('reused_committed_segments', 0)}, "
            f"processed={result.diagnostics.get('newly_processed_segments', 0)}"
        )
        self._append_log(
            f"Alignment complete: low-confidence={result.low_confidence_channels}, "
            f"params_version={result.diagnostics.get('params_version')}"
        )

    def _on_failure(self, payload: dict[str, Any]) -> None:
        self._show_error(payload)
        self._append_log(f"Alignment rejected: {payload}")

    def _on_worker_finished(self) -> None:
        self.run_button.setEnabled(True)
        self.load_button.setEnabled(True)
        self.cancel_button.setEnabled(False)
        self.worker = None

    def _fill_table(self, result: AlignmentResult) -> None:
        self.table.setRowCount(len(result.reports))
        for row, (channel, report) in enumerate(result.reports.items()):
            rates = np.asarray(report["rates_ppm_estimate"])
            bounds = np.asarray(report["marker_residual_bounds_samples"])
            values = [
                channel,
                f"{report['initial_offset_estimate_samples']:.5f}",
                f"{report['initial_ci95_samples']:.5f}",
                ", ".join(f"{value:.2f}" for value in rates),
                f"{report['max_abs_residual_samples']:.5f}",
                f"{float(np.max(bounds)):.5f}" if bounds.size else "0",
                "YES" if report["low_confidence"] else "no",
            ]
            for column, value in enumerate(values):
                item = QtWidgets.QTableWidgetItem(value)
                if column == 6 and report["low_confidence"]:
                    item.setForeground(QtGui.QColor("#fb7185"))
                self.table.setItem(row, column, item)
        self.table.resizeColumnsToContents()

    def export_current(self) -> None:
        if self.result is None:
            return
        output = Path("artifacts") / "runs" / self.result.session_id / "export"
        try:
            info = export_alignment(self.result, output)
            self._append_log(f"Exported: {info}")
            self.status_label.setText(f"Exported to {output}")
        except SyncError as exc:
            self._show_error(exc.to_dict())
        except Exception as exc:
            self._show_error({"code": "E_INTERNAL", "message": str(exc)})

    def _show_error(self, payload: dict[str, Any]) -> None:
        self.status_label.setText(f"ERROR {payload.get('code')}: {payload.get('message')}")
        self._append_log(f"ERROR {payload}")

    def closeEvent(self, event: QtGui.QCloseEvent) -> None:
        self.cancel_event.set()
        if self.worker is not None and self.worker.isRunning():
            self.worker.wait(5000)
        event.accept()


def run_gui(session_dir: str | Path | None = None) -> int:
    app = QtWidgets.QApplication.instance() or QtWidgets.QApplication([])
    window = MainWindow(session_dir)
    window.show()
    return int(app.exec())

