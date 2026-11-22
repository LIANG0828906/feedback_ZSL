"""PyQt5 diagnostics window for samples, masks and block-stage state."""

from __future__ import annotations

import json
import sys
from pathlib import Path
from typing import Any, Dict, Mapping, Optional

import cv2
import numpy as np

from .config import STAGES, STAGE_LABELS, PipelineConfig
from .diff import difference_heatmap
from .engine import CancellationToken, PipelineEngine, PipelineResult
from .errors import EdgePipeError, ErrorCode
from .ops import BlockGrid, run_whole_pipeline
from .samples import (
    decode_image,
    find_sample,
    generate_samples,
    load_manifest,
    sample_image_path,
    truth_from_sample,
)
from .states import COMMITTED, FAILED, PENDING, PROCESSING

try:
    from PyQt5.QtCore import QThread, Qt, pyqtSignal
    from PyQt5.QtGui import QColor, QImage, QPixmap
    from PyQt5.QtWidgets import (
        QApplication,
        QCheckBox,
        QComboBox,
        QFileDialog,
        QFormLayout,
        QGridLayout,
        QGroupBox,
        QHBoxLayout,
        QLabel,
        QLineEdit,
        QMainWindow,
        QMessageBox,
        QPushButton,
        QProgressBar,
        QScrollArea,
        QSpinBox,
        QSplitter,
        QTableWidget,
        QTableWidgetItem,
        QTabWidget,
        QTextEdit,
        QVBoxLayout,
        QWidget,
    )
except ImportError:  # pragma: no cover - exercised only without PyQt5
    QApplication = None  # type: ignore[assignment]
    QMainWindow = object  # type: ignore[assignment]
    QThread = object  # type: ignore[assignment]
    pyqtSignal = None  # type: ignore[assignment]


if QThread is not object:

    class PipelineWorker(QThread):
        progress = pyqtSignal(dict)
        completed = pyqtSignal(object)
        failed = pyqtSignal(dict)

        def __init__(
            self,
            engine: PipelineEngine,
            image: np.ndarray,
            truth: Mapping[str, Any],
            mode: str,
            cancel_token: CancellationToken,
        ) -> None:
            super().__init__()
            self.engine = engine
            self.image = image
            self.truth = truth
            self.mode = mode
            self.cancel_token = cancel_token

        def run(self) -> None:
            try:
                result = self.engine.run(
                    self.image,
                    truth=self.truth,
                    mode=self.mode,
                    cancel_token=self.cancel_token,
                    progress_callback=self.progress.emit,
                )
                self.completed.emit(result)
            except EdgePipeError as exc:
                self.failed.emit(exc.to_dict())
            except Exception as exc:  # pragma: no cover - defensive UI boundary
                self.failed.emit(
                    {
                        "code": ErrorCode.INTERNAL,
                        "message": f"{type(exc).__name__}: {exc}",
                        "field": None,
                        "details": {},
                    }
                )


def _qimage_from_array(array: np.ndarray) -> "QImage":
    if array.ndim == 2:
        data = np.ascontiguousarray(array)
        return QImage(
            data.data,
            data.shape[1],
            data.shape[0],
            data.strides[0],
            QImage.Format_Grayscale8,
        ).copy()
    if array.ndim == 3 and array.shape[2] == 1:
        return _qimage_from_array(array[:, :, 0])
    rgb = cv2.cvtColor(np.ascontiguousarray(array), cv2.COLOR_BGR2RGB)
    return QImage(
        rgb.data,
        rgb.shape[1],
        rgb.shape[0],
        rgb.strides[0],
        QImage.Format_RGB888,
    ).copy()


