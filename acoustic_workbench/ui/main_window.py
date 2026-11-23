"""Desktop workbench for multichannel IR analysis and calibration."""

from __future__ import annotations

import math
import os
import threading
import traceback
from pathlib import Path
from typing import Any

import numpy as np
import pyqtgraph as pg
from PyQt5.QtCore import QThread, Qt, QUrl, pyqtSignal
from PyQt5.QtGui import QFont, QFontDatabase
from PyQt5.QtWidgets import (
    QAction,
    QApplication,
    QComboBox,
    QDoubleSpinBox,
    QFileDialog,
    QFormLayout,
    QGroupBox,
    QHBoxLayout,
    QHeaderView,
    QLabel,
    QLineEdit,
    QMainWindow,
    QMessageBox,
    QPlainTextEdit,
    QProgressBar,
    QPushButton,
    QSlider,
    QSpinBox,
    QSplitter,
    QTableWidget,
    QTableWidgetItem,
    QTabWidget,
    QVBoxLayout,
    QWidget,
)

from ..config import AnalysisConfig
from ..checkpoint import remove_sqlite_checkpoint
from ..errors import WorkbenchError
from ..io import SampleRepository
from ..io_export import export_frame_csv, export_json, export_wav, temporary_wav
from ..models import RenderConfig, Sample
from ..pipeline import AcousticPipeline, PipelineRun
from ..samplegen import generate_samples
from ..verify import (
    ThreePathVerification,
    run_injection_suite,
    run_three_path_verification,
)

try:
    from PyQt5.QtMultimedia import QSoundEffect

    AUDIO_AVAILABLE = True
except Exception:  # pragma: no cover - platform dependent
    QSoundEffect = None  # type: ignore[assignment]
    AUDIO_AVAILABLE = False


COLORS = [
    "#e4572e",
    "#17bebb",
    "#ffc914",
    "#2e86ab",
    "#8f2d56",
    "#5c8001",
]


def _register_cjk_font() -> str | None:
    """Register a CJK-capable font without requiring a network download."""

    candidates = [
        os.environ.get("ACOUSTIC_WORKBENCH_FONT"),
        str(Path(__file__).resolve().parents[2] / "assets" / "NotoSansSC-Regular.otf"),
        "C:/Windows/Fonts/msyh.ttc",
        "/mnt/c/Windows/Fonts/msyh.ttc",
        "/usr/share/fonts/opentype/noto/NotoSansCJK-Regular.ttc",
        "/usr/share/fonts/truetype/wqy/wqy-microhei.ttc",
    ]
    for candidate in candidates:
        if not candidate:
            continue
        path = Path(candidate)
        if not path.exists():
            continue
        font_id = QFontDatabase.addApplicationFont(str(path))
        if font_id >= 0:
            families = QFontDatabase.applicationFontFamilies(font_id)
            if families:
                return families[0]
    return None


class WorkbenchWorker(QThread):
    progress = pyqtSignal(object)
    completed = pyqtSignal(object)
    failed = pyqtSignal(object)

    def __init__(
        self,
        operation: str,
        sample: Sample,
        analysis_config: AnalysisConfig,
        render_config: RenderConfig,
        checkpoint_path: Path,
        *,
        repository: SampleRepository,
        workspace: Path,
        parent: QWidget | None = None,
    ) -> None:
        super().__init__(parent)
        self.operation = operation
        self.sample = sample
        self.analysis_config = analysis_config
        self.render_config = render_config
        self.checkpoint_path = checkpoint_path
        self.repository = repository
        self.workspace = workspace
        self._pipeline: AcousticPipeline | None = None
        self._cancel_event = threading.Event()

    def request_cancel(self) -> None:
        self._cancel_event.set()
        pipeline = self._pipeline
        if pipeline is not None:
            pipeline.request_cancel()

    def run(self) -> None:
        try:
            if self.operation == "verify":
                result = run_three_path_verification(
                    self.sample,
                    self.analysis_config,
                    self.render_config,
                    self.workspace / "three_path",
                    interrupt_after=max(1, 3),
                )
                self.completed.emit(result)
                return
            if self.operation == "injections":
                result = run_injection_suite(
                    self.repository,
                    self.analysis_config,
                    self.render_config,
                    self.workspace / "injections",
                )
                self.completed.emit(result)
                return
            if self.operation == "realtime":
                remove_sqlite_checkpoint(self.checkpoint_path)
            pipeline = AcousticPipeline(
                self.sample,
                self.analysis_config,
                self.render_config,
                self.checkpoint_path,
            )
            self._pipeline = pipeline
            if self._cancel_event.is_set():
                pipeline.request_cancel()
            try:
                if self.operation == "recompute":
                    result = pipeline.full_recompute_from_checkpoint()
                else:
                    result = pipeline.run(progress_callback=self.progress.emit)
            finally:
                pipeline.close()
            self.completed.emit(result)
        except WorkbenchError as exc:
            self.failed.emit(exc.to_dict())
        except Exception as exc:  # pragma: no cover - defensive UI boundary
            self.failed.emit(
                {
                    "ok": False,
                    "code": "E_INTERNAL",
                    "message": str(exc),
                    "traceback": traceback.format_exc(),
                    "chain": [],
                }
            )
        finally:
            self._pipeline = None


