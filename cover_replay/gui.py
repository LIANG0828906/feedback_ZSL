"""Optional PySide6 desktop review surface.

The desktop is intentionally a consumer of the same deterministic engine used
by the scripted acceptance path. It never weakens configured worker or memory
limits.
"""

from __future__ import annotations

import os
import sys
import threading
from pathlib import Path
from typing import Any

from PySide6.QtCore import QThread, QTimer, Qt, Signal
from PySide6.QtGui import QColor, QFont, QPainter, QPen
from PySide6.QtWidgets import (
    QApplication,
    QComboBox,
    QDoubleSpinBox,
    QFileDialog,
    QGridLayout,
    QGroupBox,
    QHBoxLayout,
    QLabel,
    QListWidget,
    QMainWindow,
    QMessageBox,
    QPushButton,
    QSlider,
    QSplitter,
    QTableWidget,
    QTableWidgetItem,
    QVBoxLayout,
    QWidget,
)

from .baseline import compare_baseline
from .config import load_config
from .engine import ReplayEngine, ReplayRun, Withdrawal
from .errors import BusinessError
from .model import load_record
from .util import load_json


class ReplayWorker(QThread):
    completed = Signal(object)
    failed = Signal(object)
    progress = Signal(int)

    def __init__(
        self,
        sample: Path,
        *,
        resume: bool,
        withdrawals: tuple[Withdrawal, ...],
    ) -> None:
        super().__init__()
        self.sample = sample
        self.resume = resume
        self.withdrawals = withdrawals
        self.cancel_event = threading.Event()

    def run(self) -> None:
        try:
            config = load_config(self.sample / "config.json")
            record = load_record(self.sample / "match.json", config)
            snapshot_dir = self.sample / ".gui_snapshots"
            if not self.resume:
                _clear_snapshots(snapshot_dir)
            engine = ReplayEngine(
                record,
                config,
                snapshot_dir=snapshot_dir,
                resume=self.resume,
                withdrawals=self.withdrawals,
                allow_conflicts=True,
            )
            run = engine.run(cancel_event=self.cancel_event)
            self.progress.emit(len(run.frames))
            self.completed.emit(
                {
                    "run": run,
                    "config": config,
                    "record": record,
                    "engine": engine,
                }
            )
        except BusinessError as error:
            self.failed.emit(error.to_dict())
        except Exception as error:
            self.failed.emit(
                {
                    "code": "E_INTERNAL",
                    "message": str(error),
                }
            )


def _clear_snapshots(directory: Path) -> None:
    if not directory.exists():
        directory.mkdir(parents=True, exist_ok=True)
        return
    for item in directory.iterdir():
        if item.is_file() and (
            item.name.startswith("step-") or item.name.endswith(".tmp")
        ):
            item.unlink()


class CreditCurve(QWidget):
    def __init__(self) -> None:
        super().__init__()
        self.frames: list[dict[str, Any]] = []
        self.current_step = 1
        self.setMinimumHeight(210)

    def set_data(
        self, frames: list[dict[str, Any]], current_step: int
    ) -> None:
        self.frames = frames
        self.current_step = current_step
        self.update()

    def paintEvent(self, event: Any) -> None:  # noqa: N802 - Qt API
        del event
        painter = QPainter(self)
        painter.setRenderHint(QPainter.Antialiasing)
        painter.fillRect(self.rect(), QColor("#101820"))
        margin_left = 45
        margin_right = 12
        margin_top = 18
        margin_bottom = 26
        plot = self.rect().adjusted(
            margin_left, margin_top, -margin_right, -margin_bottom
        )
        painter.setPen(QPen(QColor("#32414f"), 1))
        painter.drawRect(plot)
        if not self.frames:
            painter.setPen(QColor("#9fb0bf"))
            painter.drawText(plot, Qt.AlignCenter, "尚无挂账数据")
            return
        block_ids = sorted(
            {cover["block_id"] for cover in self.frames[-1]["covers"]}
        )
        palette = [
            "#44c2a8",
            "#f2a65a",
            "#6fa8ff",
            "#d67ad2",
            "#e5d45b",
            "#ef6f6c",
            "#88c057",
            "#b0a1ff",
        ]
        max_step = max(1, len(self.frames))
        for index, block_id in enumerate(block_ids):
            points: list[tuple[int, int]] = []
            for frame in self.frames:
                credit = next(
                    (
                        item
                        for item in frame["credits"]
                        if item["block_id"] == block_id
                    ),
                    None,
                )
                if credit is None:
                    continue
                max_remaining = next(
                    (
                        cover
                        for cover in frame["covers"]
                        if cover["block_id"] == block_id
                    ),
                    None,
                )
                denominator = max(
                    1.0,
                    float(max_remaining["accumulated_damage"])
                    + float(max_remaining["remaining"]),
                )
                fraction = float(credit["predicted_remaining"]) / denominator
                x = plot.left() + int(
                    (frame["step"] - 1) * plot.width() / max(1, max_step - 1)
                )
                y = plot.bottom() - int(fraction * plot.height())
                points.append((x, y))
            if len(points) >= 2:
                painter.setPen(QPen(QColor(palette[index % len(palette)]), 2))
                painter.drawPolyline([self._point(x, y) for x, y in points])
        painter.setPen(QColor("#9fb0bf"))
        painter.setFont(QFont("Sans Serif", 8))
        painter.drawText(
            plot.adjusted(4, 2, -4, -2),
            Qt.AlignTop | Qt.AlignLeft,
            "客户端预测剩余耐久 / 步",
        )

    @staticmethod
    def _point(x: int, y: int) -> Any:
        from PySide6.QtCore import QPoint

        return QPoint(x, y)


