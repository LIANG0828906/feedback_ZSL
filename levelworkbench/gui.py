"""PySide6 workbench with a background generation/replay worker."""

from __future__ import annotations

import copy
import json
import threading
from pathlib import Path
from typing import Any

from PySide6.QtCore import QThread, Qt, Signal
from PySide6.QtGui import QColor, QFont, QPen
from PySide6.QtWidgets import (
    QApplication,
    QComboBox,
    QFileDialog,
    QFormLayout,
    QHBoxLayout,
    QLabel,
    QListWidget,
    QMainWindow,
    QMessageBox,
    QPlainTextEdit,
    QPushButton,
    QSpinBox,
    QSplitter,
    QVBoxLayout,
    QWidget,
)

from .config import SUPPORTED_INJECTIONS, load_config, parse_config_document
from .errors import E_BUSY, E_CANCELLED, WorkbenchError
from .models import Level, WorkbenchConfig
from .pipeline import GenerationResult, ReplayResult, generate_from_config, replay_trace
from .session import TaskGate


class GenerationWorker(QThread):
    succeeded = Signal(object)
    failed = Signal(object)

    def __init__(
        self,
        config: WorkbenchConfig | None,
        *,
        trace_document: dict[str, Any] | None = None,
    ) -> None:
        super().__init__()
        self.config = config
        self.trace_document = trace_document
        self.cancel_event = threading.Event()

    def run(self) -> None:
        try:
            if self.trace_document is not None:
                result = replay_trace(self.trace_document, cancel_event=self.cancel_event)
            else:
                if self.config is None:
                    raise WorkbenchError(E_BUSY, "worker has no generation input")
                result = generate_from_config(self.config, cancel_event=self.cancel_event)
            self.succeeded.emit(result)
        except WorkbenchError as exc:
            self.failed.emit(exc)
        except Exception as exc:  # pragma: no cover - UI boundary
            self.failed.emit(
                WorkbenchError(
                    "E_INTERNAL",
                    f"{type(exc).__name__}: {exc}",
                )
            )

    def cancel(self) -> None:
        self.cancel_event.set()
        self.requestInterruption()