class MainWindow(QMainWindow):
    def __init__(
        self,
        *,
        samples_root: str | Path | None = None,
        workspace_root: str | Path | None = None,
    ) -> None:
        super().__init__()
        app = QApplication.instance()
        if app is not None:
            family = _register_cjk_font()
            if family:
                app.setFont(QFont(family, 10))
        self.project_root = Path(__file__).resolve().parents[2]
        self.samples_root = Path(samples_root or self.project_root / "samples")
        self.workspace = Path(workspace_root or self.project_root / ".workbench")
        self.workspace.mkdir(parents=True, exist_ok=True)
        if not (self.samples_root / "manifest.json").exists():
            generate_samples(self.samples_root, force=True)
        self.repository = SampleRepository(self.samples_root)
        self.current_sample: Sample | None = None
        self.current_result: PipelineRun | None = None
        self.last_verification: ThreePathVerification | None = None
        self.worker: WorkbenchWorker | None = None
        self.sound_effect = None
        self._build_ui()
        self._load_sample_list()

    def _build_ui(self) -> None:
        self.setWindowTitle("多通道声学测量与校准工作台")
        self.resize(1440, 920)
        self.setStyleSheet(
            """
            QMainWindow { background: #f5f2ea; }
            QWidget { font-family: "Segoe UI", "Microsoft YaHei"; font-size: 13px; }
            QGroupBox {
                border: 1px solid #d8d0c0; border-radius: 8px;
                margin-top: 12px; padding-top: 10px; background: #fffdf8;
                font-weight: 600;
            }
            QGroupBox::title {
                subcontrol-origin: margin; left: 12px; padding: 0 6px; color: #43505c;
            }
            QPushButton {
                background: #263238; color: white; border: 0; border-radius: 5px;
                padding: 7px 10px;
            }
            QPushButton:hover { background: #37474f; }
            QPushButton:disabled { background: #9ba7ad; }
            QPushButton#accent { background: #d1495b; }
            QPushButton#accent:hover { background: #b33949; }
            QLineEdit, QComboBox, QSpinBox, QDoubleSpinBox {
                background: white; border: 1px solid #cfc7b8; border-radius: 4px;
                padding: 4px 6px;
            }
            QProgressBar {
                border: 0; background: #e2ddd2; border-radius: 4px; text-align: center;
                min-height: 18px;
            }
            QProgressBar::chunk { background: #17bebb; border-radius: 4px; }
            QTableWidget { background: #fffdf8; border: 1px solid #ded7c9; }
            """
        )
        self._build_menu()

        central = QSplitter(Qt.Horizontal)
        central.addWidget(self._build_control_panel())
        central.addWidget(self._build_charts())
        central.setSizes([360, 1080])
        self.setCentralWidget(central)
        self.statusBar().showMessage("就绪")

    def _build_menu(self) -> None:
        file_menu = self.menuBar().addMenu("文件")
        export_wav_action = QAction("导出 WAV", self)
        export_wav_action.triggered.connect(self._export_wav)
        file_menu.addAction(export_wav_action)
        export_csv_action = QAction("导出逐帧 CSV", self)
        export_csv_action.triggered.connect(self._export_csv)
        file_menu.addAction(export_csv_action)
        export_report_action = QAction("导出报告 JSON", self)
        export_report_action.triggered.connect(self._export_report)
        file_menu.addAction(export_report_action)
        file_menu.addSeparator()
        quit_action = QAction("退出", self)
        quit_action.triggered.connect(self.close)
        file_menu.addAction(quit_action)

    def _build_control_panel(self) -> QWidget:
        panel = QWidget()
        layout = QVBoxLayout(panel)
        layout.setContentsMargins(8, 8, 8, 8)
        layout.setSpacing(8)
        title = QLabel("声学校准工作台")
        title_font = QFont("Microsoft YaHei", 18, QFont.Bold)
        title.setFont(title_font)
        title.setStyleSheet("color: #263238; padding: 4px 2px;")
        subtitle = QLabel("IR 分析 · 参数重建 · 多通道卷积 · 可复现检查点")
        subtitle.setStyleSheet("color: #6d786f; padding-bottom: 6px;")
        layout.addWidget(title)
        layout.addWidget(subtitle)

        sample_group = QGroupBox("样本")
        sample_form = QFormLayout(sample_group)
        self.sample_combo = QComboBox()
        self.sample_combo.currentIndexChanged.connect(self._sample_changed)
        sample_form.addRow("样本", self.sample_combo)
        self.sample_info = QLabel("—")
        self.sample_info.setWordWrap(True)
        self.sample_info.setStyleSheet("color: #65707b;")
        sample_form.addRow(self.sample_info)
        layout.addWidget(sample_group)

        analysis_group = QGroupBox("分析与渲染参数")
        analysis_form = QFormLayout(analysis_group)
        self.mode_combo = QComboBox()
        self.mode_combo.addItems(["auto", "calibration", "preview"])
        self.mode_combo.setCurrentText("auto")
        self.frame_spin = QSpinBox()
        self.frame_spin.setRange(64, 65536)
        self.frame_spin.setSingleStep(256)
        self.frame_spin.setValue(4096)
        self.hop_spin = QSpinBox()
        self.hop_spin.setRange(16, 65536)
        self.hop_spin.setSingleStep(256)
        self.hop_spin.setValue(1024)
        self.window_spin = QSpinBox()
        self.window_spin.setRange(16, 65536)
        self.window_spin.setSingleStep(256)
        self.window_spin.setValue(2048)
        self.early_spin = QDoubleSpinBox()
        self.early_spin.setRange(0.1, 5000.0)
        self.early_spin.setValue(20.0)
        self.early_spin.setSuffix(" ms")
        self.conv_spin = QSpinBox()
        self.conv_spin.setRange(16, 65536)
        self.conv_spin.setSingleStep(256)
        self.conv_spin.setValue(2048)
        self.block_spin = QSpinBox()
        self.block_spin.setRange(16, 1_048_576)
        self.block_spin.setSingleStep(128)
        self.block_spin.setValue(512)
        self.parallel_spin = QSpinBox()
        self.parallel_spin.setRange(1, 16)
        self.parallel_spin.setValue(2)
        self.buffer_spin = QSpinBox()
        self.buffer_spin.setRange(1, 1024)
        self.buffer_spin.setValue(16)
        self.buffer_spin.setSuffix(" MB")
        self.mapping_edit = QLineEdit()
        self.mapping_edit.setPlaceholderText("空=原序；示例 FL=FL,FR=FR,SL=SL,SR=SR")
        self.frame_spin.valueChanged.connect(self._frame_limit_changed)
        self.hop_spin.setMaximum(self.frame_spin.value())
        self.window_spin.setMaximum(self.frame_spin.value())
        analysis_form.addRow("模式", self.mode_combo)
        analysis_form.addRow("帧长", self.frame_spin)
        analysis_form.addRow("帧移", self.hop_spin)
        analysis_form.addRow("估计窗长", self.window_spin)
        analysis_form.addRow("早期反射窗", self.early_spin)
        analysis_form.addRow("卷积窗长", self.conv_spin)
        analysis_form.addRow("渲染块大小", self.block_spin)
        analysis_form.addRow("最大并行度", self.parallel_spin)
        analysis_form.addRow("每线程缓冲", self.buffer_spin)
        analysis_form.addRow("声道映射", self.mapping_edit)
        layout.addWidget(analysis_group)

        action_group = QGroupBox("执行")
        action_layout = QVBoxLayout(action_group)
        self.realtime_button = QPushButton("实时逐帧处理")
        self.realtime_button.setObjectName("accent")
        self.realtime_button.clicked.connect(lambda: self._start("realtime"))
        self.resume_button = QPushButton("断点恢复处理")
        self.resume_button.clicked.connect(lambda: self._start("resume"))
        self.recompute_button = QPushButton("全量复算（持久化参数）")
        self.recompute_button.clicked.connect(lambda: self._start("recompute"))
        self.verify_button = QPushButton("三路一致性验证")
        self.verify_button.clicked.connect(lambda: self._start("verify"))
        self.injection_button = QPushButton("执行四类异常注入")
        self.injection_button.clicked.connect(lambda: self._start("injections"))
        self.cancel_button = QPushButton("取消当前任务（仅保留已提交帧）")
        self.cancel_button.clicked.connect(self._cancel_worker)
        self.cancel_button.setEnabled(False)
        for button in (
            self.realtime_button,
            self.resume_button,
            self.recompute_button,
            self.verify_button,
            self.injection_button,
            self.cancel_button,
        ):
            action_layout.addWidget(button)
        self.progress_bar = QProgressBar()
        self.progress_bar.setRange(0, 1)
        self.progress_bar.setValue(0)
        self.progress_label = QLabel("已提交 0 / 0 帧")
        action_layout.addWidget(self.progress_bar)
        action_layout.addWidget(self.progress_label)
        playback_layout = QHBoxLayout()
        self.play_button = QPushButton("试听渲染结果")
        self.play_button.clicked.connect(self._play_result)
        playback_layout.addWidget(self.play_button)
        action_layout.addLayout(playback_layout)
        layout.addWidget(action_group)
        layout.addStretch(1)
        return panel

    def _build_charts(self) -> QWidget:
        tabs = QTabWidget()
        self.table = QTableWidget()
        self.table.setSortingEnabled(False)
        tabs.addTab(self.table, "逐帧参数")

        delay_widget = QWidget()
        delay_layout = QVBoxLayout(delay_widget)
        self.delay_plot = pg.PlotWidget()
        self.delay_plot.setTitle("逐通道直达延迟")
        self.delay_plot.setLabel("bottom", "时间", units="s")
        self.delay_plot.setLabel("left", "延迟", units="samples")
        self.delay_plot.addLegend()
        self.delay_plot.showGrid(x=True, y=True, alpha=0.2)
        delay_layout.addWidget(self.delay_plot)
        tabs.addTab(delay_widget, "通道延迟")

        energy_widget = QWidget()
        energy_layout = QVBoxLayout(energy_widget)
        self.energy_plot = pg.PlotWidget()
        self.energy_plot.setTitle("逐帧早期反射能量（log10）")
        self.energy_plot.setLabel("bottom", "时间", units="s")
        self.energy_plot.setLabel("left", "log10(energy)")
        self.energy_plot.addLegend()
        self.energy_plot.showGrid(x=True, y=True, alpha=0.2)
        self.drr_plot = pg.PlotWidget()
        self.drr_plot.setTitle("逐帧直达/混响比")
        self.drr_plot.setLabel("bottom", "时间", units="s")
        self.drr_plot.setLabel("left", "DRR", units="dB")
        self.drr_plot.addLegend()
        self.drr_plot.showGrid(x=True, y=True, alpha=0.2)
        energy_layout.addWidget(self.energy_plot)
        energy_layout.addWidget(self.drr_plot)
        tabs.addTab(energy_widget, "能量与 DRR")

        direction_widget = QWidget()
        direction_layout = QVBoxLayout(direction_widget)
        self.direction_plot = pg.PlotWidget()
        self.direction_plot.setTitle("声场方向示意")
        self.direction_plot.setAspectLocked(True)
        self.direction_plot.setXRange(-1.3, 1.3)
        self.direction_plot.setYRange(-1.3, 1.3)
        self.direction_plot.hideAxis("bottom")
        self.direction_plot.hideAxis("left")
        self.direction_plot.setMouseEnabled(x=False, y=False)
        direction_layout.addWidget(self.direction_plot, 1)
        frame_row = QHBoxLayout()
        frame_row.addWidget(QLabel("帧"))
        self.frame_slider = QSlider(Qt.Horizontal)
        self.frame_slider.setRange(0, 0)
        self.frame_slider.valueChanged.connect(self._update_direction)
        frame_row.addWidget(self.frame_slider, 1)
        self.frame_slider_label = QLabel("0")
        frame_row.addWidget(self.frame_slider_label)
        direction_layout.addLayout(frame_row)
        tabs.addTab(direction_widget, "声场方向")

        waveform_widget = QWidget()
        waveform_layout = QVBoxLayout(waveform_widget)
        self.waveform_plot = pg.PlotWidget()
        self.waveform_plot.setTitle("原 IR 与渲染输出波形")
        self.waveform_plot.setLabel("bottom", "归一化样本位置")
        self.waveform_plot.setLabel("left", "归一化幅值")
        self.waveform_plot.addLegend()
        self.waveform_plot.showGrid(x=True, y=True, alpha=0.2)
        waveform_layout.addWidget(self.waveform_plot)
        tabs.addTab(waveform_widget, "波形对比")

        log_widget = QWidget()
        log_layout = QVBoxLayout(log_widget)
        self.log_view = QPlainTextEdit()
        self.log_view.setReadOnly(True)
        self.log_view.setStyleSheet(
            "background:#1d252b;color:#d9e2e8;font-family:Consolas,monospace;"
        )
        log_layout.addWidget(self.log_view)
        tabs.addTab(log_widget, "运行日志")
        self.tabs = tabs
        return tabs

    def _load_sample_list(self) -> None:
        self.sample_combo.blockSignals(True)
        self.sample_combo.clear()
        for entry in self.repository.sample_entries():
            duration = (
                float(entry.get("truth", {}).get("ir_length", 0))
                / float(entry.get("sample_rate", 1))
            )
            self.sample_combo.addItem(
                f"{entry['id']}  ·  {entry.get('suite', '')}  ·  {duration:.2f}s",
                entry["id"],
            )
        self.sample_combo.blockSignals(False)
        if self.sample_combo.count():
            self.sample_combo.setCurrentIndex(0)
            self._sample_changed()

    def _sample_changed(self) -> None:
        sample_id = self.sample_combo.currentData()
        if not sample_id:
            return
        try:
            self.current_sample = self.repository.load(str(sample_id))
        except WorkbenchError as exc:
            self._show_error(exc.to_dict())
            return
        self.current_result = None
        sample = self.current_sample
        duration = sample.ir.shape[1] / float(sample.sample_rate or 1)
        self.sample_info.setText(
            f"{sample.layout.name if sample.layout else '—'} · "
            f"{sample.channel_count} ch · {sample.sample_rate} Hz · "
            f"IR {duration:.3f}s · dry {sample.dry_length} samples"
        )
        if sample.layout:
            self.mapping_edit.setText(
                ",".join(f"{channel_id}={channel_id}" for channel_id in sample.layout.channel_ids)
            )
        checkpoint = self.workspace / "checkpoints" / f"{sample.id}.sqlite3"
        self._checkpoint_path = checkpoint
        self._clear_charts()
        self._append_log(f"已载入 {sample.id}，输入哈希 {sample.source_hash[:16]}…")

    def _frame_limit_changed(self, value: int) -> None:
        self.hop_spin.setMaximum(value)
        self.window_spin.setMaximum(value)

    def _configs(self) -> tuple[AnalysisConfig, RenderConfig]:
        channel_map: tuple[tuple[str, str], ...] | None = None
        mapping_text = self.mapping_edit.text().strip()
        if mapping_text:
            output: list[tuple[str, str]] = []
            for part in mapping_text.replace(";", ",").split(","):
                part = part.strip()
                if not part:
                    continue
                if "=" in part:
                    out_id, in_id = part.split("=", 1)
                elif ">" in part:
                    out_id, in_id = part.split(">", 1)
                else:
                    raise WorkbenchError(
                        "E_PARA",
                        "channel map entries must use OUT=IN",
                        field="channel_map",
                        context={"entry": part},
                    )
                output.append((out_id.strip(), in_id.strip()))
            channel_map = tuple(output)
        analysis = AnalysisConfig(
            frame_length=self.frame_spin.value(),
            hop_length=self.hop_spin.value(),
            estimation_window=self.window_spin.value(),
            early_window_ms=self.early_spin.value(),
            mode=self.mode_combo.currentText(),
            max_parallelism=self.parallel_spin.value(),
            thread_buffer_mb=self.buffer_spin.value(),
        )
        render = RenderConfig(
            convolution_window=self.conv_spin.value(),
            render_block_size=self.block_spin.value(),
            channel_map=channel_map,
        )
        return analysis, render

    def _start(self, operation: str) -> None:
        if self.worker is not None and self.worker.isRunning():
            QMessageBox.information(self, "正在执行", "请等待当前任务结束。")
            return
        if self.current_sample is None:
            QMessageBox.warning(self, "缺少样本", "请先选择样本。")
            return
        try:
            analysis, render = self._configs()
        except WorkbenchError as exc:
            self._show_error(exc.to_dict())
            return
        self._set_busy(True)
        self._append_log(f"开始 {operation} …")
        self.worker = WorkbenchWorker(
            operation,
            self.current_sample,
            analysis,
            render,
            self._checkpoint_path,
            repository=self.repository,
            workspace=self.workspace,
            parent=self,
        )
        self.worker.progress.connect(self._on_progress)
        self.worker.completed.connect(self._on_completed)
        self.worker.failed.connect(self._on_failed)
        self.worker.finished.connect(lambda: self._set_busy(False))
        self.worker.start()

    def _set_busy(self, busy: bool) -> None:
        for button in (
            self.realtime_button,
            self.resume_button,
            self.recompute_button,
            self.verify_button,
            self.injection_button,
        ):
            button.setEnabled(not busy)
        self.cancel_button.setEnabled(busy)

    def _cancel_worker(self) -> None:
        if self.worker is not None and self.worker.isRunning():
            self.worker.request_cancel()
            self._append_log("已请求取消：只会保留已经持久化的提交帧。")

    def _on_progress(self, progress: dict[str, Any]) -> None:
        total = int(progress.get("total_frames", 0))
        committed = int(progress.get("committed_frames", 0))
        self.progress_bar.setMaximum(max(1, total))
        self.progress_bar.setValue(committed)
        self.progress_label.setText(f"已提交 {committed} / {total} 帧")
        decision = progress.get("decision", {})
        if decision:
            self.statusBar().showMessage(
                f"{decision.get('effective_mode')} · 窗长 {decision.get('estimation_window')} · "
                f"重叠 {decision.get('overlap_ratio', 0):.3f}"
            )

    def _on_completed(self, result: Any) -> None:
        if isinstance(result, ThreePathVerification):
            self.last_verification = result
            self._display_run(result.realtime)
            self._append_log(
                "三路验证通过：输出 SHA-256 "
                f"{result.report['realtime_vs_resume']['output_sha256']}"
            )
            self._append_log(
                f"中断点已提交 {result.report['partial_committed_frames']} 帧；"
                "断点恢复与全量复算逐样本一致。"
            )
            self.tabs.setCurrentIndex(0)
            self.statusBar().showMessage("三路一致性验证通过")
            return
        if isinstance(result, dict) and "cases" in result:
            for item in result["cases"]:
                mark = "PASS" if item["ok"] else "FAIL"
                self._append_log(
                    f"[{mark}] {item['id']}: {item['expected_code']} -> "
                    f"{item['observed_code']}"
                )
            message = "四类异常注入链路均符合预期。" if result["ok"] else "存在异常链路不匹配。"
            self._append_log(message)
            QMessageBox.information(self, "异常注入", message)
            return
        if isinstance(result, PipelineRun):
            if not result.complete:
                self._append_log(
                    f"{'已取消' if result.cancelled else '部分完成'}："
                    f"已提交 {result.committed_frames}/{result.total_frames} 帧；"
                    "不会生成完整报告。"
                )
                return
            self._display_run(result)
            self._append_log(
                f"完成：{result.total_frames} 帧，输出 {result.render_result.output.shape[1]} 样本，"
                f"峰值 {result.render_result.stats.peak:.4f}。"
            )

    def _display_run(self, result: PipelineRun) -> None:
        self.current_result = result
        if result.render_result is None:
            return
        self._populate_table(result)
        self._plot_delays(result)
        self._plot_energy(result)
        self._plot_waveforms(result)
        self.frame_slider.setRange(0, max(0, len(result.estimates) - 1))
        self.frame_slider.setValue(0)
        self._update_direction(0)
        total = result.total_frames
        self.progress_bar.setMaximum(max(1, total))
        self.progress_bar.setValue(result.committed_frames)
        self.progress_label.setText(
            f"已提交 {result.committed_frames} / {total} 帧"
        )
        self.statusBar().showMessage(
            f"{result.decision['effective_mode']} · 窗长 "
            f"{result.decision['estimation_window']} · 重叠 "
            f"{result.decision['overlap_ratio']:.3f}"
        )

    def _populate_table(self, result: PipelineRun) -> None:
        frames = result.estimates
        channels = list(frames[0].channel_ids) if frames else []
        columns = ["帧", "起点", "方向°", "置信度"]
        for channel_id in channels:
            columns.extend(
                [
                    f"{channel_id} 延迟",
                    f"{channel_id} 早期能量",
                    f"{channel_id} DRR",
                ]
            )
        self.table.clear()
        self.table.setColumnCount(len(columns))
        self.table.setHorizontalHeaderLabels(columns)
        self.table.setRowCount(len(frames))
        for row, frame in enumerate(frames):
            values: list[Any] = [
                frame.frame_index,
                frame.frame_start,
                f"{frame.direction_deg:.1f}",
                f"{frame.confidence:.3f}",
            ]
            for channel_id in channels:
                index = frame.channel_ids.index(channel_id)
                values.extend(
                    [
                        f"{frame.delay_samples[index]:.2f}",
                        f"{frame.early_energy[index]:.5g}",
                        f"{frame.drr_db[index]:.2f}",
                    ]
                )
            for column, value in enumerate(values):
                item = QTableWidgetItem(str(value))
                item.setFlags(item.flags() & ~Qt.ItemIsEditable)
                self.table.setItem(row, column, item)
        header = self.table.horizontalHeader()
        header.setSectionResizeMode(QHeaderView.ResizeToContents)
        header.setStretchLastSection(True)

    def _plot_delays(self, result: PipelineRun) -> None:
        self.delay_plot.clear()
        self.delay_plot.addLegend(offset=(10, 10))
        if not result.estimates:
            return
        sample_rate = self.current_sample.sample_rate if self.current_sample else 48_000
        x = np.asarray([frame.frame_start / sample_rate for frame in result.estimates])
        for channel_index, channel_id in enumerate(result.estimates[0].channel_ids):
            y = np.asarray(
                [frame.delay_samples[channel_index] for frame in result.estimates]
            )
            self.delay_plot.plot(
                x,
                y,
                pen=pg.mkPen(COLORS[channel_index % len(COLORS)], width=2),
                symbol="o",
                symbolSize=4,
                name=channel_id,
            )

    def _plot_energy(self, result: PipelineRun) -> None:
        self.energy_plot.clear()
        self.energy_plot.addLegend(offset=(10, 10))
        self.drr_plot.clear()
        self.drr_plot.addLegend(offset=(10, 10))
        if not result.estimates:
            return
        sample_rate = self.current_sample.sample_rate if self.current_sample else 48_000
        x = np.asarray([frame.frame_start / sample_rate for frame in result.estimates])
        for channel_index, channel_id in enumerate(result.estimates[0].channel_ids):
            early = np.asarray(
                [max(1e-18, frame.early_energy[channel_index]) for frame in result.estimates]
            )
            drr = np.asarray([frame.drr_db[channel_index] for frame in result.estimates])
            pen = pg.mkPen(COLORS[channel_index % len(COLORS)], width=2)
            self.energy_plot.plot(x, np.log10(early), pen=pen, name=channel_id)
            self.drr_plot.plot(x, drr, pen=pen, name=channel_id)

    def _clear_charts(self) -> None:
        for plot in (
            self.delay_plot,
            self.energy_plot,
            self.drr_plot,
            self.direction_plot,
            self.waveform_plot,
        ):
            plot.clear()
        self.table.clear()
        self.table.setRowCount(0)
        self.frame_slider.setRange(0, 0)

    def _plot_waveforms(self, result: PipelineRun) -> None:
        self.waveform_plot.clear()
        self.waveform_plot.addLegend(offset=(10, 10))
        if self.current_sample is None or result.render_result is None:
            return
        ir = np.asarray(self.current_sample.ir[0], dtype=np.float64)
        rendered = np.asarray(result.render_result.output[0], dtype=np.float64)
        dry = np.asarray(self.current_sample.dry, dtype=np.float64)
        for values, name, color in (
            (ir, "原 IR (ch0)", "#263238"),
            (rendered, "渲染输出 (ch0)", "#d1495b"),
            (dry, "干声", "#17bebb"),
        ):
            peak = float(np.max(np.abs(values))) if values.size else 0.0
            normalized = values / peak if peak > 0 else values
            x = np.linspace(0.0, 1.0, normalized.size, endpoint=False)
            self.waveform_plot.plot(
                x,
                normalized,
                pen=pg.mkPen(color, width=1.2),
                name=name,
            )

    def _update_direction(self, frame_index: int) -> None:
        self.frame_slider_label.setText(str(frame_index))
        self.direction_plot.clear()
        self.direction_plot.setAspectLocked(True)
        self.direction_plot.setXRange(-1.3, 1.3)
        self.direction_plot.setYRange(-1.3, 1.3)
        self.direction_plot.hideAxis("bottom")
        self.direction_plot.hideAxis("left")
        circle = np.linspace(0.0, 2.0 * math.pi, 240)
        self.direction_plot.plot(
            np.cos(circle), np.sin(circle), pen=pg.mkPen("#c9c0b0", width=1)
        )
        self.direction_plot.plot(
            [-1.05, 1.05], [0.0, 0.0], pen=pg.mkPen("#e6dfd2", width=1)
        )
        self.direction_plot.plot(
            [0.0, 0.0], [-1.05, 1.05], pen=pg.mkPen("#e6dfd2", width=1)
        )
        self.direction_plot.plot([0.0, 0.0], [0.0, 0.0], pen=None)
        if self.current_sample and self.current_sample.layout:
            for channel in self.current_sample.layout.channels:
                angle = math.radians(channel.azimuth_deg)
                x = 1.12 * math.cos(angle)
                y = 1.12 * math.sin(angle)
                text = pg.TextItem(channel.id, color="#455a64", anchor=(0.5, 0.5))
                text.setPos(x, y)
                self.direction_plot.addItem(text)
                self.direction_plot.plot(
                    [0.0, math.cos(angle)],
                    [0.0, math.sin(angle)],
                    pen=pg.mkPen("#d8d0c0", width=1, style=Qt.DotLine),
                )
        if self.current_result and self.current_result.estimates:
            frames = self.current_result.estimates
            index = min(max(0, frame_index), len(frames) - 1)
            points = []
            for frame in frames:
                angle = math.radians(frame.direction_deg)
                radius = max(0.08, frame.confidence)
                points.append(
                    (radius * math.cos(angle), radius * math.sin(angle))
                )
            if points:
                xs, ys = zip(*points, strict=True)
                self.direction_plot.plot(
                    xs,
                    ys,
                    pen=None,
                    symbol="o",
                    symbolSize=5,
                    symbolBrush=pg.mkBrush(100, 100, 100, 80),
                    symbolPen=None,
                )
            frame = frames[index]
            angle = math.radians(frame.direction_deg)
            radius = max(0.12, frame.confidence)
            self.direction_plot.plot(
                [0.0, radius * math.cos(angle)],
                [0.0, radius * math.sin(angle)],
                pen=pg.mkPen("#d1495b", width=4),
            )
            dot = pg.ScatterPlotItem(
                [radius * math.cos(angle)],
                [radius * math.sin(angle)],
                size=13,
                brush=pg.mkBrush("#d1495b"),
                pen=pg.mkPen("#ffffff"),
            )
            self.direction_plot.addItem(dot)
            self.direction_plot.setTitle(
                f"声场方向 · 帧 {frame.frame_index} · "
                f"{frame.direction_deg:.1f}° · 宽度 {frame.direction_width_deg:.1f}°"
            )

    def _play_result(self) -> None:
        if not self.current_result or self.current_result.render_result is None:
            QMessageBox.information(self, "无结果", "请先完成渲染。")
            return
        if not AUDIO_AVAILABLE:
            QMessageBox.warning(
                self,
                "无法试听",
                "当前 PyQt5 安装缺少 QtMultimedia；可先导出 WAV 后使用系统播放器。",
            )
            return
        try:
            audition_dir = self.workspace / "audition"
            wav_path = temporary_wav(
                self.current_result.render_result,
                self.current_sample.sample_rate or 48_000,
                audition_dir,
            )
            if self.sound_effect is None:
                self.sound_effect = QSoundEffect(self)
            self.sound_effect.setSource(QUrl.fromLocalFile(str(wav_path)))
            self.sound_effect.setVolume(0.8)
            self.sound_effect.play()
            self._append_log(f"试听文件：{wav_path}")
        except Exception as exc:
            QMessageBox.warning(self, "试听失败", str(exc))

    def _export_wav(self) -> None:
        if not self.current_result or self.current_result.render_result is None:
            QMessageBox.information(self, "无结果", "请先完成渲染。")
            return
        path, _ = QFileDialog.getSaveFileName(self, "导出 WAV", "", "WAV (*.wav)")
        if not path:
            return
        try:
            export_wav(path, self.current_result.render_result, self.current_sample.sample_rate or 48_000)
            self._append_log(f"已导出 {path}")
        except WorkbenchError as exc:
            self._show_error(exc.to_dict())

    def _export_csv(self) -> None:
        if not self.current_result:
            QMessageBox.information(self, "无结果", "请先完成处理。")
            return
        path, _ = QFileDialog.getSaveFileName(self, "导出逐帧参数", "", "CSV (*.csv)")
        if not path:
            return
        try:
            export_frame_csv(
                path,
                self.current_result.estimates,
                channel_ids=self.current_result.estimates[0].channel_ids
                if self.current_result.estimates
                else [],
            )
            self._append_log(f"已导出 {path}")
        except WorkbenchError as exc:
            self._show_error(exc.to_dict())

    def _export_report(self) -> None:
        if not self.current_result:
            QMessageBox.information(self, "无结果", "请先完成处理。")
            return
        path, _ = QFileDialog.getSaveFileName(self, "导出报告", "", "JSON (*.json)")
        if not path:
            return
        payload = {
            "sample_id": self.current_sample.id if self.current_sample else None,
            "input_hash": self.current_result.input_hash,
            "complete": self.current_result.complete,
            "decision": self.current_result.decision,
            "progress": self.current_result.progress,
            "frames": [frame.to_dict() for frame in self.current_result.estimates],
            "render_stats": self.current_result.render_result.stats.to_dict()
            if self.current_result.render_result
            else None,
        }
        export_json(path, payload)
        self._append_log(f"已导出 {path}")

    def _on_failed(self, payload: dict[str, Any]) -> None:
        self._show_error(payload)

    def _show_error(self, payload: dict[str, Any]) -> None:
        code = payload.get("code", "E_INTERNAL")
        message = payload.get("message", "未知错误")
        location = []
        if payload.get("field"):
            location.append(f"字段 {payload['field']}")
        if payload.get("frame_index") is not None:
            location.append(f"帧 {payload['frame_index']}")
        if payload.get("channel"):
            location.append(f"通道 {payload['channel']}")
        suffix = f" ({', '.join(location)})" if location else ""
        self._append_log(f"[ERROR] {code}: {message}{suffix}")
        for link in payload.get("chain", []):
            self._append_log(
                f"  ↳ {link.get('stage')} · {link.get('code')} · {link.get('message')}"
            )
        if payload.get("traceback"):
            self._append_log(payload["traceback"])
        QMessageBox.critical(self, code, f"{code}: {message}{suffix}")

    def _append_log(self, text: str) -> None:
        self.log_view.appendPlainText(text)
        self.log_view.verticalScrollBar().setValue(
            self.log_view.verticalScrollBar().maximum()
        )

    def closeEvent(self, event: Any) -> None:
        if self.worker is not None and self.worker.isRunning():
            event.ignore()
            QMessageBox.information(self, "任务运行中", "请等待当前任务结束后再关闭。")
            return
        event.accept()


def create_window(
    *,
    samples_root: str | Path | None = None,
    workspace_root: str | Path | None = None,
) -> MainWindow:
    return MainWindow(samples_root=samples_root, workspace_root=workspace_root)


def run_app(argv: list[str] | None = None) -> int:
    app = QApplication.instance() or QApplication(argv or [])
    pg.setConfigOptions(antialias=True, background="#fffdf8", foreground="#35424a")
    window = create_window()
    window.show()
    return app.exec_()
