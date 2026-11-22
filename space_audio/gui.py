from __future__ import annotations

import json
import math
import os
import tempfile
import threading
from pathlib import Path
from typing import Any, Sequence

import numpy as np

from PyQt5 import QtCore, QtGui, QtWidgets

from .analysis import FrameResult
from .config import AnalysisConfig, RenderConfig, WorkbenchConfig
from .errors import WorkbenchError, para_error
from .export import export_bundle
from .generator import generate_demo
from .layout import get_layout
from .loader import load_sample
from .model import AcousticSample
from .pipeline import RunResult, run_realtime, run_resume


COLORS = (
    QtGui.QColor("#4cc9f0"),
    QtGui.QColor("#f72585"),
    QtGui.QColor("#f9c74f"),
    QtGui.QColor("#90be6d"),
    QtGui.QColor("#b5179e"),
    QtGui.QColor("#43aa8b"),
    QtGui.QColor("#577590"),
    QtGui.QColor("#f9844a"),
)


class CurvePlot(QtWidgets.QWidget):
    def __init__(self, title: str, parent: QtWidgets.QWidget | None = None) -> None:
        super().__init__(parent)
        self.title = title
        self.series: list[tuple[str, QtGui.QColor, np.ndarray, np.ndarray]] = []
        self.setMinimumHeight(150)

    def set_series(
        self,
        series: Sequence[tuple[str, QtGui.QColor, Sequence[float], Sequence[float]]],
    ) -> None:
        self.series = [
            (
                label,
                color,
                np.asarray(x, dtype=np.float64),
                np.asarray(y, dtype=np.float64),
            )
            for label, color, x, y in series
        ]
        self.update()

    def paintEvent(self, event: QtGui.QPaintEvent) -> None:
        del event
        painter = QtGui.QPainter(self)
        painter.setRenderHint(QtGui.QPainter.Antialiasing, True)
        painter.fillRect(self.rect(), QtGui.QColor("#10151d"))
        bounds = self.rect().adjusted(12, 26, -12, -20)
        painter.setPen(QtGui.QColor("#d8e1ee"))
        painter.drawText(12, 18, self.title)
        painter.setPen(QtGui.QColor("#283241"))
        for fraction in (0.25, 0.5, 0.75):
            y = bounds.top() + int(bounds.height() * fraction)
            painter.drawLine(bounds.left(), y, bounds.right(), y)
        if not self.series:
            painter.setPen(QtGui.QColor("#667085"))
            painter.drawText(bounds, QtCore.Qt.AlignCenter, "No frame data")
            return

        x_values = np.concatenate([item[2] for item in self.series if item[2].size])
        y_values = np.concatenate([item[3] for item in self.series if item[3].size])
        if x_values.size == 0 or y_values.size == 0:
            return
        x_min = float(np.min(x_values))
        x_max = float(np.max(x_values))
        y_min = float(np.min(y_values))
        y_max = float(np.max(y_values))
        if x_max <= x_min:
            x_max = x_min + 1.0
        if y_max <= y_min:
            y_max = y_min + 1.0
        y_pad = (y_max - y_min) * 0.08
        y_min -= y_pad
        y_max += y_pad

        def screen(x: float, y: float) -> QtCore.QPointF:
            px = bounds.left() + (x - x_min) / (x_max - x_min) * bounds.width()
            py = bounds.bottom() - (y - y_min) / (y_max - y_min) * bounds.height()
            return QtCore.QPointF(px, py)

        legend_x = bounds.left()
        for label, color, x, y in self.series:
            if x.size == 0:
                continue
            path = QtGui.QPainterPath()
            for index, (x_value, y_value) in enumerate(zip(x, y)):
                point = screen(float(x_value), float(y_value))
                if index == 0:
                    path.moveTo(point)
                else:
                    path.lineTo(point)
            pen = QtGui.QPen(color, 1.8)
            painter.setPen(pen)
            painter.drawPath(path)
            painter.fillRect(legend_x, 23, 9, 9, color)
            painter.setPen(QtGui.QColor("#aeb8c6"))
            painter.drawText(legend_x + 13, 32, label)
            legend_x += 24 + painter.fontMetrics().horizontalAdvance(label)


