from __future__ import annotations

import sys
from pathlib import Path
from typing import Any

from .errors import BusinessError
from .models import DescriptionInput
from .session import LayoutSession


def _load_qt() -> tuple[Any, ...]:
    try:
        from PySide6 import QtCore, QtGui, QtWidgets
    except ImportError as exc:
        raise BusinessError(
            "E_INVALID_INPUT",
            "PySide6 is required for the desktop workbench",
            {"install": "pip install PySide6"},
        ) from exc
    return QtCore, QtGui, QtWidgets


QtCore, QtGui, QtWidgets = _load_qt()


class TimelineWidget(QtWidgets.QWidget):
    def __init__(self) -> None:
        super().__init__()
        self.setMinimumHeight(180)
        self.snapshot: dict[str, Any] | None = None
        self.position_sample = 0
        self.window_samples = 240_000

    def set_snapshot(self, snapshot: dict[str, Any], position_sample: int, window_samples: int) -> None:
        self.snapshot = snapshot
        self.position_sample = position_sample
        self.window_samples = window_samples
        self.update()

    def paintEvent(self, event: Any) -> None:
        del event
        painter = QtGui.QPainter(self)
        painter.setRenderHint(QtGui.QPainter.RenderHint.Antialiasing, True)
        painter.fillRect(self.rect(), QtGui.QColor("#10151d"))
        width = max(1, self.width())
        height = self.height()
        start = self.position_sample
        end = start + self.window_samples
        painter.setPen(QtGui.QColor("#576579"))
        for tick in range(0, 11):
            x = int(width * tick / 10)
            painter.drawLine(x, 24, x, height - 24)
            painter.drawText(x + 3, 17, f"{tick * 10}%")
        colors = ["#54b689", "#5aa9e6", "#d7a84b", "#c678dd", "#e06c75"]
        snapshot = self.snapshot
        if snapshot:
            for index, sentence in enumerate(snapshot.get("sentences", [])):
                left_sample = int(sentence["start_sample"])
                right_sample = int(sentence["end_sample"])
                left = int((left_sample - start) / self.window_samples * width)
                right = int((right_sample - start) / self.window_samples * width)
                left = max(0, min(width - 1, left))
                right = max(left + 1, min(width, right))
                top = 38 + (index % 3) * 32
                color = QtGui.QColor(colors[index % len(colors)])
                color.setAlpha(190)
                painter.fillRect(left, top, right - left, 24, color)
                painter.setPen(QtGui.QColor("#ffffff"))
                painter.drawText(
                    left + 4,
                    top + 16,
                    f"{sentence['sentence_id']} {sentence['start'][3:12]}",
                )
            painter.setPen(QtGui.QPen(QtGui.QColor("#ffcc66"), 2))
            for marker in snapshot.get("markers", []):
                x = int((int(marker["sample"]) - start) / self.window_samples * width)
                painter.drawLine(x, 30, x, height - 28)
                painter.drawText(x + 4, height - 10, str(marker["speaker"]))


class CommitThread(QtCore.QThread):
    succeeded = QtCore.Signal(object)
    failed = QtCore.Signal(object)

    def __init__(self, session: LayoutSession) -> None:
        super().__init__()
        self.session = session

    def run(self) -> None:
        try:
            report = self.session.commit()
        except BusinessError as exc:
            self.failed.emit(exc.to_dict())
        except BaseException as exc:  # pragma: no cover - defensive UI boundary
            self.failed.emit({"code": "E_INVALID_INPUT", "message": str(exc), "context": {}})
        else:
            self.succeeded.emit(report)