class ReviewWindow(QMainWindow):
    def __init__(self, initial_sample: str | None = None) -> None:
        super().__init__()
        self.setWindowTitle("掩体对局确定性复核台")
        self.resize(1180, 820)
        self.worker: ReplayWorker | None = None
        self.run: ReplayRun | None = None
        self.frames: list[dict[str, Any]] = []
        self.withdrawals: list[Withdrawal] = []
        self.current_step = 1
        self.timer = QTimer(self)
        self.timer.setInterval(500)
        self.timer.timeout.connect(self._next_frame)
        self._build_ui()
        self._discover_samples(initial_sample)

    def _build_ui(self) -> None:
        central = QWidget()
        root = QVBoxLayout(central)
        sample_row = QHBoxLayout()
        sample_row.addWidget(QLabel("样本目录"))
        self.sample_combo = QComboBox()
        self.sample_combo.setMinimumWidth(520)
        sample_row.addWidget(self.sample_combo, 1)
        browse = QPushButton("选择目录")
        browse.clicked.connect(self._browse_sample)
        sample_row.addWidget(browse)
        root.addLayout(sample_row)

        controls = QHBoxLayout()
        self.start_button = QPushButton("启动")
        self.start_button.clicked.connect(lambda: self._start_replay(False))
        self.pause_button = QPushButton("暂停")
        self.pause_button.clicked.connect(self._toggle_pause)
        self.resume_button = QPushButton("从快照恢复")
        self.resume_button.clicked.connect(lambda: self._start_replay(True))
        self.cancel_button = QPushButton("协作式取消")
        self.cancel_button.clicked.connect(self._cancel)
        self.withdraw_button = QPushButton("撤回注入")
        self.withdraw_button.clicked.connect(self._inject_withdrawal)
        self.amount_spin = QDoubleSpinBox()
        self.amount_spin.setRange(0.0, 100000.0)
        self.amount_spin.setDecimals(3)
        self.amount_spin.setValue(10.0)
        self.amount_spin.setPrefix("补偿 ")
        for button in (
            self.start_button,
            self.pause_button,
            self.resume_button,
            self.cancel_button,
            self.withdraw_button,
        ):
            controls.addWidget(button)
        controls.addWidget(self.amount_spin)
        controls.addStretch(1)
        root.addLayout(controls)

        timeline_box = QGroupBox("物理步时间轴")
        timeline_layout = QVBoxLayout(timeline_box)
        self.step_label = QLabel("未加载")
        self.slider = QSlider(Qt.Horizontal)
        self.slider.valueChanged.connect(self._show_step)
        timeline_layout.addWidget(self.step_label)
        timeline_layout.addWidget(self.slider)
        root.addWidget(timeline_box)

        splitter = QSplitter(Qt.Vertical)
        top = QSplitter(Qt.Horizontal)
        self.cover_table = QTableWidget(0, 5)
        self.cover_table.setHorizontalHeaderLabels(
            ["块编号", "累计损伤", "剩余耐久", "开裂", "开裂步"]
        )
        self.queue_list = QListWidget()
        queue_box = QGroupBox("调度等待与预算")
        queue_layout = QVBoxLayout(queue_box)
        self.budget_label = QLabel("水位：-")
        queue_layout.addWidget(self.budget_label)
        queue_layout.addWidget(self.queue_list)
        top.addWidget(self.cover_table)
        top.addWidget(queue_box)
        top.setSizes([760, 380])
        splitter.addWidget(top)

        bottom = QSplitter(Qt.Horizontal)
        curve_box = QGroupBox("挂账曲线")
        curve_layout = QVBoxLayout(curve_box)
        self.curve = CreditCurve()
        curve_layout.addWidget(self.curve)
        diff_box = QGroupBox("差异列表")
        diff_layout = QVBoxLayout(diff_box)
        self.diff_table = QTableWidget(0, 5)
        self.diff_table.setHorizontalHeaderLabels(
            ["步号", "业务码", "字段", "基线/预测", "实际"]
        )
        diff_layout.addWidget(self.diff_table)
        bottom.addWidget(curve_box)
        bottom.addWidget(diff_box)
        bottom.setSizes([670, 470])
        splitter.addWidget(bottom)
        root.addWidget(splitter, 1)

        self.status = self.statusBar()
        self.status.showMessage("就绪")
        self.setCentralWidget(central)

    def _discover_samples(self, initial: str | None) -> None:
        candidates = []
        if initial:
            candidates.append(Path(initial))
        for base in (
            Path("artifacts/acceptance/samples"),
            Path("samples"),
        ):
            if base.exists():
                for child in sorted(base.iterdir()):
                    if (child / "match.json").exists():
                        candidates.append(child)
        seen: set[str] = set()
        for candidate in candidates:
            resolved = str(candidate.resolve())
            if resolved in seen:
                continue
            seen.add(resolved)
            if (candidate / "match.json").exists():
                self.sample_combo.addItem(resolved, resolved)
        if self.sample_combo.count() == 0 and initial:
            self.sample_combo.addItem(initial, initial)

    def _browse_sample(self) -> None:
        selected = QFileDialog.getExistingDirectory(self, "选择本地样本目录")
        if selected:
            self.sample_combo.addItem(selected, selected)
            self.sample_combo.setCurrentIndex(self.sample_combo.count() - 1)

    def _selected_sample(self) -> Path | None:
        value = self.sample_combo.currentData()
        return Path(value) if value else None

    def _start_replay(self, resume: bool) -> None:
        sample = self._selected_sample()
        if sample is None:
            QMessageBox.warning(self, "缺少样本", "请先选择包含 match/config 的样本目录。")
            return
        if self.worker is not None and self.worker.isRunning():
            self.worker.cancel_event.set()
            Qt_wait = 3000
            if not self.worker.wait(Qt_wait):
                QMessageBox.warning(self, "仍在取消", "工作线程尚未结束，请稍后重试。")
                return
        self.timer.stop()
        self.status.showMessage("从快照恢复中..." if resume else "确定性重放中...")
        worker = ReplayWorker(
            sample,
            resume=resume,
            withdrawals=tuple(self.withdrawals),
        )
        worker.completed.connect(self._replay_completed)
        worker.failed.connect(self._replay_failed)
        worker.finished.connect(worker.deleteLater)
        worker.finished.connect(self._worker_finished)
        self.worker = worker
        worker.start()

    def _replay_completed(self, payload: dict[str, Any]) -> None:
        self.run = payload["run"]
        self.frames = list(self.run.frames)
        if not self.frames:
            self.status.showMessage("没有可提交的物理步")
            return
        self.slider.blockSignals(True)
        self.slider.setRange(1, len(self.frames))
        self.slider.setValue(1)
        self.slider.blockSignals(False)
        self.current_step = 1
        self._load_differences()
        self._show_step(1)
        if self.run.complete:
            self.status.showMessage(
                f"完成 {len(self.frames)} 步，最终哈希 {self.run.final_state_hash}"
            )
        else:
            self.status.showMessage(
                f"已协作取消于第 {self.run.interrupted_step} 步；快照可恢复"
            )
        self.timer.start()

    def _worker_finished(self) -> None:
        self.worker = None

    def _replay_failed(self, error: dict[str, Any]) -> None:
        self.status.showMessage(f"{error.get('code')}: {error.get('message')}")
        self.diff_table.setRowCount(0)
        self._append_diff(
            [
                str(error.get("step", "-")),
                str(error.get("code", "E_INTERNAL")),
                str(error.get("field", error.get("field_path", "$"))),
                "-",
                str(error.get("message", "")),
            ]
        )

    def _load_differences(self) -> None:
        self.diff_table.setRowCount(0)
        sample = self._selected_sample()
        if sample is None or self.run is None:
            return
        baseline_path = sample / "baseline.json"
        if baseline_path.exists():
            try:
                baseline = load_json(baseline_path)
                compare_baseline(self.run, baseline)
            except BusinessError as error:
                detail = error.to_dict()
                self._append_diff(
                    [
                        str(detail.get("step", "-")),
                        str(detail.get("code", "")),
                        str(detail.get("field_path", detail.get("field", "$"))),
                        str(detail.get("baseline_value", "-")),
                        str(detail.get("replay_value", "-")),
                    ]
                )
        for conflict in self.run.conflicts:
            detail = conflict.to_dict()
            self._append_diff(
                [
                    str(detail["step"]),
                    detail["code"],
                    f"credit/{detail['block_id']}",
                    str(detail["predicted_remaining"]),
                    str(detail["authoritative_remaining"]),
                ]
            )

    def _append_diff(self, values: list[str]) -> None:
        row = self.diff_table.rowCount()
        self.diff_table.insertRow(row)
        for column, value in enumerate(values):
            self.diff_table.setItem(row, column, QTableWidgetItem(value))

    def _show_step(self, position: int) -> None:
        if not self.frames:
            return
        index = max(0, min(position - 1, len(self.frames) - 1))
        self.current_step = index + 1
        frame = self.frames[index]
        self.step_label.setText(
            f"第 {frame['step']} 步 / 共 {len(self.frames)} 步   "
            f"哈希 {frame['state_hash'][:16]}…"
        )
        covers = frame["covers"]
        self.cover_table.setRowCount(len(covers))
        for row, cover in enumerate(covers):
            values = [
                str(cover["block_id"]),
                cover["accumulated_damage"],
                cover["remaining"],
                "是" if cover["cracked"] else "否",
                "-" if cover["cracked_step"] is None else str(cover["cracked_step"]),
            ]
            for column, value in enumerate(values):
                self.cover_table.setItem(row, column, QTableWidgetItem(value))
        budget = frame["budget_usage"]
        self.budget_label.setText(
            f"预算是 {budget['memory_budget']}，峰值 {budget['peak_memory_used']}，"
            f"等待水位 {budget['queue_watermark']}，波次 {budget['wave_count']}"
        )
        self.queue_list.clear()
        for decision in frame["scheduling"]["trace"]:
            if decision["event"] == "submit":
                self.queue_list.addItem(
                    f"波次 {decision['wave']} 提交 {decision['block_ids']}；"
                    f"等待 {decision['waiting_block_ids']}"
                )
        self.curve.set_data(self.frames, self.current_step)

    def _next_frame(self) -> None:
        if not self.frames:
            return
        next_value = self.slider.value() + 1
        if next_value > self.slider.maximum():
            self.timer.stop()
            return
        self.slider.setValue(next_value)

    def _toggle_pause(self) -> None:
        if self.timer.isActive():
            self.timer.stop()
            self.pause_button.setText("继续")
        else:
            self.timer.start()
            self.pause_button.setText("暂停")

    def _cancel(self) -> None:
        if self.worker is not None and self.worker.isRunning():
            self.worker.cancel_event.set()
            self.status.showMessage("已请求协作式取消；已提交快照保留。")
        else:
            self.timer.stop()

    def _inject_withdrawal(self) -> None:
        if not self.frames:
            return
        row = self.cover_table.currentRow()
        if row < 0:
            row = 0
        block_item = self.cover_table.item(row, 0)
        if block_item is None:
            return
        block_id = int(block_item.text())
        self.withdrawals.append(
            Withdrawal(
                step=self.current_step,
                block_id=block_id,
                amount=f"{self.amount_spin.value():.3f}",
                reason="gui-injected-withdrawal",
            )
        )
        self._start_replay(False)


def launch(sample: str | None = None) -> int:
    if (
        os.name != "nt"
        and not os.environ.get("QT_QPA_PLATFORM")
        and not os.environ.get("DISPLAY")
        and not os.environ.get("WAYLAND_DISPLAY")
    ):
        raise BusinessError(
            "E_NO_DISPLAY",
            "no desktop display session is available",
            suggestion="Use run/resume/compare or QT_QPA_PLATFORM=offscreen for smoke tests.",
        )
    app = QApplication.instance() or QApplication(sys.argv)
    window = ReviewWindow(sample)
    window.show()
    return app.exec()