class PolarField(QtWidgets.QWidget):
    def __init__(self, parent: QtWidgets.QWidget | None = None) -> None:
        super().__init__(parent)
        self.layout_spec = get_layout("quad")
        self.energies: dict[str, float] = {}
        self.delays: dict[str, float] = {}
        self.setMinimumHeight(220)

    def set_frame(
        self,
        layout: Any,
        frame: FrameResult | None,
    ) -> None:
        self.layout_spec = layout
        self.energies = {}
        self.delays = {}
        if frame is not None:
            for estimate in frame.channels:
                self.energies[estimate.channel_id] = max(
                    estimate.early_reflection_energy,
                    estimate.reverb_energy,
                    1.0e-12,
                )
                self.delays[estimate.channel_id] = estimate.direct_delay_s
        self.update()

    def paintEvent(self, event: QtGui.QPaintEvent) -> None:
        del event
        painter = QtGui.QPainter(self)
        painter.setRenderHint(QtGui.QPainter.Antialiasing, True)
        painter.fillRect(self.rect(), QtGui.QColor("#10151d"))
        painter.setPen(QtGui.QColor("#d8e1ee"))
        painter.drawText(12, 18, "Sound-field direction")
        center = QtCore.QPointF(self.width() / 2.0, self.height() / 2.0 + 8)
        radius = min(self.width(), self.height() - 28) * 0.37
        painter.setPen(QtGui.QPen(QtGui.QColor("#2d3a4c"), 1.0))
        for scale in (0.33, 0.66, 1.0):
            painter.drawEllipse(center, radius * scale, radius * scale)
        painter.drawLine(
            QtCore.QPointF(center.x() - radius, center.y()),
            QtCore.QPointF(center.x() + radius, center.y()),
        )
        painter.drawLine(
            QtCore.QPointF(center.x(), center.y() - radius),
            QtCore.QPointF(center.x(), center.y() + radius),
        )
        max_energy = max(self.energies.values(), default=1.0)
        for index, channel in enumerate(self.layout_spec.channels):
            angle = math.radians(channel.azimuth_deg - 90.0)
            energy = self.energies.get(channel.id, 0.0)
            magnitude = 0.25 + 0.75 * math.sqrt(energy / max_energy)
            endpoint = QtCore.QPointF(
                center.x() + math.cos(angle) * radius * magnitude,
                center.y() + math.sin(angle) * radius * magnitude,
            )
            color = COLORS[index % len(COLORS)]
            painter.setPen(QtGui.QPen(color, 3.0))
            painter.drawLine(center, endpoint)
            painter.setBrush(color)
            painter.drawEllipse(endpoint, 4.5, 4.5)
            painter.setBrush(QtCore.Qt.NoBrush)
            painter.setPen(QtGui.QColor("#aeb8c6"))
            label = channel.id
            if channel.id in self.delays:
                label += f" {self.delays[channel.id] * 1000.0:.1f}ms"
            painter.drawText(
                QtCore.QRectF(
                    endpoint.x() - 36,
                    endpoint.y() - 18,
                    72,
                    16,
                ),
                QtCore.Qt.AlignCenter,
                label,
            )


