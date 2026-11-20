"""PyQt5 diagnostic workbench with non-blocking processing threads."""

from __future__ import annotations

import sys
import threading
from pathlib import Path
from typing import Optional

import numpy as np

from PyQt5 import QtCore, QtGui, QtWidgets

from .engine import ProcessResult, Workbench
from .errors import WorkbenchError
from .factors import FactorSet, load_factors
from .model import load_sequence
from .sample import PROFILES, generate_sample_bundle
from .util import canonical_json_text


class ProcessingWorker(QtCore.QThread):
    progress = QtCore.pyqtSignal(dict)
    completed = QtCore.pyqtSignal(object)
    failed = QtCore.pyqtSignal(object)

    def __init__(self, workbench: Workbench) -> None:
        super().__init__()
        self.workbench = workbench
        self.cancel_event = threading.Event()

    def run(self) -> None:
        try:
            result = self.workbench.process(
                cancel_event=self.cancel_event,
                progress_callback=self.progress.emit,
            )
            self.completed.emit(result)
        except WorkbenchError as exc:
            self.failed.emit(exc.to_dict())
        except BaseException as exc:
            self.failed.emit(
                {"code": "E_INTERNAL", "message": "{}: {}".format(type(exc).__name__, exc)}
            )


class ComparisonWorker(QtCore.QThread):
    completed = QtCore.pyqtSignal(object)
    failed = QtCore.pyqtSignal(object)

    def __init__(self, workbench: Workbench) -> None:
        super().__init__()
        self.workbench = workbench
        self.cancel_event = threading.Event()

    def run(self) -> None:
        try:
            self.completed.emit(
                self.workbench.compare_modes(cancel_event=self.cancel_event)
            )
        except WorkbenchError as exc:
            self.failed.emit(exc.to_dict())
        except BaseException as exc:
            self.failed.emit(
                {"code": "E_INTERNAL", "message": "{}: {}".format(type(exc).__name__, exc)}
            )


