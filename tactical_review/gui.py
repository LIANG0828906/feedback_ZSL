"""PySide6 desktop review workbench.

The workbench deliberately reads authoritative state only from atomically
published snapshot files. Sample inputs are used only for explicitly declared
metadata such as locally generated latency measurements and baseline hashes.
"""

from __future__ import annotations

import json
import sys
from collections.abc import Sequence
from pathlib import Path
from typing import Any

from PySide6.QtCore import QPointF, QRectF, QSize, Qt, QTimer
from PySide6.QtGui import (
    QBrush,
    QColor,
    QFont,
    QPainter,
    QPainterPath,
    QPen,
)
from PySide6.QtWidgets import (
    QAbstractItemView,
    QApplication,
    QComboBox,
    QFrame,
    QHBoxLayout,
    QHeaderView,
    QLabel,
    QLineEdit,
    QMainWindow,
    QMessageBox,
    QPlainTextEdit,
    QPushButton,
    QSpinBox,
    QSplitter,
    QTableWidget,
    QTableWidgetItem,
    QTabWidget,
    QVBoxLayout,
    QWidget,
)

from .errors import ReviewError
from .model import InitialState, load_revocations, load_shots
from .runner import WorkbenchRunner
from .snapshot import SnapshotStore

BG = "#0b1118"
PANEL = "#121c26"
PANEL_ALT = "#172431"
BORDER = "#293b4c"
TEXT = "#d8e6f2"
MUTED = "#8295a8"
ACCENT = "#4cb8ff"
GOOD = "#50d890"
WARN = "#ffb454"
BAD = "#ff6073"
VIOLET = "#b892ff"


def _font(size: int = 10, bold: bool = False) -> QFont:
    font = QFont("Segoe UI")
    font.setPointSize(size)
    font.setBold(bold)
    return font


class CanvasBase(QWidget):
    def __init__(self, parent: QWidget | None = None) -> None:
        super().__init__(parent)
        self.history: list[dict[str, Any]] = []
        self.initial: InitialState | None = None
        self.setMinimumHeight(220)

    def set_data(
        self,
        history: Sequence[dict[str, Any]],
        initial: InitialState,
    ) -> None:
        self.history = list(history)
        self.initial = initial
        self.update()

    def _bounds(self) -> tuple[int, int]:
        if self.initial is None:
            return (1, 1)
        return (self.initial.start_step, self.initial.end_step)


