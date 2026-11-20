from __future__ import annotations

import sys
import threading
from dataclasses import replace
from pathlib import Path
from typing import Any

from PySide6.QtCore import QThread, QTimer, Qt, Signal
from PySide6.QtGui import QAction, QFont, QFontDatabase
from PySide6.QtWidgets import (
    QApplication,
    QCheckBox,
    QComboBox,
    QDockWidget,
    QDoubleSpinBox,
    QFileDialog,
    QFormLayout,
    QGroupBox,
    QHBoxLayout,
    QLabel,
    QMainWindow,
    QMessageBox,
    QProgressBar,
    QPushButton,
    QScrollArea,
    QSlider,
    QSpinBox,
    QToolBar,
    QVBoxLayout,
    QWidget,
)

from .engine import SessionEngine
from .errors import AppError
from .models import ChainConfig, Interval
from .session import SessionSnapshot
from .timeline import TimelineWidget


class ProcessWorker(QThread):
    progress = Signal(dict)
    completed = Signal(dict)
    failed = Signal(str, str)

    def __init__(self, session_path: Path, abort_event: threading.Event) -> None:
        super().__init__()
        self.session_path = session_path
        self.abort_event = abort_event

    def run(self) -> None:
        try:
            engine = SessionEngine(self.session_path)
            report = engine.process(
                abort_event=self.abort_event,
                progress=self.progress.emit,
            )
            self.completed.emit(report)
        except AppError as exc:
            self.failed.emit(exc.code, exc.message)
        except Exception as exc:  # pragma: no cover - defensive UI boundary
            self.failed.emit("E_INTERNAL", f"{type(exc).__name__}: {exc}")


