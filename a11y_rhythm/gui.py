"""Optional PyQt5 workbench for waveform, tracks and conflict inspection."""

from __future__ import annotations

import json
import sys
import wave
from array import array
from pathlib import Path
from typing import Any

from .errors import E_STALE_HANDLE, RhythmError
from .service import PublishService, SolveTicket, open_publish_service
from .timebase import load_source_bundle


PYQT_AVAILABLE = False
QT_IMPORT_ERROR = ""
try:
    from PyQt5 import QtCore, QtGui, QtWidgets

    PYQT_AVAILABLE = True
except ImportError as exc:  # pragma: no cover - exercised only without PyQt5
    QT_IMPORT_ERROR = str(exc)


def waveform_peaks(wav_path: str, buckets: int = 2400) -> tuple[list[float], int]:
    with wave.open(wav_path, "rb") as wav:
        channels = wav.getnchannels()
        frames = wav.getnframes()
        raw = wav.readframes(frames)
    try:
        import numpy as np

        values = np.frombuffer(raw, dtype="<i2").reshape(-1, channels)
        envelope = np.max(np.abs(values.astype(np.int32)), axis=1)
        stride = max(1, frames // buckets)
        bucket_count = (frames + stride - 1) // stride
        padded = np.full(bucket_count * stride, -1, dtype=np.int32)
        padded[:frames] = envelope
        peaks_array = padded.reshape(bucket_count, stride).max(axis=1)
        return [float(value) / 32768.0 for value in peaks_array], frames
    except ImportError:
        pass
    samples = array("h")
    samples.frombytes(raw)
    if samples.itemsize != 2:
        raise RuntimeError("waveform requires 16-bit PCM")
    stride = max(1, frames // buckets)
    peaks: list[float] = []
    for frame_start in range(0, frames, stride):
        frame_end = min(frames, frame_start + stride)
        peak = 0
        for frame in range(frame_start, frame_end):
            base = frame * channels
            for channel in range(channels):
                value = samples[base + channel]
                if value < 0:
                    value = -value
                if value > peak:
                    peak = value
        peaks.append(peak / 32768.0)
    return peaks, frames


if PYQT_AVAILABLE:

    class TimelineWidget(QtWidgets.QWidget):
        def __init__(self, parent: QtWidgets.QWidget | None = None):
            super().__init__(parent)
            self.setMinimumHeight(240)
            self.payload: dict[str, Any] | None = None
            self.peaks: list[float] = []
            self.total_frames = 1
            self.sample_rate = 48_000
            self.total_samples = 1

        def set_media(self, wav_path: str, sample_count: int) -> None:
            self.peaks, self.total_frames = waveform_peaks(wav_path)
            self.total_samples = max(1, sample_count)
            self.update()

        def set_payload(self, payload: dict[str, Any]) -> None:
            self.payload = payload
            self.sample_rate = int(payload["compliance"]["sample_rate"])
            self.total_samples = max(
                [self.total_samples]
                + [int(item["end"]) for item in payload["result"]["items"]]
            )
            self.update()

        def paintEvent(self, event: QtGui.QPaintEvent) -> None:  # noqa: N802
            painter = QtGui.QPainter(self)
            painter.fillRect(self.rect(), QtGui.QColor("#11151c"))
            width = max(1, self.width())
            height = self.height()
            center = height * 0.24
            waveform_height = height * 0.34
            painter.setPen(QtGui.QPen(QtGui.QColor("#4e9ad6"), 1))
            if self.peaks:
                for x in range(width):
                    index = int(x * len(self.peaks) / width)
                    peak = self.peaks[min(index, len(self.peaks) - 1)]
                    amplitude = peak * waveform_height
                    painter.drawLine(x, int(center - amplitude), x, int(center + amplitude))

            if self.payload is None:
                painter.end()
                return
            items = self.payload["result"]["items"]
            conflict_ids = {
                conflict.get("other_id", "")
                for item in items
                for conflict in item.get("conflicts", [])
            }
            track_top = height * 0.52
            track_height = height * 0.40
            for index, item in enumerate(sorted(items, key=lambda value: (value["start"], value["id"]))):
                x1 = int(item["start"] / self.total_samples * width)
                x2 = int(item["end"] / self.total_samples * width)
                if x2 <= x1:
                    x2 = x1 + 2
                y = int(track_top + (index % 4) * (track_height / 4))
                if item["status"] == "rejected":
                    color = QtGui.QColor("#555a63")
                elif item["kind"] == "description":
                    color = QtGui.QColor("#d88d3f")
                elif item["readable"]:
                    color = QtGui.QColor("#3d9b74")
                else:
                    color = QtGui.QColor("#ad4444")
                painter.fillRect(x1, y, max(2, x2 - x1), max(8, int(track_height / 7)), color)
                if item["id"] in conflict_ids:
                    painter.setPen(QtGui.QPen(QtGui.QColor("#ff5c5c"), 2))
                    painter.drawRect(x1, y, max(2, x2 - x1), max(8, int(track_height / 7)))
                painter.setPen(QtGui.QColor("#e7edf5"))
                painter.drawText(x1 + 2, y + 12, item["id"])
            painter.end()


    class Workbench(QtWidgets.QMainWindow):
        REFRESH_MS = 100

        def __init__(
            self,
            service: PublishService,
            source_path: str,
            wav_path: str,
            sample_count: int,
        ):
            super().__init__()
            self.service = service
            self.handle = service.handle
            self.ticket: SolveTicket | None = None
            self.source_path = source_path
            self.setWindowTitle(f"Accessibility Rhythm Workbench - {service.material_id}")
            self.resize(1260, 760)
            central = QtWidgets.QWidget(self)
            self.setCentralWidget(central)
            layout = QtWidgets.QVBoxLayout(central)
            self.timeline = TimelineWidget(central)
            self.timeline.set_media(wav_path, sample_count)
            layout.addWidget(self.timeline, 3)
            self.table = QtWidgets.QTableWidget(0, 6, central)
            self.table.setHorizontalHeaderLabels(
                ["Sentence", "Kind", "Text", "Readable", "Status", "Decision"]
            )
            self.table.horizontalHeader().setSectionResizeMode(2, QtWidgets.QHeaderView.Stretch)
            layout.addWidget(self.table, 2)
            controls = QtWidgets.QHBoxLayout()
            self.edit = QtWidgets.QLineEdit(central)
            self.edit.setPlaceholderText("Edit selected sentence text, then apply")
            self.apply_button = QtWidgets.QPushButton("Apply edit && solve", central)
            self.cancel_button = QtWidgets.QPushButton("Cooperative cancel", central)
            self.recover_button = QtWidgets.QPushButton("Recover journal", central)
            controls.addWidget(self.edit, 1)
            controls.addWidget(self.apply_button)
            controls.addWidget(self.cancel_button)
            controls.addWidget(self.recover_button)
            layout.addLayout(controls)
            self.log = QtWidgets.QPlainTextEdit(central)
            self.log.setReadOnly(True)
            layout.addWidget(self.log, 2)
            self.statusBar().showMessage("ready")
            self.apply_button.clicked.connect(self.apply_edit)
            self.cancel_button.clicked.connect(self.cancel)
            self.recover_button.clicked.connect(self.recover)
            self.table.cellClicked.connect(self.select_row)
            self.timer = QtCore.QTimer(self)
            self.timer.setInterval(self.REFRESH_MS)
            self.timer.timeout.connect(self.refresh_loop)
            self.timer.start()
            self.refresh_loop(force=True)

        def select_row(self, row: int, _column: int) -> None:
            text_item = self.table.item(row, 2)
            if text_item is not None:
                self.edit.setText(text_item.text())

        def apply_edit(self) -> None:
            row = self.table.currentRow()
            if row < 0:
                self.statusBar().showMessage("select a sentence first")
                return
            item_id = self.table.item(row, 0).text()
            if self.ticket is not None and not self.ticket.done:
                self.statusBar().showMessage("a solve is already running")
                return
            try:
                self.ticket = self.service.start_update(item_id, {"text": self.edit.text()})
                self.statusBar().showMessage(f"background solve started for {item_id}")
            except RhythmError as error:
                self.show_error(error)

        def cancel(self) -> None:
            if self.ticket is None:
                return
            try:
                self.service.cancel(self.ticket)
                self.statusBar().showMessage("cooperative cancel requested")
            except RhythmError as error:
                self.show_error(error)

        def recover(self) -> None:
            try:
                status = self.service.recover()
                self.log.appendPlainText(json.dumps(status.to_dict(), ensure_ascii=False))
                self.handle = self.service.handle
                self.refresh_loop(force=True)
            except RhythmError as error:
                self.show_error(error)

        def refresh_loop(self, force: bool = False) -> None:
            try:
                view = self.service.query(self.handle)
            except RhythmError as error:
                if error.code == E_STALE_HANDLE:
                    self.handle = self.service.handle
                    view = self.service.query(self.handle)
                else:
                    self.show_error(error)
                    return
            except Exception as exc:
                self.statusBar().showMessage(f"refresh error: {exc}")
                return
            payload = view["payload"]
            if force or payload["version"] != getattr(self, "_painted_version", None):
                self.render(payload)
            if self.ticket is not None and self.ticket.done:
                if self.ticket.error:
                    self.show_error(self.ticket.error)
                else:
                    self.statusBar().showMessage(
                        f"published version {payload['version']} from ticket {self.ticket.ticket_id}"
                    )
                self.ticket = None

        def render(self, payload: dict[str, Any]) -> None:
            self._painted_version = payload["version"]
            # The service handle is authoritative; keep the UI local handle in
            # sync without guessing a chain hash.
            self.handle = self.service.handle
            self.timeline.set_payload(payload)
            result = payload["result"]
            conflict_ids = {
                conflict.get("other_id", "")
                for item in result["items"]
                for conflict in item.get("conflicts", [])
            }
            self.table.setRowCount(len(result["items"]))
            for row, item in enumerate(result["items"]):
                values = [
                    item["id"],
                    item["kind"],
                    item["text"],
                    "yes" if item["readable"] else "no",
                    item["status"],
                    item["decision"],
                ]
                for column, value in enumerate(values):
                    cell = QtWidgets.QTableWidgetItem(str(value))
                    if item["id"] in conflict_ids:
                        cell.setBackground(QtGui.QColor("#4b2323"))
                    self.table.setItem(row, column, cell)
            self.statusBar().showMessage(
                f"version {payload['version']} | affected {len(result['affected_ids'])} | "
                f"conflicts {len(result['conflicts'])} | compliant {payload['compliance']['compliant']}"
            )
            for event in payload["trace_log"]:
                if event.get("version") == payload["version"]:
                    self.log.appendPlainText(json.dumps(event, ensure_ascii=False, sort_keys=True))

        def show_error(self, error: RhythmError) -> None:
            self.statusBar().showMessage(f"{error.code}: {error.message}")
            self.log.appendPlainText(json.dumps(error.to_dict(), ensure_ascii=False, sort_keys=True))


def run_gui(source_path: str, store_path: str) -> int:
    if not PYQT_AVAILABLE:
        print(
            "PyQt5 is not installed in this environment. "
            f"Install PyQt5>=5.15 locally to launch the workbench ({QT_IMPORT_ERROR}).",
            file=sys.stderr,
        )
        return 2
    bundle, wav_info, frame_table = load_source_bundle(source_path)
    coverage = min(wav_info.sample_count, frame_table.duration_samples)
    service = open_publish_service(bundle, frame_table, coverage, store_path)
    app = QtWidgets.QApplication(sys.argv)
    window = Workbench(service, source_path, bundle.wav_path, wav_info.sample_count)
    window.show()
    return app.exec_()