class LevelCanvas(QWidget):
    def __init__(self) -> None:
        super().__init__()
        self.setMinimumSize(560, 420)
        self.level: Level | None = None
        self.highlight_rooms: set[str] = set()
        self.highlight_doors: set[str] = set()

    def set_level(
        self,
        level: Level | None,
        *,
        highlight_rooms: set[str] | None = None,
        highlight_doors: set[str] | None = None,
    ) -> None:
        self.level = level
        self.highlight_rooms = highlight_rooms or set()
        self.highlight_doors = highlight_doors or set()
        self.update()

    def paintEvent(self, event: Any) -> None:  # noqa: N802 - Qt API
        from PySide6.QtGui import QPainter, QPolygonF
        from PySide6.QtCore import QPointF

        painter = QPainter(self)
        painter.setRenderHint(QPainter.RenderHint.Antialiasing)
        painter.fillRect(self.rect(), QColor("#f6f7f9"))
        if self.level is None:
            painter.setPen(QColor("#56606a"))
            painter.setFont(QFont("Sans", 13))
            painter.drawText(self.rect(), Qt.AlignmentFlag.AlignCenter, "尚无已提交布局")
            return

        padding = 28
        scale = min(
            (self.width() - 2 * padding) / max(1, self.level.grid_width),
            (self.height() - 2 * padding) / max(1, self.level.grid_height),
        )
        origin_x = (self.width() - self.level.grid_width * scale) / 2
        origin_y = (self.height() - self.level.grid_height * scale) / 2

        painter.setPen(QPen(QColor("#d8dde3"), 1))
        for x in range(self.level.grid_width + 1):
            painter.drawLine(
                int(origin_x + x * scale),
                int(origin_y),
                int(origin_x + x * scale),
                int(origin_y + self.level.grid_height * scale),
            )
        for y in range(self.level.grid_height + 1):
            painter.drawLine(
                int(origin_x),
                int(origin_y + y * scale),
                int(origin_x + self.level.grid_width * scale),
                int(origin_y + y * scale),
            )

        centers: dict[str, tuple[float, float]] = {}
        for room in self.level.rooms:
            x = origin_x + room.x * scale
            y = origin_y + room.y * scale
            w = room.width * scale
            h = room.height * scale
            centers[room.id] = (x + w / 2, y + h / 2)
            tags = self.level.tags_at(room.id)
            kinds = {tag.kind for tag in tags}
            fill = QColor("#dcecff")
            if "start" in kinds:
                fill = QColor("#c8f1d2")
            elif "end" in kinds:
                fill = QColor("#ffd8c7")
            if room.id in self.highlight_rooms:
                fill = QColor("#ffb4b4")
            painter.fillRect(int(x), int(y), int(w), int(h), fill)
            border = QColor("#d94b4b") if room.id in self.highlight_rooms else QColor("#42637f")
            painter.setPen(QPen(border, 2.4 if room.id in self.highlight_rooms else 1.4))
            painter.drawRect(int(x), int(y), int(w), int(h))
            painter.setPen(QColor("#27343f"))
            painter.setFont(QFont("Sans", 8))
            painter.drawText(
                int(x + 3),
                int(y + 12),
                f"{room.id} {room.width}x{room.height}",
            )
            marker = "".join(
                {
                    "start": "S",
                    "end": "E",
                    "key": "K",
                    "lock": "L",
                    "reward": "R",
                }.get(kind, "")
                for kind in sorted(kinds)
            )
            if marker:
                painter.setPen(QColor("#6a4200"))
                painter.drawText(int(x + 4), int(y + h - 5), marker)

        for door in self.level.doors:
            if door.room_a not in centers or door.room_b not in centers:
                continue
            x1, y1 = centers[door.room_a]
            x2, y2 = centers[door.room_b]
            color = QColor("#b33b3b") if door.locked_by else QColor("#4d6b82")
            if door.id in self.highlight_doors:
                color = QColor("#ff3b30")
            pen = QPen(color, 2.2)
            if door.locked_by:
                pen.setStyle(Qt.PenStyle.DashLine)
            painter.setPen(pen)
            painter.drawLine(QPointF(x1, y1), QPointF(x2, y2))
            if door.locked_by:
                painter.setFont(QFont("Sans", 8))
                painter.drawText(QPointF((x1 + x2) / 2, (y1 + y2) / 2 - 3), door.locked_by)
            if door.direction in ("forward", "reverse"):
                sx, sy, tx, ty = (
                    (x1, y1, x2, y2)
                    if door.direction == "forward"
                    else (x2, y2, x1, y1)
                )
                vx, vy = tx - sx, ty - sy
                length = max(1.0, (vx * vx + vy * vy) ** 0.5)
                ux, uy = vx / length, vy / length
                tip = QPointF(sx + vx * 0.58, sy + vy * 0.58)
                left = QPointF(tip.x() - ux * 8 - uy * 4, tip.y() - uy * 8 + ux * 4)
                right = QPointF(tip.x() - ux * 8 + uy * 4, tip.y() - uy * 8 - ux * 4)
                painter.setBrush(color)
                painter.drawPolygon(QPolygonF([tip, left, right]))
                painter.setBrush(Qt.BrushStyle.NoBrush)


