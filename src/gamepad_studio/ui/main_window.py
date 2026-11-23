"""Main PySide6 dashboard window."""

from __future__ import annotations

import json
from dataclasses import replace
from pathlib import Path
from typing import Any, Callable

import numpy as np
from PySide6.QtCore import QObject, QRunnable, QThreadPool, Qt, Signal
from PySide6.QtGui import QBrush, QColor, QCloseEvent
from PySide6.QtWidgets import (
    QAbstractItemView,
    QApplication,
    QCheckBox,
    QComboBox,
    QDialog,
    QDoubleSpinBox,
    QFormLayout,
    QFrame,
    QGroupBox,
    QHBoxLayout,
    QHeaderView,
    QLabel,
    QMainWindow,
    QMessageBox,
    QPushButton,
    QScrollArea,
    QSplitter,
    QTabWidget,
    QTableWidget,
    QTableWidgetItem,
    QTextEdit,
    QVBoxLayout,
    QWidget,
)

from ..compare import change_heatmap, comparison_heatmap
from ..errors import StudioError
from ..models import DeviceSpec
from ..pipeline import PipelineRun
from ..samples import generate_samples
from ..service import SessionUpdate, StudioSession, VerificationReport
from ..session import HotSwitchResult
from ..snapshot import SnapshotLoadResult, SnapshotWriteReport
from ..validation import load_sample
from .theme import COLORS
from .widgets import (
    ActionWaveformWidget,
    DependencyGraphWidget,
    DeviceLayoutWidget,
    DiffHeatmapWidget,
    HotSwitchDialog,
    RuleDialog,
    TimelineWidget,
)


class _WorkerSignals(QObject):
    completed = Signal(object)
    failed = Signal(object)


class _Worker(QRunnable):
    def __init__(
        self,
        function: Callable[[], Any],
        token: int,
    ) -> None:
        super().__init__()
        self.function = function
        self.token = token
        self.signals = _WorkerSignals()

    def run(self) -> None:
        try:
            value = self.function()
            self.signals.completed.emit({"token": self.token, "value": value})
        except StudioError as exc:
            self.signals.failed.emit({"token": self.token, "error": exc.to_dict()})
        except Exception as exc:  # pragma: no cover - defensive UI boundary
            self.signals.failed.emit(
                {
                    "token": self.token,
                    "error": {
                        "code": "E_UI",
                        "field": "worker",
                        "message": f"{type(exc).__name__}: {exc}",
                        "event_seq": None,
                        "context": {},
                    },
                }
            )


