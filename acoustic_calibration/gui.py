"""PySide6 desktop workbench for multichannel acoustic calibration."""

from __future__ import annotations

import math
import queue
import threading
from dataclasses import replace
from pathlib import Path
from typing import Any

import numpy as np
from PySide6.QtCore import QPointF, QRectF, Qt, QTimer
from PySide6.QtGui import QAction, QColor, QFont, QPainter, QPainterPath, QPen, QPolygonF
from PySide6.QtWidgets import (
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

from .engine import CalibrationEngine, RecoveryReport
from .errors import CalibrationError, E_BUSY, E_CANCELLED, E_FILTER_INVALID
from .filters import filter_response
from .hashing import sha256_file
from .models import CalibrationParams, CalibrationState, TargetCurve, ThresholdConfig
from .propagation import build_error_report
from .samples import generate_sample_wav

CHANNEL_COLORS = (
    "#2463eb",
    "#d34d2f",
    "#16856a",
    "#8f54c7",
    "#b17b00",
    "#c53f7a",
)


class ResponseCanvas(QWidget):
    """Paint band energy, uncertainty envelope and FIR response."""

    def __init__(self, parent: QWidget | None = None) -> None:
        super().__init__(parent)
        self.state: CalibrationState | None = None
        self.selected_channel = 0
        self.setMinimumSize(620, 530)

    def set_state(self, state: CalibrationState | None, selected_channel: int = 0) -> None:
        self.state = state
        self.selected_channel = selected_channel
        self.update()

    @staticmethod
    def _x(frequency: float, rect: QRectF, minimum: float, maximum: float) -> float:
        low = math.log2(max(minimum, 1e-6))
        high = math.log2(max(maximum, minimum * 1.0001))
        value = math.log2(max(frequency, minimum))
        return rect.left() + (value - low) / (high - low) * rect.width()

    @staticmethod
    def _y(value: float, rect: QRectF, minimum: float, maximum: float) -> float:
        if maximum <= minimum:
            maximum = minimum + 1.0
        return rect.bottom() - (value - minimum) / (maximum - minimum) * rect.height()

    def _panel(self, painter: QPainter, rect: QRectF, title: str, minimum: float, maximum: float) -> None:
        painter.fillRect(rect, QColor("#fbfcff"))
        painter.setPen(QPen(QColor("#d8deea"), 1))
        painter.drawRoundedRect(rect, 6, 6)
        painter.setPen(QPen(QColor("#1f2937"), 1))
        painter.setFont(QFont("Sans Serif", 10, QFont.Weight.DemiBold))
        painter.drawText(rect.adjusted(10, 5, -10, 0), Qt.AlignmentFlag.AlignLeft, title)
        painter.setFont(QFont("Sans Serif", 8))
        painter.setPen(QPen(QColor("#8792a8"), 1, Qt.PenStyle.DotLine))
        for fraction in (0.0, 0.25, 0.5, 0.75, 1.0):
            y = rect.bottom() - fraction * rect.height()
            painter.drawLine(int(rect.left()), int(y), int(rect.right()), int(y))
            value = minimum + fraction * (maximum - minimum)
            painter.setPen(QPen(QColor("#7c879a"), 1))
            painter.drawText(QRectF(rect.left() - 48, y - 8, 42, 16), Qt.AlignmentFlag.AlignRight, f"{value:.0f}")
            painter.setPen(QPen(QColor("#8792a8"), 1, Qt.PenStyle.DotLine))

    def _frequency_grid(self, painter: QPainter, rect: QRectF, minimum: float, maximum: float) -> None:
        painter.setPen(QPen(QColor("#8792a8"), 1, Qt.PenStyle.DotLine))
        for frequency in (63.0, 125.0, 250.0, 500.0, 1000.0, 2000.0, 4000.0, 8000.0, 16000.0):
            if not minimum <= frequency <= maximum:
                continue
            x = self._x(frequency, rect, minimum, maximum)
            painter.drawLine(int(x), int(rect.top()), int(x), int(rect.bottom()))
            painter.setPen(QPen(QColor("#7c879a"), 1))
            label = f"{frequency / 1000:g}k" if frequency >= 1000 else f"{frequency:g}"
            painter.drawText(QRectF(x - 18, rect.bottom() + 3, 36, 16), Qt.AlignmentFlag.AlignCenter, label)
            painter.setPen(QPen(QColor("#8792a8"), 1, Qt.PenStyle.DotLine))

    def paintEvent(self, event: Any) -> None:  # noqa: N802 - Qt API
        painter = QPainter(self)
        painter.setRenderHint(QPainter.RenderHint.Antialiasing)
        painter.fillRect(self.rect(), QColor("#f5f7fb"))
        if self.state is None:
            painter.setPen(QColor("#5e697b"))
            painter.setFont(QFont("Sans Serif", 12))
            painter.drawText(self.rect(), Qt.AlignmentFlag.AlignCenter, "No published calibration version")
            return

        band_min = self.state.bands[0].center_hz
        band_max = self.state.bands[-1].center_hz
        margin = 20.0
        total_height = float(self.height()) - 2.0 * margin
        panel_height = (total_height - 28.0) / 3.0
        x_left = margin + 64.0
        x_right = float(self.width()) - margin
        rects = [
            QRectF(x_left, margin + 6, x_right - x_left, panel_height),
            QRectF(x_left, margin + panel_height + 24, x_right - x_left, panel_height),
            QRectF(x_left, margin + 2 * panel_height + 42, x_right - x_left, panel_height),
        ]

        energy_values = [
            value
            for channel in self.state.channels
            for band in channel.spectral.bands
            for value in (band.power_db,)
        ]
        smoothing_values = [
            value for channel in self.state.channels for value in channel.smoothing.power_db
        ]
        energy_min = math.floor(min(energy_values + smoothing_values)) - 3.0
        energy_max = math.ceil(max(energy_values + smoothing_values)) + 3.0
        self._panel(painter, rects[0], "Band energy and smoothed response (dB)", energy_min, energy_max)
        self._frequency_grid(painter, rects[0], band_min, band_max)
        for channel_index, channel in enumerate(self.state.channels):
            color = QColor(CHANNEL_COLORS[channel_index % len(CHANNEL_COLORS)])
            path = QPainterPath()
            for index, band in enumerate(channel.spectral.bands):
                point_x = self._x(band.center_hz, rects[0], band_min, band_max)
                point_y = self._y(band.power_db, rects[0], energy_min, energy_max)
                if index == 0:
                    path.moveTo(point_x, point_y)
                else:
                    path.lineTo(point_x, point_y)
            painter.setPen(QPen(color, 1.6))
            painter.drawPath(path)
            smooth_path = QPainterPath()
            for index, (band, value) in enumerate(zip(channel.spectral.bands, channel.smoothing.power_db, strict=True)):
                point_x = self._x(band.center_hz, rects[0], band_min, band_max)
                point_y = self._y(value, rects[0], energy_min, energy_max)
                if index == 0:
                    smooth_path.moveTo(point_x, point_y)
                else:
                    smooth_path.lineTo(point_x, point_y)
            lighter = QColor(color)
            lighter.setAlpha(170)
            painter.setPen(QPen(lighter, 2.2))
            painter.drawPath(smooth_path)

        selected = min(self.selected_channel, len(self.state.channels) - 1)
        channel = self.state.channels[selected]
        upper_values = [
            band.power_db + error.upper_db
            for band, error in zip(channel.spectral.bands, channel.errors, strict=True)
        ]
        lower_values = [
            band.power_db - error.upper_db
            for band, error in zip(channel.spectral.bands, channel.errors, strict=True)
        ]
        envelope_values = upper_values + lower_values + list(channel.smoothing.power_db)
        env_min = math.floor(min(envelope_values)) - 2.0
        env_max = math.ceil(max(envelope_values)) + 2.0
        self._panel(
            painter,
            rects[1],
            f"{channel.alignment.channel_name} uncertainty envelope (±dB)",
            env_min,
            env_max,
        )
        self._frequency_grid(painter, rects[1], band_min, band_max)
        polygon = QPolygonF()
        for band, value in zip(channel.spectral.bands, upper_values, strict=True):
            polygon.append(
                QPointF(
                    self._x(band.center_hz, rects[1], band_min, band_max),
                    self._y(value, rects[1], env_min, env_max),
                )
            )
        for band, value in reversed(list(zip(channel.spectral.bands, lower_values, strict=True))):
            polygon.append(
                QPointF(
                    self._x(band.center_hz, rects[1], band_min, band_max),
                    self._y(value, rects[1], env_min, env_max),
                )
            )
        painter.setPen(Qt.PenStyle.NoPen)
        painter.setBrush(QColor(36, 99, 235, 45))
        painter.drawPolygon(polygon)
        raw_path = QPainterPath()
        for index, band in enumerate(channel.spectral.bands):
            x = self._x(band.center_hz, rects[1], band_min, band_max)
            y = self._y(band.power_db, rects[1], env_min, env_max)
            if index == 0:
                raw_path.moveTo(x, y)
            else:
                raw_path.lineTo(x, y)
        painter.setBrush(Qt.BrushStyle.NoBrush)
        painter.setPen(QPen(QColor("#2463eb"), 1.7))
        painter.drawPath(raw_path)
        smooth_path = QPainterPath()
        for index, (band, value) in enumerate(zip(channel.spectral.bands, channel.smoothing.power_db, strict=True)):
            x = self._x(band.center_hz, rects[1], band_min, band_max)
            y = self._y(value, rects[1], env_min, env_max)
            if index == 0:
                smooth_path.moveTo(x, y)
            else:
                smooth_path.lineTo(x, y)
        painter.setPen(QPen(QColor("#d34d2f"), 2.0))
        painter.drawPath(smooth_path)

        response = filter_response(
            channel.filter,
            np.geomspace(20.0, 20000.0, 512),
            self.state.sample_rate,
        )
        response_db = 20.0 * np.log10(np.maximum(response, 1e-12))
        filter_min = math.floor(float(np.percentile(response_db, 2))) - 2.0
        filter_max = math.ceil(float(np.percentile(response_db, 98))) + 2.0
        self._panel(
            painter,
            rects[2],
            f"{channel.alignment.channel_name} compensation |H| (dB), group delay {channel.filter.group_delay_ms:.2f} ms",
            filter_min,
            filter_max,
        )
        frequencies = np.geomspace(20.0, 20000.0, 512)
        self._frequency_grid(painter, rects[2], 20.0, 20000.0)
        filter_path = QPainterPath()
        for index, (frequency, value) in enumerate(zip(frequencies, response_db, strict=True)):
            x = self._x(float(frequency), rects[2], 20.0, 20000.0)
            y = self._y(float(value), rects[2], filter_min, filter_max)
            if index == 0:
                filter_path.moveTo(x, y)
            else:
                filter_path.lineTo(x, y)
        painter.setPen(QPen(QColor("#16856a"), 1.8))
        painter.drawPath(filter_path)

        painter.setFont(QFont("Sans Serif", 8))
        painter.setPen(QColor("#5e697b"))
        for index, item in enumerate(self.state.channels):
            x = 74.0 + index * 102.0
            y = 2.0
            painter.fillRect(QRectF(x, y + 4, 8, 8), QColor(CHANNEL_COLORS[index % len(CHANNEL_COLORS)]))
            painter.drawText(QRectF(x + 12, y, 90, 16), Qt.AlignmentFlag.AlignLeft, item.alignment.channel_name)


class CalibrationWindow(QMainWindow):
    """Main calibration workbench."""

    def __init__(
        self,
        workspace_root: str | Path | None = None,
        wav_path: str | Path | None = None,
    ) -> None:
        super().__init__()
        self.workspace_root = Path(workspace_root or (Path.cwd() / "artifacts" / "calibration_workbench"))
        self.workspace_root.mkdir(parents=True, exist_ok=True)
        self.engine: CalibrationEngine | None = None
        self._state: CalibrationState | None = None
        self._task: Any = None
        self._recovery_thread: threading.Thread | None = None
        self._events: queue.Queue[dict[str, Any]] = queue.Queue()
        self._timer = QTimer(self)
        self._timer.setInterval(120)
        self._timer.timeout.connect(self._poll_events)
        self._timer.start()

        self._build_window()
        if wav_path is None:
            source = self.workspace_root / "source" / "deterministic_5_1.wav"
            if not source.exists():
                generate_sample_wav(source, layout="5.1", seed=19093)
            self._activate_source(source, auto_start=True)
        else:
            self._activate_source(Path(wav_path), auto_start=True)

    def _build_window(self) -> None:
        self.setWindowTitle("Virtual Studio Acoustic Calibration Workbench")
        self.resize(1460, 940)
        menu = self.menuBar()
        file_menu = menu.addMenu("File")
        load_action = QAction("Load WAV…", self)
        load_action.triggered.connect(self._choose_wav)
        generate_action = QAction("Generate deterministic 5.1 sample", self)
        generate_action.triggered.connect(self._generate_and_load_sample)
        quit_action = QAction("Quit", self)
        quit_action.triggered.connect(self.close)
        file_menu.addAction(load_action)
        file_menu.addAction(generate_action)
        file_menu.addSeparator()
        file_menu.addAction(quit_action)
        task_menu = menu.addMenu("Task")
        start_action = QAction("Start calibration", self)
        start_action.triggered.connect(self.start_calibration)
        cancel_action = QAction("Cooperative cancel", self)
        cancel_action.triggered.connect(self.cancel_calibration)
        recover_action = QAction("Recover from disk snapshot", self)
        recover_action.triggered.connect(self.start_recovery)
        task_menu.addAction(start_action)
        task_menu.addAction(cancel_action)
        task_menu.addSeparator()
        task_menu.addAction(recover_action)

        central = QWidget()
        outer = QVBoxLayout(central)
        outer.setContentsMargins(10, 8, 10, 10)
        status_row = QHBoxLayout()
        self.status_label = QLabel("Preparing workbench…")
        self.status_label.setStyleSheet("font-weight: 600; color: #344054;")
        self.progress = QProgressBar()
        self.progress.setRange(0, 100)
        self.progress.setValue(0)
        self.progress.setFixedWidth(260)
        status_row.addWidget(self.status_label, 1)
        status_row.addWidget(self.progress)
        outer.addLayout(status_row)

        splitter = QSplitter(Qt.Orientation.Horizontal)
        outer.addWidget(splitter, 1)
        splitter.addWidget(self._build_controls())
        splitter.addWidget(self._build_tabs())
        splitter.setSizes([300, 1100])
        self.setCentralWidget(central)

    def _build_controls(self) -> QWidget:
        scroll = QScrollArea()
        scroll.setWidgetResizable(True)
        scroll.setMinimumWidth(285)
        scroll.setMaximumWidth(360)
        panel = QWidget()
        layout = QVBoxLayout(panel)
        layout.setSpacing(10)

        source_group = QGroupBox("Measurement source")
        source_layout = QVBoxLayout(source_group)
        self.source_label = QLabel("Not loaded")
        self.source_label.setWordWrap(True)
        source_group.setLayout(source_layout)
        source_layout.addWidget(self.source_label)
        layout.addWidget(source_group)

        controls = QGroupBox("Parameters")
        form = QFormLayout(controls)
        self.reference_combo = QComboBox()
        self.band_mode_combo = QComboBox()
        self.band_mode_combo.addItem("Third octave", "third_octave")
        self.band_mode_combo.addItem("Octave", "octave")
        self.threshold_spin = QDoubleSpinBox()
        self.threshold_spin.setRange(-140.0, -1.0)
        self.threshold_spin.setDecimals(1)
        self.threshold_spin.setSuffix(" dBFS")
        self.threshold_spin.setValue(-60.0)
        self.smoothing_spin = QDoubleSpinBox()
        self.smoothing_spin.setRange(1.0 / 24.0, 2.0)
        self.smoothing_spin.setDecimals(4)
        self.smoothing_spin.setSingleStep(1.0 / 24.0)
        self.smoothing_spin.setSuffix(" oct")
        self.smoothing_spin.setValue(1.0 / 3.0)
        self.target_edit = QLineEdit("20:0, 1000:0, 20000:0")
        self.target_edit.setToolTip("Format: frequencyHz:gaindB, separated by commas")
        self.error_threshold_spin = QDoubleSpinBox()
        self.error_threshold_spin.setRange(0.01, 60.0)
        self.error_threshold_spin.setDecimals(2)
        self.error_threshold_spin.setSuffix(" dB")
        self.error_threshold_spin.setValue(12.0)
        self.order_spin = QSpinBox()
        self.order_spin.setRange(15, 8191)
        self.order_spin.setSingleStep(2)
        self.order_spin.setValue(4095)
        self.consistency_spin = QDoubleSpinBox()
        self.consistency_spin.setRange(0.01, 30.0)
        self.consistency_spin.setDecimals(2)
        self.consistency_spin.setSuffix(" dB")
        self.consistency_spin.setValue(3.0)
        form.addRow("Reference", self.reference_combo)
        form.addRow("Band mode", self.band_mode_combo)
        form.addRow("Direct threshold", self.threshold_spin)
        form.addRow("Smoothing width", self.smoothing_spin)
        form.addRow("Target curve", self.target_edit)
        form.addRow("Error threshold", self.error_threshold_spin)
        form.addRow("FIR order", self.order_spin)
        form.addRow("Consistency tol.", self.consistency_spin)
        layout.addWidget(controls)

        actions = QHBoxLayout()
        self.start_button = QPushButton("Start calibration")
        self.start_button.setStyleSheet("QPushButton{background:#2463eb;color:white;padding:7px;border-radius:4px;}")
        self.start_button.clicked.connect(self.start_calibration)
        self.cancel_button = QPushButton("Cancel")
        self.cancel_button.setEnabled(False)
        self.cancel_button.clicked.connect(self.cancel_calibration)
        actions.addWidget(self.start_button, 1)
        actions.addWidget(self.cancel_button)
        layout.addLayout(actions)
        self.recovery_button = QPushButton("Recover from disk snapshot")
        self.recovery_button.clicked.connect(self.start_recovery)
        layout.addWidget(self.recovery_button)
        layout.addStretch(1)
        scroll.setWidget(panel)
        return scroll

    def _build_tabs(self) -> QWidget:
        self.tabs = QTabWidget()
        self.response_canvas = ResponseCanvas()
        self.tabs.addTab(self.response_canvas, "Acoustic response")

        self.channel_table = QTableWidget(0, 9)
        self.channel_table.setHorizontalHeaderLabels(
            ["Channel", "Arrival", "Offset", "Gain", "Threshold(dBFS)", "Group delay(ms)", "Max error(dB)", "Consistency", "Filter"]
        )
        self.channel_table.horizontalHeader().setSectionResizeMode(QHeaderView.ResizeMode.Stretch)
        self.channel_table.verticalHeader().setVisible(False)
        self.channel_table.setEditTriggers(QTableWidget.EditTrigger.NoEditTriggers)
        self.channel_table.itemSelectionChanged.connect(self._on_channel_selected)
        self.tabs.addTab(self.channel_table, "Channel overview")

        alert_widget = QWidget()
        alert_layout = QVBoxLayout(alert_widget)
        self.error_alerts = QTableWidget(0, 7)
        self.error_alerts.setHorizontalHeaderLabels(
            ["Channel", "Band(Hz)", "Upper(dB)", "Threshold(dB)", "Attributed node", "Node error(dB)", "Amplification"]
        )
        self.error_alerts.horizontalHeader().setSectionResizeMode(QHeaderView.ResizeMode.Stretch)
        self.error_alerts.verticalHeader().setVisible(False)
        self.error_alerts.setEditTriggers(QTableWidget.EditTrigger.NoEditTriggers)
        self.consistency_alerts = QTableWidget(0, 6)
        self.consistency_alerts.setHorizontalHeaderLabels(
            ["Channel", "Band(Hz)", "Measured(dB)", "Peer median(dB)", "Deviation(dB)", "Tolerance(dB)"]
        )
        self.consistency_alerts.horizontalHeader().setSectionResizeMode(QHeaderView.ResizeMode.Stretch)
        self.consistency_alerts.verticalHeader().setVisible(False)
        self.consistency_alerts.setEditTriggers(QTableWidget.EditTrigger.NoEditTriggers)
        alert_layout.addWidget(QLabel("Error-threshold attribution"))
        alert_layout.addWidget(self.error_alerts, 1)
        alert_layout.addWidget(QLabel("Multichannel consistency alerts"))
        alert_layout.addWidget(self.consistency_alerts, 1)
        self.tabs.addTab(alert_widget, "Errors & consistency")

        version_widget = QWidget()
        version_layout = QHBoxLayout(version_widget)
        left = QWidget()
        left_layout = QVBoxLayout(left)
        self.version_table = QTableWidget(0, 5)
        self.version_table.setHorizontalHeaderLabels(["Version", "Content hash", "Affected channels", "Affected bands", "Max error(dB)"])
        self.version_table.horizontalHeader().setSectionResizeMode(QHeaderView.ResizeMode.Stretch)
        self.version_table.verticalHeader().setVisible(False)
        self.version_table.setEditTriggers(QTableWidget.EditTrigger.NoEditTriggers)
        buttons = QHBoxLayout()
        buttons.addWidget(QLabel("Compare with"))
        self.compare_combo = QComboBox()
        buttons.addWidget(self.compare_combo, 1)
        self.diff_button = QPushButton("Show diff")
        self.diff_button.clicked.connect(self._show_selected_diff)
        self.rollback_button = QPushButton("Rollback to selected")
        self.rollback_button.clicked.connect(self._rollback_selected)
        buttons.addWidget(self.diff_button)
        buttons.addWidget(self.rollback_button)
        left_layout.addWidget(self.version_table, 1)
        left_layout.addLayout(buttons)
        self.diff_view = QTextEdit()
        self.diff_view.setReadOnly(True)
        self.diff_view.setMinimumWidth(380)
        self.diff_view.setPlaceholderText(
            "Select a version to inspect parameter, filter and error differences. "
            "Rollback appends a new version and never rewrites history."
        )
        version_layout.addWidget(left, 3)
        version_layout.addWidget(self.diff_view, 2)
        self.tabs.addTab(version_widget, "Versions")

        self.log_view = QTextEdit()
        self.log_view.setReadOnly(True)
        self.tabs.addTab(self.log_view, "Run log")
        return self.tabs

    def _append_log(self, message: str) -> None:
        self.log_view.append(message)
        self.log_view.verticalScrollBar().setValue(self.log_view.verticalScrollBar().maximum())

    def _choose_wav(self) -> None:
        path, _ = QFileDialog.getOpenFileName(self, "Load 16-bit PCM WAV", str(self.workspace_root), "WAV (*.wav)")
        if path:
            self._activate_source(Path(path), auto_start=False)

    def _generate_and_load_sample(self) -> None:
        source = self.workspace_root / "source" / f"deterministic_{int(__import__('time').time()) % 100000}.wav"
        generate_sample_wav(source, layout="5.1", seed=19093)
        self._activate_source(source, auto_start=False)

    def _activate_source(self, path: Path, auto_start: bool) -> None:
        if self._task is not None and not self._task.done.is_set():
            QMessageBox.warning(self, "Task running", "Cancel the active calibration task first.")
            return
        path = path.resolve()
        digest = sha256_file(path)[:10]
        measurement_root = self.workspace_root / "measurements" / f"{path.stem}-{digest}"
        measurement_root.mkdir(parents=True, exist_ok=True)
        try:
            self.engine = CalibrationEngine(measurement_root, path)
        except CalibrationError as exc:
            QMessageBox.critical(self, "WAV load failed", f"{exc.code}: {exc.message}")
            return
        self.source_label.setText(f"{path}\nWorkspace: {measurement_root}")
        names = self.engine._wav_cache.channel_names if self.engine._wav_cache else ()
        self.reference_combo.clear()
        self.reference_combo.addItems(list(names))
        self._state = self.engine.current_state()
        self._apply_state_to_controls(self._state)
        self.refresh_state(self._state)
        self._append_log(f"Loaded measurement {path} with layout {self.engine._wav_cache.layout_name}")
        if auto_start and self._state is None:
            QTimer.singleShot(250, self.start_calibration)

    def _parse_target(self) -> TargetCurve:
        text = self.target_edit.text().strip()
        if not text:
            raise CalibrationError(E_FILTER_INVALID, "target curve is empty")
        points: list[tuple[float, float]] = []
        try:
            for item in text.split(","):
                frequency, gain = item.split(":", 1)
                points.append((float(frequency), float(gain)))
            return TargetCurve(tuple(points))
        except (TypeError, ValueError) as exc:
            raise CalibrationError(
                E_FILTER_INVALID,
                "target curve is invalid",
                {"reason": str(exc), "text": text},
            ) from exc

    def _read_params(self) -> CalibrationParams:
        if self.engine is None or self.engine._wav_cache is None:
            raise CalibrationError(E_FILTER_INVALID, "no measurement is loaded")
        base = self._state.params if self._state is not None else CalibrationParams()
        spectral = replace(base.spectral, band_mode=str(self.band_mode_combo.currentData()))
        alignment = replace(
            base.alignment,
            reference_channel=self.reference_combo.currentIndex(),
            threshold=replace(
                base.alignment.threshold,
                floor_dbfs=float(self.threshold_spin.value()),
            ),
        )
        return replace(
            base,
            alignment=alignment,
            spectral=spectral,
            smoothing_fraction=float(self.smoothing_spin.value()),
            target_curve=self._parse_target(),
            filter_order=int(self.order_spin.value()),
            error_threshold_db=float(self.error_threshold_spin.value()),
            consistency_tolerance_db=float(self.consistency_spin.value()),
        )

    def _apply_state_to_controls(self, state: CalibrationState | None) -> None:
        if state is None:
            return
        params = state.params
        if params.alignment.reference_channel < self.reference_combo.count():
            self.reference_combo.setCurrentIndex(params.alignment.reference_channel)
        mode_index = self.band_mode_combo.findData(params.spectral.band_mode)
        if mode_index >= 0:
            self.band_mode_combo.setCurrentIndex(mode_index)
        self.threshold_spin.setValue(params.alignment.threshold.floor_dbfs)
        self.smoothing_spin.setValue(params.smoothing_fraction)
        self.target_edit.setText(
            ", ".join(f"{frequency:g}:{gain:g}" for frequency, gain in params.target_curve.points)
        )
        self.error_threshold_spin.setValue(params.error_threshold_db)
        self.order_spin.setValue(params.filter_order)
        self.consistency_spin.setValue(params.consistency_tolerance_db)

    def start_calibration(self) -> None:
        if self.engine is None:
            return
        if self._task is not None and not self._task.done.is_set():
            self.status_label.setText("E_BUSY: a recalculation task is already running")
            return
        try:
            params = self._read_params()
            self._append_log(
                f"Submitted parameters: reference {params.alignment.reference_channel}, "
                f"smoothing {params.smoothing_fraction:.4f} oct, error threshold {params.error_threshold_db:.2f} dB"
            )
            self._task = self.engine.start_async(params, progress=self._events.put)
        except CalibrationError as exc:
            self._handle_error(exc)
            return
        self.start_button.setEnabled(False)
        self.cancel_button.setEnabled(True)
        self.progress.setValue(0)
        self.status_label.setText("Recalculating in background…")

    def cancel_calibration(self) -> None:
        if self.engine is not None and self.engine.cancel():
            self.status_label.setText("Cooperative cancellation requested; waiting for a channel/band checkpoint…")
            self._append_log("Cancellation requested; the last published version remains active.")

    def start_recovery(self) -> None:
        if self.engine is None:
            return
        if self._task is not None and not self._task.done.is_set():
            self.status_label.setText("E_BUSY: a recalculation task is already running")
            return
        if self._recovery_thread is not None and self._recovery_thread.is_alive():
            self.status_label.setText("E_BUSY: a recovery task is already running")
            return
        self.start_button.setEnabled(False)
        self.cancel_button.setEnabled(True)
        self.status_label.setText("Validating disk snapshot and recovering…")

        def worker() -> None:
            try:
                report = self.engine.recover(progress=self._events.put)  # type: ignore[union-attr]
                self._events.put({"stage": "recovery_done", "report": report})
            except BaseException as exc:
                self._events.put({"stage": "recovery_error", "error": exc})

        self._recovery_thread = threading.Thread(target=worker, name="acoustic-recovery", daemon=True)
        self._recovery_thread.start()

    def _poll_events(self) -> None:
        while True:
            try:
                event = self._events.get_nowait()
            except queue.Empty:
                break
            stage = event.get("stage")
            if stage in ("channel_complete", "recovery_reuse_channel"):
                total = max(1, int(event.get("total", 1)))
                completed = int(event.get("completed", 0))
                self.progress.setValue(int(100 * completed / total))
                prefix = "Recovery: reusing channel" if stage == "recovery_reuse_channel" else "Recomputing channel"
                self.status_label.setText(f"{prefix} {event.get('channel')} ({completed}/{total})…")
            elif stage == "published":
                self.progress.setValue(100)
                self.status_label.setText(
                    f"Published v{event.get('version')}; reused {event.get('reused_nodes')} nodes, "
                    f"recomputed {event.get('recomputed_nodes')} nodes"
                )
                self._append_log(
                    f"Published v{event.get('version')} with hash {str(event.get('content_hash'))[:16]}…"
                )
            elif stage == "recovery_done":
                report: RecoveryReport = event["report"]
                self._finish_recovery(report)
            elif stage == "recovery_error":
                self._finish_task_ui()
                self._handle_error(event["error"])

        task = self._task
        if task is not None and task.done.is_set():
            self._task = None
            self._finish_task_ui()
            if task.error is not None:
                self._handle_error(task.error)
            elif task.result is not None:
                self._state = task.result
                self.refresh_state(task.result)
                self._append_log(
                    f"Version v{task.result.version} published with hash {task.result.content_hash[:16]}…"
                )

    def _finish_recovery(self, report: RecoveryReport) -> None:
        self._finish_task_ui()
        self._state = report.state
        self.refresh_state(report.state)
        message = (
            f"Recovery complete: v{report.state.version}, reused channels {report.reused_channels}, "
            f"recomputed channels {report.recomputed_channels}, already_published={report.already_published}"
        )
        self.status_label.setText(message)
        self._append_log(message)

    def _finish_task_ui(self) -> None:
        self.start_button.setEnabled(True)
        self.cancel_button.setEnabled(False)

    def _handle_error(self, error: BaseException) -> None:
        if isinstance(error, CalibrationError):
            self.status_label.setText(f"{error.code}：{error.message}")
            self._append_log(f"{error.code}: {error.message} {error.details}")
            if error.code == E_CANCELLED:
                return
            current = self.engine.current_state() if self.engine is not None else self._state
            self._state = current
            self.refresh_state(current)
            if error.code == E_FILTER_INVALID:
                self._append_log("E_FILTER_INVALID: the previous valid filter and published version were retained.")
            QMessageBox.warning(self, error.code, f"{error.message}\n{error.details}")
        else:
            self.status_label.setText(f"Internal error: {error}")
            self._append_log(f"Internal error: {error!r}")
            QMessageBox.critical(self, "Internal error", str(error))

    @staticmethod
    def _table_item(value: Any) -> QTableWidgetItem:
        item = QTableWidgetItem(str(value))
        item.setFlags(item.flags() & ~Qt.ItemFlag.ItemIsEditable)
        return item

    def refresh_state(self, state: CalibrationState | None) -> None:
        self._state = state
        if state is None:
            self.response_canvas.set_state(None)
            self.channel_table.setRowCount(0)
            self.error_alerts.setRowCount(0)
            self.consistency_alerts.setRowCount(0)
            self.version_table.setRowCount(0)
            return
        selected = max(0, self.channel_table.currentRow())
        self.response_canvas.set_state(state, selected)
        report = build_error_report(state.channel_names, state.channels, state.bands, state.params.error_threshold_db)
        self.channel_table.blockSignals(True)
        self.channel_table.setRowCount(len(state.channels))
        for row, channel in enumerate(state.channels):
            consistency_count = sum(1 for alert in state.consistency_alerts if alert.channel_index == row)
            values = [
                channel.alignment.channel_name,
                channel.alignment.arrival_sample,
                channel.alignment.offset,
                f"{channel.alignment.gain:.4f}",
                f"{20 * math.log10(max(channel.alignment.threshold, 1e-12)):.1f}",
                f"{channel.filter.group_delay_ms:.2f}",
                f"{max(error.upper_db for error in channel.errors):.2f}",
                consistency_count,
                f"{len(channel.filter.coefficients)} taps",
            ]
            for column, value in enumerate(values):
                self.channel_table.setItem(row, column, self._table_item(value))
        if 0 <= selected < len(state.channels):
            self.channel_table.selectRow(selected)
        self.channel_table.blockSignals(False)

        self.error_alerts.setRowCount(len(report.alerts))
        for row, alert in enumerate(report.alerts):
            evidence_db = 20.0 * math.log10(1.0 + float(alert["node_relative_error"]))
            values = [
                alert["channel_name"],
                f"{alert['center_hz']:.1f}",
                f"{alert['upper_db']:.2f}",
                f"{alert['threshold_db']:.2f}",
                alert["node"],
                f"{evidence_db:.3f}",
                f"{alert['node_amplification']:.3f}",
            ]
            for column, value in enumerate(values):
                self.error_alerts.setItem(row, column, self._table_item(value))

        self.consistency_alerts.setRowCount(len(state.consistency_alerts))
        for row, alert in enumerate(state.consistency_alerts):
            values = [
                alert.channel_name,
                f"{alert.center_hz:.1f}",
                f"{alert.value_db:.2f}",
                f"{alert.peer_median_db:.2f}",
                f"{alert.deviation_db:+.2f}",
                f"{alert.tolerance_db:.2f}",
            ]
            for column, value in enumerate(values):
                self.consistency_alerts.setItem(row, column, self._table_item(value))

        versions = self.engine.state_store.list_versions() if self.engine is not None else []
        previous_compare = self.compare_combo.currentData()
        self.compare_combo.blockSignals(True)
        self.compare_combo.clear()
        for version in versions:
            self.compare_combo.addItem(f"v{version}", version)
        if previous_compare in versions:
            self.compare_combo.setCurrentIndex(versions.index(previous_compare))
        elif len(versions) > 1:
            self.compare_combo.setCurrentIndex(len(versions) - 2)
        self.compare_combo.blockSignals(False)
        self.version_table.blockSignals(True)
        self.version_table.setRowCount(len(versions))
        for row, version in enumerate(reversed(versions)):
            loaded = self.engine.state_store.load(version)  # type: ignore[union-attr]
            max_error = max(error.upper_db for channel in loaded.channels for error in channel.errors)
            values = [
                f"v{version}",
                loaded.content_hash[:16],
                len(loaded.affected_channels),
                len(loaded.affected_bands),
                f"{max_error:.2f}",
            ]
            for column, value in enumerate(values):
                item = self._table_item(value)
                item.setData(Qt.ItemDataRole.UserRole, version)
                self.version_table.setItem(row, column, item)
        if self.version_table.rowCount():
            self.version_table.selectRow(0)
        self.version_table.blockSignals(False)
        self.status_label.setText(
            f"Current v{state.version} · {state.channel_names and len(state.channel_names)} channels · "
            f"{len(state.bands)} bands · consistency alerts {len(state.consistency_alerts)}"
        )

    def _on_channel_selected(self) -> None:
        row = self.channel_table.currentRow()
        if self._state is not None and row >= 0:
            self.response_canvas.set_state(self._state, row)

    def _selected_version(self) -> int | None:
        row = self.version_table.currentRow()
        if row < 0:
            return None
        item = self.version_table.item(row, 0)
        if item is None:
            return None
        return int(item.data(Qt.ItemDataRole.UserRole))

    def _show_selected_diff(self) -> None:
        if self.engine is None:
            return
        version = self._selected_version()
        if version is None:
            return
        left = self.compare_combo.currentData()
        if left is None:
            self.diff_view.setPlainText("Select two published versions to compare.")
            return
        left = int(left)
        if left == version:
            self.diff_view.setPlainText("Choose a different comparison version.")
            return
        try:
            diff = self.engine.diff(left, version)
        except CalibrationError as exc:
            self._handle_error(exc)
            return
        lines = [
            f"v{diff['left_version']} → v{diff['right_version']}",
            f"Content hash: {diff['right_hash']}",
            "",
            "Parameter changes:",
        ]
        if diff["param_changes"]:
            for key, change in diff["param_changes"].items():
                lines.append(f"  {key}: {change['old']} → {change['new']}")
        else:
            lines.append("  (none)")
        lines.extend(["", "Channel and filter changes:"])
        for channel in diff["channel_changes"]:
            lines.append(
                f"  {channel['channel_name']}: arrival {channel['old_arrival_sample']}→{channel['new_arrival_sample']}, "
                f"coefficient RMS difference {channel['coefficient_rms_difference']:.6g}, "
                f"error bands changed {len(channel['error_changes'])}"
            )
        self.diff_view.setPlainText("\n".join(lines))

    def _rollback_selected(self) -> None:
        if self.engine is None:
            return
        version = self._selected_version()
        if version is None:
            return
        answer = QMessageBox.question(
            self,
            "Confirm rollback",
            f"Rollback to v{version}? This appends a new version and does not rewrite history.",
        )
        if answer != QMessageBox.StandardButton.Yes:
            return
        try:
            state = self.engine.rollback(version)
        except CalibrationError as exc:
            self._handle_error(exc)
            return
        self._state = state
        self.refresh_state(state)
        self._append_log(f"Rollback to v{version} was appended as v{state.version}")

    def closeEvent(self, event: Any) -> None:  # noqa: N802 - Qt API
        if self.engine is not None:
            self.engine.cancel()
        if self._task is not None:
            self._task.thread.join(1.0)
        if self._recovery_thread is not None:
            self._recovery_thread.join(1.0)
        event.accept()


def run_gui(workspace_root: str | Path | None = None, wav_path: str | Path | None = None) -> int:
    app = QApplication.instance() or QApplication([])
    window = CalibrationWindow(workspace_root=workspace_root, wav_path=wav_path)
    window.show()
    return app.exec()


__all__ = ["CalibrationWindow", "ResponseCanvas", "run_gui"]
