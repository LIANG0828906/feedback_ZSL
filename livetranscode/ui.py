from __future__ import annotations

import itertools
import threading
from pathlib import Path
from typing import Any

from PySide6.QtCore import Qt, QTimer
from PySide6.QtGui import QColor, QPainter, QPen
from PySide6.QtWidgets import (
    QApplication,
    QHBoxLayout,
    QLabel,
    QMainWindow,
    QPlainTextEdit,
    QPushButton,
    QTableWidget,
    QTableWidgetItem,
    QVBoxLayout,
    QWidget,
)

from .engine import TranscodeEngine
from .errors import BusinessError
from .simulator import simulate_adaptive_playback
from .util import read_json


class MemoryPlot(QWidget):
    def __init__(self) -> None:
        super().__init__()
        self.values: list[float] = []
        self.setMinimumHeight(120)

    def push(self, value: float) -> None:
        self.values.append(value)
        self.values = self.values[-240:]
        self.update()

    def paintEvent(self, event: Any) -> None:
        painter = QPainter(self)
        painter.fillRect(self.rect(), QColor("#101722"))
        painter.setPen(QPen(QColor("#3a4a60"), 1))
        for fraction in (0.25, 0.5, 0.75):
            y = int(self.height() * fraction)
            painter.drawLine(0, y, self.width(), y)
        if len(self.values) < 2:
            painter.setPen(QColor("#8998aa"))
            painter.drawText(12, 24, "memory waterline")
            return
        maximum = max(self.values) or 1.0
        painter.setPen(QPen(QColor("#42c59a"), 2))
        points = []
        for index, value in enumerate(self.values):
            x = int(index * (self.width() - 1) / max(1, len(self.values) - 1))
            y = int((1.0 - value / maximum) * (self.height() - 8)) + 4
            points.append((x, y))
        for left, right in itertools.pairwise(points):
            painter.drawLine(left[0], left[1], right[0], right[1])