class SequenceView(QtWidgets.QWidget):
    blockSelected = QtCore.pyqtSignal(int)

    def __init__(self, parent=None) -> None:
        super().__init__(parent)
        self.setMinimumSize(480, 360)
        self.setMouseTracking(True)
        self.result: Optional[ProcessResult] = None
        self.frame_index = 0
        self.display_mode = "校正结果"
        self.selected_block = 0
        self._image_rect = QtCore.QRect()
        self._block_size = 16

    def set_result(self, result: ProcessResult) -> None:
        self.result = result
        self._block_size = result.structure.block_size
        self.selected_block = min(
            self.selected_block, result.structure.blocks_per_frame - 1
        )
        self.update()

    def set_frame(self, frame_index: int) -> None:
        self.frame_index = int(frame_index)
        self.update()

    def set_display_mode(self, mode: str) -> None:
        self.display_mode = mode
        self.update()

    def set_selected_block(self, block_index: int) -> None:
        self.selected_block = int(block_index)
        self.update()

    def _normalise(self, data: np.ndarray) -> np.ndarray:
        finite = np.asarray(data[np.isfinite(data)], dtype=np.float64)
        if finite.size == 0:
            return np.zeros_like(data, dtype=np.float64)
        low, high = np.percentile(finite, [1.0, 99.0])
        if high <= low:
            high = low + 1.0
        return np.clip((data - low) / (high - low), 0.0, 1.0)

    @staticmethod
    def _heat_rgb(value: np.ndarray) -> np.ndarray:
        t = np.clip(value, 0.0, 1.0)
        red = np.clip(1.7 * t - 0.35, 0.0, 1.0)
        green = np.clip(1.6 - np.abs(2.2 * t - 1.05), 0.0, 1.0)
        blue = np.clip(1.1 - 1.8 * t, 0.0, 1.0)
        return np.stack((red, green, blue), axis=-1)

    def _display_rgb(self) -> np.ndarray:
        assert self.result is not None
        frame = self.result.frame(self.frame_index)
        error = self.result.frame_error(self.frame_index)
        untrusted = self.result.frame_untrusted(self.frame_index)
        if self.display_mode == "误差热力图":
            finite = error[np.isfinite(error)]
            high = (
                float(np.percentile(finite, 99.0))
                if finite.size
                else float(self.result.factors.error.threshold)
            )
            high = max(high, self.result.factors.error.threshold, 1.0e-12)
            return self._heat_rgb(np.clip(error / high, 0.0, 1.0))
        if self.display_mode == "不可信像素":
            rgb = np.zeros(frame.shape + (3,), dtype=np.float64)
            rgb[..., 0] = np.where(untrusted, 0.95, 0.05)
            rgb[..., 1] = np.where(untrusted, 0.08, 0.05)
            rgb[..., 2] = np.where(untrusted, 0.08, 0.08)
            return rgb
        gray = self._normalise(frame)
        rgb = np.repeat(gray[..., None], 3, axis=2)
        rgb[untrusted, 0] = np.clip(rgb[untrusted, 0] + 0.35, 0.0, 1.0)
        rgb[untrusted, 1:] *= 0.55
        return rgb

    def paintEvent(self, event) -> None:
        painter = QtGui.QPainter(self)
        painter.fillRect(self.rect(), QtGui.QColor(20, 22, 27))
        if self.result is None:
            painter.setPen(QtGui.QColor(150, 155, 165))
            painter.drawText(
                self.rect(), QtCore.Qt.AlignCenter, "载入序列后显示图像与诊断层"
            )
            return
        rgb = np.clip(self._display_rgb() * 255.0, 0, 255).astype(np.uint8)
        rgb = np.ascontiguousarray(rgb)
        height, width = rgb.shape[:2]
        image = QtGui.QImage(
            rgb.data, width, height, width * 3, QtGui.QImage.Format_RGB888
        ).copy()
        target = self.rect().adjusted(8, 8, -8, -8)
        scale = min(target.width() / width, target.height() / height)
        draw_width = max(1, int(round(width * scale)))
        draw_height = max(1, int(round(height * scale)))
        left = target.center().x() - draw_width // 2
        top = target.center().y() - draw_height // 2
        self._image_rect = QtCore.QRect(left, top, draw_width, draw_height)
        painter.drawImage(self._image_rect, image)

        structure = self.result.structure
        painter.setRenderHint(QtGui.QPainter.Antialiasing, False)
        grid_pen = QtGui.QPen(QtGui.QColor(70, 180, 190, 150), 1)
        painter.setPen(grid_pen)
        for col in range(1, structure.block_cols):
            x = left + int(round(col * structure.block_size * scale))
            painter.drawLine(x, top, x, top + draw_height)
        for row in range(1, structure.block_rows):
            y = top + int(round(row * structure.block_size * scale))
            painter.drawLine(left, y, left + draw_width, y)

        row, col = structure.grid_coord_for_block(self.selected_block)
        sx0 = col * structure.block_size
        sy0 = row * structure.block_size
        sx1 = min(structure.width, sx0 + structure.block_size)
        sy1 = min(structure.height, sy0 + structure.block_size)
        selected = QtCore.QRect(
            left + int(round(sx0 * scale)),
            top + int(round(sy0 * scale)),
            max(1, int(round((sx1 - sx0) * scale))),
            max(1, int(round((sy1 - sy0) * scale))),
        )
        painter.setPen(QtGui.QPen(QtGui.QColor(255, 205, 70), 2))
        painter.drawRect(selected)

    def mousePressEvent(self, event) -> None:
        if self.result is None or not self._image_rect.contains(event.pos()):
            return
        structure = self.result.structure
        x = int(
            (event.pos().x() - self._image_rect.x())
            / max(1, self._image_rect.width())
            * structure.width
        )
        y = int(
            (event.pos().y() - self._image_rect.y())
            / max(1, self._image_rect.height())
            * structure.height
        )
        x = min(structure.width - 1, max(0, x))
        y = min(structure.height - 1, max(0, y))
        block = structure.block_id_for_pixel(self.frame_index, y, x)
        self.set_selected_block(block)
        self.blockSelected.emit(block)


