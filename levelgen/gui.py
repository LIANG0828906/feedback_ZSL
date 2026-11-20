"""PySide6 workbench for editing, generating, tracing and replaying levels."""

from __future__ import annotations

import argparse
import json
import sys
import threading
from pathlib import Path
from typing import Any

from .canonical import read_json, write_json_atomic
from .errors import E_BUSY, E_CANCELLED, LevelGenError
from .generator import generate
from .models import Level
from .service import write_generation_outputs
from .trace import replay_trace
from .workbench import SingleTaskGate


try:
    from PySide6.QtCore import QObject, QPointF, QRectF, Qt, QThread, Signal, Slot
    from PySide6.QtGui import QBrush, QColor, QFont, QPainter, QPen, QPolygonF
    from PySide6.QtWidgets import (
        QAbstractItemView,
        QApplication,
        QComboBox,
        QFileDialog,
        QFormLayout,
        QHBoxLayout,
        QHeaderView,
        QLabel,
        QLineEdit,
        QListWidget,
        QListWidgetItem,
        QMainWindow,
        QMessageBox,
        QPushButton,
        QSpinBox,
        QSplitter,
        QTableWidget,
        QTableWidgetItem,
        QTextEdit,
        QVBoxLayout,
        QWidget,
    )

    PYSIDE_AVAILABLE = True
except ImportError:
    PYSIDE_AVAILABLE = False


CONSTRAINT_KINDS = (
    "room_count",
    "connectivity",
    "key_chain",
    "end_locked",
    "area_target",
    "door_count_target",
)


