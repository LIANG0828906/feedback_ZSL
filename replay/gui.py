"""PySide6 图形界面。

调度器运行在后台线程，GUI 仅通过 QTimer 定时读取线程安全的状态快照，
不在界面线程执行回放，因此刷新不会阻塞调度；关闭窗口时 shutdown 会
协作式停止任务并关闭全部文件句柄。
"""

from __future__ import annotations

import sys
from pathlib import Path

from PySide6.QtCore import Qt, QTimer
from PySide6.QtGui import QCloseEvent, QPainter, QPen
from PySide6.QtWidgets import (
    QApplication,
    QHBoxLayout,
    QHeaderView,
    QLabel,
    QMainWindow,
    QMessageBox,
    QPushButton,
    QTableWidget,
    QTableWidgetItem,
    QVBoxLayout,
    QWidget,
)

from .config import load_config
from .hashing import read_json, write_json_atomic
from .model import MatchDescription
from .scheduler import ReplayScheduler
from .verifier import verify


class ResourceTrendWidget(QWidget):
    """用 QPainter 绘制资源占用趋势，避免引入额外绘图依赖。"""

    def __init__(self) -> None:
        super().__init__()
        self._series: dict[str, list[tuple[int, int]]] = {}
        self.setMinimumHeight(180)

    def update_series(self, statuses: list[dict]) -> None:
        # 历史点随刷新累积（仅保存在内存，不写入磁盘，不影响确定性输出）。
        if not hasattr(self, "_history"):
            self._history = {}
        for status in statuses:
            points = self._history.setdefault(status["match_id"], [])
            point = (
                status["current_turn"],
                status["compute_total"],
                status["memory_total"],
            )
            if not points or points[-1] != point:
                points.append(point)
        self.update()

    def paintEvent(self, event) -> None:  # noqa: N802 (Qt 命名)
        painter = QPainter(self)
        painter.setRenderHint(QPainter.RenderHint.Antialiasing)
        width = self.width()
        height = self.height()
        painter.fillRect(0, 0, width, height, Qt.GlobalColor.white)
        history = getattr(self, "_history", {})
        if not history:
            painter.drawText(10, 20, "暂无资源数据")
            return
        max_turn = max(
            (points[-1][0] for points in history.values() if points), default=1
        )
        max_value = max(
            (max(p[1], p[2]) for points in history.values() for p in points),
            default=1,
        )
        max_turn = max(max_turn, 1)
        max_value = max(max_value, 1)
        colors = (Qt.GlobalColor.darkBlue, Qt.GlobalColor.darkRed)
        for index, (match_id, points) in enumerate(sorted(history.items())):
            for series_index in (1, 2):
                pen = QPen(colors[(index + series_index) % 2])
                pen.setWidth(2)
                painter.setPen(pen)
                prev = None
                for turn, compute, memory in points:
                    x = 10 + int((width - 20) * turn / max_turn)
                    value = compute if series_index == 1 else memory
                    y = height - 20 - int((height - 40) * value / max_value)
                    if prev is not None:
                        painter.drawLine(prev[0], prev[1], x, y)
                    prev = (x, y)
        painter.setPen(Qt.GlobalColor.black)
        painter.drawText(10, 15, "蓝线=累计算力  红线=累计内存（按任务刷新）")


