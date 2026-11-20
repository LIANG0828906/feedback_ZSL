"""PySide6 loudness meter driven by a deterministic virtual transport."""

from __future__ import annotations

import math
import sys
import tempfile
from pathlib import Path
from typing import Any

from PySide6.QtCore import QPointF, Qt, QThread, QTimer, Signal
from PySide6.QtGui import QColor, QFont, QPainter, QPainterPath, QPen
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
    QPushButton,
    QSlider,
    QSpinBox,
    QVBoxLayout,
    QWidget,
)

from .errors import LoudnessError
from .params import AnalysisParameters, downmix_51_to_stereo
from .report import save_report
from .session import MeterSession, MeterSnapshot, SessionManager
from .synth import generate_sample_file
from .wavio import AudioBuffer, read_wav


class LoudnessPlot(QWidget):
    """All series are replaced as one snapshot; versions are never mixed."""

    def __init__(self, parent: QWidget | None = None) -> None:
        super().__init__(parent)
        self.setMinimumSize(640, 300)
        self._snapshot: MeterSnapshot | None = None
        self._version = -1
        self._current_index = 0

    def set_view(self, snapshot: MeterSnapshot, current_index: int) -> None:
        if snapshot.version != self._version:
            self._snapshot = snapshot
            self._version = snapshot.version
        elif self._snapshot is not snapshot:
            self._snapshot = snapshot
        self._current_index = current_index
        self.update()

    def paintEvent(self, event: Any) -> None:
        del event
        painter = QPainter(self)
        painter.setRenderHint(QPainter.Antialiasing, True)
        painter.fillRect(self.rect(), QColor("#10161d"))
        margins = (54, 24, 20, 36)
        left, top, right, bottom = margins
        plot = self.rect().adjusted(left, top, -right, -bottom)
        painter.setPen(QPen(QColor("#42505e"), 1))
        painter.drawRect(plot)
        painter.setFont(QFont("Sans", 9))
        painter.setPen(QColor("#9fb0c0"))
        for loudness in range(-60, 1, 10):
            y = plot.bottom() - (loudness + 60) / 60.0 * plot.height()
            painter.drawLine(plot.left(), int(y), plot.right(), int(y))
            painter.drawText(4, int(y) + 4, f"{loudness:+d}")
        painter.drawText(plot.left(), plot.bottom() + 24, "LUFS / observation block")
        snapshot = self._snapshot
        if snapshot is None or not snapshot.blocks:
            painter.setPen(QColor("#c9d5df"))
            painter.drawText(plot, Qt.AlignCenter, "Waiting for committed blocks")
            return
        count = max(1, len(snapshot.blocks) - 1)

        def x_for(block_index: int) -> float:
            return plot.left() + block_index / count * plot.width()

        def y_for(value: float | None) -> float | None:
            if value is None or not math.isfinite(value):
                return None
            clipped = min(0.0, max(-60.0, value))
            return plot.bottom() - (clipped + 60.0) / 60.0 * plot.height()

        target_y = y_for(snapshot.params.target_lufs)
        if target_y is not None:
            pen = QPen(QColor("#e6b94a"), 1, Qt.DashLine)
            painter.setPen(pen)
            painter.drawLine(
                QPointF(plot.left(), target_y), QPointF(plot.right(), target_y)
            )

        self._draw_series(
            painter,
            plot,
            snapshot,
            "short_lufs",
            QColor("#43b7ff"),
            x_for,
            y_for,
        )
        self._draw_series(
            painter,
            plot,
            snapshot,
            "integrated_lufs",
            QColor("#ff9f43"),
            x_for,
            y_for,
        )
        marker_x = x_for(min(max(self._current_index, 0), len(snapshot.blocks) - 1))
        painter.setPen(QPen(QColor("#d7e3ec"), 1, Qt.DotLine))
        painter.drawLine(
            QPointF(marker_x, plot.top()), QPointF(marker_x, plot.bottom())
        )
        painter.setPen(QColor("#43b7ff"))
        painter.drawText(plot.left() + 8, plot.top() + 16, "Short-term 3 s")
        painter.setPen(QColor("#ff9f43"))
        painter.drawText(plot.left() + 130, plot.top() + 16, "Integrated (prefix)")
        painter.setPen(QColor("#e6b94a"))
        painter.drawText(plot.left() + 290, plot.top() + 16, "Target")

    @staticmethod
    def _draw_series(
        painter: QPainter,
        plot: Any,
        snapshot: MeterSnapshot,
        field: str,
        color: QColor,
        x_for: Any,
        y_for: Any,
    ) -> None:
        path = QPainterPath()
        active = False
        count = max(1, len(snapshot.blocks) - 1)
        for block in snapshot.blocks:
            y = y_for(getattr(block, field))
            if y is None:
                active = False
                continue
            x = x_for(min(block.index, count))
            if not active:
                path.moveTo(x, y)
                active = True
            else:
                path.lineTo(x, y)
        painter.setPen(QPen(color, 2))
        painter.drawPath(path)