if PYSIDE_AVAILABLE:

    class GenerationWorker(QObject):
        finished = Signal(object)

        def __init__(
            self,
            mode: str,
            payload: Any,
            output_dir: str,
        ) -> None:
            super().__init__()
            self.mode = mode
            self.payload = payload
            self.output_dir = output_dir
            self.cancel_event = threading.Event()

        def cancel(self) -> None:
            self.cancel_event.set()

        @Slot()
        def run(self) -> None:
            try:
                if self.mode == "generate":
                    result = generate(
                        self.payload,
                        cancel_event=self.cancel_event,
                    )
                    write_generation_outputs(result, self.output_dir)
                    self.finished.emit(
                        {
                            "code": result.code,
                            "error": (
                                None if result.error is None else result.error.to_dict()
                            ),
                            "level": (
                                None if result.level is None else result.level.to_dict()
                            ),
                            "mode": "generate",
                            "ok": result.ok,
                            "trace": result.trace,
                        }
                    )
                    return

                trace = read_json(self.payload)
                replay = replay_trace(trace)
                destination = Path(self.output_dir)
                destination.mkdir(parents=True, exist_ok=True)
                (destination / "level.json").unlink(missing_ok=True)
                if replay.ok and replay.level is not None:
                    write_json_atomic(
                        destination / "level.json",
                        replay.level.to_dict(),
                    )
                elif replay.conflict is not None:
                    write_json_atomic(
                        destination / "conflict.json",
                        replay.conflict,
                    )
                self.finished.emit(
                    {
                        "code": None,
                        "error": None,
                        "level": (
                            None if replay.level is None else replay.level.to_dict()
                        ),
                        "mode": "replay",
                        "ok": replay.ok,
                        "trace": replay.trace,
                    }
                )
            except LevelGenError as error:
                self.finished.emit(
                    {
                        "code": error.code,
                        "error": error.to_dict(),
                        "level": None,
                        "mode": self.mode,
                        "ok": False,
                        "trace": None,
                    }
                )
            except (json.JSONDecodeError, TypeError, ValueError) as error:
                wrapped = {
                    "code": "E_TRACE_INVALID",
                    "message": f"轨迹无法读取或解析: {error}",
                    "point": str(self.payload),
                }
                self.finished.emit(
                    {
                        "code": wrapped["code"],
                        "error": wrapped,
                        "level": None,
                        "mode": self.mode,
                        "ok": False,
                        "trace": None,
                    }
                )
            except Exception as error:
                wrapped = {
                    "code": "E_INTERNAL",
                    "message": f"工作线程失败: {error}",
                    "point": "worker",
                }
                self.finished.emit(
                    {
                        "code": wrapped["code"],
                        "error": wrapped,
                        "level": None,
                        "mode": self.mode,
                        "ok": False,
                        "trace": None,
                    }
                )


    class LevelCanvas(QWidget):
        def __init__(self) -> None:
            super().__init__()
            self.level: Level | None = None
            self.conflict_rooms: set[int] = set()
            self.conflict_doors: set[int] = set()
            self.setMinimumSize(520, 520)

        def set_level(
            self,
            level: Level | None,
            conflict: dict[str, Any] | None = None,
        ) -> None:
            self.level = level
            self.conflict_rooms = set()
            self.conflict_doors = set()
            if conflict is not None:
                evidence = conflict.get("evidence", {})
                self.conflict_rooms = set(evidence.get("rooms", []))
                self.conflict_doors = set(evidence.get("doors", []))
            self.update()

        def _room_color(self, room_id: int) -> QColor:
            if self.level is None:
                return QColor("#d9e2ec")
            kinds = {
                label.kind
                for label in self.level.labels
                if label.room == room_id
            }
            if "start" in kinds:
                return QColor("#9ae6b4")
            if "end" in kinds:
                return QColor("#90cdf4")
            if "lock" in kinds:
                return QColor("#feb2b2")
            if "key" in kinds:
                return QColor("#faf089")
            if "reward" in kinds:
                return QColor("#e9d8fd")
            return QColor("#e2e8f0")

        def paintEvent(self, event: Any) -> None:
            painter = QPainter(self)
            painter.setRenderHint(QPainter.Antialiasing, True)
            painter.fillRect(self.rect(), QColor("#f7fafc"))
            if self.level is None:
                painter.setPen(QColor("#4a5568"))
                painter.drawText(
                    self.rect(),
                    Qt.AlignCenter,
                    "编辑约束后点击“生成”",
                )
                return

            margin = 24.0
            available_width = max(1.0, self.width() - 2 * margin)
            available_height = max(1.0, self.height() - 2 * margin)
            cell_size = min(
                available_width / self.level.grid_w,
                available_height / self.level.grid_h,
            )

            def point(grid_x: float, grid_y: float) -> QPointF:
                return QPointF(
                    margin + grid_x * cell_size,
                    margin + grid_y * cell_size,
                )

            painter.setPen(QPen(QColor("#edf2f7"), 1))
            for grid_x in range(self.level.grid_w + 1):
                painter.drawLine(
                    point(grid_x, 0),
                    point(grid_x, self.level.grid_h),
                )
            for grid_y in range(self.level.grid_h + 1):
                painter.drawLine(
                    point(0, grid_y),
                    point(self.level.grid_w, grid_y),
                )

            room_rects: dict[int, QRectF] = {}
            for room in self.level.rooms:
                top_left = point(room.x, room.y)
                room_rect = QRectF(
                    top_left.x(),
                    top_left.y(),
                    room.w * cell_size,
                    room.h * cell_size,
                )
                room_rects[room.id] = room_rect
                painter.setBrush(QBrush(self._room_color(room.id)))
                if room.id in self.conflict_rooms:
                    painter.setPen(QPen(QColor("#e53e3e"), 4))
                else:
                    painter.setPen(QPen(QColor("#2d3748"), 1.5))
                painter.drawRect(room_rect)

            painter.setPen(QPen(QColor("#4a5568"), 2))
            for door in self.level.doors:
                if door.a not in room_rects or door.b not in room_rects:
                    continue
                if door.id in self.conflict_doors:
                    painter.setPen(QPen(QColor("#dd6b20"), 4))
                else:
                    painter.setPen(QPen(QColor("#4a5568"), 2))
                center_a = room_rects[door.a].center()
                center_b = room_rects[door.b].center()
                painter.drawLine(center_a, center_b)
                if door.dir != "BIDIR":
                    start = center_a if door.dir == "AB" else center_b
                    finish = center_b if door.dir == "AB" else center_a
                    direction_x = finish.x() - start.x()
                    direction_y = finish.y() - start.y()
                    length = max(1.0, (direction_x**2 + direction_y**2) ** 0.5)
                    unit_x = direction_x / length
                    unit_y = direction_y / length
                    tip = QPointF(
                        finish.x() - unit_x * cell_size * 0.35,
                        finish.y() - unit_y * cell_size * 0.35,
                    )
                    normal_x = -unit_y
                    normal_y = unit_x
                    arrow = QPolygonF(
                        [
                            tip,
                            QPointF(
                                tip.x() - unit_x * 9 + normal_x * 5,
                                tip.y() - unit_y * 9 + normal_y * 5,
                            ),
                            QPointF(
                                tip.x() - unit_x * 9 - normal_x * 5,
                                tip.y() - unit_y * 9 - normal_y * 5,
                            ),
                        ]
                    )
                    painter.setBrush(QColor("#4a5568"))
                    painter.drawPolygon(arrow)

            font = QFont()
            font.setPointSize(max(7, min(12, int(cell_size * 0.6))))
            painter.setFont(font)
            labels_by_room: dict[int, list[str]] = {}
            for label in self.level.labels:
                text = label.kind
                if label.tag:
                    text = f"{label.kind}:{label.tag}"
                labels_by_room.setdefault(label.room, []).append(text)
            for room_id, texts in labels_by_room.items():
                painter.setPen(QColor("#1a202c"))
                rect = room_rects[room_id]
                painter.drawText(
                    rect.adjusted(3, 2, -3, -2),
                    Qt.AlignCenter | Qt.TextWordWrap,
                    "\n".join(texts),
                )
            for room_id, rect in room_rects.items():
                painter.setPen(QColor("#718096"))
                painter.drawText(
                    rect.adjusted(4, 1, -4, -1),
                    Qt.AlignTop | Qt.AlignLeft,
                    str(room_id),
                )
            painter.end()


    class WorkbenchWindow(QMainWindow):
        def __init__(self) -> None:
            super().__init__()
            self.setWindowTitle("离线关卡确定性工作台")
            self.resize(1280, 820)
            self._thread: QThread | None = None
            self._worker: GenerationWorker | None = None
            self._task_active = False
            self._gate = SingleTaskGate()
            self._current_trace: dict[str, Any] | None = None
            self._current_level: Level | None = None
            self._build_ui()
            self._add_default_constraints()

        def _build_ui(self) -> None:
            central = QWidget()
            root_layout = QHBoxLayout(central)
            splitter = QSplitter(Qt.Horizontal)
            root_layout.addWidget(splitter)

            self.canvas = LevelCanvas()
            splitter.addWidget(self.canvas)

            sidebar = QWidget()
            sidebar_layout = QVBoxLayout(sidebar)
            form = QFormLayout()
            self.seed_input = QSpinBox()
            self.seed_input.setRange(0, 2**31 - 1)
            self.seed_input.setValue(2401)
            self.grid_w_input = QSpinBox()
            self.grid_w_input.setRange(4, 128)
            self.grid_w_input.setValue(24)
            self.grid_h_input = QSpinBox()
            self.grid_h_input.setRange(4, 128)
            self.grid_h_input.setValue(24)
            self.rewards_input = QSpinBox()
            self.rewards_input.setRange(0, 64)
            self.rewards_input.setValue(1)
            self.attempts_input = QSpinBox()
            self.attempts_input.setRange(1, 1000)
            self.attempts_input.setValue(32)
            form.addRow("种子", self.seed_input)
            form.addRow("网格宽度", self.grid_w_input)
            form.addRow("网格高度", self.grid_h_input)
            form.addRow("奖励数量", self.rewards_input)
            form.addRow("最大尝试次数", self.attempts_input)
            sidebar_layout.addLayout(form)

            sidebar_layout.addWidget(QLabel("约束（params 为 JSON）"))
            self.constraint_table = QTableWidget(0, 4)
            self.constraint_table.setHorizontalHeaderLabels(
                ["ID", "类型", "硬约束", "params"]
            )
            self.constraint_table.horizontalHeader().setSectionResizeMode(
                0,
                QHeaderView.ResizeToContents,
            )
            self.constraint_table.horizontalHeader().setSectionResizeMode(
                1,
                QHeaderView.ResizeToContents,
            )
            self.constraint_table.horizontalHeader().setSectionResizeMode(
                2,
                QHeaderView.ResizeToContents,
            )
            self.constraint_table.horizontalHeader().setSectionResizeMode(
                3,
                QHeaderView.Stretch,
            )
            self.constraint_table.setSelectionBehavior(
                QAbstractItemView.SelectRows
            )
            sidebar_layout.addWidget(self.constraint_table, 2)

            constraint_buttons = QHBoxLayout()
            add_constraint = QPushButton("添加约束")
            add_constraint.clicked.connect(self._add_constraint)
            remove_constraint = QPushButton("删除约束")
            remove_constraint.clicked.connect(self._remove_constraint)
            constraint_buttons.addWidget(add_constraint)
            constraint_buttons.addWidget(remove_constraint)
            sidebar_layout.addLayout(constraint_buttons)

            output_row = QHBoxLayout()
            self.output_input = QLineEdit(str(Path("gui_out").resolve()))
            browse_output = QPushButton("输出目录")
            browse_output.clicked.connect(self._choose_output_dir)
            output_row.addWidget(self.output_input, 1)
            output_row.addWidget(browse_output)
            sidebar_layout.addLayout(output_row)

            action_row = QHBoxLayout()
            self.generate_button = QPushButton("生成")
            self.generate_button.clicked.connect(
                lambda: self.start_generation()
            )
            self.cancel_button = QPushButton("取消")
            self.cancel_button.clicked.connect(self.cancel_generation)
            self.cancel_button.setEnabled(False)
            self.replay_button = QPushButton("复放轨迹")
            self.replay_button.clicked.connect(self._choose_trace_and_replay)
            action_row.addWidget(self.generate_button)
            action_row.addWidget(self.cancel_button)
            action_row.addWidget(self.replay_button)
            sidebar_layout.addLayout(action_row)

            self.status_label = QLabel("空闲")
            self.status_label.setWordWrap(True)
            sidebar_layout.addWidget(self.status_label)

            self.trace_list = QListWidget()
            self.trace_list.currentRowChanged.connect(self._show_trace_entry)
            sidebar_layout.addWidget(QLabel("轨迹条目"))
            sidebar_layout.addWidget(self.trace_list, 2)

            self.trace_detail = QTextEdit()
            self.trace_detail.setReadOnly(True)
            sidebar_layout.addWidget(self.trace_detail, 2)

            splitter.addWidget(sidebar)
            splitter.setSizes([760, 520])
            self.setCentralWidget(central)

        def _add_default_constraints(self) -> None:
            defaults = [
                ("c_rooms", "room_count", True, '{"min": 8, "max": 10}'),
                ("c_conn", "connectivity", True, "{}"),
                ("c_chain", "key_chain", True, '{"keys": ["K1", "K2"]}'),
                ("c_end", "end_locked", True, "{}"),
                ("c_area", "area_target", False, '{"target": 120, "tolerance": 60}'),
                (
                    "c_doors",
                    "door_count_target",
                    False,
                    '{"target": 10, "tolerance": 4}',
                ),
            ]
            for identifier, kind, hard, params in defaults:
                self._append_constraint(identifier, kind, hard, params)

        def _append_constraint(
            self,
            identifier: str,
            kind: str,
            hard: bool,
            params: str,
        ) -> None:
            row = self.constraint_table.rowCount()
            self.constraint_table.insertRow(row)
            self.constraint_table.setItem(row, 0, QTableWidgetItem(identifier))
            kind_combo = QComboBox()
            kind_combo.addItems(CONSTRAINT_KINDS)
            kind_combo.setCurrentText(kind)
            self.constraint_table.setCellWidget(row, 1, kind_combo)
            hard_item = QTableWidgetItem("")
            hard_item.setFlags(
                Qt.ItemIsUserCheckable
                | Qt.ItemIsEnabled
                | Qt.ItemIsSelectable
            )
            hard_item.setCheckState(Qt.Checked if hard else Qt.Unchecked)
            self.constraint_table.setItem(row, 2, hard_item)
            self.constraint_table.setItem(row, 3, QTableWidgetItem(params))

        @Slot()
        def _add_constraint(self) -> None:
            row = self.constraint_table.rowCount()
            self._append_constraint(
                f"c_custom_{row}",
                "room_count",
                True,
                '{"min": 6, "max": 12}',
            )

        @Slot()
        def _remove_constraint(self) -> None:
            row = self.constraint_table.currentRow()
            if row >= 0:
                self.constraint_table.removeRow(row)

        @Slot()
        def _choose_output_dir(self) -> None:
            selected = QFileDialog.getExistingDirectory(
                self,
                "选择输出目录",
                self.output_input.text(),
            )
            if selected:
                self.output_input.setText(selected)

        @Slot()
        def _choose_trace_and_replay(self) -> None:
            selected, _ = QFileDialog.getOpenFileName(
                self,
                "选择轨迹",
                str(Path(self.output_input.text()) / "trace.json"),
                "JSON (*.json)",
            )
            if selected:
                code = self.start_replay(selected)
                if code == E_BUSY:
                    self.status_label.setText(E_BUSY)

        def build_request(self) -> dict[str, Any]:
            constraints: list[dict[str, Any]] = []
            for row in range(self.constraint_table.rowCount()):
                identifier_item = self.constraint_table.item(row, 0)
                kind_widget = self.constraint_table.cellWidget(row, 1)
                hard_item = self.constraint_table.item(row, 2)
                params_item = self.constraint_table.item(row, 3)
                if identifier_item is None or params_item is None:
                    continue
                identifier = identifier_item.text().strip()
                kind = (
                    kind_widget.currentText()
                    if isinstance(kind_widget, QComboBox)
                    else CONSTRAINT_KINDS[0]
                )
                try:
                    params = json.loads(params_item.text() or "{}")
                except json.JSONDecodeError as error:
                    raise ValueError(
                        f"第 {row + 1} 行 params 不是合法 JSON: {error}"
                    ) from error
                constraints.append(
                    {
                        "hard": (
                            hard_item is not None
                            and hard_item.checkState() == Qt.Checked
                        ),
                        "id": identifier,
                        "kind": kind,
                        "params": params,
                    }
                )
            return {
                "constraints": constraints,
                "params": {
                    "grid_h": self.grid_h_input.value(),
                    "grid_w": self.grid_w_input.value(),
                    "max_attempts": self.attempts_input.value(),
                    "rewards": self.rewards_input.value(),
                },
                "seed": self.seed_input.value(),
            }

        def start_generation(
            self,
            request: dict[str, Any] | None = None,
        ) -> str | None:
            if self._gate.active:
                self.status_label.setText(E_BUSY)
                return E_BUSY
            try:
                selected_request = request or self.build_request()
            except ValueError as error:
                self.status_label.setText(f"E_INPUT_INVALID: {error}")
                return "E_INPUT_INVALID"
            gate_code = self._gate.try_start()
            if gate_code == E_BUSY:
                self.status_label.setText(E_BUSY)
                return E_BUSY
            self._start_worker("generate", selected_request)
            return None

        def start_replay(self, trace_path: str) -> str | None:
            if self._gate.active:
                self.status_label.setText(E_BUSY)
                return E_BUSY
            gate_code = self._gate.try_start()
            if gate_code == E_BUSY:
                self.status_label.setText(E_BUSY)
                return E_BUSY
            self._start_worker("replay", trace_path)
            return None

        def _start_worker(self, mode: str, payload: Any) -> None:
            self._task_active = True
            self.generate_button.setEnabled(False)
            self.cancel_button.setEnabled(True)
            self.replay_button.setEnabled(False)
            self.status_label.setText(
                "后台生成中…" if mode == "generate" else "后台复放中…"
            )
            self._worker = GenerationWorker(
                mode,
                payload,
                self.output_input.text(),
            )
            self._thread = QThread(self)
            self._worker.moveToThread(self._thread)
            self._thread.started.connect(self._worker.run)
            self._worker.finished.connect(self._on_worker_finished)
            self._worker.finished.connect(self._thread.quit)
            self._thread.finished.connect(self._on_thread_finished)
            self._thread.start()

        @Slot()
        def cancel_generation(self) -> None:
            if self._worker is not None and self._task_active:
                self._worker.cancel()
                self.status_label.setText(f"{E_CANCELLED}: 已请求协同取消")

        @Slot(object)
        def _on_worker_finished(self, payload: dict[str, Any]) -> None:
            self._current_trace = payload.get("trace")
            level_data = payload.get("level")
            if isinstance(level_data, dict):
                self._current_level = Level.from_dict(level_data)
            else:
                self._current_level = None
            error = payload.get("error")
            conflict = None
            if isinstance(error, dict) and isinstance(error.get("report"), dict):
                conflict = error["report"]
            self.canvas.set_level(self._current_level, conflict)
            self._populate_trace(self._current_trace)
            if payload.get("ok"):
                self.status_label.setText("完成：关卡与轨迹已写出")
            else:
                code = payload.get("code") or "E_UNKNOWN"
                message = ""
                if isinstance(error, dict):
                    message = str(error.get("message", ""))
                self.status_label.setText(f"{code}: {message}")
                if conflict is not None:
                    evidence = conflict.get("evidence", {})
                    self.status_label.setText(
                        f"{code}: 冲突房间 {evidence.get('rooms', [])}，"
                        f"门 {evidence.get('doors', [])}"
                    )

        @Slot()
        def _on_thread_finished(self) -> None:
            if self._thread is not None:
                self._thread.deleteLater()
            if self._worker is not None:
                self._worker.deleteLater()
            self._thread = None
            self._worker = None
            self._task_active = False
            self._gate.finish()
            self.generate_button.setEnabled(True)
            self.cancel_button.setEnabled(False)
            self.replay_button.setEnabled(True)

        def _populate_trace(self, trace: dict[str, Any] | None) -> None:
            self.trace_list.clear()
            if not trace:
                self.trace_detail.clear()
                return
            for entry in trace.get("entries", []):
                label = (
                    f"{entry.get('seq'):>3}  {entry.get('type')}"
                    f"  {entry.get('stage', '')}"
                )
                item = QListWidgetItem(label)
                item.setData(Qt.UserRole, entry)
                self.trace_list.addItem(item)
            if self.trace_list.count() > 0:
                self.trace_list.setCurrentRow(0)

        @Slot(int)
        def _show_trace_entry(self, row: int) -> None:
            item = self.trace_list.item(row)
            if item is None:
                self.trace_detail.clear()
                return
            entry = item.data(Qt.UserRole)
            self.trace_detail.setPlainText(
                json.dumps(entry, ensure_ascii=False, indent=2, sort_keys=True)
            )

        def closeEvent(self, event: Any) -> None:
            self.cancel_generation()
            if self._thread is not None:
                self._thread.quit()
                self._thread.wait(3000)
            super().closeEvent(event)


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description="Level generation workbench")
    parser.add_argument("--offscreen-smoke", action="store_true")
    parser.add_argument("--request")
    parser.add_argument("--trace")
    parser.add_argument("--out", default="gui_out")
    return parser


