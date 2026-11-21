"""Optional PySide6 replay workbench."""

from __future__ import annotations

import itertools
import json
import threading
import time
from typing import Any

from PySide6.QtCore import QObject, Qt, QThread, Signal, Slot
from PySide6.QtGui import QColor, QPainter, QPen
from PySide6.QtWidgets import (
    QApplication,
    QFileDialog,
    QFormLayout,
    QHBoxLayout,
    QInputDialog,
    QLabel,
    QLineEdit,
    QListWidget,
    QMainWindow,
    QMessageBox,
    QProgressBar,
    QPushButton,
    QSlider,
    QSplitter,
    QTableWidget,
    QTableWidgetItem,
    QTabWidget,
    QVBoxLayout,
    QWidget,
)

from .engine import ReplayEngine
from .errors import ReplayError
from .loader import load_scenario


class ReplayWorker(QObject):
    step_done = Signal(dict)
    finished = Signal(str)
    failed = Signal(dict)

    def __init__(self, engine: ReplayEngine, step_delay_ms: int = 120) -> None:
        super().__init__()
        self.engine = engine
        self.step_delay_ms = max(0, step_delay_ms)
        self._pause_event = threading.Event()
        self._pause_event.set()

    @Slot()
    def run_loop(self) -> None:
        status = "completed"
        try:
            while not self.engine.completed:
                if self.engine.cancel_event.is_set():
                    status = "cancelled"
                    break
                self._pause_event.wait()
                if self.engine.cancel_event.is_set():
                    status = "cancelled"
                    break
                frame = self.engine.step_once()
                self.step_done.emit(frame)
                if self.engine.baseline_diff is not None:
                    status = "baseline_mismatch"
                    break
                if self.step_delay_ms:
                    time.sleep(self.step_delay_ms / 1000)
        except ReplayError as error:
            self.failed.emit(error.to_dict())
            return
        self.finished.emit(status)

    def set_paused(self, paused: bool) -> None:
        if paused:
            self._pause_event.clear()
        else:
            self._pause_event.set()

    def cancel(self) -> None:
        self.engine.cancel()
        self._pause_event.set()


class LedgerChart(QWidget):
    def __init__(self, parent: QWidget | None = None) -> None:
        super().__init__(parent)
        self.frames: list[dict[str, Any]] = []
        self.setMinimumHeight(180)

    def set_frames(self, frames: list[dict[str, Any]]) -> None:
        self.frames = list(frames)
        self.update()

    def paintEvent(self, event: Any) -> None:
        painter = QPainter(self)
        painter.setRenderHint(QPainter.RenderHint.Antialiasing)
        painter.fillRect(self.rect(), QColor("#101820"))
        if not self.frames:
            painter.setPen(QColor("#8fa3b8"))
            painter.drawText(self.rect(), Qt.AlignmentFlag.AlignCenter, "predicted ledger")
            return
        values: dict[str, list[float]] = {}
        for frame in self.frames:
            for item in frame.get("predicted_ledger", []):
                values.setdefault(item["cover_id"], []).append(
                    float(item["predicted_remaining"])
                )
        if not values:
            return
        all_values = [value for series in values.values() for value in series]
        minimum = min(all_values)
        maximum = max(all_values)
        if maximum == minimum:
            maximum = minimum + 1.0
        margin = 18
        width = max(1, self.width() - margin * 2)
        height = max(1, self.height() - margin * 2)
        palette = (
            "#4cc9f0",
            "#f72585",
            "#b5e48c",
            "#ffd166",
            "#a78bfa",
            "#fb8500",
            "#90e0ef",
            "#e76f51",
        )
        for series_index, (cover_id, series) in enumerate(sorted(values.items())):
            painter.setPen(QPen(QColor(palette[series_index % len(palette)]), 1.6))
            points = []
            denominator = max(1, len(series) - 1)
            for index, value in enumerate(series):
                x = margin + width * index / denominator
                y = margin + height * (maximum - value) / (maximum - minimum)
                points.append((x, y))
            for start, end in itertools.pairwise(points):
                painter.drawLine(int(start[0]), int(start[1]), int(end[0]), int(end[1]))
            painter.drawText(int(points[-1][0]) + 3, int(points[-1][1]) - 3, cover_id)