class MainWindow(QtWidgets.QMainWindow):
    def __init__(self) -> None:
        super().__init__()
        self.setWindowTitle("Spatial Acoustics Workbench")
        self.resize(1320, 840)
        self.workdir = Path.cwd() / "artifacts" / "gui"
        self.workdir.mkdir(parents=True, exist_ok=True)
        self.manifest_path: Path | None = None
        self.sample: AcousticSample | None = None
        self.result: RunResult | None = None
        self._table_frames: list[int | None] = []
        self.worker: PipelineWorker | None = None
        self.pause_event = threading.Event()
        self.cancel_event = threading.Event()
        self._build_ui()
        self._refresh_visuals()
        self._set_config_enabled(True)

    def _build_ui(self) -> None:
        central = QtWidgets.QWidget()
        self.setCentralWidget(central)
        outer = QtWidgets.QHBoxLayout(central)
        controls = QtWidgets.QWidget()
        controls.setMaximumWidth(332)
        form = QtWidgets.QFormLayout(controls)
        form.setFieldGrowthPolicy(QtWidgets.QFormLayout.AllNonFixedFieldsGrow)

        self.workdir_edit = QtWidgets.QLineEdit(str(self.workdir))
        browse_workdir = QtWidgets.QPushButton("...")
        browse_workdir.setMaximumWidth(32)
        workdir_row = QtWidgets.QHBoxLayout()
        workdir_row.addWidget(self.workdir_edit)
        workdir_row.addWidget(browse_workdir)
        form.addRow("Work dir", self._row_widget(workdir_row))

        self.layout_combo = QtWidgets.QComboBox()
        self.layout_combo.addItems(["quad", "stereo", "5.1", "7.1", "mono"])
        form.addRow("Layout", self.layout_combo)

        self.frame_len_spin = self._spin(64, 1_048_576, 2048)
        self.hop_spin = self._spin(32, 1_048_576, 1024)
        self.window_spin = self._spin(64, 1_048_576, 768)
        self.order_spin = self._spin(1, 8, 2)
        self.threshold_spin = QtWidgets.QDoubleSpinBox()
        self.threshold_spin.setRange(-160.0, 0.0)
        self.threshold_spin.setValue(-80.0)
        self.threshold_spin.setSuffix(" dB")
        self.conv_window_spin = self._spin(16, 1_048_576, 1024)
        self.block_size_spin = self._spin(16, 1_048_576, 512)
        self.workers_spin = self._spin(1, 8, 2)
        self.channel_map_edit = QtWidgets.QLineEdit("")
        self.channel_map_edit.setPlaceholderText("auto, e.g. 0,1,0,1")
        form.addRow("Frame length", self.frame_len_spin)
        form.addRow("Hop", self.hop_spin)
        form.addRow("Window length", self.window_spin)
        form.addRow("Interpolation order", self.order_spin)
        form.addRow("Energy threshold", self.threshold_spin)
        form.addRow("Conv window", self.conv_window_spin)
        form.addRow("Render block", self.block_size_spin)
        form.addRow("Max workers", self.workers_spin)
        form.addRow("Channel map", self.channel_map_edit)

        self.generate_button = QtWidgets.QPushButton("Generate deterministic sample")
        self.load_button = QtWidgets.QPushButton("Open sample manifest")
        self.start_button = QtWidgets.QPushButton("Start real-time")
        self.pause_button = QtWidgets.QPushButton("Pause")
        self.stop_button = QtWidgets.QPushButton("Stop after current frame")
        self.resume_button = QtWidgets.QPushButton("Resume checkpoint")
        self.retry_button = QtWidgets.QPushButton("Retry selected frame")
        self.export_button = QtWidgets.QPushButton("Export report / WAV")
        self.audition_button = QtWidgets.QPushButton("Audition")
        for button in (
            self.generate_button,
            self.load_button,
            self.start_button,
            self.pause_button,
            self.stop_button,
            self.resume_button,
            self.retry_button,
            self.export_button,
            self.audition_button,
        ):
            form.addRow(button)

        self.progress = QtWidgets.QProgressBar()
        self.progress.setFormat("%v / %m committed")
        form.addRow(self.progress)
        self.status_label = QtWidgets.QLabel("Ready")
        self.status_label.setWordWrap(True)
        form.addRow(self.status_label)

        tabs = QtWidgets.QTabWidget()
        self.table = QtWidgets.QTableWidget(0, 10)
        self.table.setHorizontalHeaderLabels(
            [
                "Frame",
                "Channel",
                "State",
                "Delay (ms)",
                "Early E",
                "Reverb E",
                "DRR",
                "TDOA (ms)",
                "Peak err",
                "Peak bound",
            ]
        )
        self.table.horizontalHeader().setStretchLastSection(True)
        self.table.setSelectionBehavior(QtWidgets.QAbstractItemView.SelectRows)
        tabs.addTab(self.table, "Frames")

        plots = QtWidgets.QWidget()
        plot_layout = QtWidgets.QVBoxLayout(plots)
        self.delay_plot = CurvePlot("Direct-path delay by channel")
        self.energy_plot = CurvePlot("Early / late energy")
        self.error_plot = CurvePlot("Observed error and derived bound")
        plot_layout.addWidget(self.delay_plot)
        plot_layout.addWidget(self.energy_plot)
        plot_layout.addWidget(self.error_plot)
        tabs.addTab(plots, "Curves")

        field_and_wave = QtWidgets.QWidget()
        field_layout = QtWidgets.QVBoxLayout(field_and_wave)
        self.polar = PolarField()
        self.wave_plot = CurvePlot("IR vs reconstructed output (normalized)")
        field_layout.addWidget(self.polar)
        field_layout.addWidget(self.wave_plot)
        tabs.addTab(field_and_wave, "Wave / field")

        outer.addWidget(controls)
        outer.addWidget(tabs, 1)

        self.generate_button.clicked.connect(self._generate)
        self.load_button.clicked.connect(self._load)
        browse_workdir.clicked.connect(self._choose_workdir)
        self.start_button.clicked.connect(self._start)
        self.pause_button.clicked.connect(self._toggle_pause)
        self.stop_button.clicked.connect(self._stop)
        self.resume_button.clicked.connect(self._resume)
        self.retry_button.clicked.connect(self._retry_selected)
        self.export_button.clicked.connect(self._export)
        self.audition_button.clicked.connect(self._audition)
        self._config_controls = [
            self.layout_combo,
            self.frame_len_spin,
            self.hop_spin,
            self.window_spin,
            self.order_spin,
            self.threshold_spin,
            self.conv_window_spin,
            self.block_size_spin,
            self.workers_spin,
            self.channel_map_edit,
            self.generate_button,
            self.load_button,
            self.start_button,
            self.resume_button,
            self.retry_button,
        ]

    def _spin(self, minimum: int, maximum: int, value: int) -> QtWidgets.QSpinBox:
        spin = QtWidgets.QSpinBox()
        spin.setRange(minimum, maximum)
        spin.setValue(value)
        return spin

    def _row_widget(self, layout: QtWidgets.QLayout) -> QtWidgets.QWidget:
        widget = QtWidgets.QWidget()
        layout.setContentsMargins(0, 0, 0, 0)
        widget.setLayout(layout)
        return widget

    def _config(self) -> WorkbenchConfig:
        mapping_text = self.channel_map_edit.text().strip()
        channel_map = None
        if mapping_text:
            try:
                channel_map = tuple(
                    int(value.strip())
                    for value in mapping_text.split(",")
                    if value.strip()
                )
            except ValueError as exc:
                raise para_error(
                    "channel map must be comma-separated integers",
                    "channel_map",
                ) from exc
        config = WorkbenchConfig(
            analysis=AnalysisConfig(
                frame_len=self.frame_len_spin.value(),
                hop=self.hop_spin.value(),
                window_len=self.window_spin.value(),
                interpolation_order=self.order_spin.value(),
                energy_threshold_db=self.threshold_spin.value(),
            ),
            render=RenderConfig(
                conv_window_len=self.conv_window_spin.value(),
                block_size=self.block_size_spin.value(),
                interpolation_order=self.order_spin.value(),
                channel_map=channel_map,
            ),
            max_workers=self.workers_spin.value(),
        )
        config.validate()
        return config

    def _choose_workdir(self) -> None:
        selected = QtWidgets.QFileDialog.getExistingDirectory(
            self,
            "Select work directory",
            str(self.workdir),
        )
        if selected:
            self.workdir = Path(selected)
            self.workdir_edit.setText(selected)

    def _generate(self) -> None:
        try:
            self._set_status("Generating sample...")
            QtWidgets.QApplication.processEvents()
            self.workdir = Path(self.workdir_edit.text())
            self.workdir.mkdir(parents=True, exist_ok=True)
            generated = generate_demo(
                self.workdir / "sample",
                layout_name=self.layout_combo.currentText(),
                frame_len=self.frame_len_spin.value(),
                hop=self.hop_spin.value(),
                window_len=self.window_spin.value(),
                frame_count=12,
            )
            self.manifest_path = generated.manifest_path
            self.sample = load_sample(
                generated.manifest_path,
                sample_id=generated.sample_id,
            )
            self.result = None
            self._refresh_visuals()
            self._set_status(f"Loaded {self.sample.sample_id}")
        except (WorkbenchError, ValueError, OSError) as exc:
            self._show_error(exc)

    def _load(self) -> None:
        selected, _ = QtWidgets.QFileDialog.getOpenFileName(
            self,
            "Open sample manifest",
            str(self.workdir),
            "JSON manifests (*.json)",
        )
        if not selected:
            return
        try:
            self.manifest_path = Path(selected)
            self.sample = load_sample(self.manifest_path)
            self.result = None
            self._refresh_visuals()
            self._set_status(f"Loaded {self.sample.sample_id}")
        except (WorkbenchError, ValueError) as exc:
            self._show_error(exc)

    def _checkpoint_path(self) -> Path:
        return self.workdir / "checkpoints" / "desktop.sqlite"

    def _start(self) -> None:
        if self.sample is None:
            self._generate()
            if self.sample is None:
                return
        if self.worker is not None and self.worker.isRunning():
            return
        try:
            config = self._config()
        except WorkbenchError as exc:
            self._show_error(exc)
            return
        self.pause_event.clear()
        self.cancel_event.clear()
        self.progress.setMaximum(self.sample.frame_count(config.analysis))
        self._set_config_enabled(False)
        self.worker = PipelineWorker(
            sample=self.sample,
            config=config,
            checkpoint_path=self._checkpoint_path(),
            resume=False,
            pause_event=self.pause_event,
            cancel_event=self.cancel_event,
        )
        self._connect_worker(self.worker)
        self.worker.start()

    def _resume(self) -> None:
        if self.sample is None or self.worker is not None and self.worker.isRunning():
            return
        try:
            config = self._config()
        except WorkbenchError as exc:
            self._show_error(exc)
            return
        self.pause_event.clear()
        self.cancel_event.clear()
        self._set_config_enabled(False)
        self.worker = PipelineWorker(
            sample=self.sample,
            config=config,
            checkpoint_path=self._checkpoint_path(),
            resume=True,
            pause_event=self.pause_event,
            cancel_event=self.cancel_event,
        )
        self._connect_worker(self.worker)
        self.worker.start()

    def _connect_worker(self, worker: "PipelineWorker") -> None:
        worker.progress_changed.connect(self._on_progress)
        worker.result_ready.connect(self._on_result)
        worker.error_ready.connect(self._show_error)
        worker.finished.connect(self._on_worker_finished)

    def _toggle_pause(self) -> None:
        if self.pause_event.is_set():
            self.pause_event.clear()
            self.pause_button.setText("Pause")
            self._set_status("Running")
        else:
            self.pause_event.set()
            self.pause_button.setText("Continue")
            self._set_status("Paused at next frame boundary")

    def _stop(self) -> None:
        self.cancel_event.set()
        self.pause_event.clear()
        self._set_status("Stopping after the current frame; only persisted frames remain")

    def _on_progress(self, committed: int, total: int) -> None:
        self.progress.setMaximum(max(1, total))
        self.progress.setValue(committed)
        self._set_status(f"Committed {committed}/{total}")

    def _on_result(self, result: RunResult) -> None:
        self.result = result
        self._refresh_visuals()
        if result.report_complete:
            self._set_status(
                f"Complete: {result.committed_frames}/{result.total_frames} frames"
            )
        else:
            reasons = ", ".join(error.code.value for error in result.frame_errors)
            self._set_status(
                f"Partial: {result.committed_frames}/{result.total_frames}; {reasons}"
            )

    def _on_worker_finished(self) -> None:
        self.worker = None
        self.pause_event.clear()
        self.cancel_event.clear()
        self.pause_button.setText("Pause")
        self._set_config_enabled(True)

    def _set_config_enabled(self, enabled: bool) -> None:
        for widget in self._config_controls:
            widget.setEnabled(enabled)
        self.stop_button.setEnabled(not enabled)
        self.pause_button.setEnabled(not enabled)
        self.export_button.setEnabled(enabled)
        self.audition_button.setEnabled(enabled)

    def _refresh_visuals(self) -> None:
        frames = self.result.frames if self.result else ()
        channel_ids = (
            self.sample.layout.ids if self.sample is not None else ()
        )
        row_count = sum(len(frame.channels) for frame in frames)
        errors = (
            [error for error in self.result.frame_errors if error.frame_index is not None]
            if self.result is not None
            else []
        )
        row_count += len(errors)
        self.table.setRowCount(row_count)
        self._table_frames = []
        row = 0
        for frame in frames:
            for estimate in frame.channels:
                values = [
                    str(frame.frame_index),
                    estimate.channel_id,
                    "committed",
                    f"{estimate.direct_delay_s * 1000.0:.6f}",
                    f"{estimate.early_reflection_energy:.6g}",
                    f"{estimate.reverb_energy:.6g}",
                    f"{estimate.drr:.6g}",
                    f"{estimate.tdoa_s * 1000.0:.6f}",
                    f"{estimate.observed_errors.peak_quantization_s:.3g}",
                    f"{frame.bounds.peak_quantization_s:.3g}",
                ]
                for column, value in enumerate(values):
                    item = QtWidgets.QTableWidgetItem(value)
                    item.setFlags(item.flags() & ~QtCore.Qt.ItemIsEditable)
                    self.table.setItem(row, column, item)
                self._table_frames.append(frame.frame_index)
                row += 1
        for error in errors:
            values = [
                str(error.frame_index),
                str(error.channel if error.channel is not None else "-"),
                "failed",
                error.code.value,
                error.message,
                "",
                "",
                "",
                "",
                "",
            ]
            for column, value in enumerate(values):
                item = QtWidgets.QTableWidgetItem(value)
                item.setFlags(item.flags() & ~QtCore.Qt.ItemIsEditable)
                if column == 3:
                    item.setForeground(QtGui.QColor("#f72585"))
                self.table.setItem(row, column, item)
            self._table_frames.append(error.frame_index)
            row += 1
        self.table.resizeColumnsToContents()

        frame_indices = [frame.frame_index for frame in frames]
        delay_series = []
        energy_series = []
        error_series = []
        for channel_index, channel_id in enumerate(channel_ids):
            delays = []
            early = []
            reverb = []
            x_values = []
            for frame in frames:
                estimate = next(
                    (item for item in frame.channels if item.channel_id == channel_id),
                    None,
                )
                if estimate is None:
                    continue
                x_values.append(frame.frame_index)
                delays.append(estimate.direct_delay_s * 1000.0)
                early.append(estimate.early_reflection_energy)
                reverb.append(estimate.reverb_energy)
            delay_series.append(
                (channel_id, COLORS[channel_index % len(COLORS)], x_values, delays)
            )
            energy_series.append(
                (
                    f"{channel_id} early",
                    COLORS[channel_index % len(COLORS)],
                    x_values,
                    early,
                )
            )
            energy_series.append(
                (
                    f"{channel_id} late",
                    COLORS[(channel_index + 3) % len(COLORS)],
                    x_values,
                    reverb,
                )
            )
        if frames:
            observed = [
                frame.max_observed_errors.peak_quantization_s for frame in frames
            ]
            bounds = [frame.bounds.peak_quantization_s for frame in frames]
            cumulative = [
                frame.cumulative_bounds.peak_quantization_s for frame in frames
            ]
            error_series = [
                ("observed peak", COLORS[0], frame_indices, observed),
                ("frame bound", COLORS[1], frame_indices, bounds),
                ("cumulative bound", COLORS[2], frame_indices, cumulative),
            ]
        self.delay_plot.set_series(delay_series)
        self.energy_plot.set_series(energy_series)
        self.error_plot.set_series(error_series)

        last_frame = frames[-1] if frames else None
        self.polar.set_frame(
            self.sample.layout if self.sample is not None else get_layout("quad"),
            last_frame,
        )
        waveform_series: list[tuple[str, QtGui.QColor, np.ndarray, np.ndarray]] = []
        if self.sample is not None:
            ir = self.sample.ir[0]
            step = max(1, ir.size // 1500)
            x_ir = np.arange(0, ir.size, step)
            values = ir[::step]
            peak = max(float(np.max(np.abs(values))), 1.0e-12)
            waveform_series.append(
                ("original IR L", COLORS[0], x_ir / self.sample.sample_rate, values / peak)
            )
        if self.result is not None and self.result.output is not None:
            output = self.result.output[0]
            step = max(1, output.size // 1500)
            x_out = np.arange(0, output.size, step)
            values = output[::step]
            peak = max(float(np.max(np.abs(values))), 1.0e-12)
            waveform_series.append(
                (
                    "render L",
                    COLORS[1],
                    x_out / self.sample.sample_rate,
                    values / peak,
                )
            )
        self.wave_plot.set_series(waveform_series)

    def _retry_selected(self) -> None:
        if self.result is None or not self.result.frame_errors:
            self._set_status("No failed frame to retry")
            return
        selected_row = self.table.currentRow()
        selected_frame = (
            self._table_frames[selected_row]
            if 0 <= selected_row < len(self._table_frames)
            else None
        )
        failed_frames = {
            error.frame_index
            for error in self.result.frame_errors
            if error.frame_index is not None
        }
        frame_index = (
            selected_frame
            if selected_frame in failed_frames
            else self.result.frame_errors[0].frame_index
        )
        if frame_index is None:
            self._set_status("Failure has no frame index")
            return
        self._set_status(
            f"Retrying frame {frame_index} via checkpoint resume; "
            "failed frames are not persisted."
        )
        self._resume()

    def _export(self) -> None:
        if self.result is None or not self.result.report_complete:
            self._set_status("A complete run is required for export")
            return
        if self.sample is None:
            return
        target = self.workdir / "exports"
        try:
            exported = export_bundle(
                self.result,
                target,
                sample_rate=self.sample.sample_rate,
                sample=self.sample,
                prefix=self.sample.sample_id,
            )
            self._set_status(f"Exported {len(exported)} files to {target}")
        except (ValueError, OSError, WorkbenchError) as exc:
            self._show_error(exc)

    def _audition(self) -> None:
        if self.result is None or self.result.output is None or self.sample is None:
            self._set_status("Run must be complete before audition")
            return
        try:
            from PyQt5.QtMultimedia import QSoundEffect

            temp_path = Path(tempfile.gettempdir()) / "spatial_workbench_audition.wav"
            from .export import write_wav

            write_wav(self.result.output, self.sample.sample_rate, temp_path)
            self._sound_effect = QSoundEffect(self)
            self._sound_effect.setSource(QtCore.QUrl.fromLocalFile(str(temp_path)))
            self._sound_effect.setVolume(0.7)
            self._sound_effect.play()
            self._set_status("Audition started (requires a local audio device)")
        except Exception as exc:
            self._set_status(f"Audition unavailable: {exc}")

    def _show_error(self, error: Any) -> None:
        if isinstance(error, WorkbenchError):
            self._set_status(error.to_dict())
            QtWidgets.QMessageBox.warning(self, error.code.value, str(error))
        else:
            self._set_status(str(error))
            QtWidgets.QMessageBox.warning(self, "Error", str(error))

    def _set_status(self, value: Any) -> None:
        text = value if isinstance(value, str) else json.dumps(value, ensure_ascii=False)
        self.status_label.setText(text)


class PipelineWorker(QtCore.QThread):
    progress_changed = QtCore.pyqtSignal(int, int)
    result_ready = QtCore.pyqtSignal(object)
    error_ready = QtCore.pyqtSignal(object)

    def __init__(
        self,
        *,
        sample: AcousticSample,
        config: WorkbenchConfig,
        checkpoint_path: Path,
        resume: bool,
        pause_event: threading.Event,
        cancel_event: threading.Event,
    ) -> None:
        super().__init__()
        self.sample = sample
        self.config = config
        self.checkpoint_path = checkpoint_path
        self.resume = resume
        self.pause_event = pause_event
        self.cancel_event = cancel_event

    def run(self) -> None:
        try:
            kwargs = {
                "pause_event": self.pause_event,
                "cancel_event": self.cancel_event,
                "progress_callback": lambda committed, total, _result: (
                    self.progress_changed.emit(committed, total)
                ),
            }
            if self.resume:
                result = run_resume(
                    self.sample,
                    self.config,
                    self.checkpoint_path,
                    **kwargs,
                )
            else:
                result = run_realtime(
                    self.sample,
                    self.config,
                    self.checkpoint_path,
                    fresh=True,
                    **kwargs,
                )
            self.result_ready.emit(result)
        except Exception as exc:
            self.error_ready.emit(exc)


def main(argv: Sequence[str] | None = None) -> int:
    os.environ.setdefault("QT_AUTO_SCREEN_SCALE_FACTOR", "1")
    app = QtWidgets.QApplication(list(argv) if argv is not None else [])
    window = MainWindow()
    window.show()
    return app.exec_()

