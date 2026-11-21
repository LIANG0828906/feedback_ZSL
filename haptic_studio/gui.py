from __future__ import annotations

import json
import math
import sys
from pathlib import Path
from typing import Any

import numpy as np
from PySide6.QtCore import QPointF, QRectF, Qt, QThread, Signal
from PySide6.QtGui import (
    QAction,
    QColor,
    QFont,
    QPainter,
    QPainterPath,
    QPen,
    QPolygonF,
)
from PySide6.QtWidgets import (
    QAbstractItemView,
    QApplication,
    QComboBox,
    QDoubleSpinBox,
    QFileDialog,
    QFormLayout,
    QFrame,
    QHBoxLayout,
    QHeaderView,
    QLabel,
    QListWidget,
    QMainWindow,
    QMessageBox,
    QPlainTextEdit,
    QPushButton,
    QScrollArea,
    QSpinBox,
    QSplitter,
    QTableWidget,
    QTableWidgetItem,
    QTabWidget,
    QToolBar,
    QVBoxLayout,
    QWidget,
)

from .errors import ErrorCode, HapticError
from .models import DeviceConfig, Envelope, WaveSegment, WaveSequence
from .sampler import segment_bounds
from .samples import export_csv, generate_samples, load_config_file, load_wave_file
from .session import HapticSession
from .versions import PublishedSignal, QueryResult

COLORS = [
    QColor("#f5b971"),
    QColor("#77c9d4"),
    QColor("#d99be8"),
    QColor("#91d18b"),
    QColor("#ef8f8f"),
    QColor("#a8b7f0"),
]


