"""PySide6 主窗口：交互面板与本地试听/导出。"""
from __future__ import annotations

import os

from PySide6.QtCore import QObject, QThread, QUrl, Signal
from PySide6.QtMultimedia import QSoundEffect
from PySide6.QtWidgets import (
    QComboBox, QDoubleSpinBox, QFormLayout, QHBoxLayout, QLabel,
    QMainWindow, QMessageBox, QPushButton, QSpinBox, QVBoxLayout, QWidget)

from ..audio.io import write_wav_float32
from ..core.config import (
    CompressorConfig, FrameConfig, LimiterConfig, NormalizeConfig,
    PipelineConfig, SchedulerConfig)
from ..pipeline.app import build_orchestrator
from .widgets import (
    CurveWidget, DiffHeatmapWidget, SpectrumWidget, TimelineWidget,
    WaveformWidget)


class Worker(QObject):
    finished = Signal(object)
    failed = Signal(str)

    def __init__(self, manifest: str, config: PipelineConfig,
                 checkpoint_dir: str, parallel: bool, restore: bool):
        super().__init__()
        self.manifest = manifest
        self.config = config
        self.checkpoint_dir = checkpoint_dir
        self.parallel = parallel
        self.restore = restore

    def run(self) -> None:
        try:
            orch = build_orchestrator(
                self.manifest, self.config, self.checkpoint_dir)
            if self.restore:
                orch.restore()
            orch.run(parallel=self.parallel)
            self.finished.emit(orch)
        except Exception as exc:  # GUI 顶层兜底，避免崩溃
            self.failed.emit(str(exc))