class WorkbenchWindow(QMainWindow):
    def __init__(
        self,
        config_document: dict[str, Any],
        *,
        config_path: str | Path | None = None,
    ) -> None:
        super().__init__()
        self.setWindowTitle("Level Generation Workbench")
        self.resize(1280, 820)
        self.config_document = copy.deepcopy(config_document)
        self.config_path = Path(config_path) if config_path else None
        self.gate = TaskGate()
        self.worker: GenerationWorker | None = None
        self.last_committed_trace: dict[str, Any] | None = None
        self.last_committed_result: GenerationResult | None = None
        self.last_replay: ReplayResult | None = None
        self._build_ui()

    def _build_ui(self) -> None:
        splitter = QSplitter(Qt.Orientation.Horizontal)
        self.canvas = LevelCanvas()
        splitter.addWidget(self.canvas)

        side = QWidget()
        side_layout = QVBoxLayout(side)
        form = QFormLayout()
        self.seed_spin = QSpinBox()
        self.seed_spin.setRange(0, 2_147_483_647)
        constraints = self.config_document.get("constraints", [])
        self.seed_spin.setValue(int(self.config_document.get("seed", 0)))
        form.addRow("种子", self.seed_spin)
        self.injection_combo = QComboBox()
        self.injection_combo.addItem("none")
        for name in sorted(SUPPORTED_INJECTIONS):
            self.injection_combo.addItem(name)
        form.addRow("异常注入", self.injection_combo)
        side_layout.addLayout(form)

        side_layout.addWidget(QLabel("约束 JSON（可编辑）"))
        self.constraint_editor = QPlainTextEdit()
        self.constraint_editor.setPlainText(
            json.dumps(constraints, ensure_ascii=False, indent=2)
        )
        side_layout.addWidget(self.constraint_editor, 2)

        button_row = QHBoxLayout()
        self.generate_button = QPushButton("生成")
        self.replay_button = QPushButton("复放最后轨迹")
        self.open_trace_button = QPushButton("加载轨迹")
        self.cancel_button = QPushButton("取消")
        button_row.addWidget(self.generate_button)
        button_row.addWidget(self.replay_button)
        button_row.addWidget(self.open_trace_button)
        button_row.addWidget(self.cancel_button)
        side_layout.addLayout(button_row)

        self.status_label = QLabel("就绪")
        self.status_label.setWordWrap(True)
        side_layout.addWidget(self.status_label)
        side_layout.addWidget(QLabel("轨迹事件"))
        self.trace_list = QListWidget()
        side_layout.addWidget(self.trace_list, 3)
        splitter.addWidget(side)
        splitter.setStretchFactor(0, 3)
        splitter.setStretchFactor(1, 2)
        self.setCentralWidget(splitter)

        self.generate_button.clicked.connect(self.start_generation)
        self.replay_button.clicked.connect(self.start_replay)
        self.open_trace_button.clicked.connect(self.open_trace)
        self.cancel_button.clicked.connect(self.cancel_generation)

    def _edited_document(self) -> dict[str, Any]:
        document = copy.deepcopy(self.config_document)
        document["seed"] = int(self.seed_spin.value())
        try:
            constraints = json.loads(self.constraint_editor.toPlainText())
        except json.JSONDecodeError as exc:
            raise WorkbenchError(
                "E_CONFIG_INVALID",
                f"constraint editor JSON error at position {exc.pos}",
            ) from exc
        if not isinstance(constraints, list):
            raise WorkbenchError("E_CONFIG_INVALID", "constraint editor must contain a JSON array")
        document["constraints"] = constraints
        return document

    def start_generation(self) -> str:
        if not self.gate.try_acquire():
            self.status_label.setText("E_BUSY: 已有任务运行")
            return E_BUSY
        try:
            raw_document = self._edited_document()
            injection = self.injection_combo.currentText()
            if injection != "none":
                load_config(
                    raw_text=json.dumps(raw_document, ensure_ascii=False),
                    injection=injection,
                )
            config = parse_config_document(raw_document)
        except WorkbenchError as exc:
            self.gate.release()
            self._show_error(exc)
            return exc.code
        self.status_label.setText("生成中…")
        self.worker = GenerationWorker(config)
        self._wire_worker()
        self.worker.start()
        return "RUNNING"

    def start_replay(self) -> str:
        if self.last_committed_trace is None:
            self.status_label.setText("没有可复放的已提交轨迹")
            return "E_NO_TRACE"
        if not self.gate.try_acquire():
            self.status_label.setText("E_BUSY: 已有任务运行")
            return E_BUSY
        self.status_label.setText("复放中…")
        self.worker = GenerationWorker(None, trace_document=copy.deepcopy(self.last_committed_trace))
        self._wire_worker()
        self.worker.start()
        return "RUNNING"

    def _wire_worker(self) -> None:
        assert self.worker is not None
        self.worker.succeeded.connect(self._on_success)
        self.worker.failed.connect(self._on_failure)
        self.worker.finished.connect(self._on_worker_finished)

    def _on_success(self, result: Any) -> None:
        if isinstance(result, ReplayResult):
            generation = result.generation
            self.last_replay = result
            status = (
                "复放完成：布局哈希、归因哈希与事件链一致"
                if result.layout_hash_verified
                and result.attribution_hash_verified
                and result.event_chain_verified
                else "复放完成但校验不一致"
            )
        else:
            generation = result
            self.last_committed_result = generation
            self.last_committed_trace = copy.deepcopy(generation.trace)
            status = (
                f"生成完成：{len(generation.level.rooms)} 房间，"
                f"{len(generation.level.doors)} 门，哈希 {generation.layout_hash[:12]}"
            )
        highlight_rooms, highlight_doors = self._highlights(generation)
        self.canvas.set_level(
            generation.level,
            highlight_rooms=highlight_rooms,
            highlight_doors=highlight_doors,
        )
        self.trace_list.clear()
        for event in generation.trace["events"]:
            self.trace_list.addItem(
                f"{event['seq']:04d} {event['stage']} / {event['kind']}"
            )
        self.status_label.setText(status)

    def _on_failure(self, error: WorkbenchError) -> None:
        self._show_error(error)

    def _on_worker_finished(self) -> None:
        if self.worker is not None:
            self.worker.deleteLater()
            self.worker = None
        self.gate.release()

    def _show_error(self, error: WorkbenchError) -> None:
        if error.code == E_CANCELLED:
            self.status_label.setText(
                "E_CANCELLED: 已取消，最后一次已提交轨迹保持不变"
            )
            return
        self.status_label.setText(f"{error.code}: {error.message}")
        QMessageBox.warning(self, error.code, error.message)

    def cancel_generation(self) -> None:
        if self.worker is not None:
            self.worker.cancel()
            self.status_label.setText("取消中…")

    def open_trace(self) -> None:
        path, _ = QFileDialog.getOpenFileName(self, "打开轨迹", "", "JSON (*.json)")
        if not path:
            return
        try:
            from .trace import load_trace_document

            self.last_committed_trace = load_trace_document(path=path)
            self.trace_list.clear()
            for event in self.last_committed_trace["events"]:
                self.trace_list.addItem(
                    f"{event['seq']:04d} {event['stage']} / {event['kind']}"
                )
            self.status_label.setText(f"已加载轨迹：{path}")
        except WorkbenchError as exc:
            self._show_error(exc)

    @staticmethod
    def _highlights(result: GenerationResult) -> tuple[set[str], set[str]]:
        rooms: set[str] = set()
        doors: set[str] = set()
        for constraint_id, item in result.evaluation.results.items():
            if item.get("passed") and not item.get("uncertain", False):
                continue
            evidence = item.get("evidence", {})
            rooms.update(str(value) for value in evidence.get("missing_rooms", []))
            rooms.update(str(value) for value in evidence.get("path", []))
            if evidence.get("end") and evidence.get("end") != "end":
                rooms.add(str(evidence["end"]))
            if evidence.get("start"):
                rooms.add(str(evidence["start"]))
            evidence_doors = evidence.get("doors", [])
            if isinstance(evidence_doors, (list, tuple, set)):
                doors.update(str(value) for value in evidence_doors)
            if item.get("type") == "room_count" and not item.get("passed"):
                rooms.update(room.id for room in result.level.rooms)
            if item.get("type") == "key_chain":
                doors.update(
                    door.id
                    for door in result.level.doors
                    if door.locked_by is not None
                )
        return rooms, doors


def run_gui(config_path: str | Path, *, injection: str | None = None) -> int:
    config = load_config(config_path, injection=injection)
    document = copy.deepcopy(config.raw_document)
    app = QApplication.instance() or QApplication([])
    window = WorkbenchWindow(document, config_path=config_path)
    window.show()
    return app.exec()