class DrivePlot(QWidget):
    def __init__(self, parent: QWidget | None = None) -> None:
        super().__init__(parent)
        self.setMinimumHeight(260)
        self._query: QueryResult | None = None

    def set_query(self, query: QueryResult | None) -> None:
        self._query = query
        self.update()

    def paintEvent(self, event: Any) -> None:
        painter = QPainter(self)
        painter.setRenderHint(QPainter.RenderHint.Antialiasing)
        painter.fillRect(self.rect(), QColor("#11161d"))
        if self._query is None:
            painter.setPen(QColor("#8592a6"))
            painter.drawText(
                self.rect(), Qt.AlignmentFlag.AlignCenter, "No published drive signal"
            )
            return
        published = self._query.published
        left = self._query.left
        right = self._query.right
        limited = self._query.limited_mask
        config = published.config
        max_code = max(1, config.max_code)
        margin_left = 58
        margin_right = 18
        margin_top = 32
        margin_bottom = 24
        width = max(1, self.width() - margin_left - margin_right)
        height = max(1, self.height() - margin_top - margin_bottom)
        lane_gap = 12
        lane_height = max(20, (height - lane_gap) // 2)

        painter.setPen(QColor("#dce5f2"))
        font = QFont()
        font.setBold(True)
        painter.setFont(font)
        painter.drawText(
            QRectF(margin_left, 7, width, 20),
            Qt.AlignmentFlag.AlignLeft,
            f"v{published.publication_version}  c{published.config_version}  "
            f"{published.signal_hash[:12]}  "
            f"limited={published.limit_stats.limited_sample_count}",
        )
        painter.setFont(QFont())
        lanes = [
            ("L", left, QColor("#ffab5e"), margin_top),
            ("R", right, QColor("#43d6c5"), margin_top + lane_height + lane_gap),
        ]
        for label, values, color, top in lanes:
            lane_rect = QRectF(margin_left, top, width, lane_height)
            painter.fillRect(lane_rect, QColor("#171f29"))
            painter.setPen(QPen(QColor("#2a3645"), 1))
            painter.drawRect(lane_rect)
            for fraction in (0.0, 0.5, 1.0):
                y = top + fraction * lane_height
                painter.setPen(QPen(QColor("#263140"), 1, Qt.PenStyle.DotLine))
                painter.drawLine(
                    QPointF(margin_left, y), QPointF(margin_left + width, y)
                )
            painter.setPen(color)
            painter.drawText(
                QRectF(8, top + lane_height / 2 - 9, 40, 18),
                Qt.AlignmentFlag.AlignCenter,
                label,
            )
            painter.setPen(QColor("#718096"))
            painter.drawText(
                QRectF(8, top - 4, 40, 16),
                Qt.AlignmentFlag.AlignRight,
                str(max_code),
            )
            painter.drawText(
                QRectF(8, top + lane_height - 12, 40, 16),
                Qt.AlignmentFlag.AlignRight,
                str(config.min_code),
            )
            self._paint_series(
                painter,
                values,
                margin_left,
                width,
                top,
                lane_height,
                max_code,
                color,
            )
            limited_indexes = np.flatnonzero(limited)
            if limited_indexes.size:
                painter.setPen(Qt.PenStyle.NoPen)
                painter.setBrush(QColor("#ff4d5e"))
                seen_pixels: set[int] = set()
                step = max(1, int(math.ceil(limited_indexes.size / max(1, width))))
                for sample_index in limited_indexes[::step]:
                    x = margin_left + (sample_index / max(1, len(values) - 1)) * width
                    pixel = int(x)
                    if pixel in seen_pixels:
                        continue
                    seen_pixels.add(pixel)
                    painter.drawPolygon(
                        QPolygonF(
                            [
                                QPointF(x, top + 2),
                                QPointF(x - 3, top + 8),
                                QPointF(x + 3, top + 8),
                            ]
                        )
                    )
        painter.setPen(QColor("#718096"))
        total_ms = published.sequence.total_duration_ms
        for fraction in (0.0, 0.25, 0.5, 0.75, 1.0):
            x = margin_left + fraction * width
            painter.drawLine(
                QPointF(x, margin_top + height),
                QPointF(x, margin_top + height + 4),
            )
            painter.drawText(
                QRectF(x - 40, margin_top + height + 4, 80, 18),
                Qt.AlignmentFlag.AlignCenter,
                f"{fraction * total_ms:.0f} ms",
            )

    @staticmethod
    def _paint_series(
        painter: QPainter,
        values: np.ndarray,
        x0: float,
        width: float,
        top: float,
        height: float,
        max_code: int,
        color: QColor,
    ) -> None:
        count = len(values)
        if count == 0:
            return
        middle = top + height / 2.0
        amplitude = height / 2.0 - 3
        painter.setPen(QPen(color, 1.1))

        def y_for(value: int) -> float:
            return middle - (float(value) / max_code) * amplitude

        if count <= max(2, int(width * 1.5)):
            path = QPainterPath()
            for index, value in enumerate(values):
                x = x0 + (index / max(1, count - 1)) * width
                y = y_for(int(value))
                if index == 0:
                    path.moveTo(x, y)
                else:
                    path.lineTo(x, y)
            painter.drawPath(path)
            return
        bins = max(1, int(width))
        chunks = np.array_split(values, bins)
        for index, chunk in enumerate(chunks):
            if chunk.size == 0:
                continue
            x = x0 + (index + 0.5) / bins * width
            painter.drawLine(
                QPointF(x, y_for(int(chunk.min()))),
                QPointF(x, y_for(int(chunk.max()))),
            )


class WaveTimeline(QWidget):
    def __init__(self, parent: QWidget | None = None) -> None:
        super().__init__(parent)
        self.setMinimumHeight(170)
        self._query: QueryResult | None = None

    def set_query(self, query: QueryResult | None) -> None:
        self._query = query
        self.update()

    def paintEvent(self, event: Any) -> None:
        painter = QPainter(self)
        painter.setRenderHint(QPainter.RenderHint.Antialiasing)
        painter.fillRect(self.rect(), QColor("#11161d"))
        if self._query is None:
            return
        published = self._query.published
        margin_left = 32
        margin_right = 16
        margin_top = 24
        margin_bottom = 22
        width = max(1, self.width() - margin_left - margin_right)
        height = max(1, self.height() - margin_top - margin_bottom)
        painter.setPen(QColor("#dce5f2"))
        font = QFont()
        font.setBold(True)
        painter.setFont(font)
        painter.drawText(
            QRectF(margin_left, 3, width, 18),
            Qt.AlignmentFlag.AlignLeft,
            f"Waveform  {published.sequence.sequence_id}  "
            f"{published.sequence.total_duration_ms:.0f} ms",
        )
        painter.setFont(QFont())
        painter.setPen(QPen(QColor("#263140"), 1))
        painter.drawLine(
            QPointF(margin_left, margin_top + height),
            QPointF(margin_left + width, margin_top + height),
        )
        total_frames = max(1, published.sampling.total_frames)
        for ordinal, segment in enumerate(published.sequence.sorted_segments):
            start, end = segment_bounds(
                segment, published.config.sample_rate, total_frames
            )
            if end <= start:
                continue
            x_start = margin_left + start / total_frames * width
            x_end = margin_left + end / total_frames * width
            segment_width = max(2.0, x_end - x_start)
            color = COLORS[ordinal % len(COLORS)]
            fill = QColor(color)
            fill.setAlpha(44)
            painter.fillRect(QRectF(x_start, margin_top, segment_width, height), fill)
            painter.setPen(QPen(color, 1.5))
            path = QPainterPath()
            points = max(2, int(segment_width))
            for point_index in range(points):
                fraction = point_index / max(1, points - 1)
                if segment.envelope is Envelope.RECTANGULAR:
                    envelope = 1.0
                elif segment.envelope is Envelope.LINEAR_DECAY:
                    envelope = 1.0 - fraction
                else:
                    envelope = math.sin(math.pi * fraction)
                x = x_start + fraction * segment_width
                y = margin_top + height - envelope * segment.amplitude * height
                if point_index == 0:
                    path.moveTo(x, y)
                else:
                    path.lineTo(x, y)
            painter.drawPath(path)
            painter.drawText(
                QRectF(x_start + 3, margin_top + 3, max(70, segment_width - 4), 16),
                Qt.AlignmentFlag.AlignLeft,
                f"#{segment.index} {segment.envelope.value}",
            )
        painter.setPen(QColor("#718096"))
        painter.drawText(
            QRectF(margin_left, margin_top + height + 3, width, 16),
            Qt.AlignmentFlag.AlignRight,
            f"{published.sequence.total_duration_ms:.0f} ms",
        )


class VersionTimeline(QWidget):
    versionClicked = Signal(int)

    def __init__(self, parent: QWidget | None = None) -> None:
        super().__init__(parent)
        self.setMinimumHeight(92)
        self._history: tuple[PublishedSignal, ...] = ()
        self._selected = 0

    def set_history(self, history: tuple[PublishedSignal, ...], selected: int) -> None:
        self._history = history
        self._selected = selected
        self.update()

    def paintEvent(self, event: Any) -> None:
        painter = QPainter(self)
        painter.setRenderHint(QPainter.RenderHint.Antialiasing)
        painter.fillRect(self.rect(), QColor("#0d1218"))
        painter.setPen(QColor("#8592a6"))
        painter.drawText(12, 18, "Version timeline")
        if not self._history:
            return
        left = 28
        right = self.width() - 28
        y = 52
        painter.setPen(QPen(QColor("#344154"), 2))
        painter.drawLine(left, y, right, y)
        span = max(1, right - left)
        step = span / max(1, len(self._history) - 1) if len(self._history) > 1 else 0
        for index, published in enumerate(self._history):
            x = left + (index * step if len(self._history) > 1 else span / 2)
            selected = published.publication_version == self._selected
            color = QColor("#ffb454") if selected else QColor("#43d6c5")
            painter.setBrush(color)
            painter.setPen(QPen(QColor("#0d1218"), 2))
            radius = 7 if selected else 5
            painter.drawEllipse(QPointF(x, y), radius, radius)
            painter.setPen(QColor("#dce5f2") if selected else QColor("#8592a6"))
            painter.drawText(
                QRectF(x - 52, y + 10, 104, 16),
                Qt.AlignmentFlag.AlignCenter,
                f"v{published.publication_version} c{published.config_version}",
            )
            painter.setPen(QColor("#65748b"))
            painter.drawText(
                QRectF(x - 52, y + 25, 104, 14),
                Qt.AlignmentFlag.AlignCenter,
                published.signal_hash[:8],
            )

    def mousePressEvent(self, event: Any) -> None:
        if not self._history:
            return
        left = 28
        right = self.width() - 28
        span = max(1, right - left)
        step = span / max(1, len(self._history) - 1) if len(self._history) > 1 else 0
        clicked = event.position().x()
        best: PublishedSignal | None = None
        best_distance = float("inf")
        for index, published in enumerate(self._history):
            x = left + (index * step if len(self._history) > 1 else span / 2)
            distance = abs(clicked - x)
            if distance < best_distance:
                best_distance = distance
                best = published
        if best is not None:
            self.versionClicked.emit(best.publication_version)


class RecomputeWorker(QThread):
    succeeded = Signal(object)
    failed = Signal(object)

    def __init__(
        self,
        session: HapticSession,
        sequence: WaveSequence,
        config: DeviceConfig | None,
    ) -> None:
        super().__init__()
        self.session = session
        self.sequence = sequence
        self.config = config

    def run(self) -> None:
        try:
            published = self.session.recompute(
                self.sequence, new_config=self.config, operation="gui_recompute"
            )
            self.succeeded.emit(published)
        except HapticError as exc:
            self.failed.emit(exc.to_dict())
        except Exception as exc:  # defensive boundary for the UI thread
            self.failed.emit(
                {
                    "code": "E_IO",
                    "message": f"unexpected recompute failure: {exc}",
                }
            )


class RollbackWorker(QThread):
    succeeded = Signal(object)
    failed = Signal(object)

    def __init__(self, session: HapticSession, config_version: int) -> None:
        super().__init__()
        self.session = session
        self.config_version = config_version

    def run(self) -> None:
        try:
            published = self.session.rollback_config(self.config_version)
            self.succeeded.emit(published)
        except HapticError as exc:
            self.failed.emit(exc.to_dict())


class MainWindow(QMainWindow):
    def __init__(
        self,
        sample_dir: Path,
        runtime_dir: Path,
        seed: int,
    ) -> None:
        super().__init__()
        self.sample_dir = sample_dir
        self.runtime_dir = runtime_dir
        self.seed = seed
        self._loading = False
        self.session: HapticSession | None = None
        self.worker: RecomputeWorker | RollbackWorker | None = None
        self._current_query: QueryResult | None = None
        self._displayed_version: int | None = None
        self.setWindowTitle("Haptic Studio — Deterministic Waveform Preview")
        self.resize(1480, 920)
        self._build_ui()
        self._apply_style()
        self._refresh_samples()
        if self.sample_combo.count():
            self.sample_combo.setCurrentIndex(0)
            self._load_sample(self.sample_combo.currentText())

    def _build_ui(self) -> None:
        toolbar = QToolBar("Main")
        toolbar.setMovable(False)
        self.addToolBar(toolbar)
        self.sample_combo = QComboBox()
        self.sample_combo.setMinimumWidth(160)
        toolbar.addWidget(QLabel("Sample "))
        toolbar.addWidget(self.sample_combo)
        self.sample_combo.currentTextChanged.connect(self._sample_changed)

        generate_action = QAction("Generate samples", self)
        generate_action.triggered.connect(self._generate_samples)
        toolbar.addAction(generate_action)
        load_wave_action = QAction("Load wave JSON", self)
        load_wave_action.triggered.connect(self._load_wave_dialog)
        toolbar.addAction(load_wave_action)
        load_config_action = QAction("Load device JSON", self)
        load_config_action.triggered.connect(self._load_config_dialog)
        toolbar.addAction(load_config_action)
        toolbar.addSeparator()
        self.recompute_action = QAction("Recompute", self)
        self.recompute_action.triggered.connect(self._start_recompute)
        toolbar.addAction(self.recompute_action)
        self.cancel_action = QAction("Cancel", self)
        self.cancel_action.setEnabled(False)
        self.cancel_action.triggered.connect(self._cancel_recompute)
        toolbar.addAction(self.cancel_action)
        export_action = QAction("Export CSV", self)
        export_action.triggered.connect(self._export_csv)
        toolbar.addAction(export_action)

        root = QSplitter(Qt.Orientation.Horizontal)
        left_scroll = QScrollArea()
        left_scroll.setWidgetResizable(True)
        left_scroll.setMinimumWidth(430)
        left_scroll.setMaximumWidth(570)
        left_panel = QWidget()
        left_layout = QVBoxLayout(left_panel)
        left_layout.setContentsMargins(10, 10, 10, 10)
        left_scroll.setWidget(left_panel)
        root.addWidget(left_scroll)

        tabs = QTabWidget()
        segment_tab = QWidget()
        segment_layout = QVBoxLayout(segment_tab)
        total_row = QHBoxLayout()
        total_row.addWidget(QLabel("Total duration (ms)"))
        self.total_duration = QDoubleSpinBox()
        self.total_duration.setRange(1.0, 600_000.0)
        self.total_duration.setDecimals(3)
        self.total_duration.setSingleStep(10)
        total_row.addWidget(self.total_duration)
        total_row.addStretch(1)
        segment_layout.addLayout(total_row)
        self.segment_table = QTableWidget(0, 6)
        self.segment_table.setHorizontalHeaderLabels(
            ["#", "Start ms", "Duration ms", "Frequency Hz", "Amplitude", "Envelope"]
        )
        self.segment_table.horizontalHeader().setSectionResizeMode(
            QHeaderView.ResizeMode.Stretch
        )
        self.segment_table.verticalHeader().setVisible(False)
        self.segment_table.setSelectionBehavior(
            QAbstractItemView.SelectionBehavior.SelectRows
        )
        self.segment_table.setEditTriggers(
            QAbstractItemView.EditTrigger.DoubleClicked
            | QAbstractItemView.EditTrigger.EditKeyPressed
        )
        segment_layout.addWidget(self.segment_table, 1)
        segment_buttons = QHBoxLayout()
        add_button = QPushButton("Add segment")
        add_button.clicked.connect(self._add_segment)
        remove_button = QPushButton("Remove selected")
        remove_button.clicked.connect(self._remove_segment)
        segment_buttons.addWidget(add_button)
        segment_buttons.addWidget(remove_button)
        segment_buttons.addStretch(1)
        segment_layout.addLayout(segment_buttons)
        tabs.addTab(segment_tab, "Segments")

        config_tab = QWidget()
        config_form = QFormLayout(config_tab)
        self.sample_rate_spin = QSpinBox()
        self.sample_rate_spin.setRange(64, 384_000)
        self.sample_rate_spin.setSingleStep(500)
        self.bit_depth_spin = QSpinBox()
        self.bit_depth_spin.setRange(2, 24)
        self.left_ratio_spin = QDoubleSpinBox()
        self.left_ratio_spin.setRange(0.0, 1.0)
        self.left_ratio_spin.setSingleStep(0.05)
        self.left_ratio_spin.setDecimals(3)
        self.allocation_label = QLabel("")
        self.left_polarity_combo = QComboBox()
        self.left_polarity_combo.addItem("+1", 1)
        self.left_polarity_combo.addItem("-1", -1)
        self.right_polarity_combo = QComboBox()
        self.right_polarity_combo.addItem("+1", 1)
        self.right_polarity_combo.addItem("-1", -1)
        self.limiter_combo = QComboBox()
        self.limiter_combo.addItem("hard", "hard")
        self.limiter_combo.addItem("soft", "soft")
        config_form.addRow("Sample rate", self.sample_rate_spin)
        config_form.addRow("Bit depth", self.bit_depth_spin)
        config_form.addRow("Left allocation", self.left_ratio_spin)
        config_form.addRow("Allocation L/R", self.allocation_label)
        config_form.addRow("Left polarity", self.left_polarity_combo)
        config_form.addRow("Right polarity", self.right_polarity_combo)
        config_form.addRow("Limiter", self.limiter_combo)
        self.config_version_label = QLabel("—")
        self.config_version_label.setWordWrap(True)
        config_form.addRow("Committed config", self.config_version_label)
        self.left_ratio_spin.valueChanged.connect(self._refresh_allocation_label)
        tabs.addTab(config_tab, "Device config")
        left_layout.addWidget(tabs, 1)

        history_group = QFrame()
        history_layout = QVBoxLayout(history_group)
        history_layout.setContentsMargins(0, 4, 0, 0)
        history_layout.addWidget(QLabel("Published versions"))
        self.version_list = QListWidget()
        self.version_list.setMaximumHeight(160)
        self.version_list.setSelectionMode(
            QAbstractItemView.SelectionMode.ExtendedSelection
        )
        self.version_list.itemClicked.connect(self._version_list_clicked)
        history_layout.addWidget(self.version_list)
        version_buttons = QHBoxLayout()
        diff_button = QPushButton("Diff selected")
        diff_button.clicked.connect(self._diff_versions)
        rollback_button = QPushButton("Rollback config")
        rollback_button.clicked.connect(self._rollback_selected)
        version_buttons.addWidget(diff_button)
        version_buttons.addWidget(rollback_button)
        version_buttons.addStretch(1)
        history_layout.addLayout(version_buttons)
        left_layout.addWidget(history_group)

        right = QWidget()
        right_layout = QVBoxLayout(right)
        right_layout.setContentsMargins(6, 6, 6, 6)
        self.wave_timeline = WaveTimeline()
        self.drive_plot = DrivePlot()
        self.version_timeline = VersionTimeline()
        self.version_timeline.versionClicked.connect(self._show_version)
        self.trace_view = QPlainTextEdit()
        self.trace_view.setReadOnly(True)
        self.trace_view.setMaximumBlockCount(30_000)
        self.trace_view.setMinimumHeight(100)
        right_layout.addWidget(self.wave_timeline, 2)
        right_layout.addWidget(self.drive_plot, 4)
        right_layout.addWidget(self.version_timeline, 1)
        right_layout.addWidget(QLabel("Deterministic trace log"))
        right_layout.addWidget(self.trace_view, 2)
        root.addWidget(right)
        root.setStretchFactor(0, 0)
        root.setStretchFactor(1, 1)
        self.setCentralWidget(root)
        self.statusBar().showMessage("Ready")

    def _apply_style(self) -> None:
        self.setStyleSheet(
            """
            QMainWindow, QWidget { background: #0d1218; color: #dce5f2; }
            QToolBar { background: #121922; border-bottom: 1px solid #263140; spacing: 6px; }
            QTableWidget, QListWidget, QPlainTextEdit, QComboBox, QSpinBox,
            QDoubleSpinBox { background: #161f2a; color: #dce5f2; border: 1px solid #2a3645; }
            QHeaderView::section { background: #1d2836; color: #9fb0c5; border: 0; padding: 4px; }
            QPushButton { background: #243244; color: #e4edf8; border: 1px solid #3b4d64;
                          padding: 5px 10px; border-radius: 3px; }
            QPushButton:hover { background: #2e4055; }
            QTabBar::tab { background: #151d27; padding: 7px 12px; }
            QTabBar::tab:selected { background: #263447; color: #ffb454; }
            QStatusBar { background: #121922; color: #9fb0c5; }
            """
        )

    def _refresh_samples(self) -> None:
        manifest_path = self.sample_dir / "manifest.json"
        if not manifest_path.exists():
            generate_samples(self.sample_dir, seed=self.seed)
        manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
        current = self.sample_combo.currentText()
        self.sample_combo.blockSignals(True)
        self.sample_combo.clear()
        for item in manifest["samples"]:
            self.sample_combo.addItem(item["sample_id"])
        if current:
            index = self.sample_combo.findText(current)
            if index >= 0:
                self.sample_combo.setCurrentIndex(index)
        self.sample_combo.blockSignals(False)

    def _generate_samples(self) -> None:
        try:
            generate_samples(self.sample_dir, seed=self.seed)
            self._refresh_samples()
            if self.sample_combo.count():
                self._load_sample(self.sample_combo.currentText())
            self.statusBar().showMessage("Deterministic samples regenerated")
        except (HapticError, OSError, TypeError, ValueError) as exc:
            self._show_error({"code": "E_IO", "message": str(exc)})

    def _sample_changed(self, sample_id: str) -> None:
        if not sample_id or (self.session is not None and self._loading):
            return
        self._load_sample(sample_id)

    def _load_sample(self, sample_id: str) -> None:
        if not sample_id:
            return
        self._loading = True
        try:
            wave_path = self.sample_dir / f"{sample_id}_wave.json"
            config_path = self.sample_dir / f"{sample_id}_devices.json"
            sequence = load_wave_file(wave_path)
            configs = load_config_file(config_path)
            data_dir = self.runtime_dir / sample_id
            if (data_dir / "session.snapshot.json").exists():
                session = HapticSession.open(
                    data_dir, sequence=sequence, config=configs[0][2]
                )
            else:
                session = HapticSession.create(sequence, configs[0][2], data_dir)
            self.session = session
            self._populate_from_session()
            self.statusBar().showMessage(
                f"Loaded {sample_id}; {session.recovery_report.note}"
            )
        except HapticError as exc:
            self._show_error(exc.to_dict())
        finally:
            self._loading = False

    def _populate_from_session(self) -> None:
        if self.session is None:
            return
        sequence = self.session.sequence
        config = self.session.config
        self.total_duration.setValue(sequence.total_duration_ms)
        self.segment_table.blockSignals(True)
        self.segment_table.setRowCount(len(sequence.segments))
        for row, segment in enumerate(sequence.segments):
            self._set_segment_row(row, segment)
        self.segment_table.blockSignals(False)
        self._apply_config_controls(config)
        self.config_version_label.setText(
            f"v{self.session.current_config_version}  {config.content_hash[:16]}…"
        )
        self._refresh_history(select_latest=True)
        self._show_latest()

    def _apply_config_controls(self, config: DeviceConfig) -> None:
        self.sample_rate_spin.setValue(config.sample_rate)
        self.bit_depth_spin.setValue(config.bit_depth)
        self.left_ratio_spin.setValue(config.left_ratio)
        self.left_polarity_combo.setCurrentIndex(
            max(0, self.left_polarity_combo.findData(config.left_polarity))
        )
        self.right_polarity_combo.setCurrentIndex(
            max(0, self.right_polarity_combo.findData(config.right_polarity))
        )
        self.limiter_combo.setCurrentIndex(
            max(0, self.limiter_combo.findData(config.limiter))
        )
        self._refresh_allocation_label()

    def _set_segment_row(self, row: int, segment: WaveSegment) -> None:
        values = [
            str(segment.index),
            f"{segment.start_ms:.3f}",
            f"{segment.duration_ms:.3f}",
            f"{segment.frequency_hz:.3f}",
            f"{segment.amplitude:.4f}",
            segment.envelope.value,
        ]
        for column, value in enumerate(values):
            item = QTableWidgetItem(value)
            if column == 0:
                item.setFlags(item.flags() & ~Qt.ItemFlag.ItemIsEditable)
            self.segment_table.setItem(row, column, item)

    def _refresh_allocation_label(self) -> None:
        left = self.left_ratio_spin.value()
        self.allocation_label.setText(f"{left:.3f} / {1.0 - left:.3f}")

    def _add_segment(self) -> None:
        if self.session is None:
            return
        row = self.segment_table.rowCount()
        self.segment_table.insertRow(row)
        segment = WaveSegment(
            index=row,
            start_ms=0.0,
            duration_ms=50.0,
            frequency_hz=40.0,
            amplitude=0.5,
            envelope=Envelope.RECTANGULAR,
        )
        self._set_segment_row(row, segment)

    def _remove_segment(self) -> None:
        row = self.segment_table.currentRow()
        if row >= 0:
            self.segment_table.removeRow(row)
            for index in range(self.segment_table.rowCount()):
                item = self.segment_table.item(index, 0)
                if item is not None:
                    item.setText(str(index))

    def _collect_sequence(self) -> WaveSequence:
        if self.session is None:
            raise HapticError(ErrorCode.NOT_FOUND, "no active session")
        segments: list[dict[str, Any]] = []
        for row in range(self.segment_table.rowCount()):
            try:
                start_ms = float(self.segment_table.item(row, 1).text())
                duration_ms = float(self.segment_table.item(row, 2).text())
                frequency_hz = float(self.segment_table.item(row, 3).text())
                amplitude = float(self.segment_table.item(row, 4).text())
                envelope = self.segment_table.item(row, 5).text().strip()
            except (AttributeError, ValueError) as exc:
                raise HapticError(
                    ErrorCode.WAVE_INVALID,
                    f"invalid numeric value in segment row {row}",
                    segment_index=row,
                    field="segments",
                ) from exc
            segments.append(
                {
                    "start_ms": start_ms,
                    "duration_ms": duration_ms,
                    "frequency_hz": frequency_hz,
                    "amplitude": amplitude,
                    "envelope": envelope,
                }
            )
        return WaveSequence.from_mapping(
            {
                "schema_version": self.session.sequence.schema_version,
                "sequence_id": self.session.sequence.sequence_id,
                "total_duration_ms": self.total_duration.value(),
                "segments": segments,
            }
        )

    def _collect_config(self) -> DeviceConfig:
        left = round(self.left_ratio_spin.value(), 6)
        return DeviceConfig.from_mapping(
            {
                "sample_rate": self.sample_rate_spin.value(),
                "bit_depth": self.bit_depth_spin.value(),
                "allocation_ratio": [left, round(1.0 - left, 6)],
                "polarity": [
                    self.left_polarity_combo.currentData(),
                    self.right_polarity_combo.currentData(),
                ],
                "limiter": self.limiter_combo.currentData(),
            }
        )

    def _start_recompute(self) -> None:
        if self.session is None or self.worker is not None:
            self._show_error(
                {
                    "code": str(ErrorCode.BUSY),
                    "message": "a recompute task is already active",
                }
            )
            return
        try:
            sequence = self._collect_sequence()
            config = self._collect_config()
        except HapticError as exc:
            self._show_error(exc.to_dict())
            return
        new_config = (
            None if config.content_hash == self.session.config.content_hash else config
        )
        self.worker = RecomputeWorker(self.session, sequence, new_config)
        self.worker.succeeded.connect(self._recompute_succeeded)
        self.worker.failed.connect(self._recompute_failed)
        self.worker.finished.connect(self._worker_finished)
        self._set_busy(True)
        self.statusBar().showMessage("Recomputing…")
        self.worker.start()

    def _cancel_recompute(self) -> None:
        if self.session is not None and self.session.cancel():
            self.cancel_action.setEnabled(False)
            self.statusBar().showMessage(
                "Cancellation requested; last publication retained"
            )

    def _recompute_succeeded(self, published: PublishedSignal) -> None:
        self.statusBar().showMessage(
            f"Published v{published.publication_version}; "
            f"recomputed {len(published.recomputed_segments)} segment(s), "
            f"reused {len(published.reused_segments)}"
        )
        self._refresh_history(select_latest=True)
        self._show_latest()
        self._refresh_trace()
        if self.session is not None:
            self.config_version_label.setText(
                f"v{self.session.current_config_version}  "
                f"{self.session.config.content_hash[:16]}…"
            )

    def _recompute_failed(self, payload: dict[str, Any]) -> None:
        if payload.get("code") == str(ErrorCode.CANCELLED):
            self.statusBar().showMessage(
                "Recompute cancelled; last published version retained"
            )
            self._show_latest()
        else:
            self._show_error(payload)

    def _worker_finished(self) -> None:
        worker = self.worker
        self.worker = None
        if worker is not None:
            worker.deleteLater()
        self._set_busy(False)

    def _set_busy(self, busy: bool) -> None:
        self.recompute_action.setEnabled(not busy)
        self.cancel_action.setEnabled(busy)
        self.segment_table.setEnabled(not busy)
        self.sample_combo.setEnabled(not busy)
        if not busy:
            self.cancel_action.setEnabled(False)

    def _refresh_history(self, *, select_latest: bool) -> None:
        if self.session is None:
            return
        history = self.session.publication_history()
        self.version_list.blockSignals(True)
        self.version_list.clear()
        for published in history:
            self.version_list.addItem(
                f"v{published.publication_version}  c{published.config_version}  "
                f"{published.signal_hash[:12]}  "
                f"recompute={len(published.recomputed_segments)} "
                f"reuse={len(published.reused_segments)}"
            )
        if select_latest and history:
            self.version_list.setCurrentRow(len(history) - 1)
        self.version_list.blockSignals(False)
        selected = history[-1].publication_version if history else 0
        self.version_timeline.set_history(history, selected)
        self._refresh_trace()

    def _show_latest(self) -> None:
        if self.session is None:
            return
        try:
            handle = self.session.issue_handle()
            query = self.session.preview(handle)
        except HapticError as exc:
            self._show_error(exc.to_dict())
            return
        self._apply_query(query)

    def _show_version(self, version: int) -> None:
        if self.session is None:
            return
        try:
            query = self.session.preview_version(version)
        except HapticError as exc:
            self._show_error(exc.to_dict())
            return
        self._apply_query(query)
        self.version_timeline.set_history(self.session.publication_history(), version)
        self.statusBar().showMessage(
            f"Historical preview v{version}  hit={query.hit_version}  "
            f"{query.signal_hash[:16]}…"
        )

    def _version_list_clicked(self, item: Any) -> None:
        text = item.text()
        try:
            version = int(text.split()[0].lstrip("v"))
        except (ValueError, IndexError):
            return
        self._show_version(version)

    def _diff_versions(self) -> None:
        if self.session is None:
            return
        selected = [
            int(item.text().split()[0].lstrip("v"))
            for item in self.version_list.selectedItems()
        ]
        if len(selected) >= 2:
            older, newer = sorted(selected)[:2]
        elif len(selected) == 1:
            older = selected[0]
            newer = self.session.latest_publication.publication_version
            if older == newer and older > 1:
                older -= 1
        else:
            history = self.session.publication_history()
            if len(history) < 2:
                return
            older, newer = (
                history[-2].publication_version,
                history[-1].publication_version,
            )
        if older == newer:
            return
        try:
            diff = self.session.diff_publications(older, newer)
        except HapticError as exc:
            self._show_error(exc.to_dict())
            return
        QMessageBox.information(
            self,
            f"Diff v{older} → v{newer}",
            json.dumps(diff, ensure_ascii=False, indent=2, sort_keys=True),
        )

    def _rollback_selected(self) -> None:
        if self.session is None or self.worker is not None:
            self._show_error(
                {
                    "code": str(ErrorCode.BUSY),
                    "message": "a recompute task is already active",
                }
            )
            return
        selected_items = self.version_list.selectedItems()
        if not selected_items:
            self._show_error(
                {
                    "code": str(ErrorCode.NOT_FOUND),
                    "message": "select a published version to roll back",
                }
            )
            return
        selected_version = int(selected_items[0].text().split()[0].lstrip("v"))
        try:
            published = self.session.preview_version(selected_version).published
        except HapticError as exc:
            self._show_error(exc.to_dict())
            return
        answer = QMessageBox.question(
            self,
            "Append config rollback",
            f"Append config version {published.config_version} from "
            f"publication v{selected_version} as a new version?",
        )
        if answer != QMessageBox.StandardButton.Yes:
            return
        self.worker = RollbackWorker(self.session, published.config_version)
        self.worker.succeeded.connect(self._rollback_succeeded)
        self.worker.failed.connect(self._recompute_failed)
        self.worker.finished.connect(self._worker_finished)
        self._set_busy(True)
        self.statusBar().showMessage("Appending rollback config version…")
        self.worker.start()

    def _rollback_succeeded(self, published: PublishedSignal) -> None:
        if self.session is not None:
            self._apply_config_controls(self.session.config)
            self.config_version_label.setText(
                f"v{self.session.current_config_version}  "
                f"{self.session.config.content_hash[:16]}…"
            )
        self._refresh_history(select_latest=True)
        self._show_latest()
        self.statusBar().showMessage(
            f"Rollback appended as publication v{published.publication_version}, "
            f"config v{published.config_version}"
        )

    def _apply_query(self, query: QueryResult) -> None:
        self._current_query = query
        self._displayed_version = query.hit_version
        self.wave_timeline.set_query(query)
        self.drive_plot.set_query(query)
        self.version_timeline.set_history(
            self.session.publication_history(), query.hit_version
        )
        self.statusBar().showMessage(
            f"Preview v{query.hit_version}  signal {query.signal_hash[:16]}…  "
            f"limited {query.limit_stats.limited_sample_count}"
        )

    def _refresh_trace(self) -> None:
        if self.session is None:
            return
        lines = [
            json.dumps(event, ensure_ascii=False, sort_keys=True)
            for event in self.session.trace_events
        ]
        self.trace_view.setPlainText("\n".join(lines))
        scrollbar = self.trace_view.verticalScrollBar()
        scrollbar.setValue(scrollbar.maximum())

    def _export_csv(self) -> None:
        if self._current_query is None:
            self._show_error({"code": "E_NOT_FOUND", "message": "no signal to export"})
            return
        default = f"haptic_v{self._current_query.hit_version}.csv"
        path, _ = QFileDialog.getSaveFileName(
            self, "Export drive signal", str(Path.cwd() / default), "CSV (*.csv)"
        )
        if not path:
            return
        export_csv(
            path,
            self._current_query.left,
            self._current_query.right,
            sample_rate=self._current_query.published.config.sample_rate,
            limited=self._current_query.limited_mask,
        )
        self.statusBar().showMessage(f"Exported {path}")

    def _load_wave_dialog(self) -> None:
        path, _ = QFileDialog.getOpenFileName(
            self, "Load waveform JSON", str(self.sample_dir), "JSON (*.json)"
        )
        if not path:
            return
        try:
            sequence = load_wave_file(path)
            data_dir = self.runtime_dir / f"import_{Path(path).stem}"
            config = self.session.config if self.session is not None else DeviceConfig()
            self.session = HapticSession.create(sequence, config, data_dir)
            self._populate_from_session()
        except HapticError as exc:
            self._show_error(exc.to_dict())

    def _load_config_dialog(self) -> None:
        if self.session is None:
            return
        path, _ = QFileDialog.getOpenFileName(
            self, "Load device config JSON", str(self.sample_dir), "JSON (*.json)"
        )
        if not path:
            return
        try:
            configs = load_config_file(path)
        except HapticError as exc:
            self._show_error(exc.to_dict())
            return
        chosen = configs[-1]
        self.sample_rate_spin.setValue(chosen[2].sample_rate)
        self.bit_depth_spin.setValue(chosen[2].bit_depth)
        self.left_ratio_spin.setValue(chosen[2].left_ratio)
        self.left_polarity_combo.setCurrentIndex(
            max(0, self.left_polarity_combo.findData(chosen[2].left_polarity))
        )
        self.right_polarity_combo.setCurrentIndex(
            max(0, self.right_polarity_combo.findData(chosen[2].right_polarity))
        )
        self.limiter_combo.setCurrentIndex(
            max(0, self.limiter_combo.findData(chosen[2].limiter))
        )
        self.statusBar().showMessage(
            f"Loaded device config {chosen[1]}; press Recompute to commit"
        )

    def _show_error(self, payload: dict[str, Any]) -> None:
        message = f"{payload.get('code', 'E_IO')}: {payload.get('message', 'error')}"
        if payload.get("segment_index") is not None:
            message += f"\nsegment={payload['segment_index']}"
        if payload.get("field") is not None:
            message += f" field={payload['field']}"
        self.statusBar().showMessage(message)
        QMessageBox.warning(self, "Haptic Studio", message)

    def closeEvent(self, event: Any) -> None:
        if self.worker is not None:
            if self.session is not None:
                self.session.cancel()
            self.worker.wait(3000)
        event.accept()


def launch(
    *,
    sample_dir: str | Path = "samples",
    runtime_dir: str | Path = "artifacts/runtime",
    seed: int = 20261122,
) -> int:
    app = QApplication.instance() or QApplication(sys.argv)
    window = MainWindow(Path(sample_dir), Path(runtime_dir), seed)
    window.show()
    return int(app.exec())


if __name__ == "__main__":
    raise SystemExit(launch())