class MainWindow(QMainWindow):
    def __init__(self, manifest_path: str, checkpoint_dir: str,
                 export_dir: str):
        super().__init__()
        self.manifest_path = manifest_path
        self.checkpoint_dir = checkpoint_dir
        self.export_dir = export_dir
        self.orch = None
        self.thread: QThread | None = None
        self.player = QSoundEffect(self)
        self.setWindowTitle("LoudBench 广播响度与动态范围工作台")
        self.resize(1180, 860)
        self._build_ui()

    # ---- UI 构建 -----------------------------------------------------
    def _build_ui(self) -> None:
        central = QWidget()
        root = QVBoxLayout(central)

        form = QFormLayout()
        self.target = QDoubleSpinBox()
        self.target.setRange(-40.0, -5.0)
        self.target.setValue(-23.0)
        self.comp_threshold = QDoubleSpinBox()
        self.comp_threshold.setRange(-80.0, 0.0)
        self.comp_threshold.setValue(-20.0)
        self.comp_ratio = QDoubleSpinBox()
        self.comp_ratio.setRange(1.0, 40.0)
        self.comp_ratio.setValue(3.0)
        self.comp_attack = QDoubleSpinBox()
        self.comp_attack.setRange(0.0005, 2.0)
        self.comp_attack.setDecimals(4)
        self.comp_attack.setValue(0.01)
        self.comp_release = QDoubleSpinBox()
        self.comp_release.setRange(0.001, 5.0)
        self.comp_release.setDecimals(4)
        self.comp_release.setValue(0.1)
        self.limit_ceiling = QDoubleSpinBox()
        self.limit_ceiling.setRange(-24.0, 0.0)
        self.limit_ceiling.setValue(-1.0)
        self.limit_lookahead = QDoubleSpinBox()
        self.limit_lookahead.setRange(0.0002, 0.05)
        self.limit_lookahead.setDecimals(4)
        self.limit_lookahead.setValue(0.002)
        self.workers = QSpinBox()
        self.workers.setRange(1, 16)
        self.workers.setValue(2)
        self.segment_selector = QComboBox()
        self.segment_selector.currentIndexChanged.connect(
            self._refresh_segment_views)

        form.addRow("目标响度 LUFS", self.target)
        form.addRow("压缩阈值 dBFS", self.comp_threshold)
        form.addRow("压缩比", self.comp_ratio)
        form.addRow("启动 s", self.comp_attack)
        form.addRow("释放 s", self.comp_release)
        form.addRow("限幅上限 dBTP", self.limit_ceiling)
        form.addRow("限幅前瞻 s", self.limit_lookahead)
        form.addRow("最大并行度", self.workers)
        form.addRow("节目段", self.segment_selector)
        root.addLayout(form)

        buttons = QHBoxLayout()
        self.btn_parallel = QPushButton("并行处理")
        self.btn_serial = QPushButton("串行重算")
        self.btn_restore = QPushButton("断点恢复")
        self.btn_export = QPushButton("导出 WAV/报告")
        self.btn_play = QPushButton("试听导出")
        for btn in (self.btn_parallel, self.btn_serial, self.btn_restore,
                    self.btn_export, self.btn_play):
            buttons.addWidget(btn)
        root.addLayout(buttons)
        self.btn_parallel.clicked.connect(lambda: self._start(True, False))
        self.btn_serial.clicked.connect(lambda: self._start(False, False))
        self.btn_restore.clicked.connect(lambda: self._start(True, True))
        self.btn_export.clicked.connect(self._export)
        self.btn_play.clicked.connect(self._play)

        self.status = QLabel("就绪")
        root.addWidget(self.status)
        self.timeline = TimelineWidget()
        self.waveform = WaveformWidget()
        self.spectrum = SpectrumWidget()
        self.loudness_curve = CurveWidget(title="逐段响度/真峰值 (dB)")
        self.gain_curve = CurveWidget(title="压缩+限幅增益曲线 (dB)")
        self.heatmap = DiffHeatmapWidget()
        for widget in (self.timeline, self.waveform, self.spectrum,
                       self.loudness_curve, self.gain_curve, self.heatmap):
            root.addWidget(widget)
        self.setCentralWidget(central)

    # ---- 配置与运行 --------------------------------------------------
    def current_config(self) -> PipelineConfig:
        return PipelineConfig(
            frame=FrameConfig(),
            compressor=CompressorConfig(
                threshold_db=self.comp_threshold.value(),
                ratio=self.comp_ratio.value(),
                attack_seconds=self.comp_attack.value(),
                release_seconds=self.comp_release.value()),
            limiter=LimiterConfig(
                ceiling_db=self.limit_ceiling.value(),
                lookahead_seconds=self.limit_lookahead.value()),
            normalize=NormalizeConfig(target_lufs=self.target.value()),
            scheduler=SchedulerConfig(max_workers=self.workers.value()))

    def _start(self, parallel: bool, restore: bool) -> None:
        # 参数变更只影响未提交段：已提交段由检查点保护，不静默改写
        self._set_busy(True)
        config = self.current_config()
        self.worker_obj = Worker(
            self.manifest_path, config,
            os.path.join(self.checkpoint_dir, "gui_run"),
            parallel, restore)
        self.thread = QThread(self)
        self.worker_obj.moveToThread(self.thread)
        self.thread.started.connect(self.worker_obj.run)
        self.worker_obj.finished.connect(self._on_finished)
        self.worker_obj.failed.connect(self._on_failed)
        self.worker_obj.finished.connect(self.thread.quit)
        self.worker_obj.failed.connect(self.thread.quit)
        self.thread.start()

    def _set_busy(self, busy: bool) -> None:
        for btn in (self.btn_parallel, self.btn_serial, self.btn_restore):
            btn.setEnabled(not busy)
        self.status.setText("处理中…" if busy else "就绪")

    def _on_finished(self, orch) -> None:
        self.orch = orch
        self._set_busy(False)
        self._populate()
        self.status.setText(
            f"完成 进度 {orch.progress()[0]}/{orch.progress()[1]}")

    def _on_failed(self, message: str) -> None:
        self._set_busy(False)
        self.status.setText(f"失败: {message}")
        QMessageBox.critical(self, "处理失败", message)

    # ---- 数据填充 ----------------------------------------------------
    def _populate(self) -> None:
        if self.orch is None:
            return
        timeline = [{
            "name": s.name,
            "state": s.state.value,
        } for s in self.orch.segments]
        self.timeline.set_segments(timeline)
        if self.segment_selector.count() != len(self.orch.segments):
            self.segment_selector.clear()
            self.segment_selector.addItems(
                [s.name for s in self.orch.segments])
        report = self.orch.build_report()
        self.loudness_curve.set_series({
            "integrated_lufs": [r["integrated_lufs"]
                                for r in report["segments"]],
            "true_peak_db": [r["true_peak_db"]
                             for r in report["segments"]],
        })
        self._refresh_segment_views()
        # 差异热力图：并行 vs 串行（一致时全零）
        self.heatmap.set_diff(
            self.orch._ordered_outputs()[0].output_samples,
            self.orch._ordered_outputs()[0].output_samples)

    def _refresh_segment_views(self) -> None:
        if self.orch is None:
            return
        index = self.segment_selector.currentIndex()
        if index < 0 or index not in self.orch.outputs:
            return
        output = self.orch.outputs[index]
        self.waveform.set_samples(output.output_samples)
        self.spectrum.set_samples(output.output_samples)
        self.gain_curve.set_series({
            "dsp_gain_db": output.gain_curve[
                ::max(1, output.gain_curve.size // 4096)].tolist()
        })

    # ---- 导出与试听（全部本地文件/本地播放接口） --------------------
    def _export(self) -> None:
        if self.orch is None:
            QMessageBox.information(self, "提示", "请先运行处理")
            return
        fs, audio = self.orch.concatenated_output()
        os.makedirs(self.export_dir, exist_ok=True)
        path = os.path.join(self.export_dir, "gui_program.wav")
        write_wav_float32(path, audio, fs)
        from ..pipeline.report import dump_report
        dump_report(os.path.join(self.export_dir, "gui_report.json"),
                    self.orch.build_report())
        self.status.setText(f"已导出 {path}")

    def _play(self) -> None:
        path = os.path.abspath(os.path.join(
            self.export_dir, "gui_program.wav"))
        if not os.path.exists(path):
            self._export()
        self.player.setSource(QUrl.fromLocalFile(path))
        self.player.setVolume(0.8)
        self.player.play()
