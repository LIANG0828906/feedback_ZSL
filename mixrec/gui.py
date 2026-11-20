"""Optional PySide6 workbench.

The GUI is intentionally a thin observer/controller over ``MixEngine``.  It
never writes slice files itself; every durable operation still goes through the
same atomic engine paths used by the CLI and tests.
"""

from __future__ import annotations

import sys
from pathlib import Path
from typing import Any

from .errors import MixError


def _qt_modules():
    from PySide6 import QtCore, QtGui, QtWidgets

    return QtCore, QtGui, QtWidgets


def launch(config_path: str | None = None) -> int:
    QtCore, QtGui, QtWidgets = _qt_modules()
    app = QtWidgets.QApplication.instance() or QtWidgets.QApplication(sys.argv[:1])
    app.setApplicationName("MixRec Local Workbench")
    window = MainWindow(config_path)
    window.show()
    return int(app.exec())


def _build_window_classes():
    QtCore, QtGui, QtWidgets = _qt_modules()

    class EnvelopeView(QtWidgets.QWidget):
        def __init__(self):
            super().__init__()
            self._curves: list[tuple[str, Any, Any]] = []
            self.setMinimumHeight(150)

        def set_envelopes(self, curves):
            self._curves = curves
            self.update()

        def paintEvent(self, _event):
            painter = QtGui.QPainter(self)
            painter.fillRect(self.rect(), QtGui.QColor("#111827"))
            painter.setPen(QtGui.QPen(QtGui.QColor("#263244"), 1))
            for row in range(1, 4):
                y = int(self.height() * row / 4)
                painter.drawLine(0, y, self.width(), y)
            if not self._curves:
                painter.setPen(QtGui.QColor("#64748b"))
                painter.drawText(self.rect(), QtCore.Qt.AlignCenter, "No track loaded")
                return
            lane_height = max(20, self.height() // len(self._curves))
            colors = ("#38bdf8", "#a78bfa", "#34d399", "#f59e0b")
            for lane, (name, upper, lower) in enumerate(self._curves):
                top = lane * lane_height
                center = top + lane_height / 2
                scale = lane_height * 0.38
                color = QtGui.QColor(colors[lane % len(colors)])
                painter.setPen(QtGui.QPen(color, 1.2))
                points = []
                count = len(upper)
                if count == 0:
                    continue
                step = max(1, count // max(1, self.width()))
                for x in range(0, self.width()):
                    idx = min(count - 1, int(x * count / max(1, self.width())))
                    points.append(
                        QtCore.QPointF(
                            x,
                            center - float(upper[idx]) * scale,
                        )
                    )
                painter.drawPolyline(QtGui.QPolygonF(points))
                points_lower = []
                for x in range(0, self.width()):
                    idx = min(count - 1, int(x * count / max(1, self.width())))
                    points_lower.append(QtCore.QPointF(x, center - float(lower[idx]) * scale))
                painter.drawPolyline(QtGui.QPolygonF(points_lower))
                painter.setPen(QtGui.QColor("#cbd5e1"))
                painter.drawText(8, top + 14, name)

    class SliceGridView(QtWidgets.QWidget):
        def __init__(self):
            super().__init__()
            self.total_slices = 0
            self.committed = 0
            self.affected: set[int] = set()
            self.waiting: set[int] = set()
            self.setMinimumHeight(90)

        def set_state(self, total: int, committed: int, affected=None, waiting=None):
            self.total_slices = total
            self.committed = committed
            self.affected = set(affected or [])
            self.waiting = set(waiting or [])
            self.update()

        def paintEvent(self, _event):
            painter = QtGui.QPainter(self)
            painter.fillRect(self.rect(), QtGui.QColor("#0f172a"))
            if self.total_slices <= 0:
                painter.setPen(QtGui.QColor("#64748b"))
                painter.drawText(self.rect(), QtCore.Qt.AlignCenter, "Slice grid appears after a job starts")
                return
            columns = min(32, max(1, self.width() // 24))
            rows = (self.total_slices + columns - 1) // columns
            cell = min(
                22,
                max(8, (self.width() - 2) // columns),
                max(8, (self.height() - 2) // max(1, rows)),
            )
            gap = max(2, cell // 6)
            for index in range(self.total_slices):
                row, col = divmod(index, columns)
                x = 2 + col * (cell + gap)
                y = 2 + row * (cell + gap)
                if index < self.committed:
                    color = QtGui.QColor("#2563eb")
                elif index in self.affected:
                    color = QtGui.QColor("#a855f7")
                elif index in self.waiting:
                    color = QtGui.QColor("#f59e0b")
                else:
                    color = QtGui.QColor("#1e293b")
                painter.fillRect(x, y, cell, cell, color)
                painter.setPen(QtGui.QColor("#0b1120"))
                painter.drawRect(x, y, cell, cell)

    class MemoryView(QtWidgets.QWidget):
        def __init__(self):
            super().__init__()
            self.values: list[int] = []
            self.budget: int | None = None
            self.setMinimumHeight(110)

        def add_value(self, value: int):
            self.values.append(max(0, int(value)))
            self.values = self.values[-400:]
            self.update()

        def paintEvent(self, _event):
            painter = QtGui.QPainter(self)
            painter.fillRect(self.rect(), QtGui.QColor("#0f172a"))
            painter.setPen(QtGui.QColor("#263244"))
            painter.drawRect(1, 1, self.width() - 2, self.height() - 2)
            if not self.values:
                painter.setPen(QtGui.QColor("#64748b"))
                painter.drawText(self.rect(), QtCore.Qt.AlignCenter, "Memory reservations")
                return
            peak = max(self.values + ([self.budget] if self.budget else []))
            peak = max(1, peak)
            if self.budget:
                y = self.height() - int(self.height() * self.budget / peak)
                painter.setPen(QtGui.QPen(QtGui.QColor("#ef4444"), 1))
                painter.drawLine(0, y, self.width(), y)
            painter.setPen(QtGui.QPen(QtGui.QColor("#22d3ee"), 2))
            count = len(self.values)
            points = []
            for i, value in enumerate(self.values):
                x = int(i * (self.width() - 2) / max(1, count - 1))
                y = self.height() - 2 - int((self.height() - 4) * value / peak)
                points.append(QtCore.QPointF(x, y))
            painter.drawPolyline(QtGui.QPolygonF(points))

    class MainWindow(QtWidgets.QMainWindow):
        event_received = QtCore.Signal(dict)

        def __init__(self, config_path: str | None = None):
            super().__init__()
            self.setWindowTitle("MixRec Local Workbench")
            self.resize(1180, 780)
            self.config_path: Path | None = None
            self.engine = None
            self.control = None
            self.total_slices = 0
            self.committed = 0
            self.current_reserved = 0
            self.waiting: set[int] = set()
            self._build_ui()
            self.event_received.connect(self._on_event)
            self._status_timer = QtCore.QTimer(self)
            self._status_timer.timeout.connect(self._poll_thread)
            self._status_timer.start(250)
            if config_path:
                self.load_config(config_path)

        def _build_ui(self):
            central = QtWidgets.QWidget()
            root = QtWidgets.QVBoxLayout(central)
            toolbar = QtWidgets.QHBoxLayout()
            self.load_button = QtWidgets.QPushButton("Load config")
            self.start_button = QtWidgets.QPushButton("Start")
            self.pause_button = QtWidgets.QPushButton("Pause")
            self.resume_button = QtWidgets.QPushButton("Resume")
            self.cancel_button = QtWidgets.QPushButton("Cancel")
            self.recover_button = QtWidgets.QPushButton("Recover")
            self.apply_button = QtWidgets.QPushButton("Apply track edit")
            for widget in (
                self.load_button,
                self.start_button,
                self.pause_button,
                self.resume_button,
                self.cancel_button,
                self.recover_button,
                self.apply_button,
            ):
                toolbar.addWidget(widget)
            toolbar.addStretch(1)
            root.addLayout(toolbar)

            path_row = QtWidgets.QHBoxLayout()
            self.path_label = QtWidgets.QLabel("No config loaded")
            self.path_label.setTextInteractionFlags(QtCore.Qt.TextSelectableByMouse)
            path_row.addWidget(self.path_label, 1)
            root.addLayout(path_row)

            splitter = QtWidgets.QSplitter(QtCore.Qt.Horizontal)
            left = QtWidgets.QWidget()
            left_layout = QtWidgets.QVBoxLayout(left)
            left_layout.addWidget(QtWidgets.QLabel("Tracks"))
            self.track_table = QtWidgets.QTableWidget(0, 5)
            self.track_table.setHorizontalHeaderLabels(["Track", "Rate", "Ch", "Gain dB", "Mute ranges (s)"])
            self.track_table.horizontalHeader().setSectionResizeMode(QtWidgets.QHeaderView.Stretch)
            self.track_table.setSelectionBehavior(QtWidgets.QAbstractItemView.SelectRows)
            left_layout.addWidget(self.track_table, 2)
            left_layout.addWidget(QtWidgets.QLabel("Waiting queue"))
            self.queue_list = QtWidgets.QListWidget()
            left_layout.addWidget(self.queue_list, 1)
            splitter.addWidget(left)

            right = QtWidgets.QWidget()
            right_layout = QtWidgets.QVBoxLayout(right)
            self.progress = QtWidgets.QProgressBar()
            self.progress.setRange(0, 1)
            self.progress.setValue(0)
            self.progress.setFormat("%v / %m slices")
            right_layout.addWidget(self.progress)
            self.status_label = QtWidgets.QLabel("Idle")
            right_layout.addWidget(self.status_label)
            tabs = QtWidgets.QTabWidget()
            self.envelope = EnvelopeView()
            self.slice_grid = SliceGridView()
            self.memory_view = MemoryView()
            tabs.addTab(self.envelope, "Waveforms")
            tabs.addTab(self.slice_grid, "Slice grid")
            tabs.addTab(self.memory_view, "Memory / scheduling")
            right_layout.addWidget(tabs, 3)
            right_layout.addWidget(QtWidgets.QLabel("Deterministic event trace"))
            self.log = QtWidgets.QPlainTextEdit()
            self.log.setReadOnly(True)
            self.log.setMaximumBlockCount(800)
            right_layout.addWidget(self.log, 2)
            splitter.addWidget(right)
            splitter.setSizes([380, 800])
            root.addWidget(splitter, 1)
            self.setCentralWidget(central)

            self.load_button.clicked.connect(self._choose_config)
            self.start_button.clicked.connect(self.start)
            self.pause_button.clicked.connect(self.pause)
            self.resume_button.clicked.connect(self.resume)
            self.cancel_button.clicked.connect(self.cancel)
            self.recover_button.clicked.connect(self.recover)
            self.apply_button.clicked.connect(self.apply_track_edit)
            self.setStyleSheet(
                """
                QMainWindow, QWidget { background: #0b1120; color: #dbeafe; }
                QPushButton { background: #1e40af; border: 1px solid #3b82f6;
                              border-radius: 4px; padding: 6px 12px; }
                QPushButton:disabled { background: #1e293b; color: #64748b; }
                QTableWidget, QListWidget, QPlainTextEdit, QTabWidget::pane {
                    background: #0f172a; border: 1px solid #1e3a5f;
                }
                QHeaderView::section { background: #172554; color: #dbeafe; padding: 4px; }
                QProgressBar { background: #0f172a; border: 1px solid #1e3a5f;
                               text-align: center; }
                QProgressBar::chunk { background: #2563eb; }
                """
            )

        def _choose_config(self):
            path, _ = QtWidgets.QFileDialog.getOpenFileName(
                self, "Open mix config", str(Path.cwd()), "JSON (*.json)"
            )
            if path:
                self.load_config(path)

        def load_config(self, path: str):
            from .mixer import Mixer
            from .models import MixConfig
            from .wavio import read_wav

            try:
                config = MixConfig.from_file(path)
                mixer = Mixer(config)
            except MixError as exc:
                self.status_label.setText(str(exc))
                return
            self.config_path = Path(path)
            self.path_label.setText(str(self.config_path))
            self.engine = None
            self.total_slices = config.total_slices
            self.committed = 0
            self.waiting.clear()
            self.queue_list.clear()
            self.progress.setRange(0, max(1, self.total_slices))
            self.progress.setValue(0)
            self.slice_grid.set_state(self.total_slices, 0)
            self.track_table.setRowCount(len(config.tracks))
            envelopes = []
            for row, track in enumerate(config.tracks):
                wav = read_wav(config.track_path(track), track_id=track.id)
                mono = wav.pcm[:, 0].astype("float64") / 32768.0
                bucket = max(1, len(mono) // 600)
                usable = len(mono) // bucket * bucket
                if usable:
                    shaped = mono[:usable].reshape(-1, bucket)
                    upper = shaped.max(axis=1)
                    lower = shaped.min(axis=1)
                else:
                    upper = lower = mono
                envelopes.append((track.id, upper, lower))
                values = [
                    track.id,
                    str(wav.sample_rate),
                    str(wav.channels),
                    f"{track.gain_db:g}",
                    "; ".join(f"{a:g}:{b:g}" for a, b in track.mute_ranges),
                ]
                for column, value in enumerate(values):
                    item = QtWidgets.QTableWidgetItem(value)
                    if column >= 3:
                        item.setFlags(item.flags() | QtCore.Qt.ItemIsEditable)
                    else:
                        item.setFlags(item.flags() & ~QtCore.Qt.ItemIsEditable)
                    self.track_table.setItem(row, column, item)
            self.envelope.set_envelopes(envelopes)
            self.status_label.setText(
                f"Loaded {config.job_id}: {self.total_slices} slices @ {config.output_rate} Hz"
            )

        def _make_engine(self):
            from .engine import MixEngine

            if self.config_path is None:
                raise MixError("E_CONFIG", "load a config first")
            return MixEngine.from_file(self.config_path)

        def start(self):
            try:
                self.engine = self._make_engine()
                self.engine.start_async(
                    resume=True,
                    on_event=self.event_received.emit,
                )
                self.status_label.setText("Running")
            except MixError as exc:
                self.status_label.setText(str(exc))

        def pause(self):
            try:
                if self.engine:
                    self.engine.pause()
                    self.status_label.setText("Paused (cooperative)")
            except MixError as exc:
                self.status_label.setText(str(exc))

        def resume(self):
            try:
                if self.engine:
                    self.engine.resume_run()
                    self.status_label.setText("Running")
            except MixError as exc:
                self.status_label.setText(str(exc))

        def cancel(self):
            try:
                if self.engine:
                    self.engine.cancel()
                    self.status_label.setText("Cancelling after the current batch")
            except MixError as exc:
                self.status_label.setText(str(exc))

        def recover(self):
            try:
                self.engine = self._make_engine()
                self.engine.start_async(resume=True, on_event=self.event_received.emit)
                self.status_label.setText("Validating snapshot and recovering")
            except MixError as exc:
                self.status_label.setText(str(exc))

        def apply_track_edit(self):
            if self.engine is None:
                self.status_label.setText("Start or load an existing index before incremental edits")
                return
            row = self.track_table.currentRow()
            if row < 0:
                self.status_label.setText("Select a track row first")
                return
            track_id = self.track_table.item(row, 0).text()
            try:
                gain = float(self.track_table.item(row, 3).text())
                mute_text = self.track_table.item(row, 4).text().strip()
                mutes = []
                if mute_text:
                    for part in mute_text.split(";"):
                        start, end = part.strip().split(":", 1)
                        mutes.append([float(start), float(end)])
                index = self.engine.update_track(
                    track_id,
                    gain_db=gain,
                    mute_ranges=mutes,
                    on_event=self.event_received.emit,
                )
                self.committed = int(index["committed_slices"])
                self.status_label.setText(
                    f"Incremental publish complete (index v{index['index_version']})"
                )
            except (MixError, ValueError) as exc:
                self.status_label.setText(str(exc))

        def _poll_thread(self):
            if self.engine is None or self.engine.thread is None:
                return
            thread = self.engine.thread
            if not thread.is_alive():
                if self.engine.last_error is not None:
                    self.status_label.setText(str(self.engine.last_error))
                elif self.engine.last_result is not None:
                    result = self.engine.last_result
                    if result.index:
                        self.committed = int(result.index["committed_slices"])
                        self.slice_grid.set_state(
                            int(result.index["total_slices"]),
                            self.committed,
                            waiting=self.waiting,
                        )
                    self.status_label.setText(
                        "Complete" if result.ok else f"Stopped: {result.stop_reason or 'prefix'}"
                    )

        def _on_event(self, event: dict[str, Any]):
            name = str(event.get("event", ""))
            if name == "job_start":
                self.total_slices = int(event.get("total_slices", 0))
                self.progress.setRange(0, max(1, self.total_slices))
                self.slice_grid.set_state(self.total_slices, self.committed)
            elif name == "memory_reserved":
                self.current_reserved = int(event.get("bytes", 0))
                self.memory_view.add_value(self.current_reserved)
                for index in event.get("tasks", []):
                    self.waiting.discard(int(index))
            elif name == "memory_released":
                self.current_reserved = max(0, self.current_reserved - int(event.get("bytes", 0)))
                self.memory_view.add_value(self.current_reserved)
            elif name == "task_waiting":
                index = event.get("slice_index")
                if index is not None:
                    self.waiting.add(int(index))
                    self.queue_list.addItem(f"slice {index}: {event.get('reason', 'waiting')}")
            elif name == "slice_committed":
                self.committed = max(self.committed, int(event.get("slice_index", -1)) + 1)
                self.progress.setValue(self.committed)
                self.slice_grid.set_state(self.total_slices, self.committed, waiting=self.waiting)
            elif name == "incremental_start":
                affected = {int(item) for item in event.get("affected_slices", [])}
                self.slice_grid.set_state(self.total_slices, self.committed, affected=affected, waiting=self.waiting)
            elif name in {"run_complete", "incremental_published"}:
                if "index_version" in event:
                    self.status_label.setText(f"{name} (index v{event['index_version']})")
            elif name in {"run_failed", "incremental_failed"}:
                self.status_label.setText(f"{event.get('code')}: {name}")
            self.log.appendPlainText(
                f"{event.get('seq', '?'):>4}  {name}  "
                + ", ".join(f"{key}={value}" for key, value in sorted(event.items()) if key not in {"event", "seq"})
            )

    return MainWindow


class MainWindow:  # pragma: no cover - replaced by the lazy Qt class
    def __new__(cls, *args, **kwargs):
        return _build_window_classes()(*args, **kwargs)


__all__ = ["launch", "MainWindow"]
