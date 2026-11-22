from __future__ import annotations

import argparse
import json
import os
import sys
from pathlib import Path
from typing import Any, Callable, Sequence

from PySide6.QtCore import QThread, Signal
from PySide6.QtGui import QAction, QColor, QFont, QFontDatabase
from PySide6.QtWidgets import (
    QApplication,
    QComboBox,
    QDoubleSpinBox,
    QFormLayout,
    QGridLayout,
    QGroupBox,
    QHBoxLayout,
    QHeaderView,
    QLabel,
    QMainWindow,
    QMessageBox,
    QPlainTextEdit,
    QPushButton,
    QSpinBox,
    QTableWidget,
    QTableWidgetItem,
    QTabWidget,
    QToolBar,
    QVBoxLayout,
    QWidget,
)

from ..errors import ErrorCode, HapticError
from ..models import PacketState
from ..pipeline import RunResult, VerificationSummary
from .controller import OperationCancelled, StopPauseGate, WorkbenchController


class TaskThread(QThread):
    succeeded = Signal(object)
    failed = Signal(str)
    cancelled = Signal()

    def __init__(self, function: Callable[[], Any], parent: QWidget | None = None):
        super().__init__(parent)
        self.function = function

    def run(self) -> None:
        try:
            result = self.function()
        except OperationCancelled:
            self.cancelled.emit()
        except Exception as exc:
            if isinstance(exc, HapticError):
                self.failed.emit(json.dumps(exc.issue.to_dict(), ensure_ascii=False))
            else:
                self.failed.emit(str(exc))
        else:
            self.succeeded.emit(result)


def _install_cjk_font() -> None:
    """Use a Windows CJK font when running inside WSL.

    Native Windows sessions already resolve Microsoft YaHei/SimHei through Qt.
    The explicit WSL path keeps the diagnostic workbench readable in local
    verification screenshots as well.
    """

    windir = Path(os.environ.get("WINDIR", "/mnt/c/Windows"))
    candidates = (
        windir / "Fonts" / "msyh.ttc",
        windir / "Fonts" / "simhei.ttf",
        windir / "Fonts" / "Deng.ttf",
        Path("/usr/share/fonts/opentype/noto/NotoSansCJK-Regular.ttc"),
    )
    for candidate in candidates:
        if not candidate.exists():
            continue
        font_id = QFontDatabase.addApplicationFont(str(candidate))
        if font_id < 0:
            continue
        families = QFontDatabase.applicationFontFamilies(font_id)
        if families:
            QApplication.setFont(QFont(families[0], 9))
            return


def _table(headers: Sequence[str]) -> QTableWidget:
    table = QTableWidget(0, len(headers))
    table.setHorizontalHeaderLabels(list(headers))
    table.setSelectionBehavior(QTableWidget.SelectRows)
    table.setSelectionMode(QTableWidget.SingleSelection)
    table.setAlternatingRowColors(True)
    table.verticalHeader().setVisible(False)
    table.horizontalHeader().setSectionResizeMode(QHeaderView.ResizeToContents)
    table.horizontalHeader().setStretchLastSection(True)
    return table


def _set_row(table: QTableWidget, row: int, values: Sequence[Any], color: QColor | None = None) -> None:
    for column, value in enumerate(values):
        item = QTableWidgetItem("" if value is None else str(value))
        if color is not None:
            item.setBackground(color)
        table.setItem(row, column, item)