class MainWindow(QMainWindow):
    def __init__(
        self,
        input_path: str | None = None,
        output_path: str | None = None,
        resume: bool = False,
        step_delay_ms: int = 120,
    ) -> None:
        super().__init__()
        self.setWindowTitle("Cover Replay Inspector")
        self.resize(1260, 820)
        self.step_delay_ms = step_delay_ms
        self.engine: ReplayEngine | None = None
        self.worker: ReplayWorker | None = None
        self.thread: QThread | None = None
        self.frames: list[dict[str, Any]] = []
        self.frame_by_step: dict[int, dict[str, Any]] = {}
        self._build_ui()
        if input_path:
            self.input_edit.setText(input_path)
        if output_path:
            self.output_edit.setText(output_path)
        if input_path and output_path:
            self.start_replay(resume=resume)

    def _build_ui(self) -> None:
        root = QWidget()
        self.setCentralWidget(root)
        layout = QVBoxLayout(root)
        form = QFormLayout()
        input_row = QHBoxLayout()
        self.input_edit = QLineEdit()
        browse_input = QPushButton("Input...")
        browse_input.clicked.connect(self._browse_input)
        input_row.addWidget(self.input_edit)
        input_row.addWidget(browse_input)
        form.addRow("Scenario", input_row)
        output_row = QHBoxLayout()
        self.output_edit = QLineEdit()
        browse_output = QPushButton("Output...")
        browse_output.clicked.connect(self._browse_output)
        output_row.addWidget(self.output_edit)
        output_row.addWidget(browse_output)
        form.addRow("New output", output_row)
        layout.addLayout(form)

        controls = QHBoxLayout()
        self.start_button = QPushButton("Start")
        self.start_button.clicked.connect(lambda: self.start_replay(resume=False))
        self.resume_button = QPushButton("Resume")
        self.resume_button.clicked.connect(lambda: self.start_replay(resume=True))
        self.pause_button = QPushButton("Pause")
        self.pause_button.setCheckable(True)
        self.pause_button.toggled.connect(self._toggle_pause)
        self.revoke_button = QPushButton("Inject revoke")
        self.revoke_button.clicked.connect(self._inject_revoke)
        self.cancel_button = QPushButton("Cancel")
        self.cancel_button.clicked.connect(self._cancel)
        for button in (
            self.start_button,
            self.resume_button,
            self.pause_button,
            self.revoke_button,
            self.cancel_button,
        ):
            controls.addWidget(button)
        controls.addStretch(1)
        self.status_label = QLabel("idle")
        controls.addWidget(self.status_label)
        layout.addLayout(controls)

        self.progress = QProgressBar()
        self.progress.setFormat("step %v / %m")
        layout.addWidget(self.progress)
        timeline_row = QHBoxLayout()
        timeline_row.addWidget(QLabel("Timeline"))
        self.timeline = QSlider(Qt.Orientation.Horizontal)
        self.timeline.setEnabled(False)
        self.timeline.valueChanged.connect(self._show_step)
        timeline_row.addWidget(self.timeline, 1)
        self.step_label = QLabel("-")
        timeline_row.addWidget(self.step_label)
        layout.addLayout(timeline_row)

        splitter = QSplitter(Qt.Orientation.Horizontal)
        self.cover_table = QTableWidget(0, 6)
        self.cover_table.setHorizontalHeaderLabels(
            ["Cover", "Damage", "Remaining", "Predicted", "Cracked", "Segments"]
        )
        self.cover_table.horizontalHeader().setStretchLastSection(True)
        splitter.addWidget(self.cover_table)

        tabs = QTabWidget()
        self.ledger_chart = LedgerChart()
        tabs.addTab(self.ledger_chart, "Predicted ledger")
        self.queue_list = QListWidget()
        tabs.addTab(self.queue_list, "Budget queue")
        self.diff_list = QListWidget()
        tabs.addTab(self.diff_list, "Differences")
        splitter.addWidget(tabs)
        splitter.setStretchFactor(0, 3)
        splitter.setStretchFactor(1, 2)
        layout.addWidget(splitter, 1)

    def _browse_input(self) -> None:
        path = QFileDialog.getExistingDirectory(self, "Scenario directory")
        if path:
            self.input_edit.setText(path)

    def _browse_output(self) -> None:
        path = QFileDialog.getExistingDirectory(self, "Replay output directory")
        if path:
            self.output_edit.setText(path)

    def start_replay(self, *, resume: bool) -> None:
        input_text = self.input_edit.text().strip()
        output_text = self.output_edit.text().strip()
        if not input_text or not output_text:
            QMessageBox.warning(self, "Missing paths", "Scenario and output paths are required.")
            return
        if self.thread is not None and self.thread.isRunning():
            QMessageBox.warning(self, "Busy", "A replay worker is already running.")
            return
        try:
            scenario = load_scenario(input_text)
            self.engine = ReplayEngine(
                scenario,
                output_text,
                clean=not resume,
                resume=resume,
                compare_baseline=True,
            )
        except ReplayError as error:
            QMessageBox.critical(self, error.code, str(error))
            return
        self.frames = []
        self.frame_by_step = {}
        self.diff_list.clear()
        if resume:
            self._load_existing_trace()
            if self.engine.baseline_diff is not None:
                difference = self.engine.baseline_diff
                self.diff_list.addItem(
                    f"step {difference['step']} {difference['field_path']}: "
                    f"baseline={difference['expected']!r} replay={difference['actual']!r}"
                )
        self.progress.setMaximum(scenario.config.physical_steps)
        self.progress.setValue(self.engine.next_step)
        self.timeline.setRange(0, max(0, scenario.config.physical_steps - 1))
        self.timeline.setEnabled(True)
        self.timeline.setValue(max(0, self.engine.next_step - 1))
        self.worker = ReplayWorker(self.engine, self.step_delay_ms)
        self.thread = QThread(self)
        self.worker.moveToThread(self.thread)
        self.thread.started.connect(self.worker.run_loop)
        self.worker.step_done.connect(self._on_step)
        self.worker.finished.connect(self._on_finished)
        self.worker.failed.connect(self._on_failed)
        self.worker.finished.connect(self.thread.quit)
        self.worker.failed.connect(self.thread.quit)
        self.thread.finished.connect(self._thread_closed)
        self.pause_button.setChecked(False)
        self.status_label.setText("running")
        self.thread.start()

    def _load_existing_trace(self) -> None:
        if self.engine is None or not self.engine.trace_path.is_file():
            return
        for line in self.engine.trace_path.read_text(encoding="utf-8").splitlines():
            if not line.strip():
                continue
            frame = json.loads(line)
            self.frames.append(frame)
            self.frame_by_step[frame["step"]] = frame
        self.ledger_chart.set_frames(self.frames)

    @Slot(dict)
    def _on_step(self, frame: dict[str, Any]) -> None:
        self.frames.append(frame)
        self.frame_by_step[frame["step"]] = frame
        self.progress.setValue(frame["step"] + 1)
        self.timeline.setValue(frame["step"])
        self._render_frame(frame)
        self.ledger_chart.set_frames(self.frames)
        if self.engine and self.engine.baseline_diff is not None:
            difference = self.engine.baseline_diff
            self.diff_list.clear()
            self.diff_list.addItem(
                f"step {difference['step']} {difference['field_path']}: "
                f"baseline={difference['expected']!r} replay={difference['actual']!r}"
            )

    def _render_frame(self, frame: dict[str, Any]) -> None:
        self.step_label.setText(str(frame["step"]))
        self.cover_table.setRowCount(len(frame["covers"]))
        for row, cover in enumerate(frame["covers"]):
            values = (
                cover["cover_id"],
                cover["accumulated_damage"],
                cover["remaining_durability"],
                cover["predicted_remaining"],
                "yes" if cover["cracked"] else "no",
                str(cover["crack_segments"]),
            )
            for column, value in enumerate(values):
                item = QTableWidgetItem(value)
                if column == 4 and cover["cracked"]:
                    item.setForeground(QColor("#f72585"))
                self.cover_table.setItem(row, column, item)
        self.queue_list.clear()
        for event in frame["budget_usage"].get("events", []):
            if event.get("event") in {"queue", "degrade"}:
                self.queue_list.addItem(
                    f"{event.get('event')} {event.get('cover_id')} "
                    f"wave={event.get('wave', '-')} "
                    f"bytes={event.get('requested_bytes', event.get('reserved_memory_bytes'))}"
                )
        if frame.get("revocation_events"):
            self.status_label.setText(
                f"step {frame['step']} revokes={len(frame['revocation_events'])}"
            )

    @Slot(str)
    def _on_finished(self, status: str) -> None:
        self.status_label.setText(status)
        self.pause_button.setChecked(False)

    @Slot(dict)
    def _on_failed(self, error: dict[str, Any]) -> None:
        self.status_label.setText(error["code"])
        self.diff_list.addItem(
            f"{error['code']} {error.get('field_path') or ''} {error['message']}"
        )
        QMessageBox.critical(self, error["code"], error["message"])

    def _thread_closed(self) -> None:
        self.worker = None
        self.thread = None

    def _toggle_pause(self, paused: bool) -> None:
        if self.worker is None:
            return
        self.worker.set_paused(paused)
        self.pause_button.setText("Resume run" if paused else "Pause")
        self.status_label.setText("paused" if paused else "running")

    def _inject_revoke(self) -> None:
        if self.engine is None:
            return
        text, accepted = QInputDialog.getText(
            self,
            "Inject revoke",
            "player:client_seq",
            text="P1:0",
        )
        if not accepted or ":" not in text:
            return
        player_id, sequence_text = text.rsplit(":", 1)
        try:
            revoke = self.engine.inject_revoke(player_id.strip(), int(sequence_text))
        except (ReplayError, ValueError) as error:
            QMessageBox.critical(self, "Revoke rejected", str(error))
            return
        self.status_label.setText(
            f"revoke {revoke.key} queued for step {revoke.effective_step}"
        )

    def _cancel(self) -> None:
        if self.worker is not None:
            self.worker.cancel()
            self.status_label.setText("cancelling")
        elif self.engine is not None:
            self.engine.cancel()

    def _show_step(self, step: int) -> None:
        frame = self.frame_by_step.get(step)
        if frame is not None:
            self._render_frame(frame)

    def closeEvent(self, event: Any) -> None:
        if self.worker is not None:
            self.worker.cancel()
        if self.thread is not None and self.thread.isRunning():
            self.thread.quit()
            self.thread.wait(2000)
        super().closeEvent(event)


def launch_gui(
    *,
    input_path: str | None = None,
    output_path: str | None = None,
    resume: bool = False,
    step_delay_ms: int = 120,
) -> int:
    app = QApplication.instance() or QApplication([])
    window = MainWindow(
        input_path=input_path,
        output_path=output_path,
        resume=resume,
        step_delay_ms=step_delay_ms,
    )
    window.show()
    return int(app.exec())