class ParameterWorker(QThread):
    completed = Signal(object)
    failed = Signal(str)

    def __init__(
        self,
        session: MeterSession,
        updates: dict[str, Any],
        parent: QWidget | None = None,
    ) -> None:
        super().__init__(parent)
        self.session = session
        self.updates = updates

    def run(self) -> None:
        try:
            self.completed.emit(self.session.change_parameters(**self.updates))
        except Exception as exc:  # noqa: BLE001 - QThread must surface all failures
            self.failed.emit(str(exc))


class MeterWindow(QMainWindow):
    def __init__(self, refresh_ms: int = 100) -> None:
        super().__init__()
        self.setWindowTitle("Broadcast Loudness Meter / Offline-identical")
        self.resize(1120, 760)
        self.manager = SessionManager()
        self.session: MeterSession | None = None
        self.audio: AudioBuffer | None = None
        self._parameter_worker: ParameterWorker | None = None
        self._play_quantum = 960
        self._build_ui(refresh_ms)
        self._play_timer = QTimer(self)
        self._play_timer.setInterval(20)
        self._play_timer.timeout.connect(self._advance_transport)
        self._refresh_timer = QTimer(self)
        self._refresh_timer.timeout.connect(self._refresh_view)
        self._refresh_timer.start(refresh_ms)

    def _build_ui(self, refresh_ms: int) -> None:
        central = QWidget()
        self.setCentralWidget(central)
        root = QVBoxLayout(central)
        controls = QHBoxLayout()
        self.open_button = QPushButton("Open WAV")
        self.open_button.clicked.connect(self._choose_file)
        self.generate_button = QPushButton("Generate deterministic 5.1 sample")
        self.generate_button.clicked.connect(self._generate_sample)
        self.play_button = QPushButton("Play")
        self.play_button.clicked.connect(self._toggle_play)
        self.restart_button = QPushButton("Restart")
        self.restart_button.clicked.connect(self._restart)
        self.save_report_button = QPushButton("Save report")
        self.save_report_button.clicked.connect(self._save_report)
        for widget in (
            self.open_button,
            self.generate_button,
            self.play_button,
            self.restart_button,
            self.save_report_button,
        ):
            controls.addWidget(widget)
        controls.addStretch(1)
        root.addLayout(controls)

        self.plot = LoudnessPlot()
        root.addWidget(self.plot, 1)
        self.seek = QSlider(Qt.Horizontal)
        self.seek.setRange(0, 1)
        self.seek.sliderMoved.connect(self._seek_preview)
        self.seek.sliderReleased.connect(self._seek_released)
        root.addWidget(self.seek)

        status = QGridLayout()
        self.position_label = QLabel("Position: --")
        self.block_label = QLabel("Current block: --")
        self.committed_label = QLabel("Committed blocks: --")
        self.peak_label = QLabel("True-peak position: --")
        self.version_label = QLabel("Snapshot / params: --")
        self.context_label = QLabel("Source: --")
        for index, label in enumerate(
            (
                self.position_label,
                self.block_label,
                self.committed_label,
                self.peak_label,
                self.version_label,
            )
        ):
            status.addWidget(label, index // 3, index % 3)
        status.addWidget(self.context_label, 2, 0, 1, 3)
        root.addLayout(status)

        parameter_box = QGroupBox("Parameters (atomic snapshot replacement)")
        form = QFormLayout(parameter_box)
        self.target_spin = self._double_spin(-60.0, 0.0, -23.0, " LUFS")
        self.peak_limit_spin = self._double_spin(-30.0, 0.0, -1.0, " dBTP")
        self.absolute_gate_spin = self._double_spin(-100.0, -1.0, -70.0, " LUFS")
        self.relative_gate_spin = self._double_spin(-30.0, 0.0, -10.0, " LU")
        self.downmix_combo = QComboBox()
        self.downmix_combo.addItems(("identity", "5.1 -> stereo"))
        self.weights_edit = QLineEdit()
        self.weights_edit.setPlaceholderText(
            "Blank uses the BS.1770 weights for the current layout"
        )
        self.refresh_spin = QSpinBox()
        self.refresh_spin.setRange(20, 2000)
        self.refresh_spin.setSingleStep(20)
        self.refresh_spin.setValue(refresh_ms)
        self.refresh_spin.setSuffix(" ms")
        self.refresh_spin.valueChanged.connect(self._set_refresh_period)
        self.apply_button = QPushButton("Apply parameters")
        self.apply_button.clicked.connect(self._apply_parameters)
        form.addRow("Target loudness", self.target_spin)
        form.addRow("True-peak limit", self.peak_limit_spin)
        form.addRow("Absolute gate", self.absolute_gate_spin)
        form.addRow("Relative gate", self.relative_gate_spin)
        form.addRow("Downmix rule", self.downmix_combo)
        form.addRow("Channel weights", self.weights_edit)
        form.addRow("Refresh period", self.refresh_spin)
        form.addRow("", self.apply_button)
        root.addWidget(parameter_box)
        self._set_enabled(False)

    @staticmethod
    def _double_spin(
        minimum: float,
        maximum: float,
        value: float,
        suffix: str,
    ) -> QDoubleSpinBox:
        spin = QDoubleSpinBox()
        spin.setRange(minimum, maximum)
        spin.setDecimals(2)
        spin.setSingleStep(0.1)
        spin.setValue(value)
        spin.setSuffix(suffix)
        return spin

    def _set_enabled(self, enabled: bool) -> None:
        for widget in (
            self.play_button,
            self.restart_button,
            self.save_report_button,
            self.seek,
            self.apply_button,
            self.target_spin,
            self.peak_limit_spin,
            self.absolute_gate_spin,
            self.relative_gate_spin,
            self.downmix_combo,
            self.weights_edit,
        ):
            widget.setEnabled(enabled)

    def load_audio(self, path: str | Path) -> None:
        try:
            audio = read_wav(path)
            if self.session is not None:
                self.session.close()
            params = AnalysisParameters.for_channels(audio.channels)
            session = self.manager.open_session(audio, params)
        except LoudnessError as exc:
            QMessageBox.critical(self, "Cannot load material", str(exc))
            return
        self.audio = audio
        self.session = session
        self.seek.setRange(0, audio.frame_count)
        self.seek.setValue(0)
        self.context_label.setText(
            f"Source: {audio.filename} | {audio.channels} ch | "
            f"{audio.bit_depth}-bit | 48 kHz | {audio.frame_count / 48000:.3f} s"
        )
        self._sync_parameter_controls(params)
        self._set_enabled(True)
        self._refresh_view()

    def _sync_parameter_controls(self, params: AnalysisParameters) -> None:
        self.target_spin.setValue(params.target_lufs)
        self.peak_limit_spin.setValue(params.true_peak_limit_dbtp)
        self.absolute_gate_spin.setValue(params.absolute_gate_lufs)
        self.relative_gate_spin.setValue(params.relative_gate_lu)
        is_stereo_preset = (
            self.audio is not None
            and self.audio.channels == 6
            and len(params.downmix) == 2
        )
        self.downmix_combo.setCurrentIndex(1 if is_stereo_preset else 0)
        self.weights_edit.setText(",".join(f"{value:g}" for value in params.weights))

    def _choose_file(self) -> None:
        path, _ = QFileDialog.getOpenFileName(
            self,
            "Open 48 kHz PCM WAV",
            "",
            "WAV files (*.wav *.WAV)",
        )
        if path:
            self.load_audio(path)

    def _generate_sample(self) -> None:
        path = Path(tempfile.gettempdir()) / "broadcast_loudness_gui_sample.wav"
        try:
            generate_sample_file(
                path,
                seed=20261120,
                channels=6,
                duration_seconds=12.0,
                bit_depth=24,
            )
        except LoudnessError as exc:
            QMessageBox.critical(self, "Generation failed", str(exc))
            return
        self.load_audio(path)

    def _toggle_play(self) -> None:
        if self.session is None:
            return
        if self.session.playing:
            self.session.pause()
            self._play_timer.stop()
            self.play_button.setText("Play")
        else:
            self.session.play()
            self._play_timer.start()
            self.play_button.setText("Pause")
        self._refresh_view()

    def _advance_transport(self) -> None:
        if self.session is None:
            return
        try:
            self.session.advance(self._play_quantum)
        except LoudnessError as exc:
            self._play_timer.stop()
            QMessageBox.critical(self, "Playback failed", str(exc))
            return
        if not self.session.playing:
            self._play_timer.stop()
            self.play_button.setText("Play")

    def _restart(self) -> None:
        if self.session is None:
            return
        self.session.restart()
        self._refresh_view()

    def _seek_preview(self, value: int) -> None:
        self.position_label.setText(
            f"Position: {value / 48000.0:8.3f} s / {value} samples"
        )

    def _seek_released(self) -> None:
        if self.session is None:
            return
        self.session.seek(self.seek.value())
        self._refresh_view()

    def _set_refresh_period(self, milliseconds: int) -> None:
        self._refresh_timer.setInterval(milliseconds)

    def _apply_parameters(self) -> None:
        if self.session is None:
            return
        updates: dict[str, Any] = {
            "target_lufs": self.target_spin.value(),
            "true_peak_limit_dbtp": self.peak_limit_spin.value(),
            "absolute_gate_lufs": self.absolute_gate_spin.value(),
            "relative_gate_lu": self.relative_gate_spin.value(),
        }
        try:
            if self.audio is not None and self.downmix_combo.currentIndex() == 1:
                if self.audio.channels != 6:
                    raise ValueError("5.1 -> stereo requires 6 input channels")
                updates["downmix"] = downmix_51_to_stereo()
                updates["output_layout"] = ("L", "R")
                default_weights = (1.0, 1.0)
            else:
                base = AnalysisParameters.for_channels(self.audio.channels)
                updates["downmix"] = base.downmix
                updates["output_layout"] = base.output_layout
                default_weights = base.weights
            text = self.weights_edit.text().strip()
            weights = (
                tuple(float(value) for value in text.split(","))
                if text
                else default_weights
            )
            if len(weights) != len(updates["downmix"]):
                raise ValueError("Weight count must match downmix outputs")
            updates["weights"] = weights
        except ValueError as exc:
            QMessageBox.warning(self, "Invalid parameters", str(exc))
            return
        self.apply_button.setEnabled(False)
        self.apply_button.setText("Recomputing (old snapshot retained)")
        worker = ParameterWorker(self.session, updates, self)
        worker.completed.connect(self._parameter_ready)
        worker.failed.connect(self._parameter_failed)
        worker.finished.connect(worker.deleteLater)
        self._parameter_worker = worker
        worker.start()

    def _parameter_ready(self, snapshot: MeterSnapshot) -> None:
        self.apply_button.setEnabled(True)
        self.apply_button.setText("Apply parameters")
        self._parameter_worker = None
        self._refresh_view()
        self.statusBar().showMessage(
            f"Parameter version {snapshot.parameter_version} published; "
            f"affected blocks {len(snapshot.affected_blocks)}",
            5000,
        )

    def _parameter_failed(self, message: str) -> None:
        self.apply_button.setEnabled(True)
        self.apply_button.setText("Apply parameters")
        self._parameter_worker = None
        QMessageBox.warning(self, "Parameter recomputation failed", message)

    def _refresh_view(self) -> None:
        if self.session is None:
            return
        view = self.session.view()
        snapshot = view.snapshot
        position = view.playback_position_sample
        self.position_label.setText(
            f"Position: {position / 48000.0:8.3f} s / {position} samples"
        )
        self.block_label.setText(f"Current block: {view.current_block_index}")
        self.committed_label.setText(f"Committed blocks: {snapshot.committed_count}")
        peak_position = snapshot.true_peak_position_sample
        self.peak_label.setText(
            "True-peak position: --"
            if peak_position is None
            else f"True-peak position: {peak_position / 48000.0:.6f} s"
        )
        self.version_label.setText(
            f"Snapshot / params: {snapshot.version} / {snapshot.parameter_version} | "
            f"I={_format_lufs(snapshot.integrated_lufs)} | "
            f"TP={_format_dbtp(snapshot.true_peak_dbtp)}"
        )
        self.seek.blockSignals(True)
        self.seek.setValue(position)
        self.seek.blockSignals(False)
        self.plot.set_view(snapshot, view.current_block_index)

    def _save_report(self) -> None:
        if self.session is None or self.audio is None:
            return
        path, _ = QFileDialog.getSaveFileName(
            self,
            "Save analysis report",
            "loudness-report.json",
            "JSON (*.json)",
        )
        if not path:
            return
        try:
            digest = save_report(path, self.audio, self.session.snapshot())
        except (OSError, LoudnessError) as exc:
            QMessageBox.critical(self, "Save failed", str(exc))
            return
        self.statusBar().showMessage(f"Report atomically written: {digest}", 8000)

    def closeEvent(self, event: Any) -> None:
        self._play_timer.stop()
        self._refresh_timer.stop()
        if self._parameter_worker is not None:
            self._parameter_worker.wait(5000)
        if self.session is not None:
            self.session.close()
        event.accept()


def _format_lufs(value: float | None) -> str:
    return "--" if value is None else f"{value:.2f} LUFS"


def _format_dbtp(value: float | None) -> str:
    return "--" if value is None else f"{value:.2f} dBTP"


def run_gui(
    *,
    input_path: str | None = None,
    refresh_ms: int = 100,
) -> int:
    app = QApplication.instance() or QApplication(sys.argv)
    app.setApplicationName("Broadcast Loudness Meter")
    windows: list[MeterWindow] = []

    def keep_alive(window: MeterWindow) -> None:
        windows.append(window)

    window = MeterWindow(refresh_ms=refresh_ms)
    keep_alive(window)
    window.show()
    if input_path:
        window.load_audio(input_path)
    return app.exec()