class ReplayWindow(QMainWindow):
    def __init__(
        self,
        sample_dir: str,
        baseline_dir: str,
        output_dir: str,
        config_path: str | None,
    ) -> None:
        super().__init__()
        self.setWindowTitle("确定性对局回放器")
        self.sample_dir = Path(sample_dir)
        self.baseline_dir = Path(baseline_dir)
        self.output_dir = Path(output_dir)
        config = load_config(config_path)
        self.scheduler = ReplayScheduler(config, self.output_dir)
        self._build_ui()
        self._timer = QTimer(self)
        self._timer.timeout.connect(self._refresh)
        self._timer.start(200)

    def _build_ui(self) -> None:
        central = QWidget()
        layout = QVBoxLayout(central)

        button_bar = QHBoxLayout()
        for text, handler in (
            ("启动全部", self._start_all),
            ("暂停", self._pause_selected),
            ("恢复", self._resume_selected),
            ("取消", self._cancel_selected),
            ("从快照恢复", self._recover_selected),
            ("验证基线", self._verify_selected),
        ):
            button = QPushButton(text)
            button.clicked.connect(handler)
            button_bar.addWidget(button)
        layout.addLayout(button_bar)

        self.table = QTableWidget(0, 8)
        self.table.setHorizontalHeaderLabels(
            [
                "对局",
                "状态",
                "回合",
                "算力",
                "内存",
                "内存峰值",
                "错误码",
                "提交序号",
            ]
        )
        self.table.horizontalHeader().setSectionResizeMode(
            QHeaderView.ResizeMode.Stretch
        )
        self.table.setSelectionBehavior(
            QTableWidget.SelectionBehavior.SelectRows
        )
        layout.addWidget(self.table, stretch=2)

        layout.addWidget(QLabel("资源占用趋势"))
        self.trend = ResourceTrendWidget()
        layout.addWidget(self.trend, stretch=1)

        layout.addWidget(QLabel("验证差异列表"))
        self.diff_table = QTableWidget(0, 4)
        self.diff_table.setHorizontalHeaderLabels(
            ["对局", "首个差异回合", "字段路径", "业务码/信息"]
        )
        self.diff_table.horizontalHeader().setSectionResizeMode(
            QHeaderView.ResizeMode.Stretch
        )
        layout.addWidget(self.diff_table, stretch=1)
        self.setCentralWidget(central)
        self.resize(980, 760)

    def _selected_match_id(self) -> str | None:
        row = self.table.currentRow()
        if row < 0:
            return None
        return self.table.item(row, 0).text()

    def _start_all(self) -> None:
        manifest = read_json(self.sample_dir / "manifest.json")
        for entry in manifest["matches"]:
            desc = MatchDescription.load(entry["sample"])
            code, _ = self.scheduler.submit(desc)
            if code != "OK":
                QMessageBox.information(self, "提交结果", f"{desc.match_id}: {code}")

    def _pause_selected(self) -> None:
        match_id = self._selected_match_id()
        if match_id:
            self.scheduler.pause(match_id)

    def _resume_selected(self) -> None:
        match_id = self._selected_match_id()
        if match_id:
            self.scheduler.resume_task(match_id)

    def _cancel_selected(self) -> None:
        match_id = self._selected_match_id()
        if match_id:
            self.scheduler.cancel(match_id)

    def _recover_selected(self) -> None:
        match_id = self._selected_match_id()
        if not match_id:
            return
        manifest = read_json(self.sample_dir / "manifest.json")
        entry = next(
            item for item in manifest["matches"] if item["match_id"] == match_id
        )
        code, _ = self.scheduler.submit(
            MatchDescription.load(entry["sample"]), recover=True
        )
        QMessageBox.information(self, "恢复提交", f"{match_id}: {code}")

    def _verify_selected(self) -> None:
        match_id = self._selected_match_id()
        if not match_id:
            return
        baseline = self.baseline_dir / f"{match_id}.baseline.json"
        report = verify(
            match_id, self.output_dir / match_id, baseline
        ).to_dict()
        row = self.diff_table.rowCount()
        self.diff_table.insertRow(row)
        values = [
            match_id,
            str(report.get("first_mismatch_turn")),
            str(report.get("field_path")),
            f"{report['code']} {report['message']}",
        ]
        for col, value in enumerate(values):
            self.diff_table.setItem(row, col, QTableWidgetItem(value))
        write_json_atomic(
            self.output_dir / f"{match_id}.gui_verify.json", report
        )

    def _refresh(self) -> None:
        statuses = self.scheduler.statuses()
        self.table.setRowCount(len(statuses))
        for row, status in enumerate(statuses):
            values = [
                status["match_id"],
                status["status"],
                f"{status['current_turn']}/{status['total_turns']}",
                str(status["compute_total"]),
                str(status["memory_total"]),
                str(status["memory_peak"]),
                status["error_code"] or "",
                str(status["seq"]),
            ]
            for col, value in enumerate(values):
                self.table.setItem(row, col, QTableWidgetItem(value))
        self.trend.update_series(statuses)

    def closeEvent(self, event: QCloseEvent) -> None:  # noqa: N802
        self._timer.stop()
        # 协作式停止调度器并释放文件句柄；磁盘快照不受影响。
        self.scheduler.shutdown()
        event.accept()


def run_gui(
    sample_dir: str,
    baseline_dir: str,
    output_dir: str,
    config_path: str | None,
) -> int:
    app = QApplication(sys.argv)
    window = ReplayWindow(sample_dir, baseline_dir, output_dir, config_path)
    window.show()
    return app.exec()
