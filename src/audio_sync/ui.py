"""PySide6 diagnostic workbench for multi-channel clock-drift analysis."""

from __future__ import annotations

import argparse
import json
import math
import sys
from pathlib import Path

import numpy as np
from PySide6.QtCore import QTimer, Qt
from PySide6.QtGui import QColor, QFont, QPainter, QPen
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
    QLineEdit,
    QMainWindow,
    QMessageBox,
    QPlainTextEdit,
    QProgressBar,
    QPushButton,
    QSpinBox,
    QSplitter,
    QTableWidget,
    QTableWidgetItem,
    QTabWidget,
    QVBoxLayout,
    QWidget,
)

from .errors import SyncError
from .models import SyncConfig
from .service import JobState, SyncRequest, SyncService

PALETTE = [
    "#2979ff",
    "#ff7043",
    "#43a047",
    "#ab47bc",
    "#ffb300",
    "#00acc1",
    "#ec407a",
    "#8d6e63",
    "#5e35b1",
    "#7cb342",
]


class LineChartWidget(QWidget):
    def __init__(self, title: str, y_label: str, parent=None):
        super().__init__(parent)
        self.title = title
        self.y_label = y_label
        self.series: list[dict] = []
        self.setMinimumHeight(250)

    def set_series(self, series: list[dict]) -> None:
        self.series = series
        self.update()

    def paintEvent(self, event) -> None:
        painter = QPainter(self)
        painter.setRenderHint(QPainter.Antialiasing)
        painter.fillRect(self.rect(), QColor("#fbfcfe"))
        painter.setPen(QColor("#202735"))
        title_font = QFont()
        title_font.setBold(True)
        painter.setFont(title_font)
        painter.drawText(14, 22, self.title)
        painter.setFont(QFont())

        bounds = self.rect().adjusted(58, 42, -18, -46)
        painter.setPen(QPen(QColor("#c9d1dc"), 1))
        painter.drawRect(bounds)
        valid = [
            (x, y)
            for item in self.series
            for x, y in zip(item.get("x", []), item.get("y", []))
            if x is not None and y is not None and math.isfinite(float(y))
        ]
        if not valid:
            painter.setPen(QColor("#7a8494"))
            painter.drawText(bounds, Qt.AlignCenter, "No diagnostic data")
            return

        xs = [float(x) for x, _ in valid]
        ys = [float(y) for _, y in valid]
        x_min, x_max = min(xs), max(xs)
        y_min, y_max = min(ys), max(ys)
        if x_max <= x_min:
            x_max = x_min + 1.0
        if y_max <= y_min:
            pad = max(0.5, abs(y_min) * 0.05)
            y_min -= pad
            y_max += pad
        else:
            pad = (y_max - y_min) * 0.08
            y_min -= pad
            y_max += pad

        for step in range(5):
            fraction = step / 4.0
            y = bounds.bottom() - fraction * bounds.height()
            painter.setPen(QPen(QColor("#e6ebf2"), 1))
            painter.drawLine(bounds.left(), int(y), bounds.right(), int(y))
            value = y_min + fraction * (y_max - y_min)
            painter.setPen(QColor("#697386"))
            painter.drawText(4, int(y + 4), f"{value:.3g}")

        def map_point(x: float, y: float) -> tuple[float, float]:
            px = bounds.left() + (x - x_min) / (x_max - x_min) * bounds.width()
            py = bounds.bottom() - (y - y_min) / (y_max - y_min) * bounds.height()
            return px, py

        for index, item in enumerate(self.series):
            color = QColor(item.get("color", PALETTE[index % len(PALETTE)]))
            points = [
                map_point(float(x), float(y))
                for x, y in zip(item.get("x", []), item.get("y", []))
                if x is not None and y is not None and math.isfinite(float(y))
            ]
            if not points:
                continue
            mode = item.get("mode", "line")
            if mode == "points":
                painter.setPen(Qt.NoPen)
                painter.setBrush(color)
                for x, y in points:
                    painter.drawEllipse(int(x) - 2, int(y) - 2, 5, 5)
            else:
                pen = QPen(color, 2)
                if mode == "dashed":
                    pen.setStyle(Qt.DashLine)
                painter.setPen(pen)
                painter.setBrush(Qt.NoBrush)
                for first, second in zip(points, points[1:]):
                    painter.drawLine(int(first[0]), int(first[1]), int(second[0]), int(second[1]))
            legend_index = sum(
                1
                for previous in self.series[:index]
                if previous.get("mode", "line") != "points"
            )
            if mode != "points" and legend_index < 10:
                painter.setPen(color)
                painter.drawText(
                    bounds.left() + 10,
                    bounds.top() + 18 + legend_index * 16,
                    item.get("label", f"series {index}"),
                )

        painter.setPen(QColor("#697386"))
        painter.drawText(bounds.left(), bounds.bottom() + 20, f"{x_min:.4g}")
        painter.drawText(bounds.right() - 45, bounds.bottom() + 20, f"{x_max:.4g}")
        painter.drawText(
            bounds.center().x() - 30,
            bounds.bottom() + 38,
            "segment / marker index",
        )
        painter.save()
        painter.translate(14, bounds.center().y() + 30)
        painter.rotate(-90)
        painter.drawText(0, 0, self.y_label)
        painter.restore()


