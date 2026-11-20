from __future__ import annotations

import math
import os
from pathlib import Path
from typing import Any

import numpy as np
from PySide6.QtCore import QTimer, Qt
from PySide6.QtGui import QColor, QFont, QPainter, QPen, QPolygonF
from PySide6.QtWidgets import (
    QApplication,
    QComboBox,
    QDoubleSpinBox,
    QFileDialog,
    QFormLayout,
    QGroupBox,
    QHBoxLayout,
    QLabel,
    QLineEdit,
    QMainWindow,
    QMessageBox,
    QProgressBar,
    QPushButton,
    QSpinBox,
    QSplitter,
    QTableWidget,
    QTableWidgetItem,
    QTabWidget,
    QTextEdit,
    QVBoxLayout,
    QWidget,
)

from .config import TARGET_PRESETS, CalibrationConfig
from .errors import CalibrationError
from .pipeline import CalibrationPipeline, ProcessResult
from .worker import ProcessingCoordinator


CHANNEL_COLORS = {
    "FL": "#2563eb",
    "FR": "#dc2626",
    "FC": "#16a34a",
    "LFE": "#9333ea",
    "SL": "#ea580c",
    "SR": "#0891b2",
    "L": "#2563eb",
    "R": "#dc2626",
    "M": "#111827",
}


class PlotWidget(QWidget):
    def __init__(self, title: str, y_label: str = "dB", parent: QWidget | None = None) -> None:
        super().__init__(parent)
        self.title = title
        self.y_label = y_label
        self.series: list[dict[str, Any]] = []
        self.bands: list[dict[str, Any]] = []
        self.setMinimumHeight(250)

    def set_series(
        self,
        series: list[dict[str, Any]],
        bands: list[dict[str, Any]] | None = None,
    ) -> None:
        self.series = series
        self.bands = bands or []
        self.update()

    @staticmethod
    def _x(value: float, left: float, width: float) -> float:
        return left + width * (math.log10(max(value, 1.0)) - math.log10(20.0)) / (
            math.log10(20000.0) - math.log10(20.0)
        )

    def paintEvent(self, event: Any) -> None:
        painter = QPainter(self)
        painter.setRenderHint(QPainter.Antialiasing, True)
        painter.fillRect(self.rect(), QColor("#fbfdff"))
        painter.setPen(QColor("#0f172a"))
        title_font = QFont()
        title_font.setPointSize(10)
        title_font.setBold(True)
        painter.setFont(title_font)
        painter.drawText(14, 20, self.title)

        left, top, right, bottom = 58.0, 36.0, 18.0, 38.0
        width = max(10.0, self.width() - left - right)
        height = max(10.0, self.height() - top - bottom)
        all_y: list[float] = []
        for item in self.series:
            all_y.extend(float(v) for v in item["y"] if np.isfinite(v))
        for item in self.bands:
            all_y.extend(float(v) for v in item["low"] if np.isfinite(v))
            all_y.extend(float(v) for v in item["high"] if np.isfinite(v))
        if not all_y:
            painter.setPen(QColor("#64748b"))
            painter.drawText(self.rect(), Qt.AlignCenter, "No committed result")
            return
        y_min = math.floor(min(all_y) - 2.0)
        y_max = math.ceil(max(all_y) + 2.0)
        if y_max - y_min < 6.0:
            center = 0.5 * (y_min + y_max)
            y_min, y_max = center - 3.0, center + 3.0

        def y_pixel(value: float) -> float:
            return top + height * (y_max - value) / (y_max - y_min)

        painter.setPen(QPen(QColor("#cbd5e1"), 1))
        for tick in range(5):
            value = y_min + (y_max - y_min) * tick / 4.0
            yp = y_pixel(value)
            painter.drawLine(int(left), int(yp), int(left + width), int(yp))
            painter.setPen(QColor("#475569"))
            painter.drawText(6, int(yp + 4), f"{value:.1f}")
            painter.setPen(QPen(QColor("#cbd5e1"), 1))
        for frequency in (20, 50, 100, 200, 500, 1000, 2000, 5000, 10000, 20000):
            xp = self._x(frequency, left, width)
            painter.setPen(QPen(QColor("#e2e8f0"), 1))
            painter.drawLine(int(xp), int(top), int(xp), int(top + height))
            painter.setPen(QColor("#475569"))
            label = f"{frequency // 1000}k" if frequency >= 1000 else str(frequency)
            painter.drawText(int(xp - 10), int(top + height + 16), label)
        painter.setPen(QColor("#334155"))
        painter.drawText(8, 32, self.y_label)
        painter.drawText(int(left + width - 28), int(self.height() - 7), "Hz")

        for shade in self.bands:
            xs = np.asarray(shade["x"], dtype=float)
            low = np.asarray(shade["low"], dtype=float)
            high = np.asarray(shade["high"], dtype=float)
            polygon = QPolygonF(
                [(self._x(xv, left, width), y_pixel(yv)) for xv, yv in zip(xs, high)]
                + [
                    (self._x(xv, left, width), y_pixel(yv))
                    for xv, yv in zip(xs[::-1], low[::-1])
                ]
            )
            color = QColor(shade.get("color", "#94a3b8"))
            color.setAlpha(46)
            painter.setBrush(color)
            painter.setPen(Qt.NoPen)
            painter.drawPolygon(polygon)

        for item in self.series:
            xs = np.asarray(item["x"], dtype=float)
            ys = np.asarray(item["y"], dtype=float)
            pen = QPen(QColor(item.get("color", "#0f172a")), 2)
            painter.setPen(pen)
            points = [
                (self._x(xv, left, width), y_pixel(yv))
                for xv, yv in zip(xs, ys)
                if np.isfinite(yv)
            ]
            for start in range(len(points) - 1):
                painter.drawLine(
                    int(points[start][0]),
                    int(points[start][1]),
                    int(points[start + 1][0]),
                    int(points[start + 1][1]),
                )

        legend_x = left + 8
        legend_y = top + 10
        for item in self.series:
            painter.setPen(QPen(QColor(item.get("color", "#0f172a")), 3))
            painter.drawLine(int(legend_x), int(legend_y), int(legend_x + 18), int(legend_y))
            painter.setPen(QColor("#334155"))
            painter.drawText(int(legend_x + 23), int(legend_y + 4), str(item["label"]))
            legend_x += 74