class MainWindow(QMainWindow):
    def __init__(
        self,
        *,
        samples_root: str | Path = "samples",
        initial_sample: str | Path | None = None,
        snapshot_root: str | Path = "artifacts/snapshots",
        async_initial_load: bool = True,
    ) -> None:
        super().__init__()
        self.samples_root = Path(samples_root)
        self.snapshot_root = Path(snapshot_root)
        self.snapshot_root.mkdir(parents=True, exist_ok=True)
        self.session: StudioSession | None = None
        self.last_run: PipelineRun | None = None
        self.last_verification: VerificationReport | None = None
        self.last_previous_result = None
        self.last_hot_switch: HotSwitchResult | None = None
        self._busy = False
        self._token = 0
        self._active_job: _Worker | None = None
        self._suppress_dialogs = False
        self._sample_entries = self._load_sample_index()
        self.thread_pool = QThreadPool(self)
        self.thread_pool.setMaxThreadCount(2)
        self.event_buffer_limit = 128
        self.setWindowTitle("PadWeave Studio · Deterministic Gamepad Mapping Lab")
        self.resize(1580, 1000)
        self.setMinimumSize(1180, 760)
        self._build_ui()
        self._populate_samples()
        if initial_sample is not None:
            if async_initial_load:
                self._load_sample_by_reference(initial_sample)
            else:
                self.load_sample_sync(initial_sample)
        elif self._sample_entries:
            first = self._sample_entries[0]["path"]
            if async_initial_load:
                self._load_sample_by_reference(first)
            else:
                self.load_sample_sync(first)

    def _load_sample_index(self) -> list[dict[str, Any]]:
        index_path = self.samples_root / "index.json"
        entries: list[dict[str, Any]] = []
        if index_path.exists():
            try:
                payload = json.loads(index_path.read_text(encoding="utf-8"))
                for entry in payload.get("samples", []):
                    path = self.samples_root / str(entry.get("path", entry.get("device_id")))
                    if path.is_dir():
                        entries.append({**entry, "path": str(path)})
            except (OSError, json.JSONDecodeError):
                entries = []
        if not entries and self.samples_root.is_dir():
            for device_file in sorted(self.samples_root.glob("*/device.json")):
                try:
                    payload = json.loads(device_file.read_text(encoding="utf-8"))
                except (OSError, json.JSONDecodeError):
                    continue
                entries.append(
                    {
                        "device_id": payload.get("id", device_file.parent.name),
                        "name": payload.get("name", device_file.parent.name),
                        "path": str(device_file.parent),
                    }
                )
        return entries

    def _build_ui(self) -> None:
        central = QWidget()
        root_layout = QVBoxLayout(central)
        root_layout.setContentsMargins(12, 12, 12, 8)
        root_layout.setSpacing(9)

        header = QFrame()
        header.setObjectName("Header")
        header_layout = QHBoxLayout(header)
        header_layout.setContentsMargins(16, 10, 14, 10)
        title_box = QVBoxLayout()
        title = QLabel("PADWEAVE")
        title.setObjectName("AppTitle")
        subtitle = QLabel("Deterministic gamepad mapping · incremental replay laboratory")
        subtitle.setObjectName("AppSubtitle")
        title_box.addWidget(title)
        title_box.addWidget(subtitle)
        header_layout.addLayout(title_box)
        header_layout.addStretch(1)
        self.sample_combo = QComboBox()
        self.sample_combo.setMinimumWidth(230)
        self.sample_combo.setToolTip("Local deterministic device sample")
        header_layout.addWidget(self.sample_combo)
        self.load_button = QPushButton("Load sample")
        self.load_button.setObjectName("Primary")
        self.load_button.clicked.connect(self._load_selected_sample)
        header_layout.addWidget(self.load_button)
        self.verify_button = QPushButton("Verify full replay")
        self.verify_button.clicked.connect(self._verify_current)
        header_layout.addWidget(self.verify_button)
        self.snapshot_button = QPushButton("Write snapshot")
        self.snapshot_button.clicked.connect(self._save_snapshot)
        header_layout.addWidget(self.snapshot_button)
        self.switch_button = QPushButton("Hot switch…")
        self.switch_button.clicked.connect(self._open_hot_switch)
        header_layout.addWidget(self.switch_button)
        self.truth_badge = QLabel("TRUTH · pending")
        self.truth_badge.setObjectName("Badge")
        header_layout.addWidget(self.truth_badge)
        self.cache_badge = QLabel("CACHE · pending")
        self.cache_badge.setObjectName("Badge")
        header_layout.addWidget(self.cache_badge)
        self.snapshot_badge = QLabel("SNAPSHOT · new")
        self.snapshot_badge.setObjectName("Badge")
        header_layout.addWidget(self.snapshot_badge)
        root_layout.addWidget(header)

        vertical = QSplitter(Qt.Orientation.Vertical)
        horizontal = QSplitter(Qt.Orientation.Horizontal)

        left = QWidget()
        left_layout = QVBoxLayout(left)
        left_layout.setContentsMargins(0, 0, 0, 0)
        left_layout.setSpacing(8)
        device_group = QGroupBox("Device layout")
        device_layout = QVBoxLayout(device_group)
        self.device_label = QLabel("No sample loaded")
        self.device_label.setObjectName("AppSubtitle")
        self.device_widget = DeviceLayoutWidget()
        device_layout.addWidget(self.device_label)
        device_layout.addWidget(self.device_widget)
        left_layout.addWidget(device_group)

        axis_group = QGroupBox("Axis response")
        axis_form = QFormLayout(axis_group)
        self.axis_combo = QComboBox()
        self.axis_combo.currentIndexChanged.connect(self._axis_selection_changed)
        self.deadzone_spin = QDoubleSpinBox()
        self.deadzone_spin.setRange(0.0, 0.95)
        self.deadzone_spin.setSingleStep(0.01)
        self.deadzone_spin.setDecimals(2)
        self.exponent_spin = QDoubleSpinBox()
        self.exponent_spin.setRange(0.1, 8.0)
        self.exponent_spin.setSingleStep(0.1)
        self.exponent_spin.setDecimals(2)
        self.invert_check = QCheckBox("Axial inversion")
        self.apply_axis_button = QPushButton("Apply axis response")
        self.apply_axis_button.setObjectName("Primary")
        self.apply_axis_button.clicked.connect(self._apply_axis)
        axis_form.addRow("Axis", self.axis_combo)
        axis_form.addRow("Deadzone", self.deadzone_spin)
        axis_form.addRow("Curve exponent", self.exponent_spin)
        axis_form.addRow("", self.invert_check)
        axis_form.addRow("", self.apply_axis_button)
        left_layout.addWidget(axis_group)
        left_layout.addStretch(1)
        left.setMinimumWidth(315)
        horizontal.addWidget(left)

        center = QWidget()
        center_layout = QVBoxLayout(center)
        center_layout.setContentsMargins(0, 0, 0, 0)
        center_layout.setSpacing(8)
        rules_group = QGroupBox("Binding rules")
        rules_layout = QVBoxLayout(rules_group)
        self.rule_table = QTableWidget(0, 6)
        self.rule_table.setHorizontalHeaderLabels(
            ["Rule", "Action", "Kind", "Inputs", "Timing", "Curve"]
        )
        self.rule_table.verticalHeader().setVisible(False)
        self.rule_table.setAlternatingRowColors(True)
        self.rule_table.setSelectionBehavior(
            QAbstractItemView.SelectionBehavior.SelectRows
        )
        self.rule_table.setSelectionMode(
            QAbstractItemView.SelectionMode.SingleSelection
        )
        self.rule_table.setEditTriggers(
            QAbstractItemView.EditTrigger.NoEditTriggers
        )
        self.rule_table.horizontalHeader().setSectionResizeMode(
            QHeaderView.ResizeMode.Stretch
        )
        self.rule_table.doubleClicked.connect(self._edit_rule)
        rules_layout.addWidget(self.rule_table)
        rule_actions = QHBoxLayout()
        self.edit_rule_button = QPushButton("Edit selected rule")
        self.edit_rule_button.clicked.connect(self._edit_rule)
        rule_actions.addWidget(self.edit_rule_button)
        rule_actions.addStretch(1)
        rule_actions.addWidget(QLabel("Double-click a row to edit"))
        rules_layout.addLayout(rule_actions)
        center_layout.addWidget(rules_group, 3)

        timeline_group = QGroupBox("Event timeline")
        timeline_layout = QVBoxLayout(timeline_group)
        self.timeline_widget = TimelineWidget()
        timeline_layout.addWidget(self.timeline_widget)
        legend = QLabel(
            "Axis dots · button high/low steps · orange dotted lines are deterministic timing deadlines"
        )
        legend.setObjectName("AppSubtitle")
        timeline_layout.addWidget(legend)
        center_layout.addWidget(timeline_group, 2)
        horizontal.addWidget(center)

        right = QWidget()
        right_layout = QVBoxLayout(right)
        right_layout.setContentsMargins(0, 0, 0, 0)
        right_layout.setSpacing(8)
        waveform_group = QGroupBox("Action output waveform")
        waveform_layout = QVBoxLayout(waveform_group)
        self.waveform_scroll = QScrollArea()
        self.waveform_scroll.setWidgetResizable(True)
        self.waveform_widget = ActionWaveformWidget()
        self.waveform_scroll.setWidget(self.waveform_widget)
        waveform_layout.addWidget(self.waveform_scroll)
        right_layout.addWidget(waveform_group, 3)
        states_group = QGroupBox("Latest action states")
        states_layout = QVBoxLayout(states_group)
        self.states_table = QTableWidget(0, 6)
        self.states_table.setHorizontalHeaderLabels(
            ["Action", "Pressed", "Value", "Triggers", "Repeats", "Reason"]
        )
        self.states_table.verticalHeader().setVisible(False)
        self.states_table.setAlternatingRowColors(True)
        self.states_table.setEditTriggers(
            QAbstractItemView.EditTrigger.NoEditTriggers
        )
        self.states_table.horizontalHeader().setSectionResizeMode(
            QHeaderView.ResizeMode.Stretch
        )
        states_layout.addWidget(self.states_table)
        right_layout.addWidget(states_group, 2)
        right.setMinimumWidth(410)
        horizontal.addWidget(right)
        horizontal.setSizes([330, 650, 560])
        vertical.addWidget(horizontal)

        self.tabs = QTabWidget()
        self.tabs.setMinimumHeight(245)
        self._build_invalidation_tab()
        self._build_dependency_tab()
        self._build_heatmap_tab()
        self._build_nodes_tab()
        self._build_session_tab()
        vertical.addWidget(self.tabs)
        vertical.setSizes([690, 280])
        root_layout.addWidget(vertical, 1)
        self.setCentralWidget(central)

        self.status_label = QLabel("Ready")
        self.worker_label = QLabel(
            f"WORKERS 2 · EVENT BUFFER {self.event_buffer_limit} · SNAPSHOT WRITES SERIALIZED"
        )
        self.worker_label.setObjectName("AppSubtitle")
        self.statusBar().addWidget(self.status_label, 1)
        self.statusBar().addPermanentWidget(self.worker_label)

    def _build_invalidation_tab(self) -> None:
        page = QWidget()
        layout = QVBoxLayout(page)
        self.invalidation_summary = QLabel("No incremental run yet")
        self.invalidation_summary.setObjectName("SectionTitle")
        layout.addWidget(self.invalidation_summary)
        self.invalidation_table = QTableWidget(0, 5)
        self.invalidation_table.setHorizontalHeaderLabels(
            ["Node", "Invalidated", "Reused", "Reason", "Event windows"]
        )
        self.invalidation_table.verticalHeader().setVisible(False)
        self.invalidation_table.setAlternatingRowColors(True)
        self.invalidation_table.setEditTriggers(
            QAbstractItemView.EditTrigger.NoEditTriggers
        )
        self.invalidation_table.horizontalHeader().setSectionResizeMode(
            3, QHeaderView.ResizeMode.Stretch
        )
        layout.addWidget(self.invalidation_table)
        self.tabs.addTab(page, "Invalidation")

    def _build_dependency_tab(self) -> None:
        page = QWidget()
        layout = QVBoxLayout(page)
        self.dependency_scroll = QScrollArea()
        self.dependency_scroll.setWidgetResizable(True)
        self.dependency_widget = DependencyGraphWidget()
        self.dependency_widget.nodeClicked.connect(self._show_node_detail)
        self.dependency_scroll.setWidget(self.dependency_widget)
        layout.addWidget(self.dependency_scroll)
        self.node_detail = QLabel("Select a node to inspect its cache identity and invalidation reason.")
        self.node_detail.setObjectName("AppSubtitle")
        self.node_detail.setWordWrap(True)
        layout.addWidget(self.node_detail)
        self.tabs.addTab(page, "Dependency propagation")

    def _build_heatmap_tab(self) -> None:
        page = QWidget()
        layout = QHBoxLayout(page)
        self.equality_heatmap = DiffHeatmapWidget("Incremental vs full replay")
        self.change_heatmap = DiffHeatmapWidget("Previous output vs current output")
        layout.addWidget(self.equality_heatmap)
        layout.addWidget(self.change_heatmap)
        self.tabs.addTab(page, "Difference heatmap")

    def _build_nodes_tab(self) -> None:
        page = QWidget()
        layout = QHBoxLayout(page)
        self.node_table = QTableWidget(0, 5)
        self.node_table.setHorizontalHeaderLabels(
            ["Node", "Kind", "Status", "Cache key", "Windows"]
        )
        self.node_table.verticalHeader().setVisible(False)
        self.node_table.setAlternatingRowColors(True)
        self.node_table.setSelectionBehavior(
            QAbstractItemView.SelectionBehavior.SelectRows
        )
        self.node_table.setSelectionMode(
            QAbstractItemView.SelectionMode.SingleSelection
        )
        self.node_table.setEditTriggers(
            QAbstractItemView.EditTrigger.NoEditTriggers
        )
        self.node_table.horizontalHeader().setSectionResizeMode(
            3, QHeaderView.ResizeMode.Stretch
        )
        self.node_table.itemSelectionChanged.connect(self._node_table_selection)
        layout.addWidget(self.node_table, 3)
        side = QWidget()
        side_layout = QVBoxLayout(side)
        side_layout.setContentsMargins(0, 0, 0, 0)
        self.node_cache_detail = QTextEdit()
        self.node_cache_detail.setReadOnly(True)
        self.node_cache_detail.setPlaceholderText(
            "Select a node. Cache keys bind event content, node identity, "
            "parameters, and all upstream keys."
        )
        self.retry_node_button = QPushButton("Retry selected node")
        self.retry_node_button.clicked.connect(self._retry_selected_node)
        side_layout.addWidget(self.node_cache_detail)
        side_layout.addWidget(self.retry_node_button)
        layout.addWidget(side, 2)
        self.tabs.addTab(page, "Node cache")

    def _build_session_tab(self) -> None:
        page = QWidget()
        layout = QHBoxLayout(page)
        self.session_text = QTextEdit()
        self.session_text.setReadOnly(True)
        self.session_text.setPlaceholderText(
            "Snapshot and hot-switch reports appear here."
        )
        layout.addWidget(self.session_text, 3)
        controls = QVBoxLayout()
        self.reload_snapshot_button = QPushButton("Reload snapshot")
        self.reload_snapshot_button.clicked.connect(self._reload_snapshot)
        self.save_snapshot_button = QPushButton("Write snapshot")
        self.save_snapshot_button.clicked.connect(self._save_snapshot)
        self.hot_switch_button = QPushButton("Hot-switch layout…")
        self.hot_switch_button.clicked.connect(self._open_hot_switch)
        controls.addWidget(self.reload_snapshot_button)
        controls.addWidget(self.save_snapshot_button)
        controls.addWidget(self.hot_switch_button)
        controls.addStretch(1)
        layout.addLayout(controls, 1)
        self.tabs.addTab(page, "Snapshot & reconnect")

    def _populate_samples(self) -> None:
        self.sample_combo.clear()
        for entry in self._sample_entries:
            self.sample_combo.addItem(
                f"{entry.get('name', entry['device_id'])} · {entry['device_id']}",
                entry["path"],
            )
        if not self._sample_entries:
            self.sample_combo.addItem("No samples found", "")

    def _load_selected_sample(self) -> None:
        reference = self.sample_combo.currentData()
        if reference:
            self._load_sample_by_reference(str(reference))

    def _resolve_sample_path(self, reference: str | Path) -> Path:
        candidate = Path(reference)
        if candidate.is_dir():
            return candidate
        for entry in self._sample_entries:
            if entry["device_id"] == str(reference):
                return Path(entry["path"])
        return self.samples_root / str(reference)

    def _snapshot_path_for(self, sample_path: Path, device_id: str) -> Path:
        return self.snapshot_root / f"{device_id}.json"

    def _load_sample_by_reference(self, reference: str | Path) -> None:
        sample_path = self._resolve_sample_path(reference)

        def job() -> tuple[StudioSession, VerificationReport]:
            bundle = load_sample(sample_path)
            session = StudioSession(
                bundle=bundle,
                snapshot_path=self._snapshot_path_for(sample_path, bundle.device.id),
                max_parallel=2,
                event_buffer_limit=self.event_buffer_limit,
                restore_snapshot=True,
            )
            verification = session.verify(raise_on_divergence=True)
            return session, verification

        self._start_job(job, self._on_sample_loaded)

    def _on_sample_loaded(self, pair: tuple[StudioSession, VerificationReport]) -> None:
        session, verification = pair
        old = self.session
        self.session = session
        self.last_verification = verification
        self.last_run = session.last_run
        self.last_previous_result = None
        if old is not None:
            old.close()
        self._install_session()
        self._set_status(
            f"Loaded {session.bundle.device.name}; "
            f"{len(session.bundle.events)} events, "
            f"{len(session.bundle.bindings.rules)} bindings."
        )

    def load_sample_sync(self, reference: str | Path | None = None) -> None:
        if reference is None:
            reference = self.sample_combo.currentData()
        if not reference:
            return
        if self.session is not None:
            self.session.close()
            self.session = None
        sample_path = self._resolve_sample_path(str(reference))
        bundle = load_sample(sample_path)
        session = StudioSession(
            bundle=bundle,
            snapshot_path=self._snapshot_path_for(sample_path, bundle.device.id),
            max_parallel=2,
            event_buffer_limit=self.event_buffer_limit,
            restore_snapshot=True,
        )
        verification = session.verify(raise_on_divergence=True)
        self.session = session
        self.last_verification = verification
        self.last_run = session.last_run
        self.last_previous_result = None
        self._install_session()

    def _install_session(self) -> None:
        if self.session is None or self.last_run is None:
            return
        device = self.session.pipeline.device
        self.device_label.setText(
            f"{device.name} · {len(device.axes)} axes · {len(device.buttons)} buttons"
        )
        self.device_widget.set_device(device)
        self._sync_rule_table(device)
        self._sync_axis_controls(device)
        self._refresh_after_run(
            self.last_run,
            previous_result=None,
            verification=self.last_verification,
        )
        self._refresh_snapshot_badges()
        self._refresh_session_text()

    def _sync_rule_table(self, device: DeviceSpec) -> None:
        if self.session is None:
            return
        rules = self.session.pipeline.bindings.rules
        self.rule_table.setRowCount(len(rules))
        for row, rule in enumerate(rules):
            inputs = rule.source_axis or ", ".join(rule.buttons)
            timing = (
                f"hold {rule.hold_ms}ms"
                if rule.kind == "long_press"
                else f"window {rule.window_ms}ms"
                if rule.kind in {"combo", "double_press"}
                else "—"
            )
            curve = "inherit"
            if rule.curve is not None:
                curve = f"{rule.curve.kind} {rule.curve.exponent:g}"
            values = [rule.id, rule.action, rule.kind, inputs, timing, curve]
            for column, value in enumerate(values):
                item = QTableWidgetItem(str(value))
                item.setData(Qt.ItemDataRole.UserRole, row)
                self.rule_table.setItem(row, column, item)

    def _sync_axis_controls(self, device: DeviceSpec) -> None:
        selected = self.axis_combo.currentData() or device.axis_ids[0]
        self.axis_combo.blockSignals(True)
        self.axis_combo.clear()
        for axis in device.axes:
            self.axis_combo.addItem(f"{axis.id} · {axis.kind}", axis.id)
        index = max(0, self.axis_combo.findData(selected))
        self.axis_combo.setCurrentIndex(index)
        self.axis_combo.blockSignals(False)
        self._axis_selection_changed()

    def _axis_selection_changed(self) -> None:
        if self.session is None:
            return
        axis_id = self.axis_combo.currentData()
        if not axis_id:
            return
        axis = self.session.pipeline.device.axis_map[str(axis_id)]
        self.deadzone_spin.blockSignals(True)
        self.exponent_spin.blockSignals(True)
        self.invert_check.blockSignals(True)
        self.deadzone_spin.setValue(axis.deadzone)
        self.exponent_spin.setValue(axis.curve.exponent)
        self.invert_check.setChecked(axis.inverted)
        self.deadzone_spin.blockSignals(False)
        self.exponent_spin.blockSignals(False)
        self.invert_check.blockSignals(False)
        self.device_widget.set_selected_axis(str(axis_id))

    def _refresh_after_run(
        self,
        run: PipelineRun,
        *,
        previous_result: Any,
        verification: VerificationReport | None,
    ) -> None:
        if self.session is None:
            return
        device = self.session.pipeline.device
        last_frame = run.result.frames[-1] if run.result.frames else None
        axis_values = last_frame.axis_values if last_frame else {}
        self.device_widget.set_device(device)
        self.device_widget.set_axis_values(axis_values)
        self.device_widget.set_selected_axis(self.axis_combo.currentData())
        self.timeline_widget.set_data(device, self.session.bundle.events, run.result)
        if last_frame:
            self.timeline_widget.set_selected_sequence(last_frame.source_event_seq)
        self.waveform_widget.set_result(run.result, self.session.pipeline.bindings.rules)
        self._populate_states(run)
        self._populate_invalidations(run)
        self._populate_nodes(run)
        self.dependency_widget.set_graph(
            self.session.pipeline.describe_graph()["nodes"],
            list(self.session.pipeline.order),
            run.invalidations,
        )
        self._refresh_heatmaps(run, verification, previous_result)
        truth = self.session.truth_report
        if truth is not None:
            self.truth_badge.setText("TRUTH · PASS" if truth.passed else "TRUTH · FAIL")
            self.truth_badge.setObjectName("BadgeOk" if truth.passed else "BadgeWarn")
        else:
            self.truth_badge.setText("TRUTH · stale")
            self.truth_badge.setObjectName("BadgeWarn")
        self.truth_badge.style().unpolish(self.truth_badge)
        self.truth_badge.style().polish(self.truth_badge)
        self.cache_badge.setText(
            f"CACHE · {run.reused_count} reuse / {run.recomputed_count} calc"
        )
        self.cache_badge.setObjectName("BadgeOk" if run.recomputed_count else "Badge")
        self.cache_badge.style().unpolish(self.cache_badge)
        self.cache_badge.style().polish(self.cache_badge)
        if verification and not verification.equal:
            self.truth_badge.setText("E_DIVERGE")
            self.truth_badge.setObjectName("BadgeWarn")
        self._refresh_snapshot_badges()
        self._refresh_session_text()

    def _populate_states(self, run: PipelineRun) -> None:
        if not run.result.frames:
            self.states_table.setRowCount(0)
            return
        snapshot = run.result.frames[-1]
        actions = sorted(snapshot.actions)
        self.states_table.setRowCount(len(actions))
        for row, action in enumerate(actions):
            state = snapshot.actions[action]
            values = [
                action,
                "yes" if state.pressed else "no",
                f"{state.value:+.4f}",
                str(state.trigger_count),
                str(state.repeat_count),
                state.last_reason,
            ]
            for column, value in enumerate(values):
                self.states_table.setItem(row, column, QTableWidgetItem(value))

    def _populate_invalidations(self, run: PipelineRun) -> None:
        self.invalidation_table.setRowCount(len(run.invalidations))
        for row, entry in enumerate(run.invalidations):
            values = [
                entry.node_id,
                "yes" if entry.invalidated else "no",
                "yes" if entry.reused else "no",
                ", ".join(entry.reasons),
                ", ".join(entry.window_ids) or "—",
            ]
            for column, value in enumerate(values):
                item = QTableWidgetItem(value)
                if column in {1, 2}:
                    item.setTextAlignment(Qt.AlignmentFlag.AlignCenter)
                if entry.recomputed:
                    item.setBackground(QBrush(QColor(70, 48, 24)))
                elif entry.reused:
                    item.setBackground(QBrush(QColor(18, 47, 38)))
                self.invalidation_table.setItem(row, column, item)
        affected_windows = sorted(
            {
                window
                for entry in run.invalidations
                for window in entry.window_ids
                if entry.invalidated
            }
        )
        self.invalidation_summary.setText(
            f"{run.recomputed_count} recomputed · {run.reused_count} reused · "
            f"windows: {', '.join(affected_windows) or 'assembly only'}"
        )

    def _populate_nodes(self, run: PipelineRun) -> None:
        if self.session is None:
            return
        status = {
            entry.node_id: entry for entry in run.invalidations
        }
        order = list(self.session.pipeline.order) + [
            entry.node_id
            for entry in run.invalidations
            if entry.node_id not in self.session.pipeline.nodes
        ]
        self.node_table.setRowCount(len(order))
        for row, node_id in enumerate(order):
            node = self.session.pipeline.nodes.get(node_id)
            entry = status.get(node_id)
            cache = self.session.pipeline.cache_entry(node_id)
            node_kind = node.kind if node else (entry.kind if entry else "")
            state = "recomputed" if entry and entry.recomputed else "reused"
            values = [
                node_id,
                node_kind,
                state,
                cache.cache_key[:20] + "…" if cache else "removed",
                ", ".join(cache.window_ids) if cache and cache.window_ids else "—",
            ]
            for column, value in enumerate(values):
                item = QTableWidgetItem(value)
                item.setData(Qt.ItemDataRole.UserRole, node_id)
                if state == "recomputed":
                    item.setBackground(QBrush(QColor(70, 48, 24)))
                self.node_table.setItem(row, column, item)

    def _refresh_heatmaps(
        self,
        run: PipelineRun,
        verification: VerificationReport | None,
        previous_result: Any,
    ) -> None:
        action_ids = list(run.result.action_ids)
        if verification is not None:
            matrix, edges = comparison_heatmap(
                run.result,
                verification.full_result,
                action_ids=action_ids,
            )
            badge = "IDENTICAL" if verification.equal else "DIVERGENT"
        else:
            matrix = np.zeros((len(action_ids), 1), dtype=float)
            edges = [0, 1]
            badge = "PENDING"
        self.equality_heatmap.set_matrix(
            matrix,
            action_ids,
            edges,
            badge=badge,
        )
        change_matrix, change_edges = change_heatmap(
            previous_result,
            run.result,
            action_ids=action_ids,
        )
        changed = bool(np.any(change_matrix > 1e-12))
        self.change_heatmap.set_matrix(
            change_matrix,
            action_ids,
            change_edges,
            badge="CHANGED" if changed else "NO CHANGE",
        )

    def _refresh_snapshot_badges(self) -> None:
        if self.session is None:
            return
        load = self.session.snapshot_load
        if load is None:
            text = "SNAPSHOT · off"
        elif load.valid:
            text = "SNAPSHOT · valid"
        elif load.reason == "snapshot_missing":
            text = "SNAPSHOT · new"
        elif load.reason == "version_mismatch":
            text = f"SNAPSHOT · void {load.invalidated_node_count or 0}"
        else:
            text = "SNAPSHOT · rejected"
        self.snapshot_badge.setText(text)
        self.snapshot_badge.setObjectName(
            "BadgeOk" if load and load.valid else "BadgeWarn" if load else "Badge"
        )
        self.snapshot_badge.style().unpolish(self.snapshot_badge)
        self.snapshot_badge.style().polish(self.snapshot_badge)
        self.statusBar().setToolTip(text)

    def _refresh_session_text(self) -> None:
        if self.session is None:
            return
        state = self.session.state()
        snapshot_load = state.get("snapshot_load")
        snapshot_write = state.get("snapshot_write")
        verification = state.get("verification")
        lines = [
            "SESSION STATE",
            f"sample: {self.session.bundle.device.id}",
            f"input hash: {self.session.bundle.input_hash}",
            f"binding version: {self.session.bundle.binding_version}",
            f"algorithm: {self.session.bundle.algorithm_version}",
            f"workers: {state['concurrency']['max_parallel']} · "
            f"buffer: {state['concurrency']['event_buffer_limit']} events/thread",
            "",
            "SNAPSHOT LOAD",
            json.dumps(snapshot_load, ensure_ascii=False, indent=2)
            if snapshot_load
            else "not attempted",
            "",
            "SNAPSHOT WRITE",
            json.dumps(snapshot_write, ensure_ascii=False, indent=2)
            if snapshot_write
            else "not written in this session",
            "",
            "VERIFICATION",
            json.dumps(verification, ensure_ascii=False, indent=2)
            if verification
            else "not run",
        ]
        if self.last_hot_switch is not None:
            lines.extend(
                [
                    "",
                    "HOT SWITCH",
                    json.dumps(
                        self.last_hot_switch.to_dict(),
                        ensure_ascii=False,
                        indent=2,
                    ),
                ]
            )
        self.session_text.setPlainText("\n".join(lines))

    def _start_job(
        self,
        function: Callable[[], Any],
        on_success: Callable[[Any], None],
    ) -> None:
        if self._busy:
            self._set_status("Another replay task is active; wait for it to finish.")
            return
        self._busy = True
        self._token += 1
        token = self._token
        self._set_controls_enabled(False)
        worker = _Worker(function, token)
        self._active_job = worker
        worker.signals.completed.connect(
            lambda payload: self._job_completed(payload, on_success)
        )
        worker.signals.failed.connect(self._job_failed)
        self._set_status("Replaying in worker thread…")
        self.thread_pool.start(worker)

    def _job_completed(
        self,
        payload: dict[str, Any],
        callback: Callable[[Any], None],
    ) -> None:
        if payload["token"] != self._token:
            return
        self._finish_job()
        callback(payload["value"])

    def _job_failed(self, payload: dict[str, Any]) -> None:
        if payload["token"] != self._token:
            return
        self._finish_job()
        error = payload["error"]
        field = error.get("field") or "—"
        event_seq = error.get("event_seq")
        suffix = f" · field {field}" if field else ""
        if event_seq is not None:
            suffix += f" · event {event_seq}"
        self._set_status(f"{error.get('code', 'E_UI')}: {error.get('message', '')}{suffix}")
        if not self._suppress_dialogs:
            QMessageBox.warning(
                self,
                error.get("code", "Error"),
                f"{error.get('message', '')}\n\nfield: {field}\nevent: {event_seq}",
            )

    def _finish_job(self) -> None:
        self._busy = False
        self._active_job = None
        self._set_controls_enabled(True)

    def _set_controls_enabled(self, enabled: bool) -> None:
        for widget in (
            self.load_button,
            self.verify_button,
            self.snapshot_button,
            self.switch_button,
            self.apply_axis_button,
            self.edit_rule_button,
            self.retry_node_button,
            self.save_snapshot_button,
            self.reload_snapshot_button,
            self.hot_switch_button,
        ):
            widget.setEnabled(enabled)

    def _apply_axis(self) -> None:
        if self.session is None:
            return
        axis_id = self.axis_combo.currentData()
        if not axis_id:
            return
        deadzone = self.deadzone_spin.value()
        exponent = self.exponent_spin.value()
        inverted = self.invert_check.isChecked()

        def job() -> SessionUpdate:
            assert self.session is not None
            return self.session.update_axis(
                str(axis_id),
                deadzone=deadzone,
                inverted=inverted,
                curve_exponent=exponent,
                verify=True,
            )

        self._start_job(job, self._on_update_complete)

    def _on_update_complete(self, update: SessionUpdate) -> None:
        assert self.session is not None
        self.last_previous_result = update.previous_result
        self.last_run = update.run
        self.last_verification = update.verification
        self._sync_rule_table(self.session.pipeline.device)
        self._sync_axis_controls(self.session.pipeline.device)
        self._refresh_after_run(
            update.run,
            previous_result=update.previous_result,
            verification=update.verification,
        )
        self._set_status(
            f"Incremental update: {update.run.reused_count} reused, "
            f"{update.run.recomputed_count} recomputed, "
            f"full-replay match={update.verification.equal if update.verification else 'n/a'}."
        )

    def _edit_rule(self, *args: Any) -> None:
        if self.session is None or self.rule_table.currentRow() < 0:
            return
        index = self.rule_table.currentRow()
        rule = self.session.pipeline.bindings.rules[index]
        dialog = RuleDialog(rule, self.session.pipeline.device, self)
        if dialog.exec() == QDialog.DialogCode.Accepted:
            edited = dialog.result_rule()

            def job() -> SessionUpdate:
                assert self.session is not None
                return self.session.update_rule(edited, verify=True)

            self._start_job(job, self._on_update_complete)

    def _verify_current(self) -> None:
        if self.session is None:
            return

        def job() -> VerificationReport:
            assert self.session is not None
            return self.session.verify(raise_on_divergence=True)

        def done(report: VerificationReport) -> None:
            self.last_verification = report
            assert self.last_run is not None
            self._refresh_after_run(
                self.last_run,
                previous_result=self.last_previous_result,
                verification=report,
            )
            self._set_status(
                "Full replay comparison identical."
                if report.equal
                else "E_DIVERGE · first mismatch recorded in heatmap."
            )

        self._start_job(job, done)

    def _save_snapshot(self) -> None:
        if self.session is None:
            return

        def job() -> SnapshotWriteReport:
            assert self.session is not None
            return self.session.save_snapshot()

        def done(report: SnapshotWriteReport) -> None:
            self._refresh_session_text()
            self.snapshot_badge.setText(f"SNAPSHOT · written {report.node_count}")
            self.snapshot_badge.setObjectName("BadgeOk")
            self.snapshot_badge.style().unpolish(self.snapshot_badge)
            self.snapshot_badge.style().polish(self.snapshot_badge)
            self._set_status(
                f"Snapshot written atomically: {report.node_count} nodes; "
                f"version changes: {', '.join(report.version_changes) or 'none'}."
            )

        self._start_job(job, done)

    def _reload_snapshot(self) -> None:
        if self.session is None:
            return

        def job() -> SnapshotLoadResult:
            assert self.session is not None
            return self.session.reload_snapshot()

        def done(result: SnapshotLoadResult) -> None:
            assert self.session is not None
            self.last_run = self.session.last_run
            self._refresh_after_run(
                self.last_run,
                previous_result=None,
                verification=None,
            )
            self._refresh_session_text()
            self._set_status(
                f"Snapshot load: {result.reason}; "
                f"invalidated nodes: {result.invalidated_node_count}."
            )

        self._start_job(job, done)

    def _open_hot_switch(self) -> None:
        if self.session is None:
            return
        current = self.session.bundle.device.id
        targets = [
            (entry["device_id"], entry.get("name", entry["device_id"]))
            for entry in self._sample_entries
            if entry["device_id"] != current
        ]
        if not targets:
            self._set_status("No alternate device sample is available for hot switch.")
            return
        frames = self.last_run.result.frames if self.last_run else ()
        default_time = frames[len(frames) // 2].t_ms if frames else 0
        dialog = HotSwitchDialog(targets, default_time, self)
        if dialog.exec() != QDialog.DialogCode.Accepted:
            return
        target_id, switch_t = dialog.selection()
        target_path = self._resolve_sample_path(target_id)

        def job() -> HotSwitchResult:
            assert self.session is not None
            target_bundle = load_sample(target_path)
            return self.session.switch_device(
                target_bundle,
                switch_t_ms=switch_t,
            )

        def done(result: HotSwitchResult) -> None:
            self.last_hot_switch = result
            self._refresh_session_text()
            self.tabs.setCurrentIndex(self.tabs.count() - 1)
            self._set_status(
                f"Hot switch {result.analysis.source_device} → "
                f"{result.analysis.target_device}: prefix {len(result.prefix_frames)} frames "
                f"frozen, suffix {len(result.suffix_frames)} frames, "
                f"unmapped {len(result.analysis.unmapped)}, "
                f"direct match={result.direct_match}."
            )
            if not self._suppress_dialogs:
                QMessageBox.information(
                    self,
                    "Hot switch complete",
                    f"Committed prefix: {len(result.prefix_frames)} frames\n"
                    f"Target suffix: {len(result.suffix_frames)} frames\n"
                    f"Unmapped inputs: {len(result.analysis.unmapped)}\n"
                    f"Prefix hash: {result.prefix_hash[:16]}…\n"
                    f"Direct target match: {result.direct_match}",
                )

        self._start_job(job, done)

    def _show_node_detail(self, node_id: str) -> None:
        if self.session is None:
            return
        self.node_cache_detail.setPlainText(
            json.dumps(
                {
                    "node": self.session.pipeline.nodes.get(
                        node_id, {}
                    ).to_dict()
                    if node_id in self.session.pipeline.nodes
                    else {"node_id": node_id, "removed": True},
                    "cache": (
                        self.session.pipeline.cache_entry(node_id).to_dict()
                        if self.session.pipeline.cache_entry(node_id)
                        else None
                    ),
                },
                ensure_ascii=False,
                indent=2,
            )
        )

    def _node_table_selection(self) -> None:
        row = self.node_table.currentRow()
        if row < 0:
            return
        item = self.node_table.item(row, 0)
        if item is not None:
            self._show_node_detail(item.data(Qt.ItemDataRole.UserRole) or item.text())

    def _retry_selected_node(self) -> None:
        if self.session is None or self.node_table.currentRow() < 0:
            return
        item = self.node_table.item(self.node_table.currentRow(), 0)
        if item is None:
            return
        node_id = item.data(Qt.ItemDataRole.UserRole) or item.text()
        if node_id not in self.session.pipeline.nodes:
            self._set_status("Removed nodes cannot be retried.")
            return

        def job() -> SessionUpdate:
            assert self.session is not None
            return self.session.retry_node(str(node_id), verify=True)

        self._start_job(job, self._on_update_complete)

    def _set_status(self, message: str) -> None:
        self.status_label.setText(message)

    def capture(self, path: str | Path) -> bool:
        QApplication.processEvents()
        pixmap = self.grab()
        target = Path(path)
        target.parent.mkdir(parents=True, exist_ok=True)
        return pixmap.save(str(target))

    def closeEvent(self, event: QCloseEvent) -> None:
        self.thread_pool.waitForDone(1500)
        if self.session is not None:
            self.session.close()
        super().closeEvent(event)


def prepare_samples_if_needed(root: str | Path) -> None:
    path = Path(root)
    if not (path / "index.json").exists():
        generate_samples(path)


__all__ = ["MainWindow", "prepare_samples_if_needed"]