class MainWindow(QtWidgets.QMainWindow):
    def __init__(self, case_dir: Path, *, refresh_ms: int = 250) -> None:
        super().__init__()
        self.case_dir = case_dir
        self.session = LayoutSession.open(case_dir)
        if self.session.report is None:
            self.session.commit()
        self.refresh_ms = refresh_ms
        self.commit_thread: CommitThread | None = None
        self._loading = False
        self._build_ui()
        self._load_report()
        self._start_refresh_timer()

    def _build_ui(self) -> None:
        self.setWindowTitle(f"Local Subtitle Layout Workbench - {self.case_dir.name}")
        self.resize(1280, 820)
        central = QtWidgets.QWidget()
        layout = QtWidgets.QVBoxLayout(central)

        toolbar = QtWidgets.QHBoxLayout()
        self.status = QtWidgets.QLabel("")
        self.status.setStyleSheet("font-family: monospace; color: #9cc4e4;")
        toolbar.addWidget(self.status, 1)
        self.commit_button = QtWidgets.QPushButton("提交排版")
        self.cancel_button = QtWidgets.QPushButton("取消提交")
        self.discard_button = QtWidgets.QPushButton("放弃暂存")
        self.description_button = QtWidgets.QPushButton("插入无障碍描述")
        self.commit_button.clicked.connect(self._commit_async)
        self.cancel_button.clicked.connect(self.session.request_cancel)
        self.discard_button.clicked.connect(self._discard)
        self.description_button.clicked.connect(self._insert_description)
        for button in (
            self.commit_button,
            self.cancel_button,
            self.discard_button,
            self.description_button,
        ):
            toolbar.addWidget(button)
        layout.addLayout(toolbar)

        parameter_row = QtWidgets.QHBoxLayout()
        self.default_cps = QtWidgets.QLineEdit()
        self.default_cps.setMaximumWidth(70)
        self.min_duration = QtWidgets.QSpinBox()
        self.min_duration.setRange(0, 60_000)
        self.min_duration.setSuffix(" ms")
        self.min_gap = QtWidgets.QSpinBox()
        self.min_gap.setRange(0, 60_000)
        self.min_gap.setSuffix(" ms")
        self.priority = QtWidgets.QSpinBox()
        self.priority.setRange(0, 1000)
        stage_parameters = QtWidgets.QPushButton("暂存参数")
        stage_parameters.clicked.connect(self._stage_parameters)
        for label, widget in (
            ("默认字速", self.default_cps),
            ("最短时长", self.min_duration),
            ("最小间隔", self.min_gap),
            ("句优先级", self.priority),
        ):
            parameter_row.addWidget(QtWidgets.QLabel(label))
            parameter_row.addWidget(widget)
        parameter_row.addWidget(stage_parameters)
        parameter_row.addStretch(1)
        layout.addLayout(parameter_row)

        self.timeline = TimelineWidget()
        layout.addWidget(self.timeline)

        self.table = QtWidgets.QTableWidget(0, 8)
        self.table.setHorizontalHeaderLabels(
            ["句子", "开始", "结束", "说话人", "字速", "判定", "文本", "哈希"]
        )
        self.table.horizontalHeader().setStretchLastSection(True)
        self.table.setAlternatingRowColors(True)
        self.table.itemChanged.connect(self._cell_changed)
        layout.addWidget(self.table, 1)

        position_row = QtWidgets.QHBoxLayout()
        self.position = QtWidgets.QSlider(QtCore.Qt.Orientation.Horizontal)
        self.position.valueChanged.connect(lambda _: self._refresh_preview())
        self.position_label = QtWidgets.QLabel("")
        position_row.addWidget(QtWidgets.QLabel("编辑位置"))
        position_row.addWidget(self.position, 1)
        position_row.addWidget(self.position_label)
        layout.addLayout(position_row)
        self.setCentralWidget(central)

    def _start_refresh_timer(self) -> None:
        self.timer = QtCore.QTimer(self)
        self.timer.timeout.connect(self._refresh_preview)
        self.timer.start(self.refresh_ms)

    def _load_report(self) -> None:
        report = self.session.report
        if report is None:
            return
        self._loading = True
        try:
            params = report["params"]
            self.default_cps.setText(str(params["default_cps"]))
            self.min_duration.setValue(int(params["min_duration_ms"]))
            self.min_gap.setValue(int(params["min_gap_ms"]))
            self.priority.setValue(int(params["sentence_priority"]))
            entries = report["sentences"]
            self.table.setRowCount(len(entries))
            for row, entry in enumerate(entries):
                values = [
                    entry["sentence_id"],
                    entry["start"],
                    entry["end"],
                    entry["speaker"],
                    entry["cps"],
                    entry["verdict"]["status"],
                    entry["text"],
                    entry["entry_hash"][:16],
                ]
                for column, value in enumerate(values):
                    item = QtWidgets.QTableWidgetItem(str(value))
                    if column in (0, 5, 7):
                        item.setFlags(item.flags() & ~QtCore.Qt.ItemFlag.ItemIsEditable)
                    self.table.setItem(row, column, item)
            duration = max(1, int(self.session._wav.frame_count))
            self.position.setRange(0, duration)
            window = 240_000
            self.timeline.window_samples = window
            self.status.setText(
                f"revision={report['revision']} commit={report['commit_hash'][:16]} "
                f"affected={report['counts']['affected']} reused={report['counts']['reused']} "
                f"pending={'yes' if self.session.has_pending else 'no'}"
            )
        finally:
            self._loading = False
        self._refresh_preview()

    def _refresh_preview(self) -> None:
        if self._loading:
            return
        position = int(self.position.value())
        try:
            snapshot = self.session.refresh(position)
        except BusinessError:
            return
        self.timeline.set_snapshot(
            {
                "markers": self.session.report["speaker_markers"] if self.session.report else [],
                "sentences": snapshot["sentences"],
            },
            position,
            snapshot["window"][1] - snapshot["window"][0],
        )
        self.position_label.setText(
            f"#{snapshot['refresh_seq']} rev={snapshot['revision']} "
            f"{snapshot['window_timecodes'][0]} - {snapshot['window_timecodes'][1]}"
        )

    def _cell_changed(self, item: Any) -> None:
        if self._loading:
            return
        column = item.column()
        fields = {1: "start", 2: "end", 3: "speaker", 4: "cps", 6: "text"}
        field = fields.get(column)
        if field is None:
            return
        sentence_id = self.table.item(item.row(), 0).text()
        try:
            self.session.stage_sentence(sentence_id, {field: item.text()})
        except BusinessError as exc:
            QtWidgets.QMessageBox.warning(self, exc.code, exc.message)
            self._load_report()
        else:
            self.status.setText(
                f"revision={self.session.revision} pending=yes last-edit={sentence_id}.{field}"
            )

    def _stage_parameters(self) -> None:
        try:
            self.session.stage_params(
                {
                    "default_cps": self.default_cps.text().strip(),
                    "min_duration_ms": self.min_duration.value(),
                    "min_gap_ms": self.min_gap.value(),
                    "sentence_priority": self.priority.value(),
                }
            )
        except BusinessError as exc:
            QtWidgets.QMessageBox.warning(self, exc.code, exc.message)
        else:
            self.status.setText(f"revision={self.session.revision} pending=yes parameters")

    def _insert_description(self) -> None:
        row = self.table.currentRow()
        if row < 0:
            QtWidgets.QMessageBox.information(self, "描述轨", "请先选择一句作为锚点")
            return
        anchor = self.table.item(row, 0).text()
        text, accepted = QtWidgets.QInputDialog.getMultiLineText(
            self, "插入无障碍描述", f"锚点 {anchor} 之后的描述文本"
        )
        if not accepted or not text.strip():
            return
        existing = list(self.session.asset.descriptions)
        next_index = len(existing) + 1
        while any(item.id == f"D{next_index:04d}" for item in existing):
            next_index += 1
        description = DescriptionInput(
            id=f"D{next_index:04d}",
            after_sentence_id=anchor,
            text=text.strip(),
            speaker="NARRATOR",
            max_cps=self.session.params.default_cps,
            min_duration_ms=400,
            priority=70,
        )
        self.session.stage_descriptions([*existing, description])
        self.status.setText(f"revision={self.session.revision} pending=yes description={description.id}")

    def _commit_async(self) -> None:
        if self.commit_thread is not None and self.commit_thread.isRunning():
            return
        self.commit_button.setEnabled(False)
        self.commit_thread = CommitThread(self.session)
        self.commit_thread.succeeded.connect(self._commit_succeeded)
        self.commit_thread.failed.connect(self._commit_failed)
        self.commit_thread.finished.connect(lambda: self.commit_button.setEnabled(True))
        self.commit_thread.start()

    def _commit_succeeded(self, report: Any) -> None:
        del report
        self._load_report()

    def _commit_failed(self, error: Any) -> None:
        QtWidgets.QMessageBox.warning(
            self,
            str(error.get("code", "E_INVALID_INPUT")),
            str(error.get("message", "commit failed")),
        )
        self._load_report()

    def _discard(self) -> None:
        self.session.discard_pending()
        self._load_report()

    def closeEvent(self, event: Any) -> None:
        self.timer.stop()
        if self.commit_thread is not None and self.commit_thread.isRunning():
            self.session.request_cancel()
            self.commit_thread.wait(5000)
        self.session.close()
        super().closeEvent(event)


def run_ui(case_dir: Path, *, refresh_ms: int = 250) -> int:
    app = QtWidgets.QApplication.instance() or QtWidgets.QApplication(sys.argv[:1])
    window = MainWindow(case_dir, refresh_ms=refresh_ms)
    window.show()
    return int(app.exec())

