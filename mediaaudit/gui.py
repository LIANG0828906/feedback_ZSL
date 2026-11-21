"""PySide6 诊断界面。

展示各档参数版本时间轴、切片网格、复用/重算着色、回退差异与跨档一致
性告警，支持修改参数、回退、启动重算与协作式取消。无显示环境下使用
``QT_QPA_PLATFORM=offscreen`` 也可实例化（见 smoke_gui）。
"""

from __future__ import annotations

from pathlib import Path

from PySide6 import QtCore, QtGui, QtWidgets

from .engine import AuditEngine
from .errors import AuditError, ErrorCode
from .parameters import TierParams
from .samplegen import generate_samples
from .slicing import CancelledError
from .timebase import load_source

COLOR_REUSE = QtGui.QColor("#b7e4c7")
COLOR_RECOMPUTE = QtGui.QColor("#ffd166")
COLOR_STALE = QtGui.QColor("#ef476f")
COLOR_IDLE = QtGui.QColor("#e9ecef")


class SliceGridWidget(QtWidgets.QWidget):
    """按档位绘制切片网格与复用/重算/陈旧着色。"""

    def __init__(self) -> None:
        super().__init__()
        self._tiers: dict[str, list[tuple[int, str, str | None]]] = {}
        self.setMinimumHeight(220)

    def set_state(
        self, tiers: dict[str, list[tuple[int, str, str | None]]]
    ) -> None:
        self._tiers = tiers
        self.update()

    def paintEvent(self, event: QtGui.QPaintEvent) -> None:  # noqa: N802
        painter = QtGui.QPainter(self)
        painter.fillRect(self.rect(), QtCore.Qt.white)
        margin = 16
        row_h = 54
        for row, (tier, cells) in enumerate(sorted(self._tiers.items())):
            y = margin + row * row_h
            painter.drawText(margin, y + 18, tier)
            cell_w = max(18, (self.width() - 140) // max(1, len(cells)))
            for col, (index, status, reason) in enumerate(cells):
                x = 110 + col * cell_w
                color = {
                    "reuse": COLOR_REUSE,
                    "recompute": COLOR_RECOMPUTE,
                    "stale": COLOR_STALE,
                }.get(status, COLOR_IDLE)
                painter.fillRect(x, y, cell_w - 3, 30, color)
                painter.drawRect(x, y, cell_w - 3, 30)
                painter.drawText(x + 3, y + 20, str(index))


class AuditMainWindow(QtWidgets.QMainWindow):
    def __init__(self, engine: AuditEngine) -> None:
        super().__init__()
        self.engine = engine
        self.setWindowTitle("赛事素材审看诊断台")
        self.resize(1080, 720)
        self._build_ui()
        self.refresh()

    def _build_ui(self) -> None:
        central = QtWidgets.QWidget()
        self.setCentralWidget(central)
        layout = QtWidgets.QVBoxLayout(central)

        self.tier_box = QtWidgets.QComboBox()
        self.tier_box.addItems(["tier_low", "tier_high"])
        layout.addWidget(self.tier_box)

        form = QtWidgets.QFormLayout()
        self.rate_box = QtWidgets.QComboBox()
        self.rate_box.addItems(["16000", "22050", "24000", "32000", "44100", "48000"])
        self.gain_box = QtWidgets.QSpinBox()
        self.gain_box.setRange(0, 512)
        self.gain_box.setValue(256)
        self.quantum_box = QtWidgets.QSpinBox()
        self.quantum_box.setRange(250, 100000)
        self.quantum_box.setSingleStep(250)
        self.quantum_box.setValue(1000)
        self.span_box = QtWidgets.QSpinBox()
        self.span_box.setRange(1000, 5_000_000)
        self.span_box.setSingleStep(1000)
        self.span_box.setValue(500_000)
        form.addRow("目标采样率", self.rate_box)
        form.addRow("增益 Q8", self.gain_box)
        form.addRow("量化刻度(us)", self.quantum_box)
        form.addRow("切片跨度(us)", self.span_box)
        layout.addLayout(form)

        buttons = QtWidgets.QHBoxLayout()
        self.btn_submit = QtWidgets.QPushButton("提交参数并重算")
        self.btn_rollback = QtWidgets.QPushButton("回退到上一版本")
        self.btn_cancel = QtWidgets.QPushButton("协作式取消")
        self.btn_span = QtWidgets.QPushButton("跨档一致性校验")
        for btn in (
            self.btn_submit,
            self.btn_rollback,
            self.btn_cancel,
            self.btn_span,
        ):
            buttons.addWidget(btn)
        layout.addLayout(buttons)

        self.timeline = QtWidgets.QListWidget()
        layout.addWidget(QtWidgets.QLabel("参数版本时间轴"))
        layout.addWidget(self.timeline)

        self.grid = SliceGridWidget()
        layout.addWidget(QtWidgets.QLabel("切片网格（绿=复用 黄=重算 红=陈旧拒绝）"))
        layout.addWidget(self.grid)

        self.alerts = QtWidgets.QPlainTextEdit()
        self.alerts.setReadOnly(True)
        layout.addWidget(QtWidgets.QLabel("回退差异与跨档告警 / 轨迹"))
        layout.addWidget(self.alerts)

        self.btn_submit.clicked.connect(self.on_submit)
        self.btn_rollback.clicked.connect(self.on_rollback)
        self.btn_cancel.clicked.connect(self.on_cancel)
        self.btn_span.clicked.connect(self.on_span_check)

    def _current_params(self) -> TierParams:
        quantum = self.quantum_box.value()
        span = self.span_box.value()
        return TierParams(
            target_rate=int(self.rate_box.currentText()),
            gain_q8=self.gain_box.value(),
            quantum_ticks=quantum,
            slice_span_ticks=span,
        )

    def on_submit(self) -> None:
        tier = self.tier_box.currentText()
        try:
            record = self.engine.submit_params(tier, self._current_params())
            outcome = self.engine.recompute(tier)
            self.alerts.appendPlainText(
                f"{tier} v{record.version}: 复用 {len(outcome.reused)} "
                f"重算 {len(outcome.recomputed)} 陈旧拒绝 "
                f"{len(outcome.stale_rejections)}"
            )
        except AuditError as exc:
            self.alerts.appendPlainText(f"{exc.code.value}: {exc.message}")
        except CancelledError as exc:
            self.alerts.appendPlainText(f"已取消: {exc}")
        self.refresh()

    def on_rollback(self) -> None:
        tier = self.tier_box.currentText()
        history = self.engine.params.versions(tier)
        if len(history) < 2:
            self.alerts.appendPlainText("历史版本不足，无法回退")
            return
        target = history[-2].version
        try:
            record = self.engine.rollback_params(tier, target)
            outcome = self.engine.recompute(tier)
            first = outcome.stale_rejections[0] if outcome.stale_rejections else None
            self.alerts.appendPlainText(
                f"回退生成 v{record.version}，首个陈旧切片: {first}"
            )
        except AuditError as exc:
            self.alerts.appendPlainText(f"{exc.code.value}: {exc.message}")
        self.refresh()

    def on_cancel(self) -> None:
        tier = self.tier_box.currentText()
        ok = self.engine.request_cancel(tier)
        self.alerts.appendPlainText(
            f"取消信号已发送: {ok}" if ok else "该档位没有运行中的任务"
        )

    def on_span_check(self) -> None:
        try:
            mismatches = self.engine.check_spans("tier_low", "tier_high")
        except AuditError as exc:
            self.alerts.appendPlainText(f"{exc.code.value}: {exc.message}")
            return
        if not mismatches:
            self.alerts.appendPlainText("跨档起止样本一致")
        else:
            first = mismatches[0]
            self.alerts.appendPlainText(
                f"{ErrorCode.E_SPAN_MISMATCH.value}: 首个不一致窗口 "
                f"{first.first_window_index}，档位 {first.involved_tiers}"
            )
        self.refresh()

    def refresh(self) -> None:
        self.timeline.clear()
        grid_state: dict[str, list[tuple[int, str, str | None]]] = {}
        for tier in self.engine.params.tiers():
            cells: list[tuple[int, str, str | None]] = []
            for record in self.engine.params.versions(tier):
                self.timeline.addItem(
                    f"{tier} v{record.version} [{record.origin}] "
                    f"{record.content_hash[:12]}"
                )
                count = self.engine.committed.get(tier, {}).get(record.version, 0)
                for index in range(count):
                    cells.append((index, "recompute", None))
            grid_state[tier] = cells
        self.grid.set_state(grid_state)


def build_demo_engine(work_dir: str | Path, sample_name: str = "stereo_48k"):
    """供界面与 offscreen 冒烟共用的装配函数。"""
    work = Path(work_dir)
    generate_samples(work)
    source = load_source(
        work / "sources" / sample_name / "source.wav",
        work / "sources" / sample_name / "frames.json",
    )
    return AuditEngine(source, work / "audit")


def main() -> int:
    import sys

    app = QtWidgets.QApplication(sys.argv)
    engine = build_demo_engine(Path("./work_gui"))
    window = AuditMainWindow(engine)
    window.show()
    return app.exec()


if __name__ == "__main__":
    raise SystemExit(main())
