from __future__ import annotations

import json
from pathlib import Path
from typing import Any

from PySide6.QtCore import QTimer, Qt
from PySide6.QtGui import QColor
from PySide6.QtWidgets import (
    QFileDialog,
    QFrame,
    QGridLayout,
    QHBoxLayout,
    QLabel,
    QMainWindow,
    QPlainTextEdit,
    QProgressBar,
    QPushButton,
    QScrollArea,
    QSpinBox,
    QStatusBar,
    QTabWidget,
    QTableWidget,
    QTableWidgetItem,
    QVBoxLayout,
    QWidget,
    QComboBox,
)

from ..anomalies import AnomalyConfig, default_presets, load_presets
from ..checkpoint import CheckpointManager
from ..engine import EngineConfig, SessionEngine
from ..errors import WorkbenchError
from ..loader import load_session
from ..loopback import LoopbackRelayServer, LoopbackUdpClient
from ..models import RunResult
from ..profiles import CODEC_PROFILES, PROTOCOL_VERSION
from ..replaylog import ReplayLogWriter, write_run_log
from ..transport import DirectTransport
from .theme import APP_STYLE, PHASE_COLORS, STATUS_COLORS
from .fonts import register_bundled_fonts
from .widgets import (
    FragmentStrip,
    InputTimeline,
    MetricCard,
    MirrorView,
    PhaseTimeline,
    qcolor,
)
from .worker import PathComparisonWorker, PlaybackWorker

ANOMALY_LABELS = {
    "clean": "无异常 / CLEAN",
    "loss": "丢包 + 重传 / LOSS",
    "reorder": "分片乱序 / ORDER",
    "capability": "能力变化 / CAP",
    "corrupt": "分片损坏 / CORRUPT",
    "mixed": "组合异常 / MIXED",
}


def _card(title: str | None = None) -> tuple[QFrame, QVBoxLayout]:
    frame = QFrame()
    frame.setObjectName("Card")
    layout = QVBoxLayout(frame)
    layout.setContentsMargins(12, 11, 12, 12)
    layout.setSpacing(8)
    if title:
        label = QLabel(title)
        label.setObjectName("SectionTitle")
        layout.addWidget(label)
    return frame, layout


def _readonly_item(text: Any, color: str | None = None) -> QTableWidgetItem:
    item = QTableWidgetItem(str(text))
    item.setFlags(Qt.ItemFlag.ItemIsEnabled | Qt.ItemFlag.ItemIsSelectable)
    if color:
        item.setForeground(QColor(color))
    return item