class ResidualHeatmapWidget(QWidget):
    def __init__(self, parent=None):
        super().__init__(parent)
        self.channels: list[int] = []
        self.values = np.empty((0, 0), dtype=np.float64)
        self.label = "after residual RMS (samples)"
        self.setMinimumHeight(260)

    def set_data(self, channels: list[int], values: np.ndarray, label: str) -> None:
        self.channels = channels
        self.values = np.asarray(values, dtype=np.float64)
        self.label = label
        self.update()

    def paintEvent(self, event) -> None:
        painter = QPainter(self)
        painter.setRenderHint(QPainter.Antialiasing)
        painter.fillRect(self.rect(), QColor("#fbfcfe"))
        painter.setPen(QColor("#202735"))
        font = QFont()
        font.setBold(True)
        painter.setFont(font)
        painter.drawText(14, 22, self.label)
        painter.setFont(QFont())
        if self.values.size == 0:
            painter.setPen(QColor("#7a8494"))
            painter.drawText(self.rect(), Qt.AlignCenter, "No residual heat data")
            return

        margin_left, margin_top, margin_right, margin_bottom = 68, 44, 22, 54
        width = self.width() - margin_left - margin_right
        height = self.height() - margin_top - margin_bottom
        rows, columns = self.values.shape
        cell_w = width / max(1, columns)
        cell_h = height / max(1, rows)
        finite = self.values[np.isfinite(self.values)]
        maximum = max(1e-9, float(np.max(finite)) if finite.size else 1.0)
        for row in range(rows):
            for column in range(columns):
                value = self.values[row, column]
                if not math.isfinite(float(value)):
                    color = QColor("#eceff3")
                    text = "—"
                else:
                    ratio = min(1.0, max(0.0, float(value) / maximum))
                    color = QColor.fromHsvF(0.62 * (1.0 - ratio), 0.72, 0.96)
                    text = f"{float(value):.2f}"
                rect = (
                    int(margin_left + column * cell_w),
                    int(margin_top + row * cell_h),
                    max(1, int(cell_w) - 1),
                    max(1, int(cell_h) - 1),
                )
                painter.fillRect(*rect, color)
                painter.setPen(QColor("#202735") if finite.size else QColor("#7a8494"))
                painter.drawText(
                    rect[0],
                    rect[1],
                    rect[2],
                    rect[3],
                    Qt.AlignCenter,
                    text,
                )
            painter.setPen(QColor("#4a5364"))
            painter.drawText(
                8,
                int(margin_top + (row + 0.5) * cell_h + 5),
                f"ch {self.channels[row] if row < len(self.channels) else row}",
            )
        painter.setPen(QColor("#4a5364"))
        painter.drawText(margin_left, self.height() - 22, "segment index →")
        painter.drawText(
            margin_left,
            self.height() - 6,
            f"color scale: 0 … {maximum:.3f} samples; gray = no accepted marker",
        )