class WorkbenchWindow(QMainWindow):
    def __init__(self, wav_path: str, cache_dir: str) -> None:
        super().__init__()
        self.setWindowTitle("Virtual Studio Acoustic Calibration Workbench")
        self.resize(1380, 900)
        self.coordinator = ProcessingCoordinator(
            CalibrationPipeline(cache_dir, cache_enabled=True)
        )
        self.current_result: ProcessResult | None = None
        self._timer = QTimer(self)
        self._timer.setInterval(120)
        self._timer.timeout.connect(self._poll_job)
        self._timer.start()
        self._build_ui(wav_path)
        QTimer.singleShot(150, self.start_recompute)

    def _build_ui(self, wav_path: str) -> None:
        central = QWidget()
        root_layout = QVBoxLayout(central)
        self.setCentralWidget(central)

        top = QHBoxLayout()
        self.path_edit = QLineEdit(wav_path)
        browse = QPushButton("Browse")
        browse.clicked.connect(self._browse)
        top.addWidget(QLabel("Measurement"))
        top.addWidget(self.path_edit, 1)
        top.addWidget(browse)
        root_layout.addLayout(top)

        splitter = QSplitter(Qt.Horizontal)
        root_layout.addWidget(splitter, 1)
        controls = self._build_controls()
        splitter.addWidget(controls)

        tabs = QTabWidget()
        self.alignment_table = self._new_table(
            ["Channel", "Arrival", "Offset", "Offset delta", "Threshold", "Gain"]
        )
        tabs.addTab(self.alignment_table, "Alignment")
        self.energy_plot = PlotWidget("Band energy and propagated upper bound", "dB")
        tabs.addTab(self.energy_plot, "Band energy")
        self.error_plot = PlotWidget("Combined error upper bound", "dB")
        tabs.addTab(self.error_plot, "Error budget")
        self.filter_plot = PlotWidget("Linear-phase FIR compensation response", "dB")
        tabs.addTab(self.filter_plot, "Filters")
        self.warning_table = self._new_table(
            ["Kind", "Channel", "Band interval", "Value", "Source", "Evidence"]
        )
        tabs.addTab(self.warning_table, "Consistency")
        self.diff_view = QTextEdit()
        self.diff_view.setReadOnly(True)
        tabs.addTab(self.diff_view, "Version diff")
        splitter.addWidget(tabs)
        splitter.setSizes([330, 1050])

        bottom = QHBoxLayout()
        self.start_button = QPushButton("Run recompute")
        self.start_button.clicked.connect(self.start_recompute)
        self.cancel_button = QPushButton("Cancel")
        self.cancel_button.clicked.connect(self.cancel_recompute)
        self.cancel_button.setEnabled(False)
        self.status_label = QLabel("Ready")
        self.progress = QProgressBar()
        self.progress.setRange(0, 1000)
        self.progress.setValue(0)
        self.progress.setMaximumWidth(280)
        bottom.addWidget(self.start_button)
        bottom.addWidget(self.cancel_button)
        bottom.addWidget(self.status_label, 1)
        bottom.addWidget(self.progress)
        root_layout.addLayout(bottom)

    def _build_controls(self) -> QWidget:
        panel = QWidget()
        layout = QVBoxLayout(panel)
        alignment_box = QGroupBox("Alignment")
        alignment_form = QFormLayout(alignment_box)
        self.reference_combo = QComboBox()
        self.reference_combo.addItems(["FL", "FR", "FC", "LFE", "SL", "SR", "M"])
        self.reference_combo.setCurrentText("FL")
        self.sigma_spin = QDoubleSpinBox()
        self.sigma_spin.setRange(0.1, 50.0)
        self.sigma_spin.setValue(8.0)
        self.ratio_spin = QDoubleSpinBox()
        self.ratio_spin.setRange(0.0, 0.95)
        self.ratio_spin.setSingleStep(0.01)
        self.ratio_spin.setValue(0.05)
        alignment_form.addRow("Reference", self.reference_combo)
        alignment_form.addRow("Noise sigma", self.sigma_spin)
        alignment_form.addRow("Peak ratio", self.ratio_spin)
        layout.addWidget(alignment_box)

        analysis_box = QGroupBox("FFT analysis")
        analysis_form = QFormLayout(analysis_box)
        self.frame_combo = QComboBox()
        for value in (2048, 4096, 8192, 16384):
            self.frame_combo.addItem(str(value), value)
        self.frame_combo.setCurrentIndex(1)
        self.overlap_combo = QComboBox()
        for value in (0.0, 0.25, 0.5, 0.75):
            self.overlap_combo.addItem(f"{value:.2f}", value)
        self.overlap_combo.setCurrentIndex(2)
        self.window_combo = QComboBox()
        self.window_combo.addItems(["hann", "hamming", "blackman", "rectangular"])
        self.band_combo = QComboBox()
        self.band_combo.addItem("1/3 octave", 3)
        self.band_combo.addItem("1/1 octave", 1)
        analysis_form.addRow("Frame size", self.frame_combo)
        analysis_form.addRow("Overlap", self.overlap_combo)
        analysis_form.addRow("Window", self.window_combo)
        analysis_form.addRow("Bands", self.band_combo)
        layout.addWidget(analysis_box)

        design_box = QGroupBox("Smoothing and filter")
        design_form = QFormLayout(design_box)
        self.smoothing_spin = QDoubleSpinBox()
        self.smoothing_spin.setRange(0.01, 3.0)
        self.smoothing_spin.setSingleStep(0.05)
        self.smoothing_spin.setValue(0.25)
        self.target_combo = QComboBox()
        for name in TARGET_PRESETS:
            self.target_combo.addItem(name, name)
        self.taps_combo = QComboBox()
        for taps in (511, 1025, 2049, 4097, 8193):
            self.taps_combo.addItem(str(taps), taps)
        self.taps_combo.setCurrentIndex(2)
        self.error_threshold_spin = QDoubleSpinBox()
        self.error_threshold_spin.setRange(0.1, 60.0)
        self.error_threshold_spin.setSingleStep(0.5)
        self.error_threshold_spin.setValue(1.0)
        self.channel_combo = QComboBox()
        self.channel_combo.addItems(["FL", "FR", "FC", "LFE", "SL", "SR"])
        self.channel_combo.currentTextChanged.connect(self._refresh_plots)
        design_form.addRow("Smoothing oct", self.smoothing_spin)
        design_form.addRow("Target curve", self.target_combo)
        design_form.addRow("Filter taps", self.taps_combo)
        design_form.addRow("Error budget", self.error_threshold_spin)
        design_form.addRow("Plot channel", self.channel_combo)
        layout.addWidget(design_box)
        layout.addStretch(1)
        return panel

    @staticmethod
    def _new_table(headers: list[str]) -> QTableWidget:
        table = QTableWidget(0, len(headers))
        table.setHorizontalHeaderLabels(headers)
        table.setEditTriggers(QTableWidget.NoEditTriggers)
        table.setSelectionBehavior(QTableWidget.SelectRows)
        return table

    def _browse(self) -> None:
        filename, _ = QFileDialog.getOpenFileName(
            self, "Open measurement", "samples", "WAV files (*.wav)"
        )
        if filename:
            self.path_edit.setText(filename)

    def _config(self) -> CalibrationConfig:
        target_name = str(self.target_combo.currentData())
        return CalibrationConfig(
            reference_channel=self.reference_combo.currentText(),
            threshold_sigma=self.sigma_spin.value(),
            threshold_peak_ratio=self.ratio_spin.value(),
            frame_size=int(self.frame_combo.currentData()),
            overlap=float(self.overlap_combo.currentData()),
            window=self.window_combo.currentText(),
            band_fraction=int(self.band_combo.currentData()),
            smoothing_octaves=self.smoothing_spin.value(),
            target_curve=TARGET_PRESETS[target_name],
            filter_taps=int(self.taps_combo.currentData()),
            error_threshold_db=self.error_threshold_spin.value(),
        )

    def start_recompute(self) -> None:
        try:
            self.coordinator.submit(self.path_edit.text(), self._config())
        except CalibrationError as exc:
            self.status_label.setText(str(exc))
            QMessageBox.warning(self, "Recompute rejected", str(exc))
            return
        self.start_button.setEnabled(False)
        self.cancel_button.setEnabled(True)
        self.status_label.setText("Background recompute running; publishing only on completion")

    def cancel_recompute(self) -> None:
        if self.coordinator.cancel():
            self.status_label.setText("Cancellation requested; last committed version retained")

    def _poll_job(self) -> None:
        stage, fraction = self.coordinator.progress
        if self.coordinator.is_running:
            self.progress.setValue(int(max(0.0, min(1.0, fraction)) * 1000))
            self.status_label.setText(f"Running: {stage}")
            return
        completion = self.coordinator.take_completion()
        if completion is None:
            if self.start_button.isEnabled() is False and not self.coordinator.is_running:
                self.start_button.setEnabled(True)
                self.cancel_button.setEnabled(False)
            return
        self.start_button.setEnabled(True)
        self.cancel_button.setEnabled(False)
        if completion.status == "committed" and completion.result is not None:
            self.current_result = completion.result
            self._display_result(completion.result)
            summary = completion.result.cache_summary
            self.status_label.setText(
                f"Committed v{completion.result.version} | "
                f"cache {summary['hits']} hit / {summary['misses']} miss | "
                f"{completion.result.consistency['warning_count']} warnings"
            )
            self.progress.setValue(1000)
        else:
            code = completion.error.code if completion.error else "E_INTERNAL"
            message = str(completion.error) if completion.error else "unknown error"
            self.status_label.setText(
                f"{code}: last committed result retained"
                + (f" | {self.current_result.version}" if self.current_result else "")
            )
            if code != "E_CANCELLED":
                QMessageBox.warning(self, "Processing failed", message)

    def _display_result(self, result: ProcessResult) -> None:
        self.alignment_table.setRowCount(len(result.channel_names))
        for row, channel in enumerate(result.channel_names):
            alignment = result.alignments[channel]
            values = [
                channel,
                str(alignment["arrival_sample"]),
                str(alignment["offset_samples"]),
                "n/a"
                if alignment.get("offset_delta_samples") is None
                else str(alignment["offset_delta_samples"]),
                f"{alignment['threshold']:.8g}",
                f"{alignment['normalization_gain']:.5f}",
            ]
            for column, value in enumerate(values):
                self.alignment_table.setItem(row, column, QTableWidgetItem(value))
        self.alignment_table.resizeColumnsToContents()

        warnings = result.consistency["warnings"]
        self.warning_table.setRowCount(len(warnings))
        for row, warning in enumerate(warnings):
            value = ""
            if "phase_delta_deg" in warning:
                value = f"{warning['phase_delta_deg']:.1f} deg"
            elif "jump_db" in warning:
                value = f"{warning['jump_db']:.2f} dB"
            elif "value_db" in warning:
                value = f"{warning['value_db']:.2f} dB"
            interval = (
                warning["band_start"]
                if warning["band_start"] == warning["band_end"]
                else f"{warning['band_start']} - {warning['band_end']}"
            )
            evidence = str(warning.get("evidence", {}))
            values = [
                warning["kind"],
                warning["channel"],
                interval,
                value,
                str(warning.get("source", "")),
                evidence,
            ]
            for column, value_text in enumerate(values):
                self.warning_table.setItem(row, column, QTableWidgetItem(value_text))
        self.warning_table.resizeColumnsToContents()
        self._refresh_plots()
        self._display_diff(result)

    def _refresh_plots(self) -> None:
        result = self.current_result
        if result is None:
            return
        channels = result.channel_names
        centers = np.asarray([band["center_hz"] for band in result.bands])
        series = [
            {
                "label": channel,
                "x": centers,
                "y": np.asarray(result.analysis[channel]["power_db"]),
                "color": CHANNEL_COLORS.get(channel, "#0f172a"),
            }
            for channel in channels
        ]
        selected = self.channel_combo.currentText()
        if selected not in channels:
            selected = channels[0]
        selected_db = np.asarray(result.analysis[selected]["power_db"])
        selected_error = np.asarray(
            [item["total_error_db"] for item in result.errors[selected]]
        )
        self.energy_plot.set_series(
            series,
            [
                {
                    "x": centers,
                    "low": selected_db,
                    "high": selected_db + selected_error,
                    "color": CHANNEL_COLORS.get(selected, "#64748b"),
                }
            ],
        )
        error_series = [
            {
                "label": channel,
                "x": centers,
                "y": np.asarray(
                    [item["total_error_db"] for item in result.errors[channel]]
                ),
                "color": CHANNEL_COLORS.get(channel, "#0f172a"),
            }
            for channel in channels
        ]
        self.error_plot.set_series(error_series)
        self.filter_plot.set_series(
            [
                {
                    "label": channel,
                    "x": np.asarray(result.filters[channel]["response_frequencies_hz"]),
                    "y": np.asarray(result.filters[channel]["response_db"]),
                    "color": CHANNEL_COLORS.get(channel, "#0f172a"),
                }
                for channel in channels
            ]
        )

    def _display_diff(self, result: ProcessResult) -> None:
        diff = result.diff
        lines = [
            f"Version {result.version} content hash: {result.content_hash}",
            f"Base version: {diff.get('base_version')}",
            f"Content changed: {diff.get('content_hash_changed')}",
            f"Affected nodes: {len(result.affected_nodes)}",
            f"Cache: {result.cache_summary}",
            "",
            f"Config changes: {list(diff.get('config_changes', {}))}",
            f"Alignment offset changes: {diff.get('alignment_offsets', [])}",
            f"Band energy changes: {len(diff.get('band_energy_db', []))}",
            f"Filter coefficient changes: {diff.get('filter_coefficients', [])}",
            f"Error status changes: {len(diff.get('error_status', []))}",
            f"Warnings added/removed: "
            f"{len(diff.get('warnings_added', []))}/{len(diff.get('warnings_removed', []))}",
        ]
        self.diff_view.setPlainText("\n".join(lines))


def launch_gui(wav_path: str, cache_dir: str) -> int:
    if not os.environ.get("DISPLAY") and not os.environ.get("WAYLAND_DISPLAY"):
        os.environ.setdefault("QT_QPA_PLATFORM", "offscreen")
        os.environ.setdefault("QT_OPENGL", "software")
    app = QApplication.instance() or QApplication([])
    window = WorkbenchWindow(wav_path, cache_dir)
    window.show()
    return int(app.exec())


def main() -> int:
    return launch_gui("samples/surround51.wav", "cache")


if __name__ == "__main__":
    raise SystemExit(main())