class MainWindow(QMainWindow):
    def __init__(self) -> None:
        super().__init__()
        self.setWindowTitle("声迹修复 · Voice & Music Repair")
        self.resize(1260, 760)
        self.engine: SessionEngine | None = None
        self.snapshot: SessionSnapshot | None = None
        self.intervals: list[Interval] = []
        self.selected_id: int | None = None
        self.worker: ProcessWorker | None = None
        self.abort_event = threading.Event()
        self._suspend_updates = False
        self._chain_timer = QTimer(self)
        self._chain_timer.setSingleShot(True)
        self._chain_timer.setInterval(450)
        self._chain_timer.timeout.connect(self._apply_chain_now)
        self._interval_timer = QTimer(self)
        self._interval_timer.setSingleShot(True)
        self._interval_timer.setInterval(450)
        self._interval_timer.timeout.connect(self._apply_interval_now)
        self._build_ui()
        self._build_actions()

    def _build_ui(self) -> None:
        central = QWidget()
        layout = QVBoxLayout(central)
        layout.setContentsMargins(8, 8, 8, 8)
        layout.setSpacing(6)
        self.timeline = TimelineWidget()
        self.timeline.intervalsEdited.connect(self._on_timeline_intervals)
        self.timeline.selectionChanged.connect(self._on_selection_changed)
        self.timeline.message.connect(self._show_status_message)
        layout.addWidget(self.timeline, 1)

        status_row = QHBoxLayout()
        self.status_label = QLabel("未载入会话")
        self.progress_label = QLabel("当前块: - / -    已提交: 0")
        self.progress_bar = QProgressBar()
        self.progress_bar.setRange(0, 100)
        self.progress_bar.setValue(0)
        self.progress_bar.setFixedWidth(240)
        status_row.addWidget(self.status_label, 1)
        status_row.addWidget(self.progress_label)
        status_row.addWidget(self.progress_bar)
        layout.addLayout(status_row)
        self.setCentralWidget(central)
        self._build_parameter_dock()

    def _build_parameter_dock(self) -> None:
        dock = QDockWidget("处理参数", self)
        dock.setAllowedAreas(Qt.DockWidgetArea.RightDockWidgetArea)
        scroll = QScrollArea()
        scroll.setWidgetResizable(True)
        container = QWidget()
        layout = QVBoxLayout(container)

        dc_group = QGroupBox("1. 去直流偏移")
        dc_form = QFormLayout(dc_group)
        self.dc_enabled = QCheckBox("启用（按声道独立统计）")
        dc_form.addRow(self.dc_enabled)

        gate_group = QGroupBox("2. 噪声门")
        gate_form = QFormLayout(gate_group)
        self.gate_enabled = QCheckBox("启用")
        self.gate_threshold = self._double_spin(-120.0, 0.0, -42.0, 1.0, " dB")
        self.gate_attack = self._double_spin(0.1, 1000.0, 4.0, 1.0, " ms")
        self.gate_release = self._double_spin(0.1, 5000.0, 80.0, 1.0, " ms")
        self.gate_attenuation = self._double_spin(-120.0, 0.0, -60.0, 1.0, " dB")
        gate_form.addRow(self.gate_enabled)
        gate_form.addRow("阈值", self.gate_threshold)
        gate_form.addRow("启动", self.gate_attack)
        gate_form.addRow("释放", self.gate_release)
        gate_form.addRow("衰减", self.gate_attenuation)

        spectral_group = QGroupBox("3. 谱减降噪")
        spectral_form = QFormLayout(spectral_group)
        self.spectral_enabled = QCheckBox("启用（左右声道帧对齐）")
        self.spectral_nfft = QComboBox()
        for value in (256, 512, 1024, 2048):
            self.spectral_nfft.addItem(str(value), value)
        self.spectral_nfft.setCurrentIndex(1)
        self.spectral_hop = QSpinBox()
        self.spectral_hop.setRange(16, 2047)
        self.spectral_hop.setValue(128)
        self.spectral_percentile = self._double_spin(0.0, 100.0, 20.0, 1.0, " %")
        self.spectral_oversubtraction = self._double_spin(0.0, 10.0, 1.5, 0.1, " ×")
        self.spectral_reduction = self._double_spin(0.0, 60.0, 12.0, 1.0, " dB")
        self.spectral_floor = self._double_spin(-120.0, 0.0, -45.0, 1.0, " dB")
        spectral_form.addRow(self.spectral_enabled)
        spectral_form.addRow("FFT 长度", self.spectral_nfft)
        spectral_form.addRow("帧移", self.spectral_hop)
        spectral_form.addRow("噪声分位", self.spectral_percentile)
        spectral_form.addRow("过减系数", self.spectral_oversubtraction)
        spectral_form.addRow("降噪量", self.spectral_reduction)
        spectral_form.addRow("谱底", self.spectral_floor)

        limiter_group = QGroupBox("4. 区间增益包络 + 联动限幅")
        limiter_form = QFormLayout(limiter_group)
        self.limiter_enabled = QCheckBox("启用轻量限幅")
        self.limiter_threshold = self._double_spin(-30.0, 0.0, -1.0, 0.5, " dBFS")
        limiter_form.addRow(self.limiter_enabled)
        limiter_form.addRow("限幅门限", self.limiter_threshold)

        interval_group = QGroupBox("选中区间")
        interval_form = QFormLayout(interval_group)
        self.interval_label = QLabel("未选择区间")
        self.interval_gain = self._double_spin(-60.0, 24.0, 0.0, 1.0, " dB")
        self.interval_fade_in = self._double_spin(0.0, 10000.0, 10.0, 1.0, " ms")
        self.interval_fade_out = self._double_spin(0.0, 10000.0, 10.0, 1.0, " ms")
        interval_form.addRow(self.interval_label)
        interval_form.addRow("增益", self.interval_gain)
        interval_form.addRow("淡入", self.interval_fade_in)
        interval_form.addRow("淡出", self.interval_fade_out)
        self.interval_group = interval_group

        self.apply_button = QPushButton("立即应用参数")
        self.apply_button.clicked.connect(self._apply_all_parameters)
        layout.addWidget(dc_group)
        layout.addWidget(gate_group)
        layout.addWidget(spectral_group)
        layout.addWidget(limiter_group)
        layout.addWidget(interval_group)
        layout.addWidget(self.apply_button)
        layout.addStretch(1)
        scroll.setWidget(container)
        dock.setWidget(scroll)
        self.addDockWidget(Qt.DockWidgetArea.RightDockWidgetArea, dock)

        for checkbox in (
            self.dc_enabled,
            self.gate_enabled,
            self.spectral_enabled,
            self.limiter_enabled,
        ):
            checkbox.toggled.connect(self._schedule_chain_apply)
        for spin in (
            self.gate_threshold,
            self.gate_attack,
            self.gate_release,
            self.gate_attenuation,
            self.spectral_percentile,
            self.spectral_oversubtraction,
            self.spectral_reduction,
            self.spectral_floor,
            self.limiter_threshold,
        ):
            spin.valueChanged.connect(self._schedule_chain_apply)
        self.spectral_nfft.currentIndexChanged.connect(self._schedule_chain_apply)
        self.spectral_hop.valueChanged.connect(self._schedule_chain_apply)
        for spin in (self.interval_gain, self.interval_fade_in, self.interval_fade_out):
            spin.valueChanged.connect(self._schedule_interval_apply)
        self.interval_group.setEnabled(False)

    def _build_actions(self) -> None:
        toolbar = QToolBar("主工具栏", self)
        toolbar.setMovable(False)
        self.addToolBar(toolbar)
        new_action = QAction("新建会话", self)
        new_action.setShortcut("Ctrl+N")
        new_action.triggered.connect(self._new_session)
        open_action = QAction("打开会话", self)
        open_action.setShortcut("Ctrl+O")
        open_action.triggered.connect(self._open_session)
        save_action = QAction("保存", self)
        save_action.setShortcut("Ctrl+S")
        save_action.triggered.connect(self._save_session)
        self.add_action = QAction("添加区间", self)
        self.add_action.triggered.connect(self._add_interval)
        self.delete_action = QAction("删除区间", self)
        self.delete_action.triggered.connect(self._delete_interval)
        self.process_action = QAction("开始处理", self)
        self.process_action.setShortcut("F5")
        self.process_action.triggered.connect(self._start_processing)
        self.stop_action = QAction("协作式中止", self)
        self.stop_action.setEnabled(False)
        self.stop_action.triggered.connect(self._stop_processing)
        toolbar.addAction(new_action)
        toolbar.addAction(open_action)
        toolbar.addAction(save_action)
        toolbar.addSeparator()
        toolbar.addAction(self.add_action)
        toolbar.addAction(self.delete_action)
        toolbar.addSeparator()
        toolbar.addAction(self.process_action)
        toolbar.addAction(self.stop_action)
        toolbar.addSeparator()
        toolbar.addWidget(QLabel(" 缩放 "))
        self.zoom_slider = QSlider(Qt.Orientation.Horizontal)
        self.zoom_slider.setRange(20, 2000)
        self.zoom_slider.setValue(220)
        self.zoom_slider.setFixedWidth(160)
        self.zoom_slider.valueChanged.connect(self.timeline.set_zoom)
        toolbar.addWidget(self.zoom_slider)

    @staticmethod
    def _double_spin(
        minimum: float, maximum: float, value: float, step: float, suffix: str
    ) -> QDoubleSpinBox:
        spin = QDoubleSpinBox()
        spin.setRange(minimum, maximum)
        spin.setDecimals(1)
        spin.setSingleStep(step)
        spin.setValue(value)
        spin.setSuffix(suffix)
        spin.setKeyboardTracking(False)
        return spin

    def _show_error(self, code: str, message: str) -> None:
        QMessageBox.critical(self, f"操作失败 [{code}]", message)
        self._show_status_message(f"{code}: {message}")

    def _show_status_message(self, message: str) -> None:
        self.statusBar().showMessage(message, 7000)

    def open_session_path(self, path: str | Path) -> None:
        engine = SessionEngine(path)
        try:
            report = engine.recover()
            snapshot = engine.store.load()
        except AppError as exc:
            self._show_error(exc.code, exc.message)
            return
        self.engine = engine
        self._load_snapshot(snapshot)
        self.progress_label.setText(
            f"当前块: - / {report['total_blocks']}    已提交: {report['committed_blocks']}"
        )
        if report["remaining_blocks"] > 0:
            self._show_status_message(
                f"恢复成功：已提交 {report['recovered_blocks']} 块，"
                f"剩余 {report['remaining_blocks']} 块"
            )
        else:
            self._show_status_message("会话已完整载入")

    def _new_session(self) -> None:
        input_path, _ = QFileDialog.getOpenFileName(
            self, "选择 16-bit PCM WAV", "", "WAV 音频 (*.wav)"
        )
        if not input_path:
            return
        session_path, _ = QFileDialog.getSaveFileName(
            self,
            "保存修复会话",
            str(Path(input_path).with_suffix(".repair.json")),
            "修复会话 (*.json)",
        )
        if not session_path:
            return
        if not session_path.endswith(".json"):
            session_path += ".json"
        output_path = str(
            Path(session_path).with_name(Path(session_path).stem + "_repaired.wav")
        )
        force = False
        if Path(output_path).exists():
            answer = QMessageBox.question(
                self, "输出文件已存在", f"{output_path}\n是否覆盖？"
            )
            if answer != QMessageBox.StandardButton.Yes:
                return
            force = True
        engine = SessionEngine(session_path)
        try:
            engine.create(
                input_path=input_path,
                output_path=output_path,
                block_size=4096,
                intervals=[],
                chain=ChainConfig(),
                force=force,
            )
            self.engine = engine
            self._load_snapshot(engine.store.load())
            self._show_status_message(f"已创建会话: {session_path}")
        except AppError as exc:
            self._show_error(exc.code, exc.message)

    def _open_session(self) -> None:
        path, _ = QFileDialog.getOpenFileName(
            self, "打开修复会话", "", "修复会话 (*.json)"
        )
        if path:
            self.open_session_path(path)

    def _save_session(self) -> None:
        if self.engine is None:
            return
        try:
            self.engine.save()
            self._show_status_message("会话已原子保存")
        except AppError as exc:
            self._show_error(exc.code, exc.message)

    def _load_snapshot(self, snapshot: SessionSnapshot) -> None:
        self._suspend_updates = True
        try:
            self.snapshot = snapshot
            self.intervals = list(snapshot.intervals)
            self.selected_id = None
            self.timeline.set_audio(
                snapshot.input_samples, snapshot.info.sample_rate, snapshot.block_size
            )
            self.timeline.set_intervals(self.intervals)
            self._fill_chain_widgets(snapshot.chain)
            self._fill_interval_widgets(None)
            self.setWindowTitle(
                f"声迹修复 · {snapshot.path.name} · {snapshot.info.sample_rate} Hz / "
                f"{snapshot.info.channels} ch"
            )
            self._update_status_from_snapshot(snapshot)
        finally:
            self._suspend_updates = False

    def _fill_chain_widgets(self, chain: ChainConfig) -> None:
        self.dc_enabled.setChecked(chain.dc_removal.enabled)
        gate = chain.noise_gate
        self.gate_enabled.setChecked(gate.enabled)
        self.gate_threshold.setValue(gate.threshold_db)
        self.gate_attack.setValue(gate.attack_ms)
        self.gate_release.setValue(gate.release_ms)
        self.gate_attenuation.setValue(gate.attenuation_db)
        spectral = chain.spectral_subtraction
        self.spectral_enabled.setChecked(spectral.enabled)
        index = self.spectral_nfft.findData(spectral.n_fft)
        if index >= 0:
            self.spectral_nfft.setCurrentIndex(index)
        self.spectral_hop.setValue(spectral.hop_length)
        self.spectral_percentile.setValue(spectral.noise_percentile)
        self.spectral_oversubtraction.setValue(spectral.oversubtraction)
        self.spectral_reduction.setValue(spectral.reduction_db)
        self.spectral_floor.setValue(spectral.floor_db)
        self.limiter_enabled.setChecked(chain.limiter.enabled)
        self.limiter_threshold.setValue(chain.limiter.threshold_db)

    def _chain_from_widgets(self) -> ChainConfig:
        return ChainConfig.from_dict(
            {
                "dc_removal": {
                    "enabled": self.dc_enabled.isChecked(),
                    "per_channel": True,
                },
                "noise_gate": {
                    "enabled": self.gate_enabled.isChecked(),
                    "threshold_db": self.gate_threshold.value(),
                    "attack_ms": self.gate_attack.value(),
                    "release_ms": self.gate_release.value(),
                    "attenuation_db": self.gate_attenuation.value(),
                },
                "spectral_subtraction": {
                    "enabled": self.spectral_enabled.isChecked(),
                    "n_fft": int(self.spectral_nfft.currentData()),
                    "hop_length": self.spectral_hop.value(),
                    "noise_percentile": self.spectral_percentile.value(),
                    "oversubtraction": self.spectral_oversubtraction.value(),
                    "reduction_db": self.spectral_reduction.value(),
                    "floor_db": self.spectral_floor.value(),
                },
                "limiter": {
                    "enabled": self.limiter_enabled.isChecked(),
                    "threshold_db": self.limiter_threshold.value(),
                },
            }
        )

    def _update_status_from_snapshot(self, snapshot: SessionSnapshot) -> None:
        total = snapshot.block_count
        committed = len(snapshot.completed)
        state = "已完成" if snapshot.complete else ("部分完成" if committed else "待处理")
        self.status_label.setText(
            f"{state} · {snapshot.info.sample_rate} Hz · {snapshot.info.channels} ch · "
            f"{snapshot.info.frames / snapshot.info.sample_rate:.2f}s · "
            f"块大小 {snapshot.block_size}"
        )
        self.progress_label.setText(f"当前块: - / {total}    已提交: {committed}")
        self.progress_bar.setRange(0, max(1, total))
        self.progress_bar.setValue(committed)
        self.process_action.setText(
            "重新处理" if snapshot.complete else ("继续处理" if committed else "开始处理")
        )

    def _refresh_status(self) -> None:
        if self.engine is None:
            return
        try:
            snapshot = self.engine.store.load()
        except AppError as exc:
            self._show_error(exc.code, exc.message)
            return
        self.snapshot = snapshot
        self._update_status_from_snapshot(snapshot)

    def _on_selection_changed(self, interval_id: int) -> None:
        self.selected_id = None if interval_id < 0 else interval_id
        interval = self._selected_interval()
        self.timeline.set_selected(self.selected_id)
        self._fill_interval_widgets(interval)
        self.delete_action.setEnabled(interval is not None)

    def _selected_interval(self) -> Interval | None:
        if self.selected_id is None:
            return None
        return next((item for item in self.intervals if item.id == self.selected_id), None)

    def _fill_interval_widgets(self, interval: Interval | None) -> None:
        self._suspend_updates = True
        try:
            self.interval_group.setEnabled(interval is not None)
            if interval is None:
                self.interval_label.setText("未选择区间")
            else:
                start = (
                    interval.start_frame / self.snapshot.info.sample_rate
                    if self.snapshot
                    else 0.0
                )
                end = (
                    interval.end_frame / self.snapshot.info.sample_rate
                    if self.snapshot
                    else 0.0
                )
                self.interval_label.setText(f"#{interval.id}  {start:.3f}s – {end:.3f}s")
                self.interval_gain.setValue(interval.gain_db)
                self.interval_fade_in.setValue(interval.fade_in_ms)
                self.interval_fade_out.setValue(interval.fade_out_ms)
        finally:
            self._suspend_updates = False

    def _schedule_chain_apply(self, *args: Any) -> None:
        if self._suspend_updates or self.engine is None:
            return
        if self.worker is not None and self.worker.isRunning():
            self._show_status_message("处理任务运行中，暂不能修改参数")
            return
        self._chain_timer.start()

    def _schedule_interval_apply(self, *args: Any) -> None:
        if self._suspend_updates or self.engine is None or self._selected_interval() is None:
            return
        if self.worker is not None and self.worker.isRunning():
            self._show_status_message("处理任务运行中，暂不能修改区间")
            return
        self._interval_timer.start()

    def _apply_chain_now(self) -> None:
        if self.engine is None or self._suspend_updates:
            return
        try:
            report = self.engine.update_chain(self._chain_from_widgets().to_dict())
            dirty = report.get("dirty_blocks", [])
            self._refresh_status()
            self._show_status_message(
                f"参数已保存，{len(dirty)} 个受影响块标记为需重算"
            )
        except AppError as exc:
            self._show_error(exc.code, exc.message)

    def _apply_interval_now(self) -> None:
        interval = self._selected_interval()
        if self.engine is None or interval is None:
            return
        updated = replace(
            interval,
            gain_db=self.interval_gain.value(),
            fade_in_ms=self.interval_fade_in.value(),
            fade_out_ms=self.interval_fade_out.value(),
        )
        self.intervals = [
            updated if item.id == updated.id else item for item in self.intervals
        ]
        self.timeline.set_intervals(self.intervals, self.selected_id)
        self._commit_intervals()

    def _apply_all_parameters(self) -> None:
        self._chain_timer.stop()
        self._interval_timer.stop()
        self._apply_chain_now()
        self._apply_interval_now()

    def _on_timeline_intervals(self, intervals: list[Interval]) -> None:
        if self.engine is None:
            return
        if self.worker is not None and self.worker.isRunning():
            self._show_status_message("处理中不能移动或删除区间")
            self._reload_intervals()
            return
        self.intervals = intervals
        self._commit_intervals()

    def _commit_intervals(self) -> None:
        if self.engine is None:
            return
        try:
            report = self.engine.update_intervals(self.intervals)
            self.timeline.set_intervals(self.intervals, self.selected_id)
            self._fill_interval_widgets(self._selected_interval())
            self._refresh_status()
            dirty = report.get("dirty_blocks", [])
            if dirty:
                self._show_status_message(f"区间已保存，{len(dirty)} 个块需重算")
        except AppError as exc:
            self._show_error(exc.code, exc.message)
            self._reload_intervals()

    def _reload_intervals(self) -> None:
        if self.engine is None:
            return
        try:
            snapshot = self.engine.store.load()
            self.snapshot = snapshot
            self.intervals = list(snapshot.intervals)
            self.timeline.set_intervals(self.intervals, self.selected_id)
        except AppError as exc:
            self._show_error(exc.code, exc.message)

    def _add_interval(self) -> None:
        if self.snapshot is None:
            return
        rate = self.snapshot.info.sample_rate
        total = self.snapshot.info.frames
        duration = max(1, int(round(0.5 * rate)))
        start = 0
        for item in sorted(self.intervals, key=lambda value: value.start_frame):
            if start + duration <= item.start_frame:
                break
            start = max(start, item.end_frame)
        if start + duration > total:
            self._show_error(
                "E_INVALID_ARGUMENT", "时间轴剩余空间不足，无法添加 0.5 秒区间"
            )
            return
        interval = Interval(
            id=max((item.id for item in self.intervals), default=0) + 1,
            start_frame=start,
            end_frame=start + duration,
        )
        self.intervals = sorted(
            self.intervals + [interval], key=lambda item: item.start_frame
        )
        self.selected_id = interval.id
        self.timeline.set_intervals(self.intervals, interval.id)
        self._fill_interval_widgets(interval)
        self._commit_intervals()

    def _delete_interval(self) -> None:
        interval = self._selected_interval()
        if interval is None:
            return
        self.intervals = [item for item in self.intervals if item.id != interval.id]
        self.selected_id = None
        self.timeline.set_intervals(self.intervals)
        self._fill_interval_widgets(None)
        self._commit_intervals()

    def _start_processing(self) -> None:
        if self.engine is None:
            self._show_error("E_INVALID_ARGUMENT", "请先打开或创建会话")
            return
        if self.worker is not None and self.worker.isRunning():
            self._show_error("E_BUSY", "同一会话已有处理任务在运行")
            return
        self.abort_event.clear()
        self.worker = ProcessWorker(self.engine.path, self.abort_event)
        self.worker.progress.connect(self._on_progress)
        self.worker.completed.connect(self._on_process_completed)
        self.worker.failed.connect(self._on_process_failed)
        self.worker.finished.connect(self.worker.deleteLater)
        self.process_action.setEnabled(False)
        self.stop_action.setEnabled(True)
        self.apply_button.setEnabled(False)
        self._show_status_message("处理任务已启动")
        self.worker.start()

    def _stop_processing(self) -> None:
        if self.worker is None or not self.worker.isRunning():
            return
        self.abort_event.set()
        self.stop_action.setEnabled(False)
        self._show_status_message("正在协作式中止：等待当前块完整提交")

    def _on_progress(self, event: dict[str, Any]) -> None:
        total = int(event.get("total_blocks", 0))
        current = int(event.get("current_block", -1))
        committed = int(event.get("committed_blocks", 0))
        self.progress_bar.setRange(0, max(1, total))
        self.progress_bar.setValue(committed)
        self.progress_label.setText(
            f"当前块: {max(0, current + 1)} / {total}    已提交: {committed}"
        )
        self.timeline.set_progress(max(0, current), total)
        self.status_label.setText(
            f"处理中 · 已处理 {event.get('processed_blocks', 0)} · "
            f"复用 {event.get('reused_blocks', 0)}"
        )

    def _on_process_completed(self, report: dict[str, Any]) -> None:
        self.timeline.set_progress(None)
        self.process_action.setEnabled(True)
        self.stop_action.setEnabled(False)
        self.apply_button.setEnabled(True)
        try:
            snapshot = self.engine.store.load() if self.engine else None
            if snapshot is not None:
                self.snapshot = snapshot
                self.intervals = list(snapshot.intervals)
                self.timeline.set_intervals(self.intervals, self.selected_id)
                self._update_status_from_snapshot(snapshot)
        except AppError as exc:
            self._show_error(exc.code, exc.message)
            return
        status = report.get("status")
        if status == "completed":
            self._show_status_message(
                f"处理完成：{report.get('processed_blocks', 0)} 块新提交，"
                f"{report.get('reused_blocks', 0)} 块复用"
            )
        else:
            self._show_status_message(
                f"已协作式中止：已提交 {report.get('committed_blocks', 0)} 块，"
                "可再次开始处理以恢复"
            )
        self.worker = None

    def _on_process_failed(self, code: str, message: str) -> None:
        self.timeline.set_progress(None)
        self.process_action.setEnabled(True)
        self.stop_action.setEnabled(False)
        self.apply_button.setEnabled(True)
        self.worker = None
        self._show_error(code, message)

    def closeEvent(self, event) -> None:  # noqa: N802
        if self.worker is not None and self.worker.isRunning():
            self.abort_event.set()
            self.worker.wait()
        event.accept()