class TimelineCanvas(CanvasBase):
    def minimumSizeHint(self) -> QSize:
        return QSize(700, 340)

    def paintEvent(self, event: Any) -> None:
        painter = QPainter(self)
        painter.setRenderHint(QPainter.RenderHint.Antialiasing)
        painter.fillRect(self.rect(), QColor("#0e1822"))
        if self.initial is None or not self.history:
            painter.setPen(QColor(MUTED))
            painter.setFont(_font(11))
            painter.drawText(
                self.rect(),
                Qt.AlignmentFlag.AlignCenter,
                "No published snapshots yet. Start or restore a replay.",
            )
            return
        start, end = self._bounds()
        left, right = 72.0, 20.0
        top, bottom = 24.0, 48.0
        lane_area = max(60.0, self.height() - top - bottom)
        width = max(20.0, self.width() - left - right)
        covers = self.initial.cover_ids
        lane_height = lane_area / max(1, len(covers))

        painter.setPen(QPen(QColor("#1d2c3a"), 1))
        for tick in range(start, end + 1, max(1, (end - start) // 8 or 1)):
            x = left + (tick - start) / max(1, end - start) * width
            painter.drawLine(QPointF(x, top), QPointF(x, self.height() - bottom + 6))
            painter.setPen(QColor(MUTED))
            painter.setFont(_font(8))
            painter.drawText(
                QRectF(x - 24, self.height() - bottom + 10, 48, 18),
                Qt.AlignmentFlag.AlignHCenter,
                f"s{tick}",
            )
            painter.setPen(QPen(QColor("#1d2c3a"), 1))

        for cover_index, cover_id in enumerate(covers):
            lane_y = top + cover_index * lane_height
            lane = QRectF(left, lane_y + 2, width, max(8.0, lane_height - 5))
            painter.fillRect(lane, QColor(PANEL))
            painter.setPen(QColor(MUTED))
            painter.setFont(_font(9, True))
            painter.drawText(
                QRectF(8, lane_y, 58, lane_height),
                Qt.AlignmentFlag.AlignVCenter | Qt.AlignmentFlag.AlignLeft,
                cover_id,
            )
            path = QPainterPath()
            previous_cracks = 0
            first = True
            dead_x: float | None = None
            for snapshot in self.history:
                summary = snapshot.get("summary", {})
                step = int(summary.get("step", start))
                cover = next(
                    (
                        item
                        for item in summary.get("covers", [])
                        if item.get("cover_id") == cover_id
                    ),
                    None,
                )
                if cover is None:
                    continue
                x = left + (step - start) / max(1, end - start) * width
                y = lane.bottom() - float(cover.get("fraction", 1.0)) * lane.height()
                if first:
                    path.moveTo(x, y)
                    first = False
                else:
                    path.lineTo(x, y)
                crack_count = len(cover.get("crack_events", []))
                if crack_count > previous_cracks:
                    for crack in cover.get("crack_events", [])[previous_cracks:]:
                        crack_step = int(crack.get("step", step))
                        cx = left + (crack_step - start) / max(1, end - start) * width
                        painter.setPen(Qt.PenStyle.NoPen)
                        painter.setBrush(QColor(BAD))
                        painter.drawEllipse(QPointF(cx, y), 3.2, 3.2)
                    previous_cracks = crack_count
                if not bool(cover.get("alive", True)) and dead_x is None:
                    dead_x = x
            color = QColor(GOOD) if all(
                next(
                    (
                        item.get("alive", True)
                        for item in snapshot.get("summary", {}).get("covers", [])
                        if item.get("cover_id") == cover_id
                    ),
                    True,
                )
                for snapshot in self.history[-1:]
            ) else QColor(BAD)
            painter.setPen(QPen(color, 2.2))
            painter.setBrush(Qt.BrushStyle.NoBrush)
            painter.drawPath(path)
            if dead_x is not None:
                painter.fillRect(
                    QRectF(dead_x, lane.top(), max(0.0, lane.right() - dead_x), lane.height()),
                    QColor(74, 87, 99, 78),
                )

        latest = self.history[-1].get("summary", {})
        current = int(latest.get("step", start))
        x_current = left + (current - start) / max(1, end - start) * width
        painter.setPen(QPen(QColor(ACCENT), 1.5, Qt.PenStyle.DashLine))
        painter.drawLine(
            QPointF(x_current, top), QPointF(x_current, self.height() - bottom + 4)
        )

        event_y = self.height() - 27
        painters = [
            ("conflict", BAD),
            ("revocation", VIOLET),
            ("late/duplicate", WARN),
        ]
        for snapshot in self.history:
            summary = snapshot.get("summary", {})
            step = int(summary.get("step", start))
            x = left + (step - start) / max(1, end - start) * width
            codes = {str(item.get("code")) for item in summary.get("errors", [])}
            markers: list[tuple[str, str]] = []
            if "E_DAMAGE_CONFLICT" in codes:
                markers.append(("conflict", BAD))
            if summary.get("revocations"):
                markers.append(("revocation", VIOLET))
            if "E_OP_LATE" in codes or "E_OP_DUPLICATE" in codes:
                markers.append(("late/duplicate", WARN))
            for marker_index, (_, color_hex) in enumerate(markers):
                painter.setPen(Qt.PenStyle.NoPen)
                painter.setBrush(QColor(color_hex))
                painter.drawEllipse(
                    QPointF(x, event_y - marker_index * 6.0), 2.3, 2.3
                )

        painter.setFont(_font(8))
        legend_x = left
        for label, color_hex in painters:
            painter.setPen(Qt.PenStyle.NoPen)
            painter.setBrush(QColor(color_hex))
            painter.drawEllipse(QPointF(legend_x, self.height() - 8), 3, 3)
            painter.setPen(QColor(MUTED))
            painter.drawText(
                QRectF(legend_x + 7, self.height() - 18, 106, 18),
                Qt.AlignmentFlag.AlignLeft | Qt.AlignmentFlag.AlignVCenter,
                label,
            )
            legend_x += 112


class PendingCanvas(CanvasBase):
    def minimumSizeHint(self) -> QSize:
        return QSize(500, 220)

    def set_data(self, history: Sequence[dict[str, Any]], initial: InitialState) -> None:
        self.history = list(history)
        self.initial = initial
        self.update()

    def paintEvent(self, event: Any) -> None:
        painter = QPainter(self)
        painter.setRenderHint(QPainter.RenderHint.Antialiasing)
        painter.fillRect(self.rect(), QColor("#0e1822"))
        painter.setPen(QColor(MUTED))
        painter.setFont(_font(9))
        painter.drawText(14, 20, "PREDICTION LEDGER / unresolved displayed damage")
        if self.initial is None or not self.history:
            painter.drawText(
                self.rect(),
                Qt.AlignmentFlag.AlignCenter,
                "Prediction ledger appears after the first published step.",
            )
            return
        start, end = self._bounds()
        totals = [
            sum(int(value) for value in snapshot["summary"].get("pending", {}).values())
            / 1_000_000.0
            for snapshot in self.history
        ]
        maximum = max([1.0, *totals])
        left, right, top, bottom = 56.0, 18.0, 34.0, 26.0
        width = max(10.0, self.width() - left - right)
        height = max(10.0, self.height() - top - bottom)
        painter.setPen(QPen(QColor("#223341"), 1))
        for index in range(5):
            y = top + height * index / 4
            painter.drawLine(QPointF(left, y), QPointF(left + width, y))
        path = QPainterPath()
        for index, snapshot in enumerate(self.history):
            step = int(snapshot["summary"].get("step", start))
            x = left + (step - start) / max(1, end - start) * width
            y = top + height - totals[index] / maximum * height
            if index == 0:
                path.moveTo(x, y)
            else:
                path.lineTo(x, y)
        fill_path = QPainterPath(path)
        if self.history:
            last_step = int(self.history[-1]["summary"].get("step", start))
            last_x = left + (last_step - start) / max(1, end - start) * width
            fill_path.lineTo(last_x, top + height)
            fill_path.lineTo(left, top + height)
            fill_path.closeSubpath()
            painter.fillPath(fill_path, QColor(76, 184, 255, 36))
        painter.setPen(QPen(QColor(ACCENT), 2))
        painter.drawPath(path)
        painter.setFont(_font(8))
        painter.setPen(QColor(MUTED))
        painter.drawText(6, top + 10, f"{maximum:.0f}")
        painter.drawText(6, top + height, "0")
        current = totals[-1] if totals else 0
        painter.setPen(QColor(TEXT))
        painter.setFont(_font(10, True))
        painter.drawText(
            QRectF(left, self.height() - 22, width, 18),
            Qt.AlignmentFlag.AlignRight,
            f"current total {current:.3f} units",
        )


class LatencyCanvas(CanvasBase):
    def __init__(self, latency_steps: Sequence[int] | None = None) -> None:
        super().__init__()
        self.latency_steps = list(latency_steps or [])
        self.explicit = latency_steps is not None
        self.setMinimumHeight(220)

    def set_latencies(self, values: Sequence[int] | None) -> None:
        self.latency_steps = list(values or [])
        self.explicit = values is not None
        self.update()

    def paintEvent(self, event: Any) -> None:
        painter = QPainter(self)
        painter.setRenderHint(QPainter.RenderHint.Antialiasing)
        painter.fillRect(self.rect(), QColor("#0e1822"))
        painter.setPen(QColor(MUTED))
        painter.setFont(_font(9))
        painter.drawText(
            14,
            20,
            "ARRIVAL LATENCY / explicitly emitted by sample generator",
        )
        if not self.explicit:
            painter.setFont(_font(10))
            painter.drawText(
                self.rect(),
                Qt.AlignmentFlag.AlignCenter,
                "No explicit latency_steps metadata. No latency was inferred.",
            )
            return
        categories = ["-2", "-1", "0", "+1", "+2", "+3", ">+3"]

        def bucket(value: int) -> int:
            if value <= -2:
                return 0
            if value == -1:
                return 1
            if value == 0:
                return 2
            if value == 1:
                return 3
            if value == 2:
                return 4
            if value == 3:
                return 5
            return 6

        counts = [0] * len(categories)
        for value in self.latency_steps:
            counts[bucket(int(value))] += 1
        maximum = max([1, *counts])
        left, right, top, bottom = 48.0, 18.0, 40.0, 46.0
        width = max(10.0, self.width() - left - right)
        height = max(10.0, self.height() - top - bottom)
        slot = width / len(categories)
        painter.setFont(_font(8))
        for index, (label, count) in enumerate(zip(categories, counts)):
            x = left + slot * index + slot * 0.18
            bar_width = slot * 0.64
            bar_height = height * count / maximum
            rect = QRectF(x, top + height - bar_height, bar_width, bar_height)
            color = QColor(GOOD) if index <= 5 else QColor(BAD)
            painter.fillRect(rect, color)
            painter.setPen(QColor(TEXT))
            painter.drawText(
                QRectF(x, rect.top() - 16, bar_width, 14),
                Qt.AlignmentFlag.AlignHCenter,
                str(count),
            )
            painter.setPen(QColor(MUTED))
            painter.drawText(
                QRectF(x - 4, top + height + 4, bar_width + 8, 18),
                Qt.AlignmentFlag.AlignHCenter,
                label,
            )
        painter.setPen(QColor(MUTED))
        painter.drawText(
            QRectF(left, self.height() - 17, width, 15),
            Qt.AlignmentFlag.AlignRight,
            "latency_steps = arrival_step - target_step (generator metadata)",
        )


class MetricCard(QFrame):
    def __init__(self, title: str) -> None:
        super().__init__()
        self.setObjectName("metricCard")
        layout = QVBoxLayout(self)
        layout.setContentsMargins(12, 8, 12, 8)
        layout.setSpacing(2)
        self.title = QLabel(title.upper())
        self.title.setObjectName("metricTitle")
        self.value = QLabel("--")
        self.value.setObjectName("metricValue")
        layout.addWidget(self.title)
        layout.addWidget(self.value)

    def set_value(self, text: str, color: str = TEXT) -> None:
        self.value.setText(text)
        self.value.setStyleSheet(f"color: {color};")


class WorkbenchWindow(QMainWindow):
    def __init__(
        self,
        sample_dir: str,
        run_dir: str,
        *,
        step_delay: float = 0.035,
    ) -> None:
        super().__init__()
        self.sample_dir = Path(sample_dir)
        self.run_dir = Path(run_dir)
        self.initial = InitialState.load(str(self.sample_dir / "initial_state.json"))
        with (self.sample_dir / "shots.json").open("r", encoding="utf-8") as handle:
            self.shots_payload = json.load(handle)
        _, self.shots = load_shots(str(self.sample_dir / "shots.json"))
        self.revocations = load_revocations(
            str(self.sample_dir / "revocations.json")
        )
        with (self.sample_dir / "baseline.json").open("r", encoding="utf-8") as handle:
            self.baseline = json.load(handle)
        self.store = SnapshotStore(self.run_dir)
        self.runner = WorkbenchRunner(
            self.initial,
            self.shots,
            self.revocations,
            self.store,
            step_delay=step_delay,
        )
        self.history: dict[int, dict[str, Any]] = {}
        self._last_rendered_step = -1
        self._last_message = ""
        self.setWindowTitle("Tactical Cover Replay & Reconciliation Workbench")
        self.resize(1460, 930)
        self.setMinimumSize(1120, 740)
        self._build_ui()
        self._apply_style()
        self._load_latency_metadata()
        self.timer = QTimer(self)
        self.timer.setInterval(140)
        self.timer.timeout.connect(self._refresh)
        self.timer.start()
        self._refresh(force=True)

    def _build_ui(self) -> None:
        central = QWidget()
        self.setCentralWidget(central)
        outer = QVBoxLayout(central)
        outer.setContentsMargins(14, 12, 14, 12)
        outer.setSpacing(10)

        header = QFrame()
        header.setObjectName("header")
        header_layout = QVBoxLayout(header)
        header_layout.setContentsMargins(16, 12, 16, 12)
        header_layout.setSpacing(8)
        title_row = QHBoxLayout()
        title_block = QVBoxLayout()
        title = QLabel("TACTICAL COVER / DETERMINISTIC REVIEW")
        title.setObjectName("title")
        subtitle = QLabel(
            f"match {self.initial.match_id}  |  fixed step {self.initial.step_ms} ms  |  "
            f"window s{self.initial.start_step}-s{self.initial.end_step}  |  "
            f"branch {self.run_dir.name}"
        )
        subtitle.setObjectName("subtitle")
        title_block.addWidget(title)
        title_block.addWidget(subtitle)
        title_row.addLayout(title_block, 1)
        self.status_chip = QLabel("IDLE")
        self.status_chip.setObjectName("statusChip")
        title_row.addWidget(self.status_chip, 0, Qt.AlignmentFlag.AlignTop)
        header_layout.addLayout(title_row)

        control_row = QHBoxLayout()
        control_row.setSpacing(8)
        self.start_button = QPushButton("Start replay")
        self.start_button.setObjectName("primary")
        self.start_button.clicked.connect(self._start_fresh)
        self.pause_button = QPushButton("Pause")
        self.pause_button.clicked.connect(self._toggle_pause)
        self.cancel_button = QPushButton("Cancel")
        self.cancel_button.clicked.connect(self._cancel)
        self.restore_button = QPushButton("Restore snapshot")
        self.restore_button.clicked.connect(self._restore)
        for button in (
            self.start_button,
            self.pause_button,
            self.cancel_button,
            self.restore_button,
        ):
            control_row.addWidget(button)
        divider = QFrame()
        divider.setFrameShape(QFrame.Shape.VLine)
        divider.setObjectName("divider")
        control_row.addWidget(divider)
        control_row.addWidget(QLabel("Revoke:"))
        self.player_combo = QComboBox()
        players = sorted({shot.player_id for shot in self.shots})
        self.player_combo.addItems(players or ["P1"])
        self.player_combo.setMinimumWidth(72)
        control_row.addWidget(self.player_combo)
        control_row.addWidget(QLabel("seq"))
        self.seq_spin = QSpinBox()
        self.seq_spin.setRange(1, 999999)
        self.seq_spin.setValue(1)
        control_row.addWidget(self.seq_spin)
        self.revoke_reason = QLineEdit()
        self.revoke_reason.setPlaceholderText("reason (queued at next boundary)")
        self.revoke_reason.setMinimumWidth(220)
        control_row.addWidget(self.revoke_reason, 1)
        self.revoke_button = QPushButton("Inject revocation")
        self.revoke_button.clicked.connect(self._inject_revocation)
        control_row.addWidget(self.revoke_button)
        header_layout.addLayout(control_row)
        outer.addWidget(header)

        metrics_row = QHBoxLayout()
        metrics_row.setSpacing(8)
        self.step_card = MetricCard("Published step")
        self.remaining_card = MetricCard("Remaining durability")
        self.pending_card = MetricCard("Prediction hang")
        self.conflict_card = MetricCard("Conflicts")
        self.revoke_card = MetricCard("Revocations")
        for card in (
            self.step_card,
            self.remaining_card,
            self.pending_card,
            self.conflict_card,
            self.revoke_card,
        ):
            metrics_row.addWidget(card)
        outer.addLayout(metrics_row)

        splitter = QSplitter(Qt.Orientation.Horizontal)
        splitter.setChildrenCollapsible(False)
        left = QWidget()
        left_layout = QVBoxLayout(left)
        left_layout.setContentsMargins(0, 0, 0, 0)
        left_layout.setSpacing(8)
        cover_label = QLabel("COVER AUTHORITY")
        cover_label.setObjectName("sectionTitle")
        left_layout.addWidget(cover_label)
        self.cover_table = QTableWidget(0, 6)
        self.cover_table.setHorizontalHeaderLabels(
            [
                "Cover",
                "Durability",
                "Segments",
                "Cracked",
                "Alive",
                "Crack #",
            ]
        )
        self.cover_table.verticalHeader().setVisible(False)
        self.cover_table.setAlternatingRowColors(True)
        self.cover_table.setSelectionBehavior(
            QAbstractItemView.SelectionBehavior.SelectRows
        )
        self.cover_table.setEditTriggers(
            QAbstractItemView.EditTrigger.NoEditTriggers
        )
        cover_header = self.cover_table.horizontalHeader()
        for column in range(6):
            cover_header.setSectionResizeMode(
                column, QHeaderView.ResizeMode.Fixed
            )
        for column, width in enumerate((60, 145, 75, 88, 92)):
            self.cover_table.setColumnWidth(column, width)
        cover_header.setSectionResizeMode(5, QHeaderView.ResizeMode.Stretch)
        left_layout.addWidget(self.cover_table, 1)
        left.setMinimumWidth(550)

        self.log = QPlainTextEdit()
        self.log.setReadOnly(True)
        self.log.setLineWrapMode(QPlainTextEdit.LineWrapMode.WidgetWidth)
        self.log.setHorizontalScrollBarPolicy(Qt.ScrollBarPolicy.ScrollBarAlwaysOff)
        self.log.setPlaceholderText("Published business events and command feedback.")
        self.log.setMaximumBlockCount(600)
        left_layout.addWidget(self.log, 0)
        splitter.addWidget(left)

        right = QWidget()
        right_layout = QVBoxLayout(right)
        right_layout.setContentsMargins(0, 0, 0, 0)
        right_layout.setSpacing(8)
        timeline_label = QLabel("PHYSICS-STEP TIMELINE / CRACK + EVENT MARKERS")
        timeline_label.setObjectName("sectionTitle")
        right_layout.addWidget(timeline_label)
        self.timeline = TimelineCanvas()
        right_layout.addWidget(self.timeline, 3)

        self.tabs = QTabWidget()
        pending_page = QWidget()
        pending_layout = QVBoxLayout(pending_page)
        pending_layout.setContentsMargins(0, 0, 0, 0)
        self.pending_canvas = PendingCanvas()
        pending_layout.addWidget(self.pending_canvas)
        self.tabs.addTab(pending_page, "Prediction hang")

        latency_page = QWidget()
        latency_layout = QVBoxLayout(latency_page)
        latency_layout.setContentsMargins(0, 0, 0, 0)
        self.latency_canvas = LatencyCanvas()
        latency_layout.addWidget(self.latency_canvas)
        self.tabs.addTab(latency_page, "Latency (explicit)")

        reconciliation_page = QWidget()
        reconciliation_layout = QVBoxLayout(reconciliation_page)
        reconciliation_layout.setContentsMargins(0, 0, 0, 0)
        self.reconcile_table = QTableWidget(0, 8)
        self.reconcile_table.setHorizontalHeaderLabels(
            [
                "Step",
                "Shot",
                "Player",
                "Seq",
                "Status",
                "Predicted",
                "Authoritative",
                "Delta by cover",
            ]
        )
        self.reconcile_table.verticalHeader().setVisible(False)
        self.reconcile_table.setAlternatingRowColors(True)
        self.reconcile_table.setEditTriggers(
            QAbstractItemView.EditTrigger.NoEditTriggers
        )
        reconcile_header = self.reconcile_table.horizontalHeader()
        for column in range(7):
            reconcile_header.setSectionResizeMode(
                column, QHeaderView.ResizeMode.ResizeToContents
            )
        reconcile_header.setSectionResizeMode(7, QHeaderView.ResizeMode.Stretch)
        reconciliation_layout.addWidget(self.reconcile_table)
        self.tabs.addTab(reconciliation_page, "Damage reconciliation")

        revoke_page = QWidget()
        revoke_layout = QVBoxLayout(revoke_page)
        revoke_layout.setContentsMargins(0, 0, 0, 0)
        self.revoke_table = QTableWidget(0, 7)
        self.revoke_table.setHorizontalHeaderLabels(
            ["Step", "Player", "Seq", "Status", "Code", "Restored", "Reason"]
        )
        self.revoke_table.verticalHeader().setVisible(False)
        self.revoke_table.setEditTriggers(
            QAbstractItemView.EditTrigger.NoEditTriggers
        )
        self.revoke_table.horizontalHeader().setSectionResizeMode(
            QHeaderView.ResizeMode.Stretch
        )
        revoke_layout.addWidget(self.revoke_table)
        self.tabs.addTab(revoke_page, "Revocations")

        fork_page = QWidget()
        fork_layout = QVBoxLayout(fork_page)
        fork_layout.setContentsMargins(0, 0, 0, 0)
        self.divergence_label = QLabel("Waiting for published steps.")
        self.divergence_label.setObjectName("diagnosticLabel")
        fork_layout.addWidget(self.divergence_label)
        self.fork_table = QTableWidget(0, 5)
        self.fork_table.setHorizontalHeaderLabels(
            ["Step", "Baseline hash", "Replay hash", "Hash", "Cover state"]
        )
        self.fork_table.verticalHeader().setVisible(False)
        self.fork_table.setEditTriggers(
            QAbstractItemView.EditTrigger.NoEditTriggers
        )
        self.fork_table.horizontalHeader().setSectionResizeMode(
            QHeaderView.ResizeMode.Stretch
        )
        fork_layout.addWidget(self.fork_table)
        self.tabs.addTab(fork_page, "Fork diagnosis")
        right_layout.addWidget(self.tabs, 2)
        splitter.addWidget(right)
        right.setMinimumWidth(650)
        splitter.setStretchFactor(0, 4)
        splitter.setStretchFactor(1, 7)
        splitter.setSizes([565, 865])
        outer.addWidget(splitter, 1)

        footer = QLabel(
            "State source: atomically published step snapshots only.  "
            "Clock source: generator local_time_ms; latency labels use explicit "
            "latency_steps.  Hashing: canonical serialized state + previous hash."
        )
        footer.setObjectName("footer")
        outer.addWidget(footer)

    def _apply_style(self) -> None:
        self.setStyleSheet(
            f"""
            QMainWindow, QWidget {{
                background: {BG};
                color: {TEXT};
                font-family: "Segoe UI", "Noto Sans", sans-serif;
                font-size: 10pt;
            }}
            QFrame#header, QFrame#metricCard {{
                background: {PANEL};
                border: 1px solid {BORDER};
                border-radius: 7px;
            }}
            QLabel#title {{
                color: {TEXT};
                font-size: 17pt;
                font-weight: 700;
                letter-spacing: 1px;
            }}
            QLabel#subtitle, QLabel#footer {{
                color: {MUTED};
                font-size: 8.5pt;
            }}
            QLabel#metricTitle {{
                color: {MUTED};
                font-size: 8pt;
                font-weight: 600;
            }}
            QLabel#metricValue {{
                color: {TEXT};
                font-size: 15pt;
                font-weight: 700;
            }}
            QLabel#sectionTitle {{
                color: {ACCENT};
                font-size: 9pt;
                font-weight: 700;
                letter-spacing: 1px;
            }}
            QLabel#statusChip {{
                color: {ACCENT};
                background: #102b3d;
                border: 1px solid #28506c;
                border-radius: 12px;
                padding: 5px 14px;
                font-weight: 700;
            }}
            QLabel#diagnosticLabel {{
                background: {PANEL};
                border: 1px solid {BORDER};
                border-radius: 5px;
                padding: 7px;
                color: {TEXT};
            }}
            QPushButton {{
                background: {PANEL_ALT};
                border: 1px solid {BORDER};
                border-radius: 5px;
                padding: 6px 13px;
                color: {TEXT};
            }}
            QPushButton:hover {{
                border-color: {ACCENT};
                background: #1c3040;
            }}
            QPushButton:pressed {{
                background: #102535;
            }}
            QPushButton:disabled {{
                color: #536473;
                background: #101820;
                border-color: #1c2a36;
            }}
            QPushButton#primary {{
                background: #17618d;
                border-color: {ACCENT};
                font-weight: 700;
            }}
            QLineEdit, QComboBox, QSpinBox {{
                background: #0d1721;
                border: 1px solid {BORDER};
                border-radius: 4px;
                padding: 5px 7px;
                color: {TEXT};
            }}
            QComboBox QAbstractItemView {{
                background: {PANEL};
                selection-background-color: #1d4b69;
            }}
            QTableWidget {{
                background: {PANEL};
                alternate-background-color: #0f1923;
                border: 1px solid {BORDER};
                border-radius: 5px;
                gridline-color: #203140;
                selection-background-color: #19384d;
            }}
            QHeaderView::section {{
                background: {PANEL_ALT};
                color: {MUTED};
                border: none;
                border-bottom: 1px solid {BORDER};
                padding: 6px;
                font-size: 8pt;
                font-weight: 700;
            }}
            QPlainTextEdit {{
                background: #0d1721;
                border: 1px solid {BORDER};
                border-radius: 5px;
                color: #b8c9d7;
                font-family: "Cascadia Mono", "Consolas", monospace;
                font-size: 8.5pt;
            }}
            QTabWidget::pane {{
                border: 1px solid {BORDER};
                border-radius: 5px;
                background: #0e1822;
            }}
            QTabBar::tab {{
                background: {PANEL};
                color: {MUTED};
                border: 1px solid {BORDER};
                border-bottom: none;
                padding: 7px 13px;
            }}
            QTabBar::tab:selected {{
                color: {TEXT};
                background: #172b3a;
                border-top: 2px solid {ACCENT};
            }}
            QSplitter::handle {{
                background: {BG};
                width: 8px;
            }}
            QFrame#divider {{
                color: {BORDER};
            }}
            """
        )

    def _load_latency_metadata(self) -> None:
        shots = self.shots_payload.get("shots", [])
        if shots and all("latency_steps" in shot for shot in shots):
            self.latency_canvas.set_latencies(
                [int(shot["latency_steps"]) for shot in shots]
            )
        else:
            self.latency_canvas.set_latencies(None)

    def _start_fresh(self) -> None:
        try:
            self.runner.start(restore=False)
            self.history.clear()
            self._last_rendered_step = -1
            self._message("Fresh replay started.")
        except ReviewError as exc:
            self._show_business_error(exc)
        except (OSError, ValueError) as exc:
            self._message(f"Start failed: {exc}")

    def _restore(self) -> None:
        try:
            self.runner.start(restore=True)
            self._message("Continuing from the latest valid snapshot.")
        except ReviewError as exc:
            self._show_business_error(exc)
        except (OSError, ValueError) as exc:
            self._message(f"Restore failed: {exc}")

    def _toggle_pause(self) -> None:
        if self.runner.status == "PAUSED":
            self.runner.resume()
            self._message("Replay resumed at the next boundary.")
        else:
            self.runner.pause()
            self._message("Pause requested; it takes effect at a step boundary.")

    def _cancel(self) -> None:
        self.runner.cancel()
        self._message("Cancellation requested; the last published snapshot is retained.")

    def _inject_revocation(self) -> None:
        try:
            command = self.runner.inject_revocation(
                self.player_combo.currentText(),
                self.seq_spin.value(),
                reason=self.revoke_reason.text().strip()
                or "interactive revocation injection",
            )
            self._message(
                f"Revocation {command['revocation_id']} queued for the next boundary."
            )
        except ReviewError as exc:
            self._show_business_error(exc)

    def _message(self, text: str) -> None:
        self._last_message = text
        self.log.appendPlainText(text)

    def _show_business_error(self, error: ReviewError) -> None:
        payload = error.to_dict()
        self._message(f"{payload['code']}: {payload.get('message', '')}")
        QMessageBox.warning(
            self,
            payload["code"],
            f"{payload['code']}\n\n{payload.get('message', '')}",
        )

    def _read_snapshot(self, step: int) -> dict[str, Any] | None:
        path = self.store.snapshot_path(step)
        try:
            with path.open("r", encoding="utf-8") as handle:
                data = json.load(handle)
            if int(data.get("step", -1)) != step or "summary" not in data:
                raise ValueError("incomplete snapshot")
            return data
        except (OSError, ValueError, json.JSONDecodeError) as exc:
            self._message(f"E_RECOVERY_INVALID reading published step {step}: {exc}")
            return None

    def _refresh(self, force: bool = False) -> None:
        steps = self.store.list_steps()
        if not steps:
            self.history.clear()
            self._last_rendered_step = -1
        else:
            if self._last_rendered_step > steps[-1]:
                self.history.clear()
            wanted = [step for step in steps if step not in self.history]
            for step in wanted:
                snapshot = self._read_snapshot(step)
                if snapshot is not None:
                    self.history[step] = snapshot
            if self.history:
                self.history = {
                    step: value
                    for step, value in self.history.items()
                    if step in set(steps)
                }
        self._update_controls()
        if not self.history:
            latest = None
            if self._last_rendered_step >= 0:
                self._clear_display()
        else:
            latest_step = max(self.history)
            latest = self.history[latest_step]
        if latest is not None and (
            force
            or int(latest["summary"].get("step", -1)) != self._last_rendered_step
        ):
            self._render_latest(latest)
            self._last_rendered_step = int(latest["summary"].get("step", -1))
        elif latest is not None and force:
            self._render_latest(latest)

    def _clear_display(self) -> None:
        self.cover_table.setRowCount(0)
        self.revoke_table.setRowCount(0)
        self.reconcile_table.setRowCount(0)
        self.fork_table.setRowCount(0)
        self.step_card.set_value("--")
        self.remaining_card.set_value("--")
        self.pending_card.set_value("--")
        self.conflict_card.set_value("--")
        self.revoke_card.set_value("--")
        self.timeline.set_data([], self.initial)
        self.pending_canvas.set_data([], self.initial)
        self.divergence_label.setText("No published snapshots yet.")
        self._last_rendered_step = -1

    def _update_controls(self) -> None:
        status = self.runner.status
        busy = self.runner.busy
        self.status_chip.setText(status)
        self.start_button.setEnabled(not busy)
        self.restore_button.setEnabled(not busy and bool(self.store.list_steps()))
        self.pause_button.setEnabled(busy)
        self.cancel_button.setEnabled(busy)
        self.pause_button.setText("Resume" if status == "PAUSED" else "Pause")
        if status == "FAILED" and self.runner.last_error:
            error = self.runner.last_error
            text = f"{error.get('code')}: {error.get('message', '')}"
            if text != self._last_message:
                self._message(text)
        if status == "CANCELLED" and self._last_message != "Replay cancelled.":
            self._message("Replay cancelled.")
        if status == "COMPLETED" and self._last_message != "Replay completed.":
            self._message("Replay completed.")

    def _render_latest(self, snapshot: dict[str, Any]) -> None:
        summary = snapshot["summary"]
        state = snapshot.get("state", {})
        covers = summary.get("covers", [])
        self.cover_table.setRowCount(len(covers))
        for row, cover in enumerate(covers):
            material = self.initial.covers.get(cover["cover_id"])
            values = [
                cover["cover_id"],
                (
                    f"{int(cover['remaining']) / 1_000_000:.2f} / "
                    f"{int(cover['max_durability']) / 1_000_000:.0f}"
                ),
                str(cover.get("segment", 0)),
                "CRACKED" if cover.get("cracked") else "intact",
                "ALIVE" if cover.get("alive") else "DESTROYED",
                str(len(cover.get("crack_events", []))),
            ]
            for column, value in enumerate(values):
                item = QTableWidgetItem(value)
                item.setTextAlignment(Qt.AlignmentFlag.AlignCenter)
                if column == 0 and material is not None:
                    item.setToolTip(
                        f"material hardness {material.hardness:.2f}; "
                        f"segment threshold {material.segment_threshold / 1_000_000:.2f}"
                    )
                if column in (3, 4):
                    if column == 3:
                        color = BAD if cover.get("cracked") else GOOD
                    else:
                        color = GOOD if cover.get("alive") else BAD
                    item.setForeground(QBrush(QColor(color)))
                    item.setFont(_font(9, True))
                self.cover_table.setItem(row, column, item)

        self.step_card.set_value(
            f"{int(summary['step']):02d} / {self.initial.end_step}"
        )
        remaining = int(summary.get("remaining_total", 0)) / 1_000_000.0
        initial_total = self.initial.total_durability / 1_000_000.0
        self.remaining_card.set_value(f"{remaining:.1f} / {initial_total:.1f}")
        pending = sum(int(value) for value in summary.get("pending", {}).values()) / 1_000_000.0
        self.pending_card.set_value(f"{pending:.3f}", WARN if pending > 0 else GOOD)
        step_conflicts = len(summary.get("conflicts", []))
        conflicts = sum(
            1
            for item in state.get("operational_reports", [])
            if item.get("code") == "E_DAMAGE_CONFLICT"
        )
        self.conflict_card.set_value(
            f"{conflicts} total / {step_conflicts} step",
            BAD if conflicts else GOOD,
        )
        revocations = state.get("revocations", [])
        restore_sum = sum(
            int(item.get("restore_amount", 0))
            for item in revocations
            if item.get("status") == "ACCEPTED"
        ) / 1_000_000.0
        self.revoke_card.set_value(
            f"{len(revocations)} / +{restore_sum:.1f}",
            VIOLET if revocations else TEXT,
        )

        ordered = [self.history[step] for step in sorted(self.history)]
        self.timeline.set_data(ordered, self.initial)
        self.pending_canvas.set_data(ordered, self.initial)
        self._render_revocations(revocations)
        self._render_reconciliation(state.get("settled_shots", []))
        self._render_fork(ordered)
        self._render_events(state, summary)
        self._update_sequence_defaults(state.get("buffer", []))

    def _render_revocations(self, revocations: Sequence[dict[str, Any]]) -> None:
        self.revoke_table.setRowCount(len(revocations))
        for row, item in enumerate(revocations):
            values = [
                str(item.get("step", "")),
                str(item.get("player_id", "")),
                str(item.get("client_seq", "")),
                str(item.get("status", "")),
                str(item.get("reason_code") or ""),
                f"{int(item.get('restore_amount', 0)) / 1_000_000:.3f}",
                str(item.get("reason", "")),
            ]
            for column, value in enumerate(values):
                cell = QTableWidgetItem(value)
                cell.setTextAlignment(Qt.AlignmentFlag.AlignCenter)
                cell.setToolTip(
                    "before="
                    + json.dumps(item.get("before_pending", {}), ensure_ascii=False)
                    + "\nafter="
                    + json.dumps(item.get("after_pending", {}), ensure_ascii=False)
                )
                if column == 3:
                    cell.setForeground(
                        QBrush(
                            QColor(
                                VIOLET
                                if item.get("status") == "ACCEPTED"
                                else BAD
                            )
                        )
                    )
                self.revoke_table.setItem(row, column, cell)

    def _render_reconciliation(
        self, outcomes: Sequence[dict[str, Any]]
    ) -> None:
        ordered = sorted(
            outcomes,
            key=lambda item: (
                int(item.get("settlement_step", 0)),
                str(item.get("record", {}).get("player_id", "")),
                int(item.get("record", {}).get("client_seq", 0)),
            ),
        )
        self.reconcile_table.setRowCount(len(ordered))
        for row, outcome in enumerate(ordered):
            record = outcome.get("record", {})
            predicted = {
                str(key): float(value)
                for key, value in dict(outcome.get("predicted", {})).items()
            }
            authoritative = {
                str(key): float(value)
                for key, value in dict(outcome.get("authoritative", {})).items()
            }
            predicted_total = sum(predicted.values())
            authoritative_total = sum(authoritative.values())
            differences = []
            for cover_id in sorted(set(predicted) | set(authoritative)):
                delta = authoritative.get(cover_id, 0) - predicted.get(cover_id, 0)
                if abs(delta) > 1e-9:
                    differences.append(
                        f"{cover_id}:{predicted.get(cover_id, 0):.2f}"
                        f"→{authoritative.get(cover_id, 0):.2f}"
                    )
            values = [
                str(outcome.get("settlement_step", "")),
                str(record.get("record_id", "")),
                str(record.get("player_id", "")),
                str(record.get("client_seq", "")),
                str(outcome.get("status", "")),
                f"{predicted_total:.3f}",
                f"{authoritative_total:.3f}",
                "; ".join(differences) if differences else "none",
            ]
            for column, value in enumerate(values):
                cell = QTableWidgetItem(value)
                cell.setTextAlignment(Qt.AlignmentFlag.AlignCenter)
                if column == 4:
                    status = str(outcome.get("status", ""))
                    color = (
                        GOOD
                        if status == "APPLIED"
                        else BAD
                        if status == "CONFLICT"
                        else WARN
                    )
                    cell.setForeground(QBrush(QColor(color)))
                    cell.setFont(_font(9, True))
                self.reconcile_table.setItem(row, column, cell)

    def _render_fork(self, ordered: Sequence[dict[str, Any]]) -> None:
        baseline_steps = {
            int(item["step"]): item for item in self.baseline.get("steps", [])
        }
        rows: list[tuple[int, str, str, str, str]] = []
        first_divergence: int | None = None
        for snapshot in ordered:
            summary = snapshot["summary"]
            step = int(summary["step"])
            expected = baseline_steps.get(step)
            if expected is None:
                rows.append((step, "--", str(summary.get("state_hash", "")), "NO BASE", "--"))
                if first_divergence is None:
                    first_divergence = step
                continue
            expected_hash = str(expected.get("state_hash", ""))
            actual_hash = str(summary.get("state_hash", ""))
            match = expected_hash == actual_hash
            expected_covers = {
                item["cover_id"]: item for item in expected.get("covers", [])
            }
            cover_equal = True
            differing: list[str] = []
            for cover in summary.get("covers", []):
                base = expected_covers.get(cover["cover_id"])
                if base is None or any(
                    base.get(field) != cover.get(field)
                    for field in ("remaining", "cracked", "alive")
                ):
                    cover_equal = False
                    differing.append(str(cover["cover_id"]))
            if not match and first_divergence is None:
                first_divergence = step
            rows.append(
                (
                    step,
                    expected_hash[:16],
                    actual_hash[:16],
                    "MATCH" if match else "DIVERGED",
                    "same" if cover_equal else ", ".join(differing),
                )
            )
        self.fork_table.setRowCount(len(rows))
        for row_index, values in enumerate(rows):
            for column, value in enumerate(values):
                cell = QTableWidgetItem(str(value))
                cell.setTextAlignment(Qt.AlignmentFlag.AlignCenter)
                if column == 3:
                    cell.setForeground(
                        QBrush(QColor(GOOD if value == "MATCH" else BAD))
                    )
                self.fork_table.setItem(row_index, column, cell)
        if first_divergence is None:
            self.divergence_label.setText(
                "Branch diagnosis: no divergence from baseline in published steps."
            )
            self.divergence_label.setStyleSheet(f"color: {GOOD};")
        else:
            self.divergence_label.setText(
                f"Branch diagnosis: first divergence at step {first_divergence}. "
                "Inspect cover state and conflict events below."
            )
            self.divergence_label.setStyleSheet(f"color: {BAD};")

    def _render_events(
        self, state: dict[str, Any], summary: dict[str, Any]
    ) -> None:
        reports = state.get("operational_reports", [])
        lines: list[str] = []
        for item in reports[-160:]:
            code = item.get("code", "EVENT")
            step = item.get("step", "?")
            subject = (
                item.get("shot_id")
                or item.get("revocation_id")
                or item.get("cover_id")
                or item.get("player_id")
                or ""
            )
            detail = ""
            if code == "E_DAMAGE_CONFLICT":
                detail = (
                    f" {item.get('cover_id')} predicted="
                    f"{float(item.get('predicted', 0)):.3f} authoritative="
                    f"{float(item.get('authoritative', 0)):.3f}"
                )
            elif code == "E_OP_LATE":
                detail = f" lag_steps={item.get('lag_steps')}"
            elif code == "E_OP_DUPLICATE":
                detail = f" kept={item.get('kept_record_id')}"
            elif code in ("E_REVOKE_CLOSED", "E_REVOKE_NOT_FOUND"):
                detail = f" player={item.get('player_id')} seq={item.get('client_seq')}"
            lines.append(f"s{step:>02} {code:<20} {subject}{detail}")
        if not lines:
            lines.append(
                f"s{int(summary['step']):02d} state hash "
                f"{str(summary.get('state_hash', ''))[:20]}..."
            )
        self.log.setPlainText("\n".join(lines))

    def _update_sequence_defaults(self, buffer: Sequence[dict[str, Any]]) -> None:
        if not buffer:
            return
        if self.player_combo.hasFocus() or self.seq_spin.hasFocus():
            return
        first = buffer[0]
        index = self.player_combo.findText(str(first.get("player_id", "")))
        if index >= 0:
            self.player_combo.setCurrentIndex(index)
        self.seq_spin.setValue(int(first.get("client_seq", 1)))

    def closeEvent(self, event: Any) -> None:
        self.timer.stop()
        self.runner.cancel()
        self.runner.wait(2.0)
        super().closeEvent(event)


def launch_gui(
    *,
    sample_dir: str,
    run_dir: str,
    step_delay: float = 0.035,
) -> int:
    app = QApplication.instance()
    owns_app = app is None
    if app is None:
        app = QApplication(sys.argv[:1])
    app.setStyle("Fusion")
    window = WorkbenchWindow(
        sample_dir=sample_dir,
        run_dir=run_dir,
        step_delay=step_delay,
    )
    window.show()
    if not owns_app:
        window.setAttribute(Qt.WidgetAttribute.WA_DeleteOnClose)
        return 0
    return int(app.exec())