class MainWindow(QtWidgets.QMainWindow):
    def __init__(
        self,
        *,
        block_size: int = 64,
        snapshot_root: Optional[str] = None,
    ) -> None:
        super().__init__()
        self.setWindowTitle("科学成像序列处理工作台")
        self.resize(1180, 760)
        self.block_size = int(block_size)
        self.snapshot_root = snapshot_root
        self.workbench: Optional[Workbench] = None
        self.sequence_path: Optional[Path] = None
        self.calibration_path: Optional[Path] = None
        self.worker: Optional[ProcessingWorker] = None
        self.comparison_worker: Optional[ComparisonWorker] = None
        self._build_ui()
        self._build_menu()

    def _build_menu(self) -> None:
        menu = self.menuBar().addMenu("文件")
        open_sequence = menu.addAction("打开序列...")
        open_sequence.triggered.connect(self._choose_sequence)
        open_calibration = menu.addAction("打开校准...")
        open_calibration.triggered.connect(self._choose_calibration)
        menu.addSeparator()
        load_demo = menu.addAction("载入内置样本")
        load_demo.triggered.connect(self._load_demo)
        quit_action = menu.addAction("退出")
        quit_action.triggered.connect(self.close)

    def _build_ui(self) -> None:
        central = QtWidgets.QWidget()
        self.setCentralWidget(central)
        layout = QtWidgets.QVBoxLayout(central)

        timeline = QtWidgets.QHBoxLayout()
        self.frame_slider = QtWidgets.QSlider(QtCore.Qt.Horizontal)
        self.frame_slider.setEnabled(False)
        self.frame_spin = QtWidgets.QSpinBox()
        self.frame_spin.setEnabled(False)
        self.display_combo = QtWidgets.QComboBox()
        self.display_combo.addItems(("校正结果", "误差热力图", "不可信像素"))
        timeline.addWidget(QtWidgets.QLabel("帧时间轴"))
        timeline.addWidget(self.frame_slider, 1)
        timeline.addWidget(self.frame_spin)
        timeline.addSpacing(16)
        timeline.addWidget(QtWidgets.QLabel("显示层"))
        timeline.addWidget(self.display_combo)
        layout.addLayout(timeline)

        splitter = QtWidgets.QSplitter()
        splitter.setOrientation(QtCore.Qt.Horizontal)
        self.view = SequenceView()
        splitter.addWidget(self.view)

        controls = QtWidgets.QGroupBox("影响因子与处理")
        form = QtWidgets.QFormLayout(controls)
        self.mode_combo = QtWidgets.QComboBox()
        self.mode_combo.addItems(("fixed", "float"))
        self.frac_spin = QtWidgets.QSpinBox()
        self.frac_spin.setRange(0, 52)
        self.frac_spin.setValue(12)
        self.threshold_spin = QtWidgets.QDoubleSpinBox()
        self.threshold_spin.setDecimals(6)
        self.threshold_spin.setRange(0.0, 1.0e9)
        self.threshold_spin.setValue(8.0)
        self.sigma_spin = QtWidgets.QDoubleSpinBox()
        self.sigma_spin.setDecimals(4)
        self.sigma_spin.setRange(0.0, 20.0)
        self.sigma_spin.setSingleStep(0.1)
        self.sigma_spin.setValue(0.85)
        self.dark_offset_spin = QtWidgets.QDoubleSpinBox()
        self.dark_offset_spin.setDecimals(4)
        self.dark_offset_spin.setRange(-10000.0, 10000.0)
        self.dark_offset_spin.setValue(0.0)
        self.flat_scale_spin = QtWidgets.QDoubleSpinBox()
        self.flat_scale_spin.setDecimals(5)
        self.flat_scale_spin.setRange(0.5, 1.5)
        self.flat_scale_spin.setSingleStep(0.001)
        self.flat_scale_spin.setValue(1.0)
        self.gain_value_spin = QtWidgets.QDoubleSpinBox()
        self.gain_value_spin.setDecimals(5)
        self.gain_value_spin.setRange(0.0, 10.0)
        self.gain_value_spin.setValue(1.0)

        self.mode_label = QtWidgets.QLabel("未载入")
        self.block_label = QtWidgets.QLabel("块 0")
        form.addRow("精度模式", self.mode_combo)
        form.addRow("小数位", self.frac_spin)
        form.addRow("不可信阈值", self.threshold_spin)
        form.addRow("噪声 sigma", self.sigma_spin)
        form.addRow("暗场偏置", self.dark_offset_spin)
        form.addRow("平场比例", self.flat_scale_spin)
        form.addRow(self.block_label, self.gain_value_spin)
        form.addRow("状态", self.mode_label)

        self.apply_button = QtWidgets.QPushButton("应用影响因子")
        self.apply_button.clicked.connect(self._apply_factors)
        self.process_button = QtWidgets.QPushButton("启动重算")
        self.process_button.clicked.connect(self._start_processing)
        self.cancel_button = QtWidgets.QPushButton("协作式取消")
        self.cancel_button.setEnabled(False)
        self.cancel_button.clicked.connect(self._cancel_processing)
        self.compare_button = QtWidgets.QPushButton("定点/浮点对比")
        self.compare_button.clicked.connect(self._start_comparison)
        form.addRow(self.apply_button)
        form.addRow(self.process_button)
        form.addRow(self.cancel_button)
        form.addRow(self.compare_button)

        self.progress = QtWidgets.QProgressBar()
        self.progress.setRange(0, 100)
        form.addRow(self.progress)
        self.diagnostic_label = QtWidgets.QLabel("无结果")
        self.diagnostic_label.setWordWrap(True)
        form.addRow(self.diagnostic_label)
        splitter.addWidget(controls)
        splitter.setStretchFactor(0, 4)
        splitter.setStretchFactor(1, 1)
        layout.addWidget(splitter, 1)

        self.frame_slider.valueChanged.connect(self.frame_spin.setValue)
        self.frame_spin.valueChanged.connect(self.frame_slider.setValue)
        self.frame_spin.valueChanged.connect(self._frame_changed)
        self.display_combo.currentTextChanged.connect(self.view.set_display_mode)
        self.view.blockSelected.connect(self._block_selected)

    def _choose_sequence(self) -> None:
        path, _ = QtWidgets.QFileDialog.getOpenFileName(
            self, "选择序列", str(self.sequence_path or Path.cwd()), "*.simg"
        )
        if path:
            self.sequence_path = Path(path)
            if self.calibration_path is not None:
                self._load_workspace()
            else:
                self._choose_calibration()

    def _choose_calibration(self) -> None:
        path, _ = QtWidgets.QFileDialog.getOpenFileName(
            self, "选择校准", str(self.calibration_path or Path.cwd()), "*.cal"
        )
        if path:
            self.calibration_path = Path(path)
            if self.sequence_path is not None:
                self._load_workspace()

    def _load_demo(self) -> None:
        root = Path.cwd() / "sciworkbench_gui_demo"
        bundle = generate_sample_bundle(
            root,
            PROFILES["micro"],
            write_baseline=False,
            include_mode_comparison=False,
        )
        self.sequence_path = Path(bundle["sequence"])
        self.calibration_path = Path(bundle["calibration"])
        self._load_workspace()

    def _load_workspace(self) -> None:
        if self.sequence_path is None or self.calibration_path is None:
            return
        try:
            sequence = load_sequence(self.sequence_path, block_size=self.block_size)
            factors = load_factors(self.calibration_path, sequence.structure)
            workbench = Workbench(
                block_size=self.block_size,
                snapshot_root=self.snapshot_root or (sequence.path.parent / "snapshots"),
            )
            workbench.initialize(sequence, factors, self.mode_combo.currentText())
        except WorkbenchError as exc:
            QtWidgets.QMessageBox.critical(
                self, "载入失败", canonical_json_text(exc.to_dict())
            )
            return
        self.workbench = workbench
        self.frame_slider.setRange(0, sequence.header.frame_count - 1)
        self.frame_spin.setRange(0, sequence.header.frame_count - 1)
        self.frame_slider.setEnabled(True)
        self.frame_spin.setEnabled(True)
        self.frac_spin.setValue(factors.frac_bits)
        self.threshold_spin.setValue(factors.error.threshold)
        self.sigma_spin.setValue(factors.noise.sigma)
        self.sequence_path = sequence.path
        self.calibration_path = Path(self.calibration_path)
        self.mode_label.setText(
            "{}x{}x{}，{} 块".format(
                sequence.header.width,
                sequence.header.height,
                sequence.header.frame_count,
                sequence.structure.total_blocks,
            )
        )
        self._sync_gain_from_selection()
        self.diagnostic_label.setText("工作区已载入，可启动重算")

    def _sync_gain_from_selection(self) -> None:
        if self.workbench is None or self.workbench.structure is None:
            return
        factors = self.workbench.factors
        structure = self.workbench.structure
        block = structure.block(self.view.frame_index, self.view.selected_block)
        rows, cols = factors.block_gain.shape
        cy = (block.y0 + block.y1 - 1) / 2.0
        cx = (block.x0 + block.x1 - 1) / 2.0
        gy = (
            int(round(cy / max(1, structure.height - 1) * (rows - 1)))
            if rows > 1
            else 0
        )
        gx = (
            int(round(cx / max(1, structure.width - 1) * (cols - 1)))
            if cols > 1
            else 0
        )
        self.gain_value_spin.setValue(float(factors.block_gain[gy, gx]))
        self.block_label.setText("块 {} 增益".format(block.block_index))

    def _block_selected(self, block_index: int) -> None:
        self.view.set_selected_block(block_index)
        self._sync_gain_from_selection()

    def _frame_changed(self, value: int) -> None:
        self.view.set_frame(value)
        self._sync_gain_from_selection()
        if self.workbench is not None and self.workbench.result is not None:
            result = self.workbench.result
            self.diagnostic_label.setText(
                "帧 {}：不可信 {}，最大误差上界 {:.6g}".format(
                    value,
                    int(result.frame_untrusted(value).sum()),
                    float(np.max(result.frame_error(value))),
                )
            )

    def _apply_factors(self) -> None:
        if self.workbench is None or self.workbench.factors is None:
            QtWidgets.QMessageBox.warning(self, "未初始化", "请先载入序列与校准。")
            return
        if self.worker is not None and self.worker.isRunning():
            QtWidgets.QMessageBox.warning(self, "E_BUSY", "处理任务仍在运行。")
            return
        factors = self.workbench.factors
        structure = self.workbench.structure
        assert structure is not None
        dark = np.array(factors.dark, copy=True)
        dark += self.dark_offset_spin.value()
        flat = np.array(factors.flat, copy=True) * self.flat_scale_spin.value()
        gain = np.array(factors.block_gain, copy=True)
        block = structure.block(self.view.frame_index, self.view.selected_block)
        rows, cols = gain.shape
        cy = (block.y0 + block.y1 - 1) / 2.0
        cx = (block.x0 + block.x1 - 1) / 2.0
        gy = int(round(cy / max(1, structure.height - 1) * (rows - 1))) if rows > 1 else 0
        gx = int(round(cx / max(1, structure.width - 1) * (cols - 1))) if cols > 1 else 0
        gain[gy, gx] = self.gain_value_spin.value()
        try:
            replacement = FactorSet(
                dark=dark,
                flat=flat,
                bad_mask=factors.bad_mask,
                block_gain=gain,
                noise=type(factors.noise)(
                    sigma=self.sigma_spin.value(),
                    coeff_bits=factors.noise.coeff_bits,
                ),
                error=type(factors.error)(
                    dark_error=factors.error.dark_error,
                    flat_rel_error=factors.error.flat_rel_error,
                    flat_floor=factors.error.flat_floor,
                    threshold=self.threshold_spin.value(),
                ),
                frac_bits=self.frac_spin.value(),
            )
            self.workbench.set_factors(replacement)
            self.workbench.set_mode(self.mode_combo.currentText())
        except WorkbenchError as exc:
            QtWidgets.QMessageBox.critical(
                self, "影响因子无效", canonical_json_text(exc.to_dict())
            )
            return
        self.dark_offset_spin.setValue(0.0)
        self.flat_scale_spin.setValue(1.0)
        self.diagnostic_label.setText("影响因子已更新，等待重算")

    def _start_processing(self) -> None:
        if self.workbench is None:
            QtWidgets.QMessageBox.warning(self, "未初始化", "请先载入序列与校准。")
            return
        if self.worker is not None and self.worker.isRunning():
            QtWidgets.QMessageBox.warning(
                self, "E_BUSY", "同一序列同时只允许一个处理任务。"
            )
            return
        try:
            self.workbench.set_mode(self.mode_combo.currentText())
        except WorkbenchError as exc:
            QtWidgets.QMessageBox.critical(
                self, "模式无效", canonical_json_text(exc.to_dict())
            )
            return
        self.worker = ProcessingWorker(self.workbench)
        self.worker.progress.connect(self._on_progress)
        self.worker.completed.connect(self._on_completed)
        self.worker.failed.connect(self._on_failed)
        self.worker.finished.connect(self._worker_finished)
        self._set_controls_busy(True)
        self.progress.setValue(0)
        self.diagnostic_label.setText("后台重算中…")
        self.worker.start()

    def _start_comparison(self) -> None:
        if self.workbench is None:
            QtWidgets.QMessageBox.warning(self, "未初始化", "请先载入序列与校准。")
            return
        if (
            (self.worker is not None and self.worker.isRunning())
            or (
                self.comparison_worker is not None
                and self.comparison_worker.isRunning()
            )
        ):
            QtWidgets.QMessageBox.warning(
                self, "E_BUSY", "同一序列同时只允许一个处理任务。"
            )
            return
        self.comparison_worker = ComparisonWorker(self.workbench)
        self.comparison_worker.completed.connect(self._on_comparison)
        self.comparison_worker.failed.connect(self._on_failed)
        self.comparison_worker.finished.connect(self._comparison_finished)
        self._set_controls_busy(True)
        self.diagnostic_label.setText("后台计算双模式对比…")
        self.comparison_worker.start()

    def _cancel_processing(self) -> None:
        if self.worker is not None and self.worker.isRunning():
            self.worker.cancel_event.set()
            self.cancel_button.setEnabled(False)
            self.diagnostic_label.setText("已请求取消，等待当前块提交完成…")
        if (
            self.comparison_worker is not None
            and self.comparison_worker.isRunning()
        ):
            self.comparison_worker.cancel_event.set()
            self.cancel_button.setEnabled(False)
            self.diagnostic_label.setText("已请求取消模式对比…")

    def _on_progress(self, payload: dict) -> None:
        total = max(1, int(payload["total"]))
        self.progress.setValue(int(round(payload["completed"] * 100 / total)))
        self.diagnostic_label.setText(
            "帧 {} 块 {} · {} · 重算 {} / 复用 {} / 恢复 {}".format(
                payload["frame"],
                payload["block"],
                payload["action"],
                payload["recomputed_blocks"],
                payload["reused_blocks"],
                payload["recovered_blocks"],
            )
        )

    def _on_completed(self, result: ProcessResult) -> None:
        self.view.set_result(result)
        self.view.set_frame(self.frame_spin.value())
        self.progress.setValue(100)
        self.diagnostic_label.setText(
            "发布版本 {} · 重算 {} / 复用 {} / 恢复 {} · 不可信 {} · 模式差 {:.6g}".format(
                result.version,
                result.recomputed_blocks,
                result.reused_blocks,
                result.recovered_blocks,
                int(result.untrusted.sum()),
                result.fixed_float_comparison["max_abs_diff"],
            )
        )

    def _on_failed(self, payload: dict) -> None:
        code = payload.get("code", "E_INTERNAL")
        self.diagnostic_label.setText("失败：{}".format(code))
        QtWidgets.QMessageBox.critical(
            self, code, canonical_json_text(payload)
        )

    def _on_comparison(self, report: dict) -> None:
        self.diagnostic_label.setText(
            "双模式：mean |Δ| {:.6g}，max |Δ| {:.6g}".format(
                report["mean_abs_diff"], report["max_abs_diff"]
            )
        )
        QtWidgets.QMessageBox.information(
            self,
            "定点/浮点对比",
            canonical_json_text(
                {
                    key: report[key]
                    for key in (
                        "mean_abs_diff",
                        "max_abs_diff",
                        "rms_abs_diff",
                        "p95_abs_diff",
                        "error_bound_delta_mean",
                        "error_bound_delta_max",
                        "fixed_digest",
                        "float_digest",
                    )
                }
            ),
        )

    def _worker_finished(self) -> None:
        self._set_controls_busy(False)
        self.worker = None

    def _comparison_finished(self) -> None:
        self._set_controls_busy(False)
        self.comparison_worker = None

    def _set_controls_busy(self, busy: bool) -> None:
        for widget in (
            self.process_button,
            self.apply_button,
            self.compare_button,
            self.mode_combo,
            self.frac_spin,
            self.threshold_spin,
            self.sigma_spin,
            self.dark_offset_spin,
            self.flat_scale_spin,
            self.gain_value_spin,
        ):
            widget.setEnabled(not busy)
        self.cancel_button.setEnabled(busy)

    def closeEvent(self, event) -> None:
        running = (
            self.worker is not None and self.worker.isRunning()
        ) or (
            self.comparison_worker is not None
            and self.comparison_worker.isRunning()
        )
        if running:
            if self.worker is not None:
                self.worker.cancel_event.set()
            if self.comparison_worker is not None:
                self.comparison_worker.cancel_event.set()
            if self.worker is not None:
                self.worker.wait()
            if self.comparison_worker is not None:
                self.comparison_worker.wait()
        event.accept()


def run_gui(
    *,
    sequence_path: Optional[str] = None,
    calibration_path: Optional[str] = None,
    snapshot_root: Optional[str] = None,
    block_size: int = 64,
    demo: bool = False,
) -> int:
    QtWidgets.QApplication.setAttribute(QtCore.Qt.AA_EnableHighDpiScaling, True)
    app = QtWidgets.QApplication.instance() or QtWidgets.QApplication(sys.argv)
    window = MainWindow(block_size=block_size, snapshot_root=snapshot_root)
    if sequence_path is not None:
        window.sequence_path = Path(sequence_path)
    if calibration_path is not None:
        window.calibration_path = Path(calibration_path)
    if demo and (window.sequence_path is None or window.calibration_path is None):
        window._load_demo()
    elif window.sequence_path is not None and window.calibration_path is not None:
        window._load_workspace()
    window.show()
    return int(app.exec_())