def run_gui(session_path: str | None = None) -> int:
    app = QApplication.instance() or QApplication(sys.argv)
    app.setStyle("Fusion")
    preferred_fonts = (
        "Microsoft YaHei UI",
        "Microsoft YaHei",
        "Noto Sans CJK SC",
        "WenQuanYi Micro Hei",
        "SimSun",
    )
    available = set(QFontDatabase.families())
    for family in preferred_fonts:
        if family in available:
            app.setFont(QFont(family, 9))
            break
    app.setStyleSheet(
        """
        QMainWindow, QWidget { background: #151b23; color: #dce6f2; }
        QToolBar { background: #1d2631; border: 0; spacing: 6px; padding: 4px; }
        QToolButton { color: #dce6f2; padding: 5px 9px; border-radius: 4px; }
        QToolButton:hover { background: #2b3a4b; }
        QGroupBox { border: 1px solid #354557; border-radius: 5px; margin-top: 12px; padding-top: 8px; }
        QGroupBox::title { subcontrol-origin: margin; left: 8px; padding: 0 4px; color: #8fc5e8; }
        QLineEdit, QSpinBox, QDoubleSpinBox, QComboBox {
            background: #10161d; color: #dce6f2; border: 1px solid #354557; padding: 3px;
        }
        QPushButton { background: #2878a8; color: white; border: 0; padding: 6px 12px; border-radius: 4px; }
        QPushButton:disabled { background: #323d48; color: #768291; }
        QProgressBar { border: 1px solid #354557; text-align: center; background: #10161d; }
        QProgressBar::chunk { background: #3f9fcf; }
        QDockWidget { color: #dce6f2; }
        QScrollArea { border: 0; }
        """
    )
    window = MainWindow()
    window.show()
    if session_path:
        QTimer.singleShot(0, lambda: window.open_session_path(session_path))
    return app.exec()