def main(argv: list[str] | None = None) -> int:
    if not PYSIDE_AVAILABLE:
        print(
            json.dumps(
                {
                    "code": "E_GUI_UNAVAILABLE",
                    "message": "PySide6 不可导入",
                },
                ensure_ascii=False,
                sort_keys=True,
            )
        )
        return 4
    args = build_parser().parse_args(argv)
    app = QApplication.instance() or QApplication(sys.argv[:1])
    window = WorkbenchWindow()
    if args.offscreen_smoke:
        window.output_input.setText(args.out)
        if args.trace:
            code = window.start_replay(args.trace)
            smoke_mode = "replay"
        elif args.request:
            request = read_json(args.request)
            code = window.start_generation(request)
            smoke_mode = "generate"
        else:
            print("--offscreen-smoke 需要 --request 或 --trace")
            return 2
        if code == E_BUSY:
            print(E_BUSY)
            return 2

        def finish_smoke() -> None:
            if not window._gate.active:
                app.quit()
            else:
                from PySide6.QtCore import QTimer

                QTimer.singleShot(100, finish_smoke)

        from PySide6.QtCore import QTimer

        QTimer.singleShot(100, finish_smoke)
        app.exec()
        print(
            json.dumps(
                {
                    "level_loaded": window._current_level is not None,
                    "mode": smoke_mode,
                    "status": window.status_label.text(),
                },
                ensure_ascii=False,
                sort_keys=True,
            )
        )
        return 0 if window._current_level is not None else 2
    window.show()
    return app.exec()


if __name__ == "__main__":
    raise SystemExit(main())