class MaskPanel(QWidget):
    def __init__(self, title: str) -> None:
        super().__init__()
        layout = QVBoxLayout(self)
        layout.setContentsMargins(4, 4, 4, 4)
        self.title = QLabel(title)
        self.title.setAlignment(Qt.AlignCenter)
        self.image = QLabel("尚无结果")
        self.image.setAlignment(Qt.AlignCenter)
        self.image.setMinimumSize(260, 190)
        self.image.setStyleSheet("background:#20242b; color:#aeb6c2;")
        layout.addWidget(self.title)
        layout.addWidget(self.image, 1)
        self._array: Optional[np.ndarray] = None

    def set_array(self, array: Optional[np.ndarray]) -> None:
        self._array = array
        if array is None:
            self.image.setPixmap(QPixmap())
            self.image.setText("尚无结果")
            return
        pixmap = QPixmap.fromImage(_qimage_from_array(array))
        self.image.setPixmap(
            pixmap.scaled(
                max(260, self.image.width()),
                max(190, self.image.height()),
                Qt.KeepAspectRatio,
                Qt.SmoothTransformation,
            )
        )

    def resizeEvent(self, event: Any) -> None:
        super().resizeEvent(event)
        if self._array is not None:
            self.set_array(self._array)


class MainWindow(QMainWindow):
    def __init__(
        self,
        *,
        manifest_path: str | Path = "samples/index.json",
        checkpoint_root: str | Path = "checkpoints/gui",
    ) -> None:
        if QApplication is None:
            raise RuntimeError("PyQt5 未安装")
        super().__init__()
        self.setWindowTitle("EdgePipe 边缘检测与形态学诊断")
        self.resize(1440, 920)
        self.manifest_path = Path(manifest_path)
        self.checkpoint_root = Path(checkpoint_root)
        self.manifest: Dict[str, Any] = {}
        self.current_sample: Optional[Dict[str, Any]] = None
        self.current_image: Optional[np.ndarray] = None
        self.current_truth: Optional[Dict[str, Any]] = None
        self.result: Optional[PipelineResult] = None
        self.worker: Optional[PipelineWorker] = None
        self.cancel_token: Optional[CancellationToken] = None
        self.selected_block_id: Optional[str] = None
        self.selected_stage: Optional[str] = None
        self._build_ui()
        self._load_samples()

    def _build_ui(self) -> None:
        central = QWidget()
        self.setCentralWidget(central)
        outer = QHBoxLayout(central)
        splitter = QSplitter(Qt.Horizontal)
        outer.addWidget(splitter)

        controls = QWidget()
        controls_layout = QVBoxLayout(controls)
        splitter.addWidget(controls)
        splitter.setStretchFactor(0, 0)
        splitter.setStretchFactor(1, 1)

        sample_box = QGroupBox("样本")
        sample_form = QFormLayout(sample_box)
        self.sample_combo = QComboBox()
        self.sample_combo.currentIndexChanged.connect(self._sample_changed)
        sample_form.addRow("样本", self.sample_combo)
        self.manifest_edit = QLineEdit(str(self.manifest_path))
        browse = QPushButton("选择清单")
        browse.clicked.connect(self._browse_manifest)
        sample_form.addRow(self.manifest_edit, browse)
        reload_button = QPushButton("重新加载")
        reload_button.clicked.connect(self._load_samples)
        generate_button = QPushButton("生成确定性样本")
        generate_button.clicked.connect(self._generate_samples)
        sample_form.addRow(reload_button, generate_button)
        controls_layout.addWidget(sample_box)

        parameter_box = QGroupBox("参数（修改后需全量重算）")
        self.parameter_box = parameter_box
        parameter_form = QFormLayout(parameter_box)
        self.operator_combo = QComboBox()
        self.operator_combo.addItems(["sobel", "scharr", "prewitt"])
        self.low_spin = self._spin(0, 4096, 20)
        self.high_spin = self._spin(0, 4096, 45)
        self.open_spin = self._odd_spin(1, 31, 1)
        self.close_spin = self._odd_spin(1, 31, 3)
        self.thin_spin = self._spin(0, 16, 4)
        self.prune_spin = self._spin(0, 128, 8)
        self.open_enabled = QCheckBox("启用开运算")
        self.open_enabled.setChecked(True)
        self.close_enabled = QCheckBox("启用闭运算")
        self.close_enabled.setChecked(True)
        self.thin_enabled = QCheckBox("启用细化")
        self.thin_enabled.setChecked(True)
        self.prune_enabled = QCheckBox("启用剪枝")
        self.prune_enabled.setChecked(True)
        self.block_spin = self._spin(16, 512, 48)
        self.block_spin.valueChanged.connect(
            lambda _value: self._render_state_matrix(self.result)
        )
        self.worker_spin = self._spin(1, 4, 4)
        self.buffer_spin = self._spin(1, 1024, 64)
        parameter_form.addRow("梯度算子", self.operator_combo)
        parameter_form.addRow("低阈值", self.low_spin)
        parameter_form.addRow("高阈值", self.high_spin)
        parameter_form.addRow("开运算核", self.open_spin)
        parameter_form.addRow(self.open_enabled)
        parameter_form.addRow("闭运算核", self.close_spin)
        parameter_form.addRow(self.close_enabled)
        parameter_form.addRow("细化迭代", self.thin_spin)
        parameter_form.addRow(self.thin_enabled)
        parameter_form.addRow("剪枝长度", self.prune_spin)
        parameter_form.addRow(self.prune_enabled)
        parameter_form.addRow("分块尺寸", self.block_spin)
        parameter_form.addRow("最大并行度", self.worker_spin)
        parameter_form.addRow("单线程缓冲(MB)", self.buffer_spin)
        controls_layout.addWidget(parameter_box)

        run_box = QGroupBox("执行")
        run_layout = QVBoxLayout(run_box)
        self.mode_combo = QComboBox()
        self.mode_combo.addItems(["full", "resume", "incremental"])
        self.mode_combo.currentTextChanged.connect(self._mode_changed)
        run_layout.addWidget(self.mode_combo)
        self.run_button = QPushButton("开始 / 全量重算")
        self.run_button.clicked.connect(lambda: self._run_pipeline(self.mode_combo.currentText()))
        self.pause_button = QPushButton("暂停（保留已提交）")
        self.pause_button.clicked.connect(self._pause)
        self.retry_button = QPushButton("重试所选失败块")
        self.retry_button.clicked.connect(self._retry_selected)
        run_layout.addWidget(self.run_button)
        run_layout.addWidget(self.pause_button)
        run_layout.addWidget(self.retry_button)
        self.progress = QProgressBar()
        self.progress.setFormat("%v / %m 分块阶段")
        run_layout.addWidget(self.progress)
        self.progress_label = QLabel("就绪")
        self.progress_label.setWordWrap(True)
        run_layout.addWidget(self.progress_label)
        controls_layout.addWidget(run_box)
        controls_layout.addStretch(1)

        tabs = QTabWidget()
        splitter.addWidget(tabs)

        image_tab = QWidget()
        image_layout = QGridLayout(image_tab)
        self.mask_panels: Dict[str, MaskPanel] = {}
        panels = [("original", "原图")] + [
            (stage, STAGE_LABELS[stage]) for stage in STAGES
        ] + [("diff", "分块 vs 整图差异热力图")]
        for index, (key, title) in enumerate(panels):
            panel = MaskPanel(title)
            self.mask_panels[key] = panel
            image_layout.addWidget(panel, index // 3, index % 3)
        image_scroll = QScrollArea()
        image_scroll.setWidgetResizable(True)
        image_scroll.setWidget(image_tab)
        tabs.addTab(image_scroll, "图像 / 掩膜")

        state_tab = QWidget()
        state_layout = QVBoxLayout(state_tab)
        self.selected_label = QLabel("点击单元格查看分块阶段状态")
        self.selected_label.setWordWrap(True)
        state_layout.addWidget(self.selected_label)
        self.state_table = QTableWidget()
        self.state_table.setEditTriggers(QTableWidget.NoEditTriggers)
        self.state_table.cellClicked.connect(self._cell_clicked)
        state_layout.addWidget(self.state_table, 1)
        tabs.addTab(state_tab, "分块阶段状态")

        geometry_tab = QWidget()
        geometry_layout = QVBoxLayout(geometry_tab)
        self.geometry_text = QTextEdit()
        self.geometry_text.setReadOnly(True)
        self.geometry_text.setPlainText("运行完成后显示几何真值统计。")
        geometry_layout.addWidget(self.geometry_text, 2)
        self.log_text = QTextEdit()
        self.log_text.setReadOnly(True)
        self.log_text.setMaximumHeight(190)
        geometry_layout.addWidget(self.log_text, 1)
        tabs.addTab(geometry_tab, "几何统计 / 日志")

    def _spin(self, minimum: int, maximum: int, value: int) -> QSpinBox:
        spin = QSpinBox()
        spin.setRange(minimum, maximum)
        spin.setValue(value)
        return spin

    def _odd_spin(self, minimum: int, maximum: int, value: int) -> QSpinBox:
        spin = QSpinBox()
        spin.setRange(minimum, maximum)
        spin.setSingleStep(2)
        spin.setValue(value)
        return spin

    def _load_samples(self) -> None:
        self.manifest_path = Path(self.manifest_edit.text()) if hasattr(
            self, "manifest_edit"
        ) else self.manifest_path
        try:
            if not self.manifest_path.exists() and self.manifest_path.parent.exists():
                generate_samples(self.manifest_path.parent, clean=False)
            self.manifest = load_manifest(self.manifest_path)
        except EdgePipeError as exc:
            self._log(f"加载样本失败: {exc}")
            return
        self.sample_combo.blockSignals(True)
        self.sample_combo.clear()
        for sample in self.manifest.get("samples", []):
            self.sample_combo.addItem(sample["name"])
        self.sample_combo.blockSignals(False)
        if self.sample_combo.count():
            self.sample_combo.setCurrentIndex(0)
            self._sample_changed(0)

    def _generate_samples(self) -> None:
        target = self.manifest_path.parent if self.manifest_path.suffix else self.manifest_path
        try:
            generate_samples(target, clean=False)
            self.manifest_edit.setText(str(Path(target) / "index.json"))
            self._load_samples()
            self._log(f"已生成样本: {target}")
        except Exception as exc:
            self._show_error("生成样本失败", str(exc))

    def _browse_manifest(self) -> None:
        path, _ = QFileDialog.getOpenFileName(
            self, "选择样本清单", str(self.manifest_path.parent), "JSON (*.json)"
        )
        if path:
            self.manifest_edit.setText(path)
            self._load_samples()

    def _sample_changed(self, index: int) -> None:
        if not self.manifest or index < 0:
            return
        sample = dict(self.manifest["samples"][index])
        self.current_sample = sample
        try:
            self.current_image = decode_image(
                sample_image_path(sample, self.manifest_path.parent)
            )
            self.current_truth = truth_from_sample(sample)
        except EdgePipeError as exc:
            self._show_error("样本读取失败", str(exc))
            return
        self.mask_panels["original"].set_array(self.current_image)
        for stage in STAGES:
            self.mask_panels[stage].set_array(None)
        self.mask_panels["diff"].set_array(None)
        recommended = sample.get("recommended_config", {})
        operator = str(recommended.get("gradient_operator", "sobel"))
        index = self.operator_combo.findText(operator)
        if index >= 0:
            self.operator_combo.setCurrentIndex(index)
        self.low_spin.setValue(int(float(recommended.get("low_threshold", 20))))
        self.high_spin.setValue(int(float(recommended.get("high_threshold", 45))))
        self.open_spin.setValue(int(recommended.get("open_kernel_size", 1)))
        self.close_spin.setValue(int(recommended.get("close_kernel_size", 3)))
        self.thin_spin.setValue(int(recommended.get("thinning_iterations", 4)))
        self.prune_spin.setValue(int(recommended.get("prune_length", 8)))
        self.block_spin.setValue(int(recommended.get("block_size", 48)))
        self._render_state_matrix(None)
        self.geometry_text.setPlainText("尚未运行。")
        self._log(f"已加载样本 {sample['name']}，shape={self.current_image.shape}")

    def _config(self) -> PipelineConfig:
        return PipelineConfig(
            gradient_operator=self.operator_combo.currentText(),
            low_threshold=float(self.low_spin.value()),
            high_threshold=float(self.high_spin.value()),
            open_enabled=self.open_enabled.isChecked(),
            open_kernel_size=self.open_spin.value(),
            close_enabled=self.close_enabled.isChecked(),
            close_kernel_size=self.close_spin.value(),
            thin_enabled=self.thin_enabled.isChecked(),
            thinning_iterations=self.thin_spin.value(),
            prune_enabled=self.prune_enabled.isChecked(),
            prune_length=self.prune_spin.value(),
            block_size=self.block_spin.value(),
            max_workers=self.worker_spin.value(),
            thread_buffer_bytes=self.buffer_spin.value() * 1024 * 1024,
        )

    def _run_pipeline(self, mode: str) -> None:
        if self.worker is not None and self.worker.isRunning():
            self._log("已有任务在运行。")
            return
        if self.current_sample is None or self.current_image is None:
            return
        try:
            config = self._config()
        except EdgePipeError as exc:
            self._show_error("参数错误", json.dumps(exc.to_dict(), ensure_ascii=False))
            return
        grid = BlockGrid.create(
            self.current_image.shape[0],
            self.current_image.shape[1],
            config.block_size,
        )
        self.progress.setRange(0, len(grid.blocks) * len(STAGES))
        self.progress.setValue(0)
        self.cancel_token = CancellationToken()
        checkpoint_dir = self.checkpoint_root / str(self.current_sample["name"])
        engine = PipelineEngine(config, checkpoint_dir)
        self.worker = PipelineWorker(
            engine,
            self.current_image,
            self.current_truth or {},
            mode,
            self.cancel_token,
        )
        self.worker.progress.connect(self._on_progress)
        self.worker.completed.connect(self._on_completed)
        self.worker.failed.connect(self._on_failed)
        self.worker.finished.connect(self._on_worker_finished)
        self.run_button.setEnabled(False)
        self.sample_combo.setEnabled(False)
        self.mode_combo.setEnabled(False)
        self.parameter_box.setEnabled(False)
        self.pause_button.setEnabled(True)
        self.retry_button.setEnabled(False)
        self.progress_label.setText(f"运行中：{mode}")
        self._log(f"启动 {mode}，检查点 {checkpoint_dir}")
        self.worker.start()

    def _pause(self) -> None:
        if self.cancel_token is not None:
            self.cancel_token.cancel()
            self.progress_label.setText("已请求暂停：等待当前批次结束，仅保留已提交结果")
            self._log("暂停请求已发送。")

    def _retry_selected(self) -> None:
        if self.result is None:
            return
        if self.selected_block_id is None or self.selected_stage is None:
            self._log("请先在状态矩阵选择一个分块阶段。")
            return
        state = self.result.states["states"][int(self.selected_block_id[1:])][
            STAGES.index(self.selected_stage)
        ]
        if state not in (FAILED, PENDING):
            self._log(f"{self.selected_block_id}/{self.selected_stage} 无需重试")
            return
        self._run_pipeline("resume")

    def _cell_clicked(self, row: int, column: int) -> None:
        self.selected_block_id = f"b{row:04d}"
        self.selected_stage = STAGES[column]
        state = "unknown"
        attempts = "-"
        error = "-"
        if self.result is not None:
            task = self.result.states["states"][row][column]
            state = task
            for item in self.result.states.get("tasks", []):
                if (
                    item["block_id"] == self.selected_block_id
                    and item["stage"] == self.selected_stage
                ):
                    attempts = item["attempts"]
                    error = json.dumps(item.get("error"), ensure_ascii=False)
                    break
        self.selected_label.setText(
            f"分块 {self.selected_block_id} / 阶段 {self.selected_stage}："
            f"{state}，attempts={attempts}，error={error}"
        )

    def _on_progress(self, progress: Dict[str, Any]) -> None:
        committed = int(progress.get("committed", 0))
        self.progress.setValue(committed)
        self.progress_label.setText(
            f"已提交 {committed}/{progress.get('total', 0)} "
            f"({progress.get('fraction', 0.0):.1%})，"
            f"当前 {progress.get('block_id', '')}/{progress.get('stage', '')}"
        )

    def _on_completed(self, result: PipelineResult) -> None:
        self.result = result
        for stage in STAGES:
            self.mask_panels[stage].set_array(result.stage_masks.get(stage))
        self._render_state_matrix(result)
        if result.geometry is not None:
            self.geometry_text.setPlainText(
                json.dumps(result.geometry, ensure_ascii=False, indent=2)
            )
        else:
            self.geometry_text.setPlainText("管线未完整结束，暂无完整几何统计。")
        self._render_reference_diff(result)
        self.progress.setRange(0, int(result.progress["total"]))
        self.progress.setValue(result.progress["committed"])
        status = "完成" if result.complete else "未完成（可恢复）"
        self.progress_label.setText(
            f"{status}：{result.progress['committed']}/{result.progress['total']}，"
            f"复用 {result.restored_commits}，新提交 {result.newly_committed}"
        )
        self._log(
            f"{status}，mode={result.mode}，failed={len(result.failed_tasks)}，"
            f"elapsed={result.elapsed_seconds:.3f}s"
        )
        if result.invalidated_reason:
            self._log(f"检查点已整体作废：{result.invalidated_reason}")

    def _render_reference_diff(self, result: PipelineResult) -> None:
        if result.final_mask is None or self.current_image is None:
            return
        try:
            config = result.config
            whole, _ = run_whole_pipeline(self.current_image, config)
            heat = difference_heatmap(result.final_mask, whole)
            self.mask_panels["diff"].set_array(heat)
            diff_pixels = int(np.count_nonzero(result.final_mask != whole))
            self._log(f"分块最终掩膜与整图参考差异像素：{diff_pixels}")
        except Exception as exc:
            self._log(f"整图参考比对失败：{exc}")

    def _render_state_matrix(self, result: Optional[PipelineResult]) -> None:
        if self.current_image is None:
            return
        block_size = self.block_spin.value()
        grid = BlockGrid.create(
            self.current_image.shape[0],
            self.current_image.shape[1],
            block_size,
        )
        if result is None:
            states = [[PENDING for _ in STAGES] for _ in grid.blocks]
            tasks = []
        else:
            states = result.states["states"]
            tasks = result.states.get("tasks", [])
        self.state_table.clear()
        self.state_table.setRowCount(len(states))
        self.state_table.setColumnCount(len(STAGES))
        self.state_table.setHorizontalHeaderLabels(
            [STAGE_LABELS[stage] for stage in STAGES]
        )
        self.state_table.setVerticalHeaderLabels(
            [f"b{index:04d}" for index in range(len(states))]
        )
        colors = {
            PENDING: QColor("#5b6270"),
            PROCESSING: QColor("#c49b28"),
            COMMITTED: QColor("#2d7d46"),
            FAILED: QColor("#a33b3b"),
        }
        for row, row_states in enumerate(states):
            for col, state in enumerate(row_states):
                item = QTableWidgetItem(state)
                item.setBackground(colors.get(state, QColor("#333333")))
                item.setTextAlignment(Qt.AlignCenter)
                self.state_table.setItem(row, col, item)
        self.state_table.resizeColumnsToContents()

    def _on_failed(self, error: Dict[str, Any]) -> None:
        self._log(f"任务失败: {json.dumps(error, ensure_ascii=False)}")
        self._show_error(
            f"任务失败 {error.get('code', '')}",
            error.get("message", "") + f"\nfield={error.get('field')}",
        )

    def _on_worker_finished(self) -> None:
        self.run_button.setEnabled(True)
        self.sample_combo.setEnabled(True)
        self.mode_combo.setEnabled(True)
        self.parameter_box.setEnabled(True)
        self.pause_button.setEnabled(False)
        self.retry_button.setEnabled(True)

    def _mode_changed(self, mode: str) -> None:
        self.run_button.setText(
            "开始 / 全量重算" if mode == "full" else f"开始 {mode}"
        )

    def _show_error(self, title: str, message: str) -> None:
        QMessageBox.critical(self, title, message)

    def _log(self, message: str) -> None:
        self.log_text.append(message)

    def closeEvent(self, event: Any) -> None:
        if self.worker is not None and self.worker.isRunning():
            if self.cancel_token is not None:
                self.cancel_token.cancel()
            if not self.worker.wait(5000):
                QMessageBox.warning(
                    self,
                    "任务仍在停止",
                    "当前批次尚未结束。请稍候再关闭，避免中断原子提交。",
                )
                event.ignore()
                return
        event.accept()


def main(
    argv: Optional[list[str]] = None,
    *,
    manifest_path: str | Path = "samples/index.json",
    checkpoint_root: str | Path = "checkpoints/gui",
) -> int:
    if QApplication is None:
        print("PyQt5 未安装，无法启动 GUI", file=sys.stderr)
        return 1
    app = QApplication(argv if argv is not None else sys.argv)
    window = MainWindow(
        manifest_path=manifest_path, checkpoint_root=checkpoint_root
    )
    window.show()
    return int(app.exec_())


if __name__ == "__main__":
    raise SystemExit(main())
