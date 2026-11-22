"""PySide6 desktop visualization panel."""

from __future__ import annotations

import tempfile
from pathlib import Path
from typing import Any

import numpy as np

import matplotlib

matplotlib.use("QtAgg")
from matplotlib.backends.backend_qtagg import FigureCanvasQTAgg
from matplotlib.figure import Figure
from PySide6.QtCore import QObject, QThread, QUrl, Signal, Slot
from PySide6.QtGui import QAction
from PySide6.QtWidgets import (
    QApplication,
    QComboBox,
    QDoubleSpinBox,
    QFileDialog,
    QFormLayout,
    QGroupBox,
    QHBoxLayout,
    QLabel,
    QLineEdit,
    QMainWindow,
    QMessageBox,
    QPushButton,
    QSpinBox,
    QSplitter,
    QStatusBar,
    QTableWidget,
    QTableWidgetItem,
    QVBoxLayout,
    QWidget,
)

from .audio import export_render
from .errors import ErrorCode, WorkbenchError
from .io import load_sample
from .models import AcousticSample, AnalysisConfig
from .pipeline import PipelineOutcome, ReconstructionPipeline
from .state import FrameState


class PipelineWorker(QObject):
    finished = Signal(object)
    failed = Signal(object)

    def __init__(
        self,
        pipeline: ReconstructionPipeline,
        mode: str,
    ) -> None:
        super().__init__()
        self.pipeline = pipeline
        self.mode = mode

    @Slot()
    def run(self) -> None:
        try:
            if self.mode == "realtime":
                outcome = self.pipeline.run_realtime()
            elif self.mode == "recovery":
                outcome = self.pipeline.run_recovery()
            else:
                outcome = self.pipeline.run_replay()
            self.finished.emit(outcome)
        except Exception as exc:
            self.failed.emit(exc)