class MainWindow(QMainWindow):
    def __init__(self):
        super().__init__()
        self.setWindowTitle("8-Channel Clock Drift Workbench")
        self.resize(1320, 920)
        self.service: SyncService | None = None
        self.job: JobState | None = None
        self.snapshot = None
        self._last_status = ""
        self.timer = QTimer(self)
        self.timer.setInterval(180)
        self.timer.timeout.connect(self._poll)
        self._build_ui()
        QTimer.singleShot(0, self._load_committed_if_any)

    def _build_ui(self) -> None:
        central = QWidget()
        self.setCentralWidget(central)
        outer = QVBoxLayout(central)

        files_box = QGroupBox("Source material")
        files_layout = QGridLayout(files_box)
        self.wav_edit = QLineEdit()
        self.markers_edit = QLineEdit()
        self.output_edit = QLineEdit(str(Path.cwd() / "artifacts" / "workbench"))
        for row, (label, edit, dialog) in enumerate(
            [
                ("WAV", self.wav_edit, self._browse_wav),
                ("Markers", self.markers_edit, self._browse_markers),
                ("Output", self.output_edit, self._browse_output),
            ]
        ):
            files_layout.addWidget(QLabel(label), row, 0)
            files_layout.addWidget(edit, row, 1)
            button = QPushButton("Browse")
            button.clicked.connect(dialog)
            files_layout.addWidget(button, row, 2)
        outer.addWidget(files_box)

        controls = QGroupBox("Synchronization parameters")
        control_layout = QHBoxLayout(controls)
        self.segment_spin = QDoubleSpinBox()
        self.segment_spin.setRange(0.25, 3600.0)
        self.segment_spin.setValue(2.0)
        self.segment_spin.setDecimals(2)
        self.reference_spin = QSpinBox()
        self.reference_spin.setRange(0, 255)
        self.reference_spin.setValue(0)
        self.interpolation_combo = QComboBox()
        self.interpolation_combo.addItems(["linear", "windowed_sinc"])
        self.strategy_combo = QComboBox()
        self.strategy_combo.addItems(["auto", "full", "segmented"])
        self.tolerance_spin = QDoubleSpinBox()
        self.tolerance_spin.setRange(0.01, 1000.0)
        self.tolerance_spin.setValue(1.5)
        self.tolerance_spin.setDecimals(2)
        for label, widget in [
            ("Segment seconds", self.segment_spin),
            ("Reference channel", self.reference_spin),
            ("Interpolation", self.interpolation_combo),
            ("Strategy", self.strategy_combo),
            ("Tolerance (samples)", self.tolerance_spin),
        ]:
            box = QVBoxLayout()
            box.addWidget(QLabel(label))
            box.addWidget(widget)
            control_layout.addLayout(box)
        self.start_button = QPushButton("Start synchronization")
        self.start_button.setMinimumHeight(38)
        self.start_button.clicked.connect(self._start)
        self.cancel_button = QPushButton("Cancel")
        self.cancel_button.setEnabled(False)
        self.cancel_button.clicked.connect(self._cancel)
        control_layout.addWidget(self.start_button)
        control_layout.addWidget(self.cancel_button)
        outer.addWidget(controls)

        status_row = QHBoxLayout()
        self.status_label = QLabel("Idle")
        self.progress = QProgressBar()
        self.progress.setRange(0, 1000)
        status_row.addWidget(self.status_label, 1)
        status_row.addWidget(self.progress, 1)
        outer.addLayout(status_row)

        splitter = QSplitter(Qt.Vertical)
        tabs = QTabWidget()
        self.marker_chart = LineChartWidget(
            "Detected marker arrival positions",
            "arrival sample",
        )
        self.delay_chart = LineChartWidget(
            "Segment delay and fitted first-order drift",
            "delay (samples)",
        )
        heatmap_page = QWidget()
        heatmap_layout = QVBoxLayout(heatmap_page)
        heatmap_controls = QHBoxLayout()
        heatmap_controls.addWidget(QLabel("Heat data"))
        self.heatmap_combo = QComboBox()
        self.heatmap_combo.addItems(
            ["after RMS", "after max", "before RMS", "before max"]
        )
        self.heatmap_combo.currentIndexChanged.connect(self._update_heatmap)
        heatmap_controls.addWidget(self.heatmap_combo)
        heatmap_controls.addStretch(1)
        heatmap_layout.addLayout(heatmap_controls)
        self.heatmap_widget = ResidualHeatmapWidget()
        heatmap_layout.addWidget(self.heatmap_widget)
        tabs.addTab(self.marker_chart, "Marker arrivals")
        tabs.addTab(self.delay_chart, "Delay / drift")
        tabs.addTab(heatmap_page, "Residual heatmap")
        splitter.addWidget(tabs)

        bottom = QWidget()
        bottom_layout = QVBoxLayout(bottom)
        self.summary_label = QLabel("No committed result loaded.")
        self.summary_label.setWordWrap(True)
        bottom_layout.addWidget(self.summary_label)
        self.table = QTableWidget(0, 8)
        self.table.setHorizontalHeaderLabels(
            [
                "Channel",
                "Model",
                "Drift ppm",
                "Offset (samples)",
                "Step (samples)",
                "Fit RMS",
                "After max",
                "Tolerance",
            ]
        )
        bottom_layout.addWidget(self.table)
        self.log = QPlainTextEdit()
        self.log.setReadOnly(True)
        self.log.setMaximumBlockCount(800)
        self.log.setPlaceholderText("Job and cache diagnostics")
        bottom_layout.addWidget(self.log)
        splitter.addWidget(bottom)
        splitter.setSizes([560, 320])
        outer.addWidget(splitter, 1)

    def _browse_wav(self) -> None:
        path, _ = QFileDialog.getOpenFileName(
            self,
            "Select multichannel WAV",
            self.wav_edit.text() or str(Path.cwd()),
            "WAV files (*.wav *.WAV);;All files (*)",
        )
        if path:
            self.wav_edit.setText(path)

    def _browse_markers(self) -> None:
        path, _ = QFileDialog.getOpenFileName(
            self,
            "Select marker description",
            self.markers_edit.text() or str(Path.cwd()),
            "JSON files (*.json);;All files (*)",
        )
        if path:
            self.markers_edit.setText(path)

    def _browse_output(self) -> None:
        path = QFileDialog.getExistingDirectory(
            self,
            "Select output root",
            self.output_edit.text() or str(Path.cwd()),
        )
        if path:
            self.output_edit.setText(path)

    def _start(self) -> None:
        if self.job is not None and self.job.status in {"queued", "running", "cancelling"}:
            self._append("A synchronization job is already active.")
            return
        output = self.output_edit.text().strip()
        if not self.wav_edit.text().strip() or not self.markers_edit.text().strip() or not output:
            QMessageBox.warning(self, "Missing paths", "WAV, marker JSON and output root are required.")
            return
        config = SyncConfig(
            reference_channel=self.reference_spin.value(),
            segment_seconds=self.segment_spin.value(),
            interpolation=self.interpolation_combo.currentText(),
            strategy=self.strategy_combo.currentText(),
            tolerance_samples=self.tolerance_spin.value(),
        )
        try:
            if self.service is None or str(self.service.output_root) != str(Path(output).resolve()):
                self.service = SyncService(output)
            self.job = self.service.submit(
                SyncRequest(
                    wav_path=self.wav_edit.text().strip(),
                    markers_path=self.markers_edit.text().strip(),
                    config=config,
                )
            )
        except SyncError as exc:
            self._append(exc.code)
            QMessageBox.warning(self, exc.code, exc.message)
            return
        except Exception as exc:
            QMessageBox.critical(self, "Cannot start", str(exc))
            return
        self._append(f"Submitted job {self.job.job_id[:8]}")
        self.start_button.setEnabled(False)
        self.cancel_button.setEnabled(True)
        self.timer.start()
        self._poll()

    def _cancel(self) -> None:
        if self.service is None or self.job is None:
            return
        try:
            self.job = self.service.cancel(self.job.job_id)
            self._append("Cancellation requested; stopping at the next segment checkpoint.")
        except SyncError as exc:
            self._append(str(exc))

    def _poll(self) -> None:
        if self.service is None or self.job is None:
            self.timer.stop()
            return
        try:
            self.job = self.service.get_job(self.job.job_id)
        except SyncError:
            self.timer.stop()
            return
        self.progress.setValue(int(self.job.progress * 1000))
        status = (
            f"{self.job.status} · {self.job.phase} · "
            f"{self.job.detail} · checkpoint={self.job.checkpoint}"
        )
        if status != self._last_status:
            self.status_label.setText(status)
            self._last_status = status
        if self.job.status in {"succeeded", "failed", "cancelled"}:
            self.timer.stop()
            self.start_button.setEnabled(True)
            self.cancel_button.setEnabled(False)
            if self.job.status == "succeeded":
                self._load_result()
            elif self.job.error:
                self._append(
                    f"{self.job.error.get('code')}: {self.job.error.get('message')}"
                )

    def _load_result(self) -> None:
        if self.service is None or self.job is None:
            return
        try:
            snapshot = self.service.get_result(
                version=self.job.result_version,
                content_hash=self.job.result_hash,
            )
        except SyncError as exc:
            self._append(str(exc))
            return
        self.snapshot = snapshot
        self._apply_snapshot(snapshot)

    def _apply_snapshot(self, snapshot) -> None:
        payload = snapshot.payload
        self.summary_label.setText(
            f"version {snapshot.version} · hash {snapshot.content_hash[:12]} · "
            f"strategy {payload['strategy']['selected']} · "
            f"mode {payload['compensation']['mode']} · "
            f"worst residual {payload['diagnostics']['measured_worst_residual_samples']} samples · "
            f"tolerance {'PASS' if payload['compensation']['tolerance_ok'] else 'FAIL'} · "
            f"cache hits {payload['cache']['stats']['segment_hits']} segments"
        )
        self._update_markers(payload)
        self._update_delay(payload)
        self._update_heatmap()
        self._update_table(payload)
        self._append(
            "Committed result loaded: "
            + json.dumps(payload["cache"]["stats"], sort_keys=True)
        )
        for error in payload.get("errors", []):
            self._append(f"{error['code']}: {error['message']}")

    def _load_committed_if_any(self) -> None:
        try:
            service = SyncService(self.output_edit.text().strip())
            self.service = service
            snapshot = service.store.current
            if snapshot is not None:
                self.snapshot = snapshot
                self._apply_snapshot(snapshot)
                self._append(
                    f"Recovered committed disk report version {snapshot.version}."
                )
        except (OSError, SyncError) as exc:
            self._append(f"No committed report recovered: {exc}")

    def _update_markers(self, payload: dict) -> None:
        series = []
        for index, item in enumerate(payload["marker_detection"]["channels"]):
            accepted = [
                hit for hit in item["markers"] if hit["accepted"]
            ]
            series.append(
                {
                    "label": f"ch {item['channel']}",
                    "color": PALETTE[index % len(PALETTE)],
                    "mode": "points",
                    "x": [hit["marker_index"] for hit in accepted],
                    "y": [hit["arrival_sample"] for hit in accepted],
                }
            )
        self.marker_chart.set_series(series)

    def _update_delay(self, payload: dict) -> None:
        series = []
        signal_count = 0
        for index, item in enumerate(payload["delay_estimation"]["channels"]):
            channel = item["channel"]
            points = [
                segment
                for segment in item["segments"]
                if segment["valid"] and segment["delay_samples"] is not None
            ]
            color = PALETTE[index % len(PALETTE)]
            series.append(
                {
                    "label": f"ch {channel}",
                    "color": color,
                    "mode": "points",
                    "x": [segment["center_sample"] for segment in points],
                    "y": [segment["delay_samples"] for segment in points],
                }
            )
            drift = next(
                (
                    model
                    for model in payload["drift"]["channels"]
                    if model["channel"] == channel
                ),
                None,
            )
            if drift and points:
                xs = np.linspace(
                    points[0]["center_sample"],
                    points[-1]["center_sample"],
                    140,
                )
                offset = drift["offset_samples"]
                ppm = drift["drift_ppm"]
                step = drift["step_samples"]
                step_at = drift["step_sample"]
                ys = offset + ppm * 1e-6 * xs
                if step and step_at is not None:
                    ys = ys + np.where(xs >= step_at, step, 0.0)
                series.append(
                    {
                        "label": f"fit ch {channel}",
                        "color": color,
                        "mode": "dashed",
                        "x": xs.tolist(),
                        "y": ys.tolist(),
                    }
                )
                signal_count += 1
        self.delay_chart.set_series(series)
        self._append(f"Delay chart updated across {signal_count} signal channels.")

    def _update_heatmap(self) -> None:
        if self.snapshot is None:
            return
        payload = self.snapshot.payload
        heatmap_path = Path(payload["outputs"]["residual_heatmap_npz"])
        try:
            with np.load(heatmap_path, allow_pickle=False) as archive:
                key = {
                    0: "after_rms",
                    1: "after_max",
                    2: "before_rms",
                    3: "before_max",
                }[self.heatmap_combo.currentIndex()]
                values = np.asarray(archive[key], dtype=np.float64)
                channels = [int(value) for value in archive["channel_ids"]]
        except (OSError, ValueError, KeyError) as exc:
            self._append(f"Cannot read residual heatmap: {exc}")
            return
        self.heatmap_widget.set_data(
            channels,
            values,
            f"{self.heatmap_combo.currentText()} (samples)",
        )

    def _update_table(self, payload: dict) -> None:
        models = {
            model["channel"]: model for model in payload["drift"]["channels"]
        }
        compensation = {
            item["channel"]: item for item in payload["compensation"]["channels"]
        }
        self.table.setRowCount(len(models))
        for row, (channel, model) in enumerate(sorted(models.items())):
            comp = compensation.get(channel, {})
            after_values = [
                value for value in comp.get("after_max", []) if value is not None
            ]
            values = [
                channel,
                model.get("model_kind", ""),
                f"{model.get('drift_ppm', 0.0):.4f}",
                f"{model.get('offset_samples', 0.0):.3f}",
                "" if not model.get("step_samples") else f"{model['step_samples']:.3f}",
                (
                    ""
                    if model.get("fit_residual_rms") is None
                    else f"{model['fit_residual_rms']:.4f}"
                ),
                f"{max(after_values):.3f}" if after_values else "—",
                "PASS" if comp.get("tolerance_ok") else "FAIL",
            ]
            for column, value in enumerate(values):
                item = QTableWidgetItem(str(value))
                if column == 7:
                    item.setForeground(
                        QColor("#2e7d32") if value == "PASS" else QColor("#c62828")
                    )
                self.table.setItem(row, column, item)
        self.table.resizeColumnsToContents()

    def _append(self, message: str) -> None:
        self.log.appendPlainText(message)

    def closeEvent(self, event) -> None:
        if self.service is not None and self.job is not None:
            if self.job.status in {"queued", "running", "cancelling"}:
                try:
                    self.service.cancel(self.job.job_id)
                    self._append("Cancel requested during window close.")
                except SyncError:
                    pass
        super().closeEvent(event)


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(add_help=True)
    parser.add_argument("--wav", default="")
    parser.add_argument("--markers", default="")
    parser.add_argument("--output", default=str(Path.cwd() / "artifacts" / "workbench"))
    args, qt_args = parser.parse_known_args(argv)
    app = QApplication([sys.argv[0], *qt_args])
    app.setApplicationName("8-Channel Clock Drift Workbench")
    window = MainWindow()
    if args.wav:
        window.wav_edit.setText(args.wav)
    if args.markers:
        window.markers_edit.setText(args.markers)
    window.output_edit.setText(args.output)
    window.show()
    return app.exec()


if __name__ == "__main__":
    sys.exit(main())