class MainWindow(QMainWindow):
    def __init__(self, workdir: str, port: int = 0):
        super().__init__()
        _install_cjk_font()
        self.setWindowTitle("触觉事件调度内核 · 诊断工作台")
        self.resize(1680, 1020)
        self.gate = StopPauseGate()
        self.controller = WorkbenchController(workdir, port)
        self.task: TaskThread | None = None
        self.pending_restart = False
        self._build_ui()
        self._populate_inputs()
        self.statusBar().showMessage(
            f"回环设备 127.0.0.1:{self.controller.port} · 检查点 {Path(workdir).resolve()}"
        )

    def _build_ui(self) -> None:
        toolbar = QToolBar("运行", self)
        toolbar.setMovable(False)
        self.addToolBar(toolbar)
        self.run_action = QAction("▶ 实时运行", self)
        self.run_action.triggered.connect(self.start_realtime)
        toolbar.addAction(self.run_action)
        self.verify_action = QAction("⇄ 三路径验证", self)
        self.verify_action.triggered.connect(self.start_verification)
        toolbar.addAction(self.verify_action)
        self.pause_action = QAction("Ⅱ 暂停", self)
        self.pause_action.triggered.connect(self.toggle_pause)
        toolbar.addAction(self.pause_action)
        self.resume_action = QAction("↻ 断点恢复", self)
        self.resume_action.triggered.connect(self.start_resume)
        toolbar.addAction(self.resume_action)
        self.restart_action = QAction("⟳ 重启续跑", self)
        self.restart_action.triggered.connect(self.request_restart)
        toolbar.addAction(self.restart_action)
        self.retry_action = QAction("↺ 重试选中包", self)
        self.retry_action.triggered.connect(self.retry_selected)
        toolbar.addAction(self.retry_action)
        self.export_action = QAction("⇩ 导出报告", self)
        self.export_action.triggered.connect(self.export_report)
        toolbar.addAction(self.export_action)

        central = QWidget(self)
        outer = QVBoxLayout(central)
        outer.setContentsMargins(8, 8, 8, 8)
        outer.addWidget(self._build_edit_bar())

        grid = QGridLayout()
        grid.setSpacing(8)
        left_top = QTabWidget()
        self.event_table = _table(
            ["事件", "帧", "帧内序", "通道", "类型", "强度", "优先级", "禁止打断"]
        )
        self.event_table.itemSelectionChanged.connect(self._event_selection_changed)
        left_top.addTab(self.event_table, "事件流时间轴")
        self.decision_table = _table(["事件", "决策", "依据", "目标包", "原因", "优先级"])
        left_top.addTab(self.decision_table, "逐事件调度决策")
        grid.addWidget(left_top, 0, 0)

        right_top = QWidget()
        right_top_layout = QVBoxLayout(right_top)
        right_top_layout.setContentsMargins(0, 0, 0, 0)
        self.packet_table = _table(
            ["包序", "通道", "本地序", "起点ms", "事件数", "字节数", "状态", "复用", "失效", "回显"]
        )
        self.packet_table.itemSelectionChanged.connect(self._packet_selection_changed)
        right_top_layout.addWidget(QLabel("逐通道事件包与状态"))
        right_top_layout.addWidget(self.packet_table)
        self.invalidation_table = _table(["包序", "通道", "失效事件", "原因"])
        right_top_layout.addWidget(QLabel("失效传播与复用"))
        right_top_layout.addWidget(self.invalidation_table, 1)
        grid.addWidget(right_top, 0, 1)

        bottom_left = QWidget()
        bottom_left_layout = QVBoxLayout(bottom_left)
        bottom_left_layout.setContentsMargins(0, 0, 0, 0)
        self.byte_view = QPlainTextEdit()
        self.byte_view.setReadOnly(True)
        self.byte_view.setFont(QFont("Monospace", 9))
        self.byte_view.setPlaceholderText("选中包查看指令包字节与回显对齐详情")
        bottom_left_layout.addWidget(QLabel("指令包字节、CRC 与回显"))
        bottom_left_layout.addWidget(self.byte_view)
        self.alignment_table = _table(["包序", "通道", "状态", "首次时刻", "Attempt", "详情"])
        bottom_left_layout.addWidget(self.alignment_table)
        grid.addWidget(bottom_left, 1, 0)

        bottom_right = QWidget()
        bottom_right_layout = QVBoxLayout(bottom_right)
        bottom_right_layout.setContentsMargins(0, 0, 0, 0)
        self.error_table = _table(["代码", "包序", "通道", "字段", "首次时刻", "信息"])
        bottom_right_layout.addWidget(QLabel("异常分类明细（双击或选中后使用重试）"))
        bottom_right_layout.addWidget(self.error_table)
        self.comparison_table = _table(["路径", "一致", "字段", "包序", "通道", "两侧取值"])
        bottom_right_layout.addWidget(QLabel("三路径自动比对"))
        bottom_right_layout.addWidget(self.comparison_table)
        grid.addWidget(bottom_right, 1, 1)
        grid.setColumnStretch(0, 1)
        grid.setColumnStretch(1, 1)
        grid.setRowStretch(0, 1)
        grid.setRowStretch(1, 1)
        outer.addLayout(grid, 1)
        self.setCentralWidget(central)

        self.summary_label = QLabel("就绪")
        self.summary_label.setStyleSheet("padding: 4px; background: #e8eef5; border-radius: 4px;")
        outer.addWidget(self.summary_label)

    def _build_edit_bar(self) -> QWidget:
        container = QWidget()
        layout = QHBoxLayout(container)
        layout.setContentsMargins(0, 0, 0, 0)
        event_box = QGroupBox("事件优先级编辑")
        event_form = QFormLayout(event_box)
        self.edit_event_id = QComboBox()
        self.edit_priority = QSpinBox()
        self.edit_priority.setRange(0, 255)
        self.apply_priority = QPushButton("应用并增量失效")
        self.apply_priority.clicked.connect(self.apply_event_priority)
        row = QHBoxLayout()
        row.addWidget(self.edit_event_id)
        row.addWidget(self.edit_priority)
        row.addWidget(self.apply_priority)
        event_form.addRow("事件 / 优先级", row)
        layout.addWidget(event_box)

        cap_box = QGroupBox("通道能力参数编辑")
        cap_form = QFormLayout(cap_box)
        self.cap_channel = QComboBox()
        self.cap_field = QComboBox()
        self.cap_field.addItems(["min_event_interval_ms", "max_events_per_packet", "packet_window_ms"])
        self.cap_value = QDoubleSpinBox()
        self.cap_value.setRange(0, 10000)
        self.cap_value.setDecimals(2)
        self.apply_cap = QPushButton("应用并增量失效")
        self.apply_cap.clicked.connect(self.apply_capability)
        cap_row = QHBoxLayout()
        cap_row.addWidget(self.cap_channel)
        cap_row.addWidget(self.cap_field)
        cap_row.addWidget(self.cap_value)
        cap_row.addWidget(self.apply_cap)
        cap_form.addRow("通道 / 参数 / 值", cap_row)
        layout.addWidget(cap_box)
        return container

    def _populate_inputs(self) -> None:
        self.event_table.setRowCount(0)
        for row, event in enumerate(self.controller.stream.events):
            self.event_table.insertRow(row)
            _set_row(
                self.event_table,
                row,
                [
                    event.event_id,
                    event.frame,
                    event.frame_index,
                    event.channel,
                    event.event_type,
                    f"{event.intensity:.2f}",
                    event.priority,
                    "是" if event.no_interrupt else "否",
                ],
            )
        self.edit_event_id.clear()
        self.edit_event_id.addItems([event.event_id for event in self.controller.stream.events])
        self.cap_channel.clear()
        self.cap_channel.addItems(
            [str(channel.channel) for channel in self.controller.capabilities.channels]
        )

    def _busy(self) -> bool:
        return self.task is not None and self.task.isRunning()

    def _start_task(self, function: Callable[[], Any], label: str) -> None:
        if self._busy():
            return
        self.gate = StopPauseGate()
        gate = self.gate
        self.task = TaskThread(lambda: function(), self)
        self.task.succeeded.connect(lambda result: self._task_succeeded(result, label))
        self.task.failed.connect(self._task_failed)
        self.task.cancelled.connect(self._task_cancelled)
        self.task.finished.connect(self._task_finished)
        self.run_action.setEnabled(False)
        self.verify_action.setEnabled(False)
        self.statusBar().showMessage(f"{label}中…")
        self.task.start()

    def start_realtime(self) -> None:
        self.pending_restart = False
        self._start_task(lambda: self.controller.run_realtime(self.gate), "实时调度、编码与下发")

    def start_resume(self) -> None:
        self.pending_restart = False
        self._start_task(lambda: self.controller.run_resume(self.gate), "断点恢复下发")

    def start_verification(self) -> None:
        self.pending_restart = False
        self._start_task(self.controller.verify, "三路径自动验证")

    def request_restart(self) -> None:
        self.pending_restart = True
        if self._busy():
            self.gate.stop()
            self.statusBar().showMessage("正在停止并保存当前窗口…")
        else:
            self.start_resume()

    def toggle_pause(self) -> None:
        if not self._busy():
            return
        if self.gate.paused:
            self.gate.resume()
            self.pause_action.setText("Ⅱ 暂停")
            self.statusBar().showMessage("已继续；检查点前缀保持可恢复")
        else:
            self.gate.pause()
            self.pause_action.setText("▶ 继续")
            self.statusBar().showMessage("已暂停于持久化窗口边界")

    def _task_succeeded(self, result: Any, label: str) -> None:
        if isinstance(result, VerificationSummary):
            self.controller.verification = result
            self.controller.result = result.realtime
            self.controller.incremental = None
            self._render_verification(result)
            self.statusBar().showMessage(f"{label}完成：三路径{'一致' if result.ok else '存在分歧'}")
        else:
            self._render_result(result)
            self.statusBar().showMessage(f"{label}完成：{len(result.packets)} 包")
        self.summary_label.setText(self.statusBar().currentMessage())

    def _task_failed(self, message: str) -> None:
        self.summary_label.setText(f"失败：{message}")
        self.statusBar().showMessage("任务失败")
        QMessageBox.warning(self, "执行失败", message)

    def _task_cancelled(self) -> None:
        self.statusBar().showMessage("已停止；已提交窗口可用于断点恢复")

    def _task_finished(self) -> None:
        self.task = None
        self.run_action.setEnabled(True)
        self.verify_action.setEnabled(True)
        self.pause_action.setText("Ⅱ 暂停")
        if self.pending_restart:
            self.pending_restart = False
            self.start_resume()

    def _render_result(self, result: RunResult) -> None:
        self._render_packets()
        self._render_decisions()
        self._render_alignment(result)
        self._render_invalidations()
        self._render_errors(result)
        self.summary_label.setText(
            f"{result.mode} · 包 {len(result.packets)} · 决策 {len(result.decisions)} · "
            f"回显 {dict(result.alignment.stats)} · 复用 {result.reused_count}"
        )

    def _render_verification(self, summary: VerificationSummary) -> None:
        self._render_result(summary.realtime)
        self.comparison_table.setRowCount(0)
        for row, comparison in enumerate(summary.comparisons):
            self.comparison_table.insertRow(row)
            divergence = comparison.divergence
            values = ""
            if divergence is not None:
                values = f"{divergence.left!r} / {divergence.right!r}"
            _set_row(
                self.comparison_table,
                row,
                [
                    f"{comparison.left_mode} ⇄ {comparison.right_mode}",
                    "一致" if comparison.equal else "E_DIVERGE",
                    "" if divergence is None else divergence.field,
                    "" if divergence is None else divergence.packet_seq,
                    "" if divergence is None else divergence.channel,
                    values,
                ],
                QColor("#d8f5dd") if comparison.equal else QColor("#ffd8d8"),
            )
        self.summary_label.setText(
            f"三路径验证：{'一致' if summary.ok else '不一致'} · 服务异常 {', '.join(summary.service_codes)}"
        )

    def _render_packets(self) -> None:
        self.packet_table.setRowCount(0)
        packets = self.controller.current_packets()
        result = self.controller.result
        for row, packet in enumerate(packets):
            status = packet.state.value
            echo_status = "-"
            if self.controller.incremental is not None:
                if packet.invalidated:
                    status = f"{status}/INVALIDATED"
                elif packet.reused:
                    status = f"{status}/REUSED"
            elif result is not None:
                observation = result.alignment.packet(packet.packet_seq)
                echo_status = "-" if observation is None else observation.status
            self.packet_table.insertRow(row)
            color = None
            if packet.invalidated:
                color = QColor("#ffd8d8")
            elif packet.reused:
                color = QColor("#d8ecff")
            elif status.startswith("CONFIRMED"):
                color = QColor("#d8f5dd")
            elif status.startswith("FAILED"):
                color = QColor("#ffe4b8")
            _set_row(
                self.packet_table,
                row,
                [
                    packet.packet_seq,
                    packet.channel,
                    packet.local_index,
                    f"{packet.timing_mark_ms:.0f}",
                    packet.event_count,
                    len(packet.data),
                    status,
                    "是" if packet.reused else "否",
                    "是" if packet.invalidated else "否",
                    echo_status,
                ],
                color,
            )
        if self.packet_table.rowCount() > 0 and self.packet_table.currentRow() < 0:
            self.packet_table.selectRow(0)

    def _render_decisions(self) -> None:
        self.decision_table.setRowCount(0)
        for row, decision in enumerate(self.controller.current_decisions()):
            self.decision_table.insertRow(row)
            color = None
            if decision.decision_type.value == "DISCARD":
                color = QColor("#ffe4b8")
            elif decision.decision_type.value == "INTERRUPT":
                color = QColor("#ffd8d8")
            elif decision.decision_type.value == "MERGE":
                color = QColor("#e2e7ff")
            _set_row(
                self.decision_table,
                row,
                [
                    decision.event_id,
                    decision.decision_type.value,
                    ", ".join(decision.basis_event_ids),
                    decision.target_packet_seq,
                    decision.reason,
                    decision.priority,
                ],
                color,
            )

    def _render_alignment(self, result: RunResult) -> None:
        self.alignment_table.setRowCount(0)
        for row, packet in enumerate(result.alignment.packets):
            issue = packet.issue
            self.alignment_table.insertRow(row)
            color = QColor("#d8f5dd") if issue is None else QColor("#ffd8d8")
            _set_row(
                self.alignment_table,
                row,
                [
                    packet.packet_seq,
                    packet.channel,
                    packet.status,
                    "" if packet.first_seen_ms() is None else f"{packet.first_seen_ms():.3f}",
                    packet.echoes[0].attempt if packet.echoes else "-",
                    "" if issue is None else issue.message,
                ],
                color,
            )

    def _render_errors(self, result: RunResult) -> None:
        self.error_table.setRowCount(0)
        for row, issue in enumerate(result.alignment.issues):
            self.error_table.insertRow(row)
            _set_row(
                self.error_table,
                row,
                [
                    issue.code.value,
                    issue.packet_seq,
                    issue.channel,
                    issue.field,
                    "" if issue.first_seen_ms is None else f"{issue.first_seen_ms:.3f}",
                    issue.message,
                ],
                QColor("#ffd8d8"),
            )

    def _render_invalidations(self) -> None:
        self.invalidation_table.setRowCount(0)
        invalidations = (
            self.controller.incremental.invalidations
            if self.controller.incremental is not None
            else ()
        )
        for row, item in enumerate(invalidations):
            self.invalidation_table.insertRow(row)
            _set_row(
                self.invalidation_table,
                row,
                [
                    item.packet_seq,
                    item.channel,
                    ", ".join(item.invalidated_event_ids),
                    f"{item.reason.value} ({item.cause_event_id or '-'})",
                ],
                QColor("#ffe4b8"),
            )

    def _clear_transient_alignment(self) -> None:
        self.alignment_table.setRowCount(0)
        self.error_table.setRowCount(0)
        self.comparison_table.setRowCount(0)
        self.byte_view.clear()
        self.byte_view.setPlaceholderText("增量更新后请重新运行以生成新的下发回显与对齐记录")

    def _event_selection_changed(self) -> None:
        row = self.event_table.currentRow()
        if 0 <= row < len(self.controller.stream.events):
            event = self.controller.stream.events[row]
            self.edit_event_id.setCurrentText(event.event_id)
            self.edit_priority.setValue(event.priority)

    def _packet_selection_changed(self) -> None:
        row = self.packet_table.currentRow()
        packets = self.controller.current_packets()
        if not (0 <= row < len(packets)):
            return
        packet = packets[row]
        lines = [
            f"packet_seq={packet.packet_seq} channel={packet.channel} local_index={packet.local_index}",
            f"state={packet.state.value} reused={packet.reused} invalidated={packet.invalidated}",
            f"length={len(packet.data)} sha256={packet.digest}",
            "",
            packet.data.hex(" "),
        ]
        result = self.controller.result
        if result is not None:
            observation = result.alignment.packet(packet.packet_seq)
            if observation is not None:
                lines.extend(
                    [
                        "",
                        f"alignment={observation.status}",
                        f"first_seen_ms={observation.first_seen_ms()}",
                    ]
                )
                for echo in observation.echoes:
                    lines.append(
                        f"echo seq={echo.received_seq} ch={echo.channel} "
                        f"delivered={echo.delivered_index} processed={echo.processed_at_ms} "
                        f"attempt={echo.attempt}"
                    )
        self.byte_view.setPlainText("\n".join(lines))

    def _require_graph(self) -> bool:
        if self.controller._dependencies() is None:
            QMessageBox.information(self, "需要基线", "请先执行一次实时运行或断点恢复，再编辑。")
            return False
        return True

    def apply_event_priority(self) -> None:
        if not self._require_graph():
            return
        event_id = self.edit_event_id.currentText()
        try:
            self.controller.edit_priority(event_id, self.edit_priority.value())
        except Exception as exc:
            QMessageBox.warning(self, "编辑失败", str(exc))
            return
        self._populate_inputs()
        self._render_packets()
        self._render_decisions()
        self._render_invalidations()
        self._clear_transient_alignment()
        self.summary_label.setText(
            f"增量更新：{event_id} 优先级={self.edit_priority.value()}，"
            f"失效 {len(self.controller.incremental.invalidations)} 包，"
            f"复用 {self.controller.incremental.reused_count} 包"
        )

    def apply_capability(self) -> None:
        if not self._require_graph():
            return
        channel = int(self.cap_channel.currentText())
        field = self.cap_field.currentText()
        value = self.cap_value.value()
        try:
            if field == "min_event_interval_ms":
                self.controller.edit_capability(
                    channel, min_event_interval_ms=value
                )
            elif field == "max_events_per_packet":
                self.controller.edit_capability(
                    channel, max_events_per_packet=max(1, int(value))
                )
            else:
                self.controller.edit_capability(
                    channel, packet_window_ms=(0.0, max(1.0, value))
                )
        except Exception as exc:
            QMessageBox.warning(self, "编辑失败", str(exc))
            return
        self._render_packets()
        self._render_decisions()
        self._render_invalidations()
        self._clear_transient_alignment()
        self.summary_label.setText(
            f"能力参数更新：通道 {channel} {field}={value}，"
            f"失效 {len(self.controller.incremental.invalidations)} 包，"
            f"复用 {self.controller.incremental.reused_count} 包"
        )

    def retry_selected(self) -> None:
        if self.controller.result is None:
            return
        row = self.error_table.currentRow()
        if row < 0 or row >= len(self.controller.result.alignment.issues):
            QMessageBox.information(self, "选择异常", "请先在异常表中选择一个包异常。")
            return
        issue = self.controller.result.alignment.issues[row]
        if issue.packet_seq is None:
            return
        if issue.code == ErrorCode.E_LOCAL:
            QMessageBox.information(
                self,
                "不可重试",
                "E_LOCAL 是本地内核侧包序违规，不能通过服务重试修复。",
            )
            return
        try:
            result = self.controller.retry(issue.packet_seq)
        except Exception as exc:
            QMessageBox.warning(self, "重试失败", str(exc))
            return
        self._render_packets()
        self._render_alignment(result)
        self._render_errors(result)
        self.summary_label.setText(
            f"包 {issue.packet_seq} 单包重试完成；其他包记录与统计未重算"
        )

    def export_report(self) -> None:
        target = Path(self.controller.workdir) / "workbench_report.json"
        payload: dict[str, Any] = {"port": self.controller.port}
        if self.controller.result is not None:
            payload["result"] = self.controller.result.to_dict()
        if self.controller.incremental is not None:
            payload["incremental"] = self.controller.incremental.to_dict()
        if self.controller.verification is not None:
            payload["verification"] = self.controller.verification.to_dict()
        target.write_text(json.dumps(payload, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
        self.statusBar().showMessage(f"报告已写入 {target}")

    def closeEvent(self, event) -> None:
        self.gate.stop()
        if self.task is not None and self.task.isRunning():
            self.task.wait(3000)
        self.controller.close()
        event.accept()


def main(argv: Sequence[str] | None = None) -> int:
    parser = argparse.ArgumentParser(prog="haptic-workbench")
    parser.add_argument("--workdir", default="artifacts/workbench")
    parser.add_argument("--port", type=int, default=0, help="loopback port; 0 selects a free port")
    args = parser.parse_args(argv)
    app = QApplication.instance() or QApplication(sys.argv[:1])
    try:
        window = MainWindow(args.workdir, args.port)
    except HapticError as exc:
        QMessageBox.critical(
            None,
            "回环端口不可用",
            f"{exc.issue.code.value}: {exc.issue.message}\n"
            f"请使用 --port 0 或选择其他空闲端口。",
        )
        return 2
    window.show()
    return int(app.exec())


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))

