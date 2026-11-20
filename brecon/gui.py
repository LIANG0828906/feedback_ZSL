from __future__ import annotations

import json
import os
from pathlib import Path
from typing import Any

from .config import SampleConfig
from .engine import ReplayEngine
from .errors import BreconError
from .lock import RoomBusyLock, room_lock_path
from .recovery import recover_engine


def _qt():
    from PySide6 import QtCore, QtGui, QtWidgets

    return QtCore, QtGui, QtWidgets


class InventoryCurve:
    def __new__(cls, *args: Any, **kwargs: Any):
        QtCore, QtGui, QtWidgets = _qt()

        class _InventoryCurve(QtWidgets.QWidget):
            def __init__(self) -> None:
                super().__init__()
                self._history: list[dict[str, Any]] = []
                self.setMinimumHeight(210)

            def set_history(self, history: list[dict[str, Any]]) -> None:
                self._history = history
                self.update()

            def paintEvent(self, event: Any) -> None:
                painter = QtGui.QPainter(self)
                painter.setRenderHint(QtGui.QPainter.Antialiasing)
                painter.fillRect(self.rect(), QtGui.QColor("#101820"))
                painter.setPen(QtGui.QColor("#9fb3c8"))
                painter.drawText(12, 20, "玩家物资曲线（总持有量）")
                if not self._history:
                    painter.drawText(12, 46, "等待推进")
                    return
                left, top, right, bottom = 42, 34, 18, 28
                width = max(1, self.width() - left - right)
                height = max(1, self.height() - top - bottom)
                player_ids = [
                    entry["player_id"] for entry in self._history[-1]["state"]["inventory"]
                ]
                series: dict[str, list[int]] = {player: [] for player in player_ids}
                for record in self._history:
                    for player in record["state"]["inventory"]:
                        series[player["player_id"]].append(
                            sum(int(item["quantity"]) for item in player["items"])
                        )
                maximum = max(
                    [1]
                    + [
                        value
                        for values in series.values()
                        for value in values
                    ]
                )
                painter.setPen(QtGui.QColor("#31465c"))
                painter.drawLine(left, top, left, top + height)
                painter.drawLine(left, top + height, left + width, top + height)
                painter.setPen(QtGui.QColor("#6f8499"))
                painter.drawText(5, top + 4, str(maximum))
                painter.drawText(5, top + height, "0")
                colors = (
                    "#58c4dd",
                    "#f4a261",
                    "#90be6d",
                    "#e76f51",
                    "#c77dff",
                    "#ffd166",
                    "#4cc9f0",
                    "#f28482",
                )
                count = max(1, len(self._history) - 1)
                for index, player_id in enumerate(player_ids):
                    values = series[player_id]
                    color = QtGui.QColor(colors[index % len(colors)])
                    painter.setPen(QtGui.QPen(color, 2))
                    previous = None
                    for position, value in enumerate(values):
                        x = left + int(width * position / count)
                        y = top + height - int(height * value / maximum)
                        point = QtCore.QPointF(x, y)
                        if previous is not None:
                            painter.drawLine(previous, point)
                        previous = point
                    painter.fillRect(
                        left + index * 90,
                        self.height() - 18,
                        10,
                        5,
                        color,
                    )
                    painter.setPen(QtGui.QColor("#d7e3ee"))
                    painter.drawText(
                        left + index * 90 + 15,
                        self.height() - 12,
                        player_id,
                    )

        return _InventoryCurve(*args, **kwargs)