class MainWindow(QMainWindow):
    def __init__(
        self,
        sample_path: str | Path,
        checkpoint_path: str | Path,
    ) -> None:
        super().__init__()
        self.setWindowTitle("空间声学与声场重建工作台")
        self.resize(1480, 920)
        self.sample_path = Path(sample_path)
        self.checkpoint_path = Path(checkpoint_path)
        self.sample: AcousticSample | None = None
        self.pipeline: ReconstructionPipeline | None = None
        self.outcome: PipelineOutcome | None = None
        self.config = AnalysisConfig()
        self._thread: QThread | None = None
        self._worker: PipelineWorker | None = None
        self._last_export_dir: Path | None = None
        self._player: Any = None
        self._audio_output: Any = None
        self._running_mode: str | None = None
        self._build_ui()
        self._load_sample(self.sample_path)

    def _build_ui(self) -> None:
        central = QWidget(self)
        layout = QVBoxLayout(central)
        controls = QHBoxLayout()

        self.sample_edit = QLineEdit(str(self.sample_path))
        open_button = QPushButton("打开样本")
        open_button.clicked.connect(self._choose_sample)
        controls.addWidget(QLabel("样本"))
        controls.addWidget(self.sample_edit, 1)
        controls.addWidget(open_button)
        layout.addLayout(controls)

        splitter = QSplitter()
        left = QWidget()
        left_layout = QVBoxLayout(left)
        left_layout.addWidget(self._build_configuration_group())
        left_layout.addWidget(self._build_actions_group())
        left_layout.addWidget(self._build_info_group())
        left_layout.addStretch(1)

        right = QWidget()
        right_layout = QVBoxLayout(right)
        self.figure = Figure(figsize=(9.6, 7.2), tight_layout=True)
        self.canvas = FigureCanvasQTAgg(self.figure)
        right_layout.addWidget(self.canvas, 3)
        self.table = QTableWidget(0, 8)
        self.table.setHorizontalHeaderLabels(
            [
                "帧",
                "状态",
                "干声起点",
                "直达延迟 FL",
                "早期能量 FL",
                "方向角",
                "置信度",
                "输出 RMS",
            ]
        )
        self.table.setAlternatingRowColors(True)
        self.table.verticalHeader().setVisible(False)
        right_layout.addWidget(self.table, 2)
        splitter.addWidget(left)
        splitter.addWidget(right)
        splitter.setStretchFactor(0, 0)
        splitter.setStretchFactor(1, 1)
        splitter.setSizes([340, 1120])
        layout.addWidget(splitter, 1)
        self.setCentralWidget(central)
        self.setStatusBar(QStatusBar(self))
        self.statusBar().showMessage("就绪")

        export_action = QAction("导出", self)
        export_action.triggered.connect(self._export)
        file_menu = self.menuBar().addMenu("文件")
        file_menu.addAction(export_action)

    def _build_configuration_group(self) -> QGroupBox:
        group = QGroupBox("分析配置（仅作用于未提交帧）")
        form = QFormLayout(group)
        self.frame_length_spin = QSpinBox()
        self.frame_length_spin.setRange(16, 262_144)
        self.frame_length_spin.setSingleStep(128)
        self.frame_length_spin.setValue(self.config.frame_length)
        self.hop_spin = QSpinBox()
        self.hop_spin.setRange(1, 262_144)
        self.hop_spin.setSingleStep(64)
        self.hop_spin.setValue(self.config.hop_length)
        self.threshold_spin = QDoubleSpinBox()
        self.threshold_spin.setRange(0.0, 1.0)
        self.threshold_spin.setDecimals(5)
        self.threshold_spin.setSingleStep(0.001)
        self.threshold_spin.setValue(self.config.energy_threshold)
        self.layout_combo = QComboBox()
        self.layout_combo.addItems(["", "mono", "stereo", "quad", "5.1", "binaural"])
        self.workers_spin = QSpinBox()
        self.workers_spin.setRange(1, 64)
        self.workers_spin.setValue(self.config.max_workers)
        self.buffer_spin = QSpinBox()
        self.buffer_spin.setRange(1, 4096)
        self.buffer_spin.setSuffix(" MiB")
        self.buffer_spin.setValue(self.config.max_buffer_bytes // (1024 * 1024))
        form.addRow("帧长", self.frame_length_spin)
        form.addRow("帧移", self.hop_spin)
        form.addRow("能量阈值", self.threshold_spin)
        form.addRow("输出布局", self.layout_combo)
        form.addRow("最大并行度", self.workers_spin)
        form.addRow("线程缓冲上限", self.buffer_spin)
        return group

    def _build_actions_group(self) -> QGroupBox:
        group = QGroupBox("运行与验证")
        layout = QVBoxLayout(group)
        first = QHBoxLayout()
        self.realtime_button = QPushButton("实时逐帧")
        self.recovery_button = QPushButton("断点恢复")
        self.replay_button = QPushButton("参数复算")
        self.realtime_button.clicked.connect(lambda: self._run("realtime"))
        self.recovery_button.clicked.connect(lambda: self._run("recovery"))
        self.replay_button.clicked.connect(lambda: self._run("replay"))
        first.addWidget(self.realtime_button)
        first.addWidget(self.recovery_button)
        first.addWidget(self.replay_button)
        layout.addLayout(first)
        second = QHBoxLayout()
        self.apply_button = QPushButton("应用配置到未提交帧")
        self.export_button = QPushButton("导出")
        self.audition_button = QPushButton("试听")
        self.cancel_button = QPushButton("取消")
        self.cancel_button.setEnabled(False)
        self.apply_button.clicked.connect(self._apply_config)
        self.export_button.clicked.connect(self._export)
        self.audition_button.clicked.connect(self._audition)
        self.cancel_button.clicked.connect(self._cancel)
        second.addWidget(self.apply_button)
        second.addWidget(self.export_button)
        second.addWidget(self.audition_button)
        second.addWidget(self.cancel_button)
        layout.addLayout(second)
        return group

    def _build_info_group(self) -> QGroupBox:
        group = QGroupBox("状态")
        layout = QVBoxLayout(group)
        self.state_label = QLabel("尚未运行")
        self.state_label.setWordWrap(True)
        self.hash_label = QLabel("")
        self.hash_label.setWordWrap(True)
        self.hash_label.setTextInteractionFlags(
            self.hash_label.textInteractionFlags()
        )
        layout.addWidget(self.state_label)
        layout.addWidget(self.hash_label)
        return group

    def _load_sample(self, path: str | Path) -> None:
        try:
            self.sample = load_sample(path)
        except WorkbenchError as exc:
            QMessageBox.critical(self, "样本错误", exc.to_dict()["message"])
            self.statusBar().showMessage(f"{exc.code.value}: {exc.message}")
            return
        self.sample_path = Path(path)
        self.sample_edit.setText(str(self.sample_path))
        channels = ", ".join(self.sample.layout.channels)
        self.statusBar().showMessage(
            f"已加载 {self.sample_path.name}: {channels}, "
            f"{self.sample.sample_rate} Hz, IR {self.sample.ir.shape[1]} 点"
        )
        self.pipeline = ReconstructionPipeline(
            self.sample,
            self.config,
            self.checkpoint_path,
        )
        self.outcome = None
        self._plot()

    def _choose_sample(self) -> None:
        path, _ = QFileDialog.getOpenFileName(
            self,
            "选择确定性样本",
            str(self.sample_path.parent),
            "NPZ sample (*.npz);;All files (*)",
        )
        if path:
            self._load_sample(path)

    def _current_config(self) -> AnalysisConfig:
        output_layout = self.layout_combo.currentText() or None
        buffer_bytes = self.buffer_spin.value() * 1024 * 1024
        return self.config.replace(
            frame_length=self.frame_length_spin.value(),
            hop_length=self.hop_spin.value(),
            energy_threshold=self.threshold_spin.value(),
            output_layout=output_layout,
            max_workers=self.workers_spin.value(),
            max_buffer_bytes=buffer_bytes,
        )

    def _apply_config(self) -> None:
        if self.sample is None:
            return
        try:
            new_config = self._current_config()
        except WorkbenchError as exc:
            QMessageBox.warning(self, "配置错误", exc.message)
            return
        frame_length_changed = new_config.frame_length != self.config.frame_length
        hop_changed = new_config.hop_length != self.config.hop_length
        if hop_changed:
            QMessageBox.warning(
                self,
                "帧移限制",
                "帧移决定时间网格；已有提交帧后不能改变帧移。"
                "仍可调整帧长、能量阈值和输出布局。",
            )
            self.hop_spin.setValue(self.config.hop_length)
            return
        if self.pipeline is None:
            self.pipeline = ReconstructionPipeline(
                self.sample,
                new_config,
                self.checkpoint_path,
            )
        else:
            try:
                self.pipeline.reconfigure_uncommitted(
                    frame_length=new_config.frame_length,
                    energy_threshold=new_config.energy_threshold,
                    output_layout=new_config.output_layout,
                )
                self.pipeline.config = new_config
            except WorkbenchError as exc:
                QMessageBox.warning(self, "配置错误", exc.message)
                return
        self.config = new_config
        suffix = "；帧长已改变，仅未提交帧会重估" if frame_length_changed else ""
        self.statusBar().showMessage(
            "配置已应用；已提交帧参数保持冻结。点击“断点恢复”处理未提交帧" + suffix
        )

    def _run(self, mode: str) -> None:
        if self.sample is None:
            return
        if self._thread is not None:
            self.statusBar().showMessage("已有任务正在运行")
            return
        try:
            self.config = self._current_config()
        except WorkbenchError as exc:
            QMessageBox.warning(self, "配置错误", exc.message)
            return
        if (
            mode != "realtime"
            and self.pipeline is not None
            and self.hop_spin.value() != self.pipeline.config.hop_length
        ):
            QMessageBox.warning(
                self,
                "帧移限制",
                "已有时间网格后不能改变帧移；请恢复原帧移再运行。",
            )
            self.hop_spin.setValue(self.pipeline.config.hop_length)
            return
        if self.pipeline is None:
            self.pipeline = ReconstructionPipeline(
                self.sample,
                self.config,
                self.checkpoint_path,
            )
        elif mode == "realtime":
            self.pipeline = ReconstructionPipeline(
                self.sample,
                self.config,
                self.checkpoint_path,
            )
        else:
            # Committed frames are restored from the checkpoint; only the
            # uncommitted suffix sees this configuration.
            self.pipeline.config = self.config
        self._running_mode = mode
        self._set_running(True)
        self.statusBar().showMessage(f"运行 {mode} ...")
        thread = QThread(self)
        worker = PipelineWorker(self.pipeline, mode)
        worker.moveToThread(thread)
        thread.started.connect(worker.run)
        worker.finished.connect(self._on_outcome)
        worker.failed.connect(self._on_worker_failure)
        worker.finished.connect(thread.quit)
        worker.failed.connect(thread.quit)
        thread.finished.connect(worker.deleteLater)
        thread.finished.connect(thread.deleteLater)
        thread.finished.connect(self._thread_finished)
        self._thread = thread
        self._worker = worker
        thread.start()

    def _set_running(self, running: bool) -> None:
        for button in (
            self.realtime_button,
            self.recovery_button,
            self.replay_button,
            self.apply_button,
            self.export_button,
            self.audition_button,
        ):
            button.setEnabled(not running)
        self.cancel_button.setEnabled(running)

    @Slot()
    def _thread_finished(self) -> None:
        self._thread = None
        self._worker = None
        self._running_mode = None
        self._set_running(False)

    @Slot()
    def _cancel(self) -> None:
        if self.pipeline is None:
            return
        self.pipeline.cancel()
        self.statusBar().showMessage("已请求取消；将停在当前帧边界并保留已提交检查点")

    @Slot(object)
    def _on_worker_failure(self, exc: object) -> None:
        self.statusBar().showMessage(f"任务失败: {exc}")
        QMessageBox.critical(self, "任务失败", str(exc))

    @Slot(object)
    def _on_outcome(self, outcome: PipelineOutcome) -> None:
        self.outcome = outcome
        if outcome.error is not None:
            self.statusBar().showMessage(
                f"{outcome.error.code.value}: {outcome.error.message}"
            )
            QMessageBox.warning(
                self,
                "运行返回错误",
                f"{outcome.error.code.value}\n{outcome.error.message}",
            )
        elif outcome.complete:
            self.statusBar().showMessage(
                f"{outcome.mode} 完成：{outcome.scheduled_frames} 帧"
            )
        else:
            self.statusBar().showMessage(f"{outcome.mode} 未完成（可断点恢复）")
        counts = outcome.state.counts() if outcome.state is not None else {}
        self.state_label.setText(
            f"模式: {outcome.mode}\n"
            f"状态: {counts}\n"
            f"检查点代次: {outcome.checkpoint_generation}\n"
            f"快照失效: {'是' if outcome.checkpoint_invalidated else '否'}"
            f"{chr(10)}已取消: {'是' if outcome.cancelled else '否'}"
        )
        if outcome.result is not None:
            self.hash_label.setText(
                f"输出哈希: {outcome.result.output_hash}\n"
                f"输出: {outcome.result.output.shape[0]} 通道 × "
                f"{outcome.result.output.shape[1]} 点"
            )
        self._refresh_table()
        self._plot()

    def _refresh_table(self) -> None:
        self.table.setRowCount(0)
        if self.outcome is None or self.outcome.result is None:
            return
        result = self.outcome.result
        state = self.outcome.state
        self.table.setRowCount(len(result.frames))
        for row, frame in enumerate(result.frames):
            stats = result.render_stats.get(frame.frame_index)
            values = [
                str(frame.frame_index),
                (
                    state.state(frame.frame_index).value
                    if state is not None
                    else "committed"
                ),
                str(frame.dry_start),
                str(frame.direct_delays_samples[0] if frame.direct_delays_samples else ""),
                f"{frame.early_reflection_energy[0]:.6g}"
                if frame.early_reflection_energy
                else "",
                f"{frame.direction_azimuth_deg:.1f}°",
                f"{frame.direction_confidence:.3f}",
                f"{stats.output_rms:.6g}" if stats is not None else "",
            ]
            for column, text in enumerate(values):
                self.table.setItem(row, column, QTableWidgetItem(text))
        self.table.resizeColumnsToContents()

    def _plot(self) -> None:
        self.figure.clear()
        if self.sample is None:
            axes = self.figure.add_subplot(111)
            axes.text(0.5, 0.5, "Load a sample", ha="center", va="center")
            self.canvas.draw_idle()
            return
        axes = self.figure.subplots(2, 2)
        self._plot_delays_energies(axes[0, 0])
        self._plot_frame_parameters(axes[0, 1])
        self._plot_direction(axes[1, 0])
        self._plot_waveforms(axes[1, 1])
        self.canvas.draw_idle()

    def _frames(self) -> list[Any]:
        if self.outcome is not None and self.outcome.result is not None:
            return list(self.outcome.result.frames)
        return []

    def _plot_delays_energies(self, axes: Any) -> None:
        frames = self._frames()
        if not frames:
            axes.set_title("Channel delay / early energy")
            axes.text(0.5, 0.5, "No reconstructed frames", ha="center", va="center")
            axes.set_axis_off()
            return
        indices = np.array([frame.frame_index for frame in frames])
        for channel_index, channel in enumerate(self.sample.layout.channels):
            delays = [
                frame.direct_delays_samples[channel_index]
                if channel_index < len(frame.direct_delays_samples)
                else 0.0
                for frame in frames
            ]
            energies = [
                frame.early_reflection_energy[channel_index]
                if channel_index < len(frame.early_reflection_energy)
                else 0.0
                for frame in frames
            ]
            axes.plot(indices, delays, label=f"{channel} delay", alpha=0.75)
            axes.plot(indices, energies, "--", label=f"{channel} early", alpha=0.65)
        axes.set_title("Channel delay (samples) / early energy")
        axes.set_xlabel("frame")
        axes.grid(True, alpha=0.25)
        axes.legend(fontsize=7, ncols=2)

    def _plot_frame_parameters(self, axes: Any) -> None:
        frames = self._frames()
        if not frames:
            axes.set_title("Frame energy parameters")
            axes.text(0.5, 0.5, "No reconstructed frames", ha="center", va="center")
            axes.set_axis_off()
            return
        indices = np.array([frame.frame_index for frame in frames])
        total = np.array([frame.frame_energy for frame in frames])
        confidence = np.array([frame.direction_confidence for frame in frames])
        axes.plot(indices, total, color="#1f77b4", label="total energy")
        axes.plot(indices, confidence, color="#ff7f0e", label="direction confidence")
        axes.set_title("Per-frame energy and direction confidence")
        axes.set_xlabel("frame")
        axes.grid(True, alpha=0.25)
        axes.legend(fontsize=8)

    def _plot_direction(self, axes: Any) -> None:
        frames = self._frames()
        if not frames:
            axes.set_title("Direction cue")
            axes.text(0.5, 0.5, "No reconstructed frames", ha="center", va="center")
            axes.set_axis_off()
            return
        x = np.array([frame.direction_x for frame in frames])
        y = np.array([frame.direction_y for frame in frames])
        scatter = axes.scatter(x, y, c=np.arange(len(frames)), cmap="viridis", s=18)
        axes.arrow(0, 0, x[-1], y[-1], color="#d62728", width=0.01, length_includes_head=True)
        axes.set_xlim(-1.1, 1.1)
        axes.set_ylim(-1.1, 1.1)
        axes.axhline(0, color="0.7", linewidth=0.8)
        axes.axvline(0, color="0.7", linewidth=0.8)
        axes.set_title("Direction vectors (latest in red)")
        axes.grid(True, alpha=0.25)
        self.figure.colorbar(scatter, ax=axes, shrink=0.75, label="frame")

    def _plot_waveforms(self, axes: Any) -> None:
        if self.sample is None:
            return
        ir = self.sample.ir[0]
        samples = min(ir.size, 8_192)
        axes.plot(
            np.arange(samples) / float(self.sample.sample_rate),
            ir[:samples],
            color="0.45",
            linewidth=0.8,
            label="IR FL",
        )
        if self.outcome is not None and self.outcome.result is not None:
            output = self.outcome.result.output
            if output.shape[0]:
                rendered = output[0]
                shown = min(rendered.size, samples)
                axes.plot(
                    np.arange(shown) / float(self.outcome.result.sample_rate),
                    rendered[:shown],
                    color="#1f77b4",
                    linewidth=0.8,
                    label="render FL",
                )
        axes.set_title("Original IR vs rendered waveform (first 8192 samples)")
        axes.set_xlabel("seconds")
        axes.grid(True, alpha=0.25)
        axes.legend(fontsize=8)

    def _export(self) -> None:
        if self.outcome is None or self.outcome.result is None:
            QMessageBox.information(self, "导出", "当前没有可导出的渲染结果。")
            return
        directory = QFileDialog.getExistingDirectory(
            self,
            "选择导出目录",
            str(self._last_export_dir or self.checkpoint_path.parent),
        )
        if not directory:
            return
        try:
            files = export_render(
                self.outcome.result,
                directory,
                basename="spatial_render",
            )
        except WorkbenchError as exc:
            QMessageBox.critical(self, "导出失败", exc.message)
            return
        self._last_export_dir = Path(directory)
        self.statusBar().showMessage(f"已导出: {files['wav']}")

    def _audition(self) -> None:
        if self.outcome is None or self.outcome.result is None:
            QMessageBox.information(self, "试听", "当前没有可试听的渲染结果。")
            return
        try:
            from PySide6.QtMultimedia import QAudioOutput, QMediaPlayer
        except Exception as exc:
            QMessageBox.warning(self, "试听不可用", f"QtMultimedia 不可用: {exc}")
            return
        directory = Path(tempfile.mkdtemp(prefix="spatial-audition-"))
        try:
            files = export_render(self.outcome.result, directory, basename="audition")
        except WorkbenchError as exc:
            QMessageBox.critical(self, "试听失败", exc.message)
            return
        if self._player is not None:
            self._player.stop()
            self._player = None
            self._audio_output = None
        self._audio_output = QAudioOutput(self)
        self._audio_output.setVolume(0.8)
        self._player = QMediaPlayer(self)
        self._player.setAudioOutput(self._audio_output)
        self._player.setSource(QUrl.fromLocalFile(files["wav"]))
        self._player.play()
        self.statusBar().showMessage("开始试听（输出设备不可用时由 Qt 静默跳过）")

    def closeEvent(self, event: Any) -> None:
        if self._thread is not None:
            self._thread.quit()
            self._thread.wait(2_000)
        super().closeEvent(event)


def run_gui(
    *,
    sample_path: str | Path,
    checkpoint_path: str | Path,
) -> int:
    app = QApplication.instance()
    owns_app = app is None
    if app is None:
        app = QApplication([])
    window = MainWindow(sample_path, checkpoint_path)
    window.show()
    if owns_app:
        return int(app.exec())
    return 0