class MainWindow(QMainWindow):
    def __init__(
        self,
        sample_path: str | Path,
        data_dir: str | Path,
        *,
        use_loopback: bool = True,
        auto_start: bool = True,
        parent: QWidget | None = None,
    ) -> None:
        super().__init__(parent)
        register_bundled_fonts()
        self.sample_path = Path(sample_path)
        self.data_dir = Path(data_dir)
        self.data_dir.mkdir(parents=True, exist_ok=True)
        self.sample = load_session(self.sample_path)
        preset_path = self.sample_path.parent / "anomalies.json"
        self.anomaly_presets = (
            load_presets(preset_path)
            if preset_path.exists()
            else default_presets()
        )
        self.loopback_enabled = use_loopback
        self.server: LoopbackRelayServer | None = None
        if use_loopback:
            self.server = LoopbackRelayServer().start()
        self.engine: SessionEngine | None = None
        self.worker: PlaybackWorker | None = None
        self.comparison_worker: PathComparisonWorker | None = None
        self.log_writer: ReplayLogWriter | None = None
        self.log_path = self.data_dir / "logs" / "workbench-live.ndjson"
        self.log_path.parent.mkdir(parents=True, exist_ok=True)
        self.frame_records: dict[int, dict[str, Any]] = {}
        self.input_records: dict[int, dict[str, Any]] = {}
        self.event_cursor = 0
        self.phase_visited: set[str] = {"negotiating"}
        self.load_status = "missing"
        self.load_reason = "尚无检查点"
        self.last_result: RunResult | None = None
        self.last_comparison: dict[str, Any] | None = None
        self._playback_soft_paused = False
        self._build_ui()
        self.setStyleSheet(APP_STYLE)
        self.setWindowTitle("Cast Workbench · 多屏互动投屏工作台")
        self.resize(1520, 960)
        self.setMinimumSize(1240, 780)
        self._prepare_engine(restore=True)
        if auto_start and self.engine is not None and not self.engine.finished:
            QTimer.singleShot(120, self.start_playback)

    def _build_ui(self) -> None:
        root = QWidget()
        root.setObjectName("AppRoot")
        root_layout = QVBoxLayout(root)
        root_layout.setContentsMargins(0, 0, 0, 0)
        root_layout.setSpacing(0)
        root_layout.addWidget(self._build_topbar())

        content = QWidget()
        content_layout = QHBoxLayout(content)
        content_layout.setContentsMargins(14, 10, 14, 10)
        content_layout.setSpacing(10)
        content_layout.addWidget(self._build_left_panel(), 1)
        content_layout.addWidget(self._build_right_panel())
        root_layout.addWidget(content, 1)
        self.setCentralWidget(root)
        self.setStatusBar(QStatusBar())
        self.statusBar().showMessage("Workbench ready · 本地回环已启动")

    def _build_topbar(self) -> QWidget:
        top = QFrame()
        top.setObjectName("TopBar")
        outer = QVBoxLayout(top)
        outer.setContentsMargins(18, 10, 18, 8)
        outer.setSpacing(8)
        row = QHBoxLayout()
        row.setSpacing(10)
        brand = QLabel("SC")
        brand.setObjectName("BrandMark")
        brand.setAlignment(Qt.AlignmentFlag.AlignCenter)
        brand.setFixedSize(42, 42)
        row.addWidget(brand)
        title_box = QVBoxLayout()
        title_box.setSpacing(0)
        title = QLabel("CAST WORKBENCH")
        title.setObjectName("Title")
        subtitle = QLabel("多屏互动投屏 · 状态演进 / 断点恢复 / 确定性重演")
        subtitle.setObjectName("Subtitle")
        title_box.addWidget(title)
        title_box.addWidget(subtitle)
        row.addLayout(title_box)
        row.addSpacing(18)
        session_box = QVBoxLayout()
        session_box.setSpacing(0)
        self.session_name_label = QLabel(self.sample.session_name)
        self.session_name_label.setStyleSheet("font-size: 12px; font-weight: 600;")
        self.session_id_label = QLabel(
            f"{self.sample.session_id} · {self.sample.declared_width}×{self.sample.declared_height}@{self.sample.declared_fps} · {PROTOCOL_VERSION}"
        )
        self.session_id_label.setObjectName("Muted")
        session_box.addWidget(self.session_name_label)
        session_box.addWidget(self.session_id_label)
        row.addLayout(session_box)
        row.addStretch(1)
        self.phase_pill = QLabel("协商中")
        self.phase_pill.setObjectName("PhasePill")
        self.phase_pill.setAlignment(Qt.AlignmentFlag.AlignCenter)
        row.addWidget(self.phase_pill)
        if self.server is not None:
            relay_text = f"LOOPBACK 127.0.0.1:{self.server.port}"
        else:
            relay_text = "DIRECT LOOPBACK"
        self.relay_chip = QLabel(relay_text)
        self.relay_chip.setObjectName("Chip")
        row.addWidget(self.relay_chip)
        protocol_chip = QLabel(f"PROTOCOL {PROTOCOL_VERSION}")
        protocol_chip.setObjectName("Chip")
        row.addWidget(protocol_chip)
        self.compare_button = QPushButton("三路一致性")
        self.compare_button.clicked.connect(self.start_comparison)
        row.addWidget(self.compare_button)
        self.export_button = QPushButton("导出回放日志")
        self.export_button.clicked.connect(self.choose_export_log)
        row.addWidget(self.export_button)
        outer.addLayout(row)

        controls = QHBoxLayout()
        controls.setSpacing(8)
        run_label = QLabel("RUN CONTROL")
        run_label.setObjectName("SectionTitle")
        controls.addWidget(run_label)
        self.anomaly_combo = QComboBox()
        for name in self.anomaly_presets:
            self.anomaly_combo.addItem(ANOMALY_LABELS.get(name, name.upper()), name)
            index = self.anomaly_combo.count() - 1
            self.anomaly_combo.setItemData(
                index,
                self.anomaly_presets[name].description,
                Qt.ItemDataRole.ToolTipRole,
            )
        clean_index = self.anomaly_combo.findData("clean")
        if clean_index >= 0:
            self.anomaly_combo.setCurrentIndex(clean_index)
        self.anomaly_combo.currentIndexChanged.connect(self._on_anomaly_changed)
        controls.addWidget(self.anomaly_combo)
        self.tier_combo = QComboBox()
        allowed = tuple(
            sorted(
                set(self.sample.sender.capabilities.codecs).intersection(
                    self.sample.receiver.capabilities.codecs
                ),
                key=lambda name: CODEC_PROFILES[name].rank,
                reverse=True,
            )
        )
        for name in allowed:
            self.tier_combo.addItem(CODEC_PROFILES[name].label, name)
        self.tier_combo.currentIndexChanged.connect(self._on_tier_changed)
        controls.addWidget(self.tier_combo)
        retry_label = QLabel("重传上限")
        retry_label.setObjectName("Muted")
        controls.addWidget(retry_label)
        self.retry_spin = QSpinBox()
        self.retry_spin.setRange(0, 8)
        self.retry_spin.setValue(2)
        self.retry_spin.valueChanged.connect(self._on_retry_changed)
        controls.addWidget(self.retry_spin)
        controls.addStretch(1)
        self.start_button = QPushButton("启动 / 继续")
        self.start_button.setObjectName("Primary")
        self.start_button.clicked.connect(self.start_playback)
        controls.addWidget(self.start_button)
        self.pause_button = QPushButton("暂停")
        self.pause_button.clicked.connect(self.toggle_pause)
        controls.addWidget(self.pause_button)
        self.reconnect_button = QPushButton("触发重连")
        self.reconnect_button.clicked.connect(self.trigger_reconnect)
        controls.addWidget(self.reconnect_button)
        self.checkpoint_button = QPushButton("立即检查点")
        self.checkpoint_button.clicked.connect(self.checkpoint_now)
        controls.addWidget(self.checkpoint_button)
        self.new_button = QPushButton("新建运行")
        self.new_button.setObjectName("Danger")
        self.new_button.clicked.connect(self.new_run)
        controls.addWidget(self.new_button)
        outer.addLayout(controls)
        return top

    def _build_left_panel(self) -> QWidget:
        panel = QWidget()
        layout = QVBoxLayout(panel)
        layout.setContentsMargins(0, 0, 0, 0)
        layout.setSpacing(9)
        mirror_card, mirror_layout = _card()
        header = QHBoxLayout()
        title = QLabel("MIRROR OUTPUT")
        title.setObjectName("SectionTitle")
        header.addWidget(title)
        header.addStretch(1)
        self.mirror_meta_label = QLabel("WAITING")
        self.mirror_meta_label.setObjectName("Muted")
        header.addWidget(self.mirror_meta_label)
        mirror_layout.addLayout(header)
        self.mirror_view = MirrorView()
        mirror_layout.addWidget(self.mirror_view, 1)
        self.fragment_strip = FragmentStrip()
        mirror_layout.addWidget(self.fragment_strip)
        layout.addWidget(mirror_card, 5)

        self.tabs = QTabWidget()
        self.tabs.setDocumentMode(True)
        self.tabs.addTab(self._build_frame_tab(), "帧 / 分片")
        self.tabs.addTab(self._build_input_tab(), "输入 / 桌面")
        self.tabs.addTab(self._build_log_tab(), "回放日志")
        self.tabs.addTab(self._build_compare_tab(), "三路一致性")
        layout.addWidget(self.tabs, 3)
        return panel

    def _make_table(self, headers: list[str], widths: list[int]) -> QTableWidget:
        table = QTableWidget(0, len(headers))
        table.setHorizontalHeaderLabels(headers)
        table.verticalHeader().setVisible(False)
        table.setAlternatingRowColors(True)
        table.setShowGrid(False)
        table.setSelectionBehavior(QTableWidget.SelectionBehavior.SelectRows)
        table.setEditTriggers(QTableWidget.EditTrigger.NoEditTriggers)
        table.horizontalHeader().setStretchLastSection(True)
        for index, width in enumerate(widths):
            table.setColumnWidth(index, width)
        table.verticalHeader().setDefaultSectionSize(21)
        return table

    def _build_frame_tab(self) -> QWidget:
        widget = QWidget()
        layout = QVBoxLayout(widget)
        layout.setContentsMargins(5, 6, 5, 5)
        self.frame_table = self._make_table(
            ["SEQ", "STATUS", "PKTS", "FRAG", "RETX", "CODES", "FRAME SHA256", "PHASE"],
            [44, 72, 48, 58, 44, 120, 180, 80],
        )
        layout.addWidget(self.frame_table)
        return widget

    def _build_input_tab(self) -> QWidget:
        widget = QWidget()
        layout = QVBoxLayout(widget)
        layout.setContentsMargins(5, 4, 5, 5)
        self.input_timeline = InputTimeline()
        layout.addWidget(self.input_timeline)
        self.input_table = self._make_table(
            ["SEQ", "t(ms)", "TYPE", "STATUS", "SOURCE", "MAPPED", "CLIP", "CODE", "DESKTOP SHA256"],
            [42, 52, 52, 66, 76, 88, 42, 74, 170],
        )
        layout.addWidget(self.input_table, 1)
        return widget

    def _build_log_tab(self) -> QWidget:
        widget = QWidget()
        layout = QVBoxLayout(widget)
        layout.setContentsMargins(5, 6, 5, 5)
        self.log_view = QPlainTextEdit()
        self.log_view.setReadOnly(True)
        self.log_view.setMaximumBlockCount(2000)
        layout.addWidget(self.log_view)
        return widget

    def _build_compare_tab(self) -> QWidget:
        widget = QWidget()
        layout = QVBoxLayout(widget)
        layout.setContentsMargins(10, 10, 10, 8)
        self.compare_status = QLabel("尚未执行三路比对")
        self.compare_status.setObjectName("BigValue")
        self.compare_status.setStyleSheet("font-size: 18px; color: #8ba3b8;")
        layout.addWidget(self.compare_status)
        self.compare_detail = QPlainTextEdit()
        self.compare_detail.setReadOnly(True)
        layout.addWidget(self.compare_detail, 1)
        return widget

    def _build_right_panel(self) -> QWidget:
        scroll = QScrollArea()
        scroll.setWidgetResizable(True)
        scroll.setFixedWidth(390)
        scroll.setFrameShape(QFrame.Shape.NoFrame)
        scroll.setHorizontalScrollBarPolicy(Qt.ScrollBarPolicy.ScrollBarAlwaysOff)
        panel = QWidget()
        layout = QVBoxLayout(panel)
        layout.setContentsMargins(0, 0, 4, 0)
        layout.setSpacing(9)

        phase_card, phase_layout = _card("SESSION PHASE")
        self.phase_timeline = PhaseTimeline()
        phase_layout.addWidget(self.phase_timeline)
        self.transition_label = QLabel("等待会话建立")
        self.transition_label.setObjectName("Muted")
        self.transition_label.setWordWrap(True)
        phase_layout.addWidget(self.transition_label)
        self.frame_progress_label = QLabel("FRAMES 0 / 0")
        self.frame_progress_label.setObjectName("Muted")
        phase_layout.addWidget(self.frame_progress_label)
        self.frame_progress = QProgressBar()
        self.frame_progress.setRange(0, 1)
        phase_layout.addWidget(self.frame_progress)
        self.input_progress_label = QLabel("INPUTS 0 / 0")
        self.input_progress_label.setObjectName("Muted")
        phase_layout.addWidget(self.input_progress_label)
        self.input_progress = QProgressBar()
        self.input_progress.setObjectName("InputProgress")
        self.input_progress.setRange(0, 1)
        phase_layout.addWidget(self.input_progress)
        layout.addWidget(phase_card)

        stats_card, stats_layout = _card("RUNTIME TELEMETRY")
        grid = QGridLayout()
        grid.setHorizontalSpacing(7)
        grid.setVerticalSpacing(7)
        self.metric_frames = MetricCard("COMMITTED FRAMES")
        self.metric_inputs = MetricCard("REPLAYED INPUTS")
        self.metric_missing = MetricCard("MISSING FRAMES")
        self.metric_retrans = MetricCard("RETRANSMIT REQUESTS")
        self.metric_order = MetricCard("ORDER EVENTS")
        self.metric_corrupt = MetricCard("CORRUPT / SESSION")
        cards = [
            self.metric_frames,
            self.metric_inputs,
            self.metric_missing,
            self.metric_retrans,
            self.metric_order,
            self.metric_corrupt,
        ]
        for index, card in enumerate(cards):
            grid.addWidget(card, index // 2, index % 2)
        stats_layout.addLayout(grid)
        layout.addWidget(stats_card)

        desktop_card, desktop_layout = _card("DESKTOP REPLAY STATE")
        desktop_grid = QGridLayout()
        desktop_grid.setHorizontalSpacing(10)
        desktop_grid.setVerticalSpacing(4)
        self.desktop_labels: dict[str, QLabel] = {}
        rows = [
            ("active", "ACTIVE WINDOW"),
            ("cursor", "CURSOR"),
            ("keys", "KEY BUFFER"),
            ("scroll", "SCROLL OFFSET"),
            ("revision", "REVISION"),
            ("clip", "LAST CLIP TRACE"),
        ]
        for row, (key, title) in enumerate(rows):
            caption = QLabel(title)
            caption.setObjectName("Muted")
            value = QLabel("—")
            value.setStyleSheet("font-family: Consolas; font-size: 10px;")
            value.setWordWrap(True)
            desktop_grid.addWidget(caption, row, 0)
            desktop_grid.addWidget(value, row, 1)
            self.desktop_labels[key] = value
        desktop_layout.addLayout(desktop_grid)
        layout.addWidget(desktop_card)

        recovery_card, recovery_layout = _card("RECOVERY / CHECKPOINT")
        self.checkpoint_status_label = QLabel("检查点: 尚无")
        self.checkpoint_status_label.setWordWrap(True)
        self.checkpoint_status_label.setStyleSheet("font-size: 11px;")
        recovery_layout.addWidget(self.checkpoint_status_label)
        self.checkpoint_detail_label = QLabel("—")
        self.checkpoint_detail_label.setObjectName("Muted")
        self.checkpoint_detail_label.setWordWrap(True)
        recovery_layout.addWidget(self.checkpoint_detail_label)
        self.checkpoint_button_2 = QPushButton("立即写入检查点")
        self.checkpoint_button_2.clicked.connect(self.checkpoint_now)
        recovery_layout.addWidget(self.checkpoint_button_2)
        layout.addWidget(recovery_card)

        anomaly_card, anomaly_layout = _card("FAULT INJECTION")
        self.anomaly_detail = QLabel("—")
        self.anomaly_detail.setWordWrap(True)
        self.anomaly_detail.setObjectName("Muted")
        anomaly_layout.addWidget(self.anomaly_detail)
        self.anomaly_codes = QLabel("E_LOSS · E_ORDER · E_CAP · E_CORRUPT · E_SESSION · E_DUP · E_STATE")
        self.anomaly_codes.setWordWrap(True)
        self.anomaly_codes.setStyleSheet("font-family: Consolas; font-size: 9px; color: #ff9e5c;")
        anomaly_layout.addWidget(self.anomaly_codes)
        layout.addWidget(anomaly_card)
        layout.addStretch(1)
        scroll.setWidget(panel)
        return scroll

    def _selected_anomaly(self) -> AnomalyConfig:
        key = self.anomaly_combo.currentData()
        return self.anomaly_presets.get(key, AnomalyConfig())

    def _checkpoint_manager(self) -> CheckpointManager:
        negotiation = self.engine.negotiation if self.engine else None
        if negotiation is None:
            from ..negotiation import negotiate

            negotiation = negotiate(self.sample)
        return CheckpointManager(
            self.data_dir / "checkpoints" / "gui",
            self.sample.session_id,
            self.sample.content_hash,
            negotiation.capabilities_hash,
            protocol_version=self.sample.protocol_version,
        )

    def _preferred_tier(self) -> str:
        data = self.tier_combo.currentData()
        return str(data) if data else self.sample.sender.capabilities.codecs[0]

    def _prepare_engine(self, restore: bool) -> None:
        if self.engine is not None:
            self.engine.close()
        manager = self._checkpoint_manager()
        transport = (
            LoopbackUdpClient(port=self.server.port)
            if self.server is not None
            else DirectTransport()
        )
        config = EngineConfig(
            retry_limit=self.retry_spin.value(),
            checkpoint_every=5,
            max_parallelism=2,
            per_thread_buffer=4,
            anomaly=self._selected_anomaly(),
            enable_checkpoints=True,
            initial_tier=self._preferred_tier(),
        )
        engine = SessionEngine(
            self.sample,
            config=config,
            checkpoint_manager=manager,
            transport=transport,
        )
        self.load_status = "missing"
        self.load_reason = "尚无检查点，从协商阶段开始"
        if restore:
            load = manager.load()
            self.load_status = load.status
            self.load_reason = load.reason
            if load.status == "loaded" and load.payload is not None:
                if not engine.restore(load.payload):
                    self.load_status = "invalid"
                    self.load_reason = "检查点 payload 校验失败，整体作废"
                    manager.clear()
            elif load.status == "invalid":
                manager.clear()
        if not engine._started:
            engine.start()
        self.engine = engine
        self.load_status = self.load_status
        self._open_log_writer()
        self._clear_runtime_views()
        self._sync_from_engine(initial=True)
        if self.load_status == "loaded":
            self.statusBar().showMessage(
                f"已从版本匹配检查点恢复: {manager.path}"
            )
        elif self.load_status == "invalid":
            self.statusBar().showMessage(
                f"检查点已整体作废并从协商阶段重来: {self.load_reason}"
            )
        else:
            self.statusBar().showMessage("会话已完成能力协商，进入镜像中阶段")

    def _open_log_writer(self) -> None:
        if self.log_writer is not None:
            self.log_writer.abort()
        self.log_writer = ReplayLogWriter(
            self.log_path,
            {
                "session_id": self.sample.session_id,
                "content_hash": self.sample.content_hash,
                "mode": "gui-live",
                "source": str(self.sample_path),
                "checksum_note": "footer complete=true only after clean session completion",
            },
        )

    def _clear_runtime_views(self) -> None:
        self.frame_records.clear()
        self.input_records.clear()
        self.event_cursor = 0
        self.phase_visited = {"negotiating"}
        self.log_view.clear()
        self.frame_table.setRowCount(0)
        self.input_table.setRowCount(0)
        self.last_result = None

    def _sync_from_engine(self, initial: bool = False) -> None:
        if self.engine is None:
            return
        snapshot = self.engine.ui_snapshot()
        result = self.engine.to_run_result("sync")
        for item in result.frames:
            self.frame_records[item.frame_seq] = item.to_dict()
        for item in result.inputs:
            self.input_records[item.input_seq] = item.to_dict()
        events = self.engine.events_since(0)
        for event in events:
            self._append_log_event(event)
        self.event_cursor = len(events)
        self._apply_snapshot(snapshot)
        image = self.engine.current_image
        if image is not None and result.frames:
            last = result.frames[-1]
            self._set_mirror(image, last.to_dict())
        self._refresh_checkpoint_card()
        if initial:
            self._refresh_tables()

    def _append_log_event(self, event: dict[str, Any]) -> None:
        frame = event.get("frame_seq")
        input_seq = event.get("input_seq")
        target = (
            f"F{frame:03d}" if frame is not None else f"I{input_seq:03d}" if input_seq is not None else "S"
        )
        codes = event.get("codes") or ([event["code"]] if event.get("code") else [])
        self.log_view.appendPlainText(
            f"{event.get('index', 0):04d}  {event.get('timestamp_ms', 0):5d}ms  "
            f"{event.get('phase', '-'):<12} {target:<6} "
            f"{event.get('type', '-'):<27} {' '.join(codes)}"
        )

    def _apply_snapshot(self, snapshot: dict[str, Any]) -> None:
        phase = snapshot["phase"]
        self.phase_visited.add(phase)
        self.phase_pill.setText(snapshot["phase_label"])
        color = PHASE_COLORS.get(phase, "#49d5ff")
        self.phase_pill.setStyleSheet(
            f"background: {qcolor(color, 34).name(QColor.NameFormat.HexArgb)};"
            f"border: 1px solid {color}; color: {color}; border-radius: 11px;"
            "padding: 4px 12px; font-size: 11px; font-weight: 700;"
        )
        self.phase_timeline.set_phase(phase)
        phase_events = snapshot.get("phase_events", [])
        if phase_events:
            last = phase_events[-1]
            code = f" · {last['code']}" if last.get("code") else ""
            self.transition_label.setText(
                f"{last['action']} · {last['from_phase']} → {last['to_phase']}{code}\n"
                f"计数 F{last['frames_committed']} / I{last['inputs_replayed']}"
            )
        stats = snapshot["stats"]
        total_frames = snapshot["total_frames"]
        total_inputs = snapshot["total_inputs"]
        committed = stats["frames_committed"]
        replayed = stats["inputs_replayed"]
        self.frame_progress.setRange(0, max(1, total_frames))
        self.frame_progress.setValue(committed)
        self.input_progress.setRange(0, max(1, total_inputs))
        self.input_progress.setValue(replayed)
        self.frame_progress_label.setText(f"FRAMES  {committed} / {total_frames}")
        self.input_progress_label.setText(f"INPUTS  {replayed} / {total_inputs}")
        self.metric_frames.set_value(f"{committed}/{total_frames}", "#49d5ff")
        self.metric_inputs.set_value(f"{replayed}/{total_inputs}", "#ffbd63")
        self.metric_missing.set_value(
            stats["frames_missing"], "#ff567f" if stats["frames_missing"] else None
        )
        self.metric_retrans.set_value(stats["retransmission_requests"])
        self.metric_order.set_value(stats["order_events"])
        self.metric_corrupt.set_value(
            f"{stats['packets_corrupt']} / {stats['packets_session_rejected']}",
            "#ff9e5c"
            if stats["packets_corrupt"] or stats["packets_session_rejected"]
            else None,
        )
        desktop = snapshot["desktop"]
        self.desktop_labels["active"].setText(
            "—" if desktop["active_window"] is None else f"W{desktop['active_window']}"
        )
        self.desktop_labels["cursor"].setText(
            "—" if desktop["cursor"] is None else f"{desktop['cursor'][0]}, {desktop['cursor'][1]}"
        )
        self.desktop_labels["keys"].setText(desktop["key_buffer"] or "—")
        self.desktop_labels["scroll"].setText(str(desktop["scroll_offset"]))
        self.desktop_labels["revision"].setText(str(desktop["revision"]))
        trace = desktop.get("last_clip_trace", [])
        clipped_inputs = [
            item
            for item in self.input_records.values()
            if item.get("clipped")
        ]
        if clipped_inputs:
            trace = sorted(
                clipped_inputs, key=lambda item: item["input_seq"]
            )[-1].get("clip_trace", [])
        self.desktop_labels["clip"].setText(
            json.dumps(trace, ensure_ascii=False, separators=(",", ":"))
            if trace
            else "—"
        )
        self.mirror_meta_label.setText(
            f"{snapshot['negotiation']['width']}×{snapshot['negotiation']['height']} · "
            f"{snapshot['negotiation']['codec']} · "
            f"{snapshot['negotiation']['fps']} FPS · CP {snapshot['checkpoint_count']}"
        )
        anomaly = snapshot["anomaly"]
        self.anomaly_detail.setText(
            f"{ANOMALY_LABELS.get(anomaly.get('name', ''), anomaly.get('name', ''))}\n"
            f"{anomaly.get('description', '')}\n"
            f"runtime tier: {snapshot['runtime_tier']} · retry: {snapshot['retry_limit']} · "
            f"worker pool: 2 / buffer 4"
        )
        recent_frames = snapshot.get("frames", [])
        for item in recent_frames:
            self.frame_records[item["frame_seq"]] = item
        for item in snapshot.get("inputs", []):
            self.input_records[item["input_seq"]] = item
        if recent_frames:
            last = recent_frames[-1]
            self.fragment_strip.set_fragments(
                last.get("fragment_status", []), last["frame_seq"]
            )
        self.input_timeline.set_data(self.sample.inputs, self.input_records)
        self._refresh_tables()
        self._refresh_controls(snapshot)
        self._refresh_checkpoint_card()

    def _refresh_controls(self, snapshot: dict[str, Any]) -> None:
        running = bool(self.worker and self.worker.isRunning())
        phase = snapshot["phase"]
        self.pause_button.setText("恢复" if phase == "paused" else "暂停")
        self.pause_button.setEnabled(phase in {"mirroring", "paused"})
        self.reconnect_button.setEnabled(phase == "mirroring")
        self.start_button.setEnabled(not running and not snapshot["finished"])
        self.checkpoint_button.setEnabled(self.engine is not None)
        self.checkpoint_button_2.setEnabled(self.engine is not None)
        if self.tier_combo.currentData() != snapshot["runtime_tier"]:
            index = self.tier_combo.findData(snapshot["runtime_tier"])
            if index >= 0:
                self.tier_combo.blockSignals(True)
                self.tier_combo.setCurrentIndex(index)
                self.tier_combo.blockSignals(False)
        if self.retry_spin.value() != snapshot["retry_limit"]:
            self.retry_spin.blockSignals(True)
            self.retry_spin.setValue(snapshot["retry_limit"])
            self.retry_spin.blockSignals(False)

    def _refresh_tables(self) -> None:
        frames = [self.frame_records[seq] for seq in sorted(self.frame_records)]
        self.frame_table.setRowCount(len(frames))
        for row, item in enumerate(frames):
            values = [
                item["frame_seq"],
                item["status"],
                item["packets_sent"],
                f"{item['fragments_received']}/{item['fragments_total']}",
                item["retransmissions"],
                " ".join(item.get("codes", [])),
                item["frame_sha256"][:18],
                item["phase"],
            ]
            status_color = STATUS_COLORS.get(item["status"], "#c9dceb")
            for column, value in enumerate(values):
                color = status_color if column == 1 else None
                if column == 5 and item.get("codes"):
                    color = "#ff9e5c"
                self.frame_table.setItem(row, column, _readonly_item(value, color))
        inputs = [self.input_records[seq] for seq in sorted(self.input_records)]
        self.input_table.setRowCount(len(inputs))
        for row, item in enumerate(inputs):
            source = ",".join(str(value) for value in item.get("source_coords", []))
            mapped = ",".join(str(value) for value in item.get("mapped_coords", []))
            values = [
                item["input_seq"],
                item["timestamp_ms"],
                item["event_type"],
                item["status"],
                source,
                mapped,
                "YES" if item.get("clipped") else "",
                item.get("code") or "",
                item["desktop_digest"][:18],
            ]
            status_color = STATUS_COLORS.get(item["status"], "#c9dceb")
            for column, value in enumerate(values):
                color = status_color if column == 3 else None
                if column == 7 and value:
                    color = "#ff9e5c"
                self.input_table.setItem(row, column, _readonly_item(value, color))

    def _set_mirror(self, image: Any, result: dict[str, Any]) -> None:
        self.mirror_view.set_frame(
            image,
            frame_seq=result["frame_seq"],
            phase=result["phase"],
            status=result["status"],
            resolution=f"{result['width']}×{result['height']}",
            codec=result["codec"],
        )

    def _refresh_checkpoint_card(self) -> None:
        if self.engine is None:
            return
        snapshot = self.engine.ui_snapshot()
        if self.load_status == "loaded":
            status_text = "已从匹配检查点恢复"
        elif self.load_status == "invalid":
            status_text = "旧检查点整体作废"
        elif snapshot["checkpoint_count"] > 0:
            status_text = "新运行 · 检查点已持久化"
        else:
            status_text = "尚无检查点"
        reason_suffix = (
            f"\n{self.load_reason}"
            if self.load_status in {"loaded", "invalid"} and self.load_reason
            else ""
        )
        self.checkpoint_status_label.setText(
            f"状态: {status_text} · 快照数 {snapshot['checkpoint_count']}"
            + reason_suffix
        )
        manager = self.engine.checkpoint_manager
        path = str(manager.path) if manager else "—"
        self.checkpoint_detail_label.setText(
            f"阶段 {snapshot['phase_label']} · timeline {snapshot['timeline_index']}/"
            f"{snapshot['timeline_total']}\n"
            f"原子写入: temp + fsync + os.replace\n{path}"
        )

    def _on_frame_ready(self, image: Any, result: dict[str, Any]) -> None:
        self._set_mirror(image, result)
        self.frame_records[result["frame_seq"]] = result

    def start_playback(self) -> None:
        if self.engine is None:
            return
        if self.worker is not None and self.worker.isRunning():
            return
        if self.engine.finished:
            self.statusBar().showMessage("会话已结束；点击“新建运行”开始可重复演示")
            return
        self._open_log_writer()
        self.worker = PlaybackWorker(self.engine, self.log_writer, interval_ms=55)
        self.worker.snapshot_ready.connect(self._apply_snapshot)
        self.worker.frame_ready.connect(self._on_frame_ready)
        self.worker.completed.connect(self._on_playback_completed)
        self.worker.failed.connect(self._on_worker_failed)
        self.worker.finished.connect(self._on_worker_finished)
        self.worker.start()
        self.statusBar().showMessage("实时处理路径运行中 · 媒体包经 127.0.0.1 回环传输")

    def _on_playback_completed(self, result: RunResult) -> None:
        self.last_result = result
        self.statusBar().showMessage(
            f"会话完成 · {result.stats.frames_committed}/{len(self.sample.frames)} 帧 · "
            f"{result.stats.inputs_replayed}/{len(self.sample.inputs)} 输入 · "
            f"阶段 {result.final_phase}"
        )

    def _on_worker_failed(self, trace: str) -> None:
        self.statusBar().showMessage("工作线程异常，详见日志")
        self.log_view.appendPlainText("WORKER ERROR\n" + trace)

    def _on_worker_finished(self) -> None:
        self._refresh_controls(self.engine.ui_snapshot())

    def toggle_pause(self) -> None:
        if self.engine is None:
            return
        try:
            if self.engine.phase.value == "mirroring":
                if self.worker is not None:
                    self.worker.set_playback_paused(True)
                self._playback_soft_paused = True
                self.engine.pause()
                self.statusBar().showMessage("会话已暂停；帧序号与输入重放进度保持不变")
            elif self.engine.phase.value == "paused":
                self.engine.resume()
                self._playback_soft_paused = False
                if self.worker is not None:
                    self.worker.set_playback_paused(False)
                self.statusBar().showMessage("会话恢复；从已提交进度继续")
            self._sync_from_engine()
        except WorkbenchError as exc:
            self.statusBar().showMessage(f"{exc.code}: {exc.message}")

    def trigger_reconnect(self) -> None:
        if self.engine is None:
            return
        try:
            self.engine.trigger_reconnect()
            self.statusBar().showMessage(
                "手动重连完成；已从已提交检查点状态继续，未重建整场会话"
            )
            self._sync_from_engine()
        except WorkbenchError as exc:
            self.statusBar().showMessage(f"{exc.code}: {exc.message}")

    def checkpoint_now(self) -> None:
        if self.engine is None:
            return
        try:
            self.engine.save_checkpoint()
            self._refresh_checkpoint_card()
            self.statusBar().showMessage("检查点已原子串行写入")
        except Exception as exc:
            self.statusBar().showMessage(f"检查点写入失败: {exc}")

    def new_run(self) -> None:
        self._stop_worker()
        if self.engine is not None:
            manager = self.engine.checkpoint_manager
            self.engine.close()
            self.engine = None
            if manager is not None:
                manager.clear()
        self._clear_runtime_views()
        self.load_status = "missing"
        self.load_reason = "新建运行，从协商阶段开始"
        self._prepare_engine(restore=False)
        self.start_playback()
        self.statusBar().showMessage("新建运行已启动 · 已清空旧检查点")

    def _stop_worker(self) -> None:
        if self.worker is not None:
            self.worker.request_stop()
            self.worker.wait(3500)
            self.worker = None
        self._playback_soft_paused = False

    def _on_anomaly_changed(self) -> None:
        anomaly = self._selected_anomaly()
        self.anomaly_detail.setText(anomaly.description)
        if self.engine is not None:
            self.statusBar().showMessage(
                "异常参数将在“新建运行”时生效；运行中的会话配置保持不变"
            )

    def _on_tier_changed(self) -> None:
        if self.engine is None:
            return
        try:
            self.engine.set_runtime_tier(str(self.tier_combo.currentData()))
            self.statusBar().showMessage(
                "编码档位已切换；仅影响尚未提交的帧，已提交画面不变"
            )
        except WorkbenchError as exc:
            self.statusBar().showMessage(f"{exc.code}: {exc.message}")

    def _on_retry_changed(self, value: int) -> None:
        if self.engine is None:
            return
        try:
            self.engine.set_retry_limit(value)
            self.statusBar().showMessage("重传上限已更新；仅影响尚未提交的帧")
        except WorkbenchError as exc:
            self.statusBar().showMessage(f"{exc.code}: {exc.message}")

    def start_comparison(self) -> None:
        if self.comparison_worker is not None and self.comparison_worker.isRunning():
            return
        if self.engine is None:
            return
        config = EngineConfig(
            retry_limit=self.engine.retry_limit,
            checkpoint_every=5,
            max_parallelism=2,
            per_thread_buffer=4,
            anomaly=self._selected_anomaly(),
            enable_checkpoints=True,
            initial_tier=self.engine.runtime_tier,
        )
        self.compare_button.setEnabled(False)
        self.compare_status.setText("正在运行实时 / 断点恢复 / 全量重演…")
        self.compare_status.setStyleSheet("font-size: 18px; color: #f2b34c;")
        self.compare_detail.setPlainText(
            "恢复路径将在第 18 个已提交帧后加载版本匹配检查点并继续。\n"
            "比较字段: frame_sha256, status, codec, fragment_order, desktop_digest, "
            "mapped_coords, clip_trace, phase。"
        )
        self.comparison_worker = PathComparisonWorker(
            self.sample,
            config,
            self.data_dir / "checkpoints" / "compare",
            self.data_dir / "generated" / "three_path_consistency.json",
            relay_port=self.server.port if self.server is not None else None,
        )
        self.comparison_worker.completed.connect(self._on_comparison_completed)
        self.comparison_worker.failed.connect(self._on_comparison_failed)
        self.comparison_worker.finished.connect(self._on_comparison_finished)
        self.comparison_worker.start()

    def _on_comparison_completed(self, report: Any, document: dict[str, Any]) -> None:
        self.last_comparison = document
        if report.ok:
            self.compare_status.setText("PASS · 三路逐帧逐事件一致")
            self.compare_status.setStyleSheet("font-size: 18px; color: #65e6a5;")
        else:
            self.compare_status.setText(
                f"E_DIFF · {report.component} #{report.index} · {report.field} · {report.phase}"
            )
            self.compare_status.setStyleSheet("font-size: 18px; color: #ff567f;")
        summary = {
            "comparison": report.to_dict(),
            "realtime": self._path_summary(document["realtime"]),
            "recovery": {
                **self._path_summary(document["recovery"]),
                "checkpoint": document["recovery_checkpoint"],
            },
            "replay": self._path_summary(document["replay"]),
            "artifact": str(self.data_dir / "generated" / "three_path_consistency.json"),
        }
        self.compare_detail.setPlainText(
            json.dumps(summary, ensure_ascii=False, indent=2)
        )
        self.tabs.setCurrentIndex(3)
        self.statusBar().showMessage(
            "三路一致性比对完成: " + ("PASS" if report.ok else "E_DIFF")
        )

    @staticmethod
    def _path_summary(path_data: dict[str, Any]) -> dict[str, Any]:
        stats = path_data["stats"]
        return {
            "final_phase": path_data["final_phase"],
            "frames": len(path_data["frames"]),
            "committed": stats["frames_committed"],
            "inputs": len(path_data["inputs"]),
            "replayed": stats["inputs_replayed"],
            "missing": stats["frames_missing"],
            "desktop_revision": path_data["desktop_state"]["revision"],
        }

    def _on_comparison_failed(self, trace: str) -> None:
        self.compare_status.setText("E_DIFF / WORKER ERROR")
        self.compare_status.setStyleSheet("font-size: 18px; color: #ff567f;")
        self.compare_detail.setPlainText(trace)
        self.statusBar().showMessage("三路一致性任务失败")

    def _on_comparison_finished(self) -> None:
        self.compare_button.setEnabled(True)

    def choose_export_log(self) -> None:
        default = self.data_dir / "generated" / "replay-export.json"
        path, _ = QFileDialog.getSaveFileName(
            self,
            "导出回放日志",
            str(default),
            "Replay JSON (*.json);;JSON Lines (*.ndjson)",
        )
        if path:
            self.export_log_to(path)

    def export_log_to(self, path: str | Path) -> Path:
        target = Path(path)
        target.parent.mkdir(parents=True, exist_ok=True)
        if self.engine is None:
            raise RuntimeError("no active engine")
        result = self.engine.to_run_result("export")
        complete = self.engine.finished and self.engine.phase.value == "disconnected"
        if complete:
            write_run_log(
                result,
                target,
                metadata={
                    "complete": True,
                    "note": "会话在已断开阶段正常结束，日志包含完整事件与结果",
                },
            )
        else:
            with ReplayLogWriter(
                target,
                {
                    "session_id": result.session_id,
                    "content_hash": result.content_hash,
                    "mode": "partial-export",
                    "complete_expected": False,
                },
            ) as writer:
                for event in result.events:
                    writer.append(event)
                writer._write(
                    {
                        "type": "partial",
                        "complete": False,
                        "message": "会话未正常完成；不声称具备完整回放日志",
                        "phase": result.final_phase,
                        "frames_committed": result.stats.frames_committed,
                        "inputs_replayed": result.stats.inputs_replayed,
                    }
                )
        self.statusBar().showMessage(
            f"回放日志已导出: {target} ({'complete' if complete else 'partial'})"
        )
        return target

    def advance_demo(self, steps: int) -> None:
        """Synchronous stepping hook used by automated screenshots/tests."""
        if self.engine is None:
            return
        for _ in range(steps):
            if self.engine.finished:
                break
            self.engine.step()
            image = self.engine.current_image
            snapshot = self.engine.ui_snapshot()
            if snapshot["frames"]:
                last = snapshot["frames"][-1]
                self.frame_records[last["frame_seq"]] = last
                self._set_mirror(image, last)
            for item in snapshot["inputs"]:
                self.input_records[item["input_seq"]] = item
            new_events = self.engine.events_since(self.event_cursor)
            for event in new_events:
                self._append_log_event(event)
            self.event_cursor += len(new_events)
            self._apply_snapshot(snapshot)

    def closeEvent(self, event: Any) -> None:
        self._stop_worker()
        if self.comparison_worker is not None and self.comparison_worker.isRunning():
            self.comparison_worker.wait(5000)
        if self.engine is not None:
            self.engine.close()
        if self.log_writer is not None:
            self.log_writer.abort()
        if self.server is not None:
            self.server.stop()
        super().closeEvent(event)