class LatencyHistogram:
    def __new__(cls, *args: Any, **kwargs: Any):
        QtCore, QtGui, QtWidgets = _qt()

        class _LatencyHistogram(QtWidgets.QWidget):
            def __init__(self) -> None:
                super().__init__()
                self._values: list[int] = []
                self.setMinimumHeight(170)

            def set_values(self, values: list[int]) -> None:
                self._values = list(values)
                self.update()

            def paintEvent(self, event: Any) -> None:
                painter = QtGui.QPainter(self)
                painter.setRenderHint(QtGui.QPainter.Antialiasing)
                painter.fillRect(self.rect(), QtGui.QColor("#101820"))
                painter.setPen(QtGui.QColor("#9fb3c8"))
                painter.drawText(12, 20, "端到端延迟分布 (ms)")
                if not self._values:
                    painter.drawText(12, 46, "等待操作结算")
                    return
                left, top, width, height = 38, 34, self.width() - 54, self.height() - 62
                maximum = max(self._values)
                minimum = min(self._values)
                bins = 10
                span = max(1, maximum - minimum + 1)
                counts = [0] * bins
                for value in self._values:
                    index = min(bins - 1, int((value - minimum) * bins / span))
                    counts[index] += 1
                peak = max(counts)
                bar_width = max(1, width // bins)
                for index, count in enumerate(counts):
                    bar_height = int((height - 8) * count / peak) if peak else 0
                    x = left + index * bar_width
                    y = top + height - bar_height
                    painter.fillRect(
                        x + 2,
                        y,
                        max(1, bar_width - 4),
                        bar_height,
                        QtGui.QColor("#58c4dd"),
                    )
                painter.setPen(QtGui.QColor("#6f8499"))
                painter.drawLine(left, top + height, left + width, top + height)
                painter.drawText(left, top + height + 18, f"{minimum}")
                painter.drawText(left + width - 28, top + height + 18, f"{maximum}")
                ordered = sorted(self._values)
                p95 = ordered[max(0, int(0.95 * len(ordered) + 0.999) - 1)]
                painter.setPen(QtGui.QColor("#f4a261"))
                painter.drawText(12, self.height() - 6, f"P95 = {p95} ms")

        return _LatencyHistogram(*args, **kwargs)


def _main_window_class():
    QtCore, QtGui, QtWidgets = _qt()

    class MainWindow(QtWidgets.QMainWindow):
        def __init__(self, workspace: Path, sample: str | None) -> None:
            super().__init__()
            self.workspace = workspace
            self.setWindowTitle("BR Econ Replay · 本地赛事经济重放")
            self.resize(1280, 900)
            self.timer = QtCore.QTimer(self)
            self.timer.setInterval(450)
            self.timer.timeout.connect(self._advance)
            self.lock: RoomBusyLock | None = None
            self.config: SampleConfig | None = None
            self.engine: ReplayEngine | None = None
            self._build_ui()
            self._load_manifest(sample)

        def _build_ui(self) -> None:
            central = QtWidgets.QWidget()
            self.setCentralWidget(central)
            root = QtWidgets.QVBoxLayout(central)
            controls = QtWidgets.QHBoxLayout()
            self.sample_combo = QtWidgets.QComboBox()
            self.sample_combo.setMinimumWidth(180)
            self.sample_combo.currentTextChanged.connect(self._select_sample)
            controls.addWidget(QtWidgets.QLabel("样本"))
            controls.addWidget(self.sample_combo)
            self.start_button = QtWidgets.QPushButton("启动")
            self.pause_button = QtWidgets.QPushButton("暂停")
            self.revoke_button = QtWidgets.QPushButton("撤回注入")
            self.resume_button = QtWidgets.QPushButton("从快照恢复")
            for button in (
                self.start_button,
                self.pause_button,
                self.revoke_button,
                self.resume_button,
            ):
                controls.addWidget(button)
            self.start_button.clicked.connect(self._start)
            self.pause_button.clicked.connect(self._pause)
            self.revoke_button.clicked.connect(self._inject_revoke)
            self.resume_button.clicked.connect(self._resume_snapshot)
            controls.addStretch(1)
            root.addLayout(controls)

            self.progress = QtWidgets.QProgressBar()
            self.progress.setFormat("%v / %m 步")
            root.addWidget(self.progress)
            self.status = QtWidgets.QLabel("未启动")
            self.status.setStyleSheet("color:#9fb3c8;padding:2px;")
            root.addWidget(self.status)

            tabs = QtWidgets.QTabWidget()
            root.addWidget(tabs, 1)
            economy = QtWidgets.QWidget()
            econ_layout = QtWidgets.QVBoxLayout(economy)
            tables = QtWidgets.QSplitter(QtCore.Qt.Horizontal)
            self.box_table = self._table(
                ["空投箱", "槽位", "物资", "数量", "可用", "归属", "客户端序号"]
            )
            self.inventory_table = self._table(
                ["玩家", "物资", "权威持有量", "挂账可见量"]
            )
            tables.addWidget(self.box_table)
            tables.addWidget(self.inventory_table)
            tables.setSizes([760, 440])
            econ_layout.addWidget(tables, 2)
            self.curve = InventoryCurve()
            econ_layout.addWidget(self.curve)
            lower = QtWidgets.QSplitter(QtCore.Qt.Horizontal)
            self.hold_table = self._table(
                ["玩家", "序号", "凭证", "空投箱", "槽位", "物资", "撤回意图"]
            )
            self.revoke_table = self._table(
                ["玩家", "目标序号", "撤回序号", "箱", "槽", "物资", "数量"]
            )
            lower.addWidget(self.hold_table)
            lower.addWidget(self.revoke_table)
            lower.setSizes([620, 620])
            econ_layout.addWidget(lower, 2)
            tabs.addTab(economy, "经济状态")

            latency_tab = QtWidgets.QWidget()
            latency_layout = QtWidgets.QVBoxLayout(latency_tab)
            self.latency_summary = QtWidgets.QLabel("等待结算")
            self.latency_summary.setStyleSheet(
                "font-size:15px;color:#d7e3ee;padding:4px;"
            )
            latency_layout.addWidget(self.latency_summary)
            self.histogram = LatencyHistogram()
            latency_layout.addWidget(self.histogram)
            sync_split = QtWidgets.QSplitter(QtCore.Qt.Horizontal)
            self.sync_table = self._table(
                ["同步步", "窗口起点", "窗口终点", "差异", "容差", "状态", "来源"]
            )
            self.latency_table = self._table(
                ["到达序", "玩家", "序号", "类型", "结算时刻", "端到端延迟", "状态"]
            )
            sync_split.addWidget(self.sync_table)
            sync_split.addWidget(self.latency_table)
            latency_layout.addWidget(sync_split, 2)
            tabs.addTab(latency_tab, "延迟与对账")

            trace_tab = QtWidgets.QWidget()
            trace_layout = QtWidgets.QVBoxLayout(trace_tab)
            self.trace_view = QtWidgets.QPlainTextEdit()
            self.trace_view.setReadOnly(True)
            self.trace_view.setStyleSheet(
                "font-family:Consolas,monospace;background:#101820;color:#d7e3ee;"
            )
            trace_layout.addWidget(self.trace_view)
            tabs.addTab(trace_tab, "轨迹日志")

        def _table(self, headers: list[str]):
            table = _qt()[2].QTableWidget(0, len(headers))
            table.setHorizontalHeaderLabels(headers)
            table.horizontalHeader().setStretchLastSection(True)
            table.setAlternatingRowColors(True)
            table.setSelectionBehavior(_qt()[2].QAbstractItemView.SelectRows)
            table.setEditTriggers(_qt()[2].QAbstractItemView.NoEditTriggers)
            return table

        def _load_manifest(self, initial_sample: str | None) -> None:
            manifest_path = self.workspace / "active.json"
            if not manifest_path.is_file():
                self.status.setText(f"工作目录缺少 active.json: {manifest_path}")
                return
            with manifest_path.open("r", encoding="utf-8") as handle:
                self.manifest = json.load(handle)
            names = [
                description["name"] for description in self.manifest.get("samples", [])
            ]
            self.sample_combo.blockSignals(True)
            self.sample_combo.addItems(names)
            self.sample_combo.blockSignals(False)
            selected = initial_sample or self.manifest.get("active_sample")
            if selected in names:
                self.sample_combo.setCurrentText(selected)
            if names:
                self._select_sample(self.sample_combo.currentText())

        def _sample_path(self, name: str) -> Path:
            for description in self.manifest.get("samples", []):
                if description["name"] == name:
                    return self.workspace / description["directory"]
            raise BreconError("E_SAMPLE_NOT_FOUND", f"sample not found: {name}")

        def _select_sample(self, name: str) -> None:
            if not name:
                return
            self._pause()
            self._release_lock()
            try:
                self.config = SampleConfig.load(self._sample_path(name))
                self.engine = ReplayEngine.from_sample_dir(
                    self.config.sample_dir, write_snapshots=False
                )
                self.progress.setRange(0, self.config.observation.end_step)
                self.progress.setValue(0)
                self.status.setText(
                    f"{self.config.sample_dir.name} · 等待启动 · "
                    f"{len(self.config.players)} 名玩家 / "
                    f"{self.config.observation.end_step} 步"
                )
                self._refresh()
            except BreconError as exc:
                self.status.setText(f"{exc.code}: {exc.message}")

        def _acquire_lock(self) -> bool:
            if self.lock is not None or self.config is None:
                return True
            try:
                self.lock = RoomBusyLock(
                    self.config.room_id, room_lock_path(self.config.sample_dir)
                ).acquire()
                return True
            except BreconError as exc:
                self.status.setText(f"{exc.code}: {exc.message}")
                return False

        def _release_lock(self) -> None:
            if self.lock is not None:
                self.lock.release()
                self.lock = None

        def _start(self) -> None:
            if self.config is None or self.engine is None:
                return
            if self.engine.current_step >= self.config.observation.end_step:
                self.engine = ReplayEngine.from_sample_dir(
                    self.config.sample_dir, write_snapshots=False
                )
            if not self._acquire_lock():
                return
            self.timer.start()
            self.status.setText(
                f"{self.config.sample_dir.name} · 推进中 · "
                f"step {self.engine.current_step}/{self.config.observation.end_step}"
            )

        def _pause(self) -> None:
            self.timer.stop()
            if self.engine is not None:
                self.status.setText(
                    f"已暂停 · step {self.engine.current_step}/"
                    f"{self.engine.config.observation.end_step}"
                )

        def _advance(self) -> None:
            if self.engine is None:
                self.timer.stop()
                return
            entry = self.engine.step_once()
            if entry is None:
                self.timer.stop()
                self._release_lock()
                self.status.setText("推进完成")
            else:
                self.status.setText(
                    f"推进中 · step {self.engine.current_step}/"
                    f"{self.engine.config.observation.end_step} · "
                    f"生效 {len(entry['effective_ops'])} · 拒绝 "
                    f"{len(entry['rejections'])}"
                )
            self._refresh()

        def _selected_hold(self) -> tuple[str, int] | None:
            row = self.hold_table.currentRow()
            if row < 0:
                return None
            player_item = self.hold_table.item(row, 0)
            sequence_item = self.hold_table.item(row, 1)
            if player_item is None or sequence_item is None:
                return None
            return player_item.text(), int(sequence_item.text())

        def _inject_revoke(self) -> None:
            if self.engine is None:
                return
            target = self._selected_hold()
            if target is None:
                self.status.setText("请先在挂账列表选择一条预扣除")
                return
            try:
                operation = self.engine.inject_revoke(
                    player_id=target[0], target_client_seq=target[1]
                )
                self.status.setText(
                    f"已注入撤回 #{operation['client_seq']} → "
                    f"{target[0]}:{target[1]}，将在下一步结算"
                )
                self._refresh()
            except BreconError as exc:
                self.status.setText(f"{exc.code}: {exc.message}")

        def _resume_snapshot(self) -> None:
            if self.config is None:
                return
            self._pause()
            self._release_lock()
            try:
                engine, path = recover_engine(self.config, write_snapshots=False)
                self.engine = engine
                self.progress.setValue(self.engine.current_step)
                self.status.setText(
                    f"已从 {path.name} 恢复 · step {self.engine.current_step}"
                )
                self._refresh()
            except BreconError as exc:
                self.status.setText(f"{exc.code}: {exc.message}")

        def _refresh(self) -> None:
            if self.engine is None:
                return
            engine = self.engine
            self.progress.setValue(engine.current_step)
            box_rows = []
            for box in engine.state["boxes"]:
                for slot in box["slots"]:
                    box_rows.append(
                        [
                            box["box_id"],
                            slot["slot_id"],
                            slot["item"],
                            str(slot["quantity"]),
                            "是" if slot["available"] else "否",
                            slot["claimed_by"] or "—",
                            str(slot["claimed_client_seq"] or "—"),
                        ]
                    )
            self._fill(self.box_table, box_rows)

            inventory_rows = []
            for player in engine.state["inventory"]:
                held = {
                    item: 0 for item in engine.client_view.get(player["player_id"], {})
                }
                for hold in engine.state["holds"]:
                    if hold["player_id"] == player["player_id"]:
                        held[hold["item"]] = held.get(hold["item"], 0) + int(
                            hold["quantity"]
                        )
                for item in player["items"]:
                    inventory_rows.append(
                        [
                            player["player_id"],
                            item["item"],
                            str(item["quantity"]),
                            str(held.get(item["item"], 0)),
                        ]
                    )
            self._fill(self.inventory_table, inventory_rows)

            hold_rows = [
                [
                    hold["player_id"],
                    str(hold["client_seq"]),
                    hold["voucher_id"],
                    hold["box_id"],
                    hold["slot_id"],
                    hold["item"],
                    str(hold.get("revoke_pending_seq") or "—"),
                ]
                for hold in engine.state["holds"]
            ]
            self._fill(self.hold_table, hold_rows)
            revoke_rows = [
                [
                    record["player_id"],
                    str(record["target_client_seq"]),
                    str(record["revoke_client_seq"]),
                    record["box_id"],
                    record["slot_id"],
                    record["item"],
                    str(record["quantity"]),
                ]
                for record in engine.state["revocations"]
            ]
            self._fill(self.revoke_table, revoke_rows)
            self.curve.set_history(engine.state_history)

            latency = engine.latency_payload()
            distribution = latency["distribution"]
            self.latency_summary.setText(
                "延迟 "
                f"n={distribution['count']} "
                f"mean={distribution['mean_ms']}ms "
                f"P50={distribution['p50_ms']}ms "
                f"P95={distribution['p95_ms']}ms "
                f"P99={distribution['p99_ms']}ms    "
                f"补偿 {latency['compensation']['hits']}/"
                f"{latency['compensation']['attempts']} "
                f"({latency['compensation']['hit_rate']:.0%})"
            )
            self.histogram.set_values(
                [int(record["end_to_end_delay_ms"]) for record in latency["operations"]]
            )
            sync_rows = [
                [
                    str(record["step"]),
                    str(record["window_start_step"]),
                    str(record["window_end_step"]),
                    str(record["difference"]),
                    str(record["tolerance"]),
                    record["status"],
                    record["source"] or "—",
                ]
                for record in latency["sync_observations"]
            ]
            self._fill(self.sync_table, sync_rows)
            latency_rows = [
                [
                    str(record["arrival_seq"]),
                    record["player_id"],
                    str(record["client_seq"]),
                    record["op_type"],
                    str(record["settle_ts_ms"]),
                    str(record["end_to_end_delay_ms"]),
                    record["status"],
                ]
                for record in latency["operations"][-250:]
            ]
            self._fill(self.latency_table, latency_rows)
            self.trace_view.setPlainText(
                "\n".join(
                    json.dumps(entry, separators=(",", ":"), ensure_ascii=False)
                    for entry in engine.trace_entries
                )
            )

        @staticmethod
        def _fill(table: Any, rows: list[list[str]]) -> None:
            table.setRowCount(len(rows))
            for row_index, row in enumerate(rows):
                for column_index, value in enumerate(row):
                    table.setItem(
                        row_index,
                        column_index,
                        _qt()[2].QTableWidgetItem(str(value)),
                    )
            table.resizeColumnsToContents()

        def closeEvent(self, event: Any) -> None:
            self.timer.stop()
            self._release_lock()
            super().closeEvent(event)

    return MainWindow


def launch_gui(
    *,
    workspace: Path,
    sample: str | None = None,
    offscreen: bool = False,
    auto_run: bool = False,
    screenshot: str | None = None,
) -> int:
    if offscreen:
        os.environ.setdefault("QT_QPA_PLATFORM", "offscreen")
    QtCore, QtGui, QtWidgets = _qt()
    app = QtWidgets.QApplication.instance() or QtWidgets.QApplication([])
    window = _main_window_class()(workspace, sample)
    window.show()
    if auto_run and window.engine is not None:
        while window.engine.current_step < window.engine.config.observation.end_step:
            window.engine.step_once()
        window._refresh()
    app.processEvents()
    if screenshot:
        target = Path(screenshot)
        target.parent.mkdir(parents=True, exist_ok=True)
        window.grab().save(str(target))
    if offscreen:
        if auto_run:
            window.close()
            return 0
        QtCore.QTimer.singleShot(1500, window.close)
    return int(app.exec())