class MainWindow(QMainWindow):
    def __init__(
        self,
        workspace: str | Path,
        sample: str | Path | None,
        bandwidth: str | Path | None,
    ) -> None:
        super().__init__()
        self.setWindowTitle("Live Transcode Slice Lab")
        self.resize(1040, 760)
        self.workspace = Path(workspace).resolve()
        self.sample = Path(sample).resolve() if sample else None
        self.bandwidth = Path(bandwidth).resolve() if bandwidth else None
        self.engine = self._open_engine()
        self.worker: threading.Thread | None = None
        self.worker_result: dict[str, Any] | None = None
        self.worker_error: BaseException | None = None
        self._build_ui()
        self.timer = QTimer(self)
        self.timer.timeout.connect(self._poll)
        self.timer.start(300)
        self._refresh()

    def _open_engine(self) -> TranscodeEngine:
        if self.sample is not None:
            audio = self.sample / "audio.wav"
            frames = self.sample / "frames.json"
            config = self.sample / "config.json"
            engine = TranscodeEngine.prepare_workspace(self.workspace, audio, frames, config)
            sample_bandwidth = self.sample / "bandwidth.json"
            if self.bandwidth is None and sample_bandwidth.exists():
                self.bandwidth = sample_bandwidth
            return engine
        return TranscodeEngine.load_workspace(self.workspace)

    def _build_ui(self) -> None:
        central = QWidget()
        layout = QVBoxLayout(central)
        controls = QHBoxLayout()
        self.start_button = QPushButton("Start / Continue")
        self.pause_button = QPushButton("Pause at boundary")
        self.cancel_button = QPushButton("Cancel")
        self.recover_button = QPushButton("Recover snapshot")
        controls.addWidget(self.start_button)
        controls.addWidget(self.pause_button)
        controls.addWidget(self.cancel_button)
        controls.addWidget(self.recover_button)
        controls.addStretch(1)
        layout.addLayout(controls)
        self.status = QLabel("idle")
        layout.addWidget(self.status)

        self.table = QTableWidget(0, 5)
        self.table.setHorizontalHeaderLabels(["Track", "Committed", "Total", "Status", "Last committed"])
        self.table.horizontalHeader().setStretchLastSection(True)
        layout.addWidget(self.table)

        layout.addWidget(QLabel("Memory waterline"))
        self.memory_plot = MemoryPlot()
        layout.addWidget(self.memory_plot)
        lower = QHBoxLayout()
        queue_box = QVBoxLayout()
        queue_box.addWidget(QLabel("Scheduler wait / active queue"))
        self.queue_view = QPlainTextEdit()
        self.queue_view.setReadOnly(True)
        queue_box.addWidget(self.queue_view)
        switch_box = QVBoxLayout()
        switch_box.addWidget(QLabel("Switch trace"))
        self.switch_view = QPlainTextEdit()
        self.switch_view.setReadOnly(True)
        switch_box.addWidget(self.switch_view)
        lower.addLayout(queue_box, 1)
        lower.addLayout(switch_box, 1)
        layout.addLayout(lower, 1)
        self.setCentralWidget(central)

        self.start_button.clicked.connect(self.start)
        self.pause_button.clicked.connect(self.engine.pause)
        self.cancel_button.clicked.connect(self.engine.cancel)
        self.recover_button.clicked.connect(self.recover)

    def _set_busy(self, busy: bool) -> None:
        self.start_button.setEnabled(not busy)
        self.recover_button.setEnabled(not busy)
        self.pause_button.setEnabled(busy)
        self.cancel_button.setEnabled(busy)

    def start(self) -> None:
        if self.worker is not None and self.worker.is_alive():
            return
        self.worker_result = None
        self.worker_error = None
        self._set_busy(True)
        self.status.setText("running")
        self.worker = threading.Thread(target=self._work, name="ui-run", daemon=True)
        self.worker.start()

    def _work(self) -> None:
        try:
            result = self.engine.run()
            bandwidth = self.bandwidth or (self.workspace / "bandwidth.json")
            if bandwidth.exists() and result.get("status") not in {"paused", "cancelled"}:
                trace = simulate_adaptive_playback(
                    self.workspace,
                    self.engine.config,
                    self.engine.timeline,
                    self.engine._indexes,
                    read_json(bandwidth),
                )
                result["switch_summary"] = {
                    "valid": trace["valid"],
                    "switch_count": trace["switch_count"],
                    "final_track_id": trace["final_track_id"],
                }
            self.worker_result = result
        except BaseException as exc:  # shown in the UI, never swallowed silently
            self.worker_error = exc

    def recover(self) -> None:
        try:
            self.engine = TranscodeEngine.load_workspace(self.workspace)
            self.status.setText("snapshot and disk indexes validated; ready to continue")
            self._refresh()
        except BusinessError as exc:
            self.status.setText(f"{exc.code}: {exc.message}")

    def _poll(self) -> None:
        if self.worker is not None and not self.worker.is_alive():
            if self.worker_error is not None:
                if isinstance(self.worker_error, BusinessError):
                    self.status.setText(f"{self.worker_error.code}: {self.worker_error.message}")
                else:
                    self.status.setText(f"error: {self.worker_error}")
            elif self.worker_result is not None:
                summary = self.worker_result.get("switch_summary")
                self.status.setText(
                    f"run {self.worker_result.get('status')}"
                    + (f"; switches={summary['switch_count']}" if summary else "")
                )
            self.worker = None
            self._set_busy(False)
            self._load_switch_trace()
        self._refresh()

    def _refresh(self) -> None:
        state = self.engine.state_summary()
        tracks = state["tracks"]
        self.table.setRowCount(len(tracks))
        for row, (track_id, item) in enumerate(sorted(tracks.items())):
            values = [
                track_id,
                str(item["completed_slices"]),
                str(item["slice_count"]),
                item["status"],
                str(item["completed_slices"] - 1 if item["completed_slices"] else "-"),
            ]
            for column, text in enumerate(values):
                cell = QTableWidgetItem(text)
                cell.setFlags(cell.flags() & ~Qt.ItemFlag.ItemIsEditable)
                self.table.setItem(row, column, cell)
        self.memory_plot.push(float(state["memory_reserved_bytes"]))
        self.queue_view.setPlainText(
            "\n".join(
                f"seq={item['task_seq']} {item['track_id']}[{item['slice_index']}] "
                f"{item['status']} bytes={item['estimated_bytes']}"
                for item in state["waiting_queue"]
            )
        )

    def _load_switch_trace(self) -> None:
        path = self.workspace / "switches.json"
        if not path.exists():
            return
        value = read_json(path)
        lines = []
        for switch in value.get("switches", []):
            lines.append(
                f"{switch['boundary_ticks']}  {switch['from_track_id']} -> {switch['to_track_id']}  "
                f"{switch['bandwidth_kbps']:.0f} kbps  {switch['reason']}  "
                f"buffer {switch['buffer_ms_after']:.0f} ms"
            )
        self.switch_view.setPlainText("\n".join(lines))

    def closeEvent(self, event: Any) -> None:
        self.engine.cancel()
        if self.worker is not None and self.worker.is_alive():
            self.worker.join(timeout=2.0)
        event.accept()


def run_ui(
    workspace: str | Path,
    sample: str | Path | None = None,
    bandwidth: str | Path | None = None,
) -> int:
    app = QApplication.instance() or QApplication([])
    window = MainWindow(workspace, sample, bandwidth)
    window.show()
    return int(app.exec())

