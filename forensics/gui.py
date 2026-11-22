"""PyQt5 desktop workbench (thin controller over the tested engine).

The GUI owns no forensics math: it builds :class:`AnalysisParams`, drives the
preview/precise engine and renders block states and heatmaps.  A short
debounce timer implements the drag -> coarse preview -> stop -> precise
recompute interaction, and every parameter edit creates a fresh version
snapshot whose number is shown together with invalidated vs reused blocks.
"""

import os

import numpy as np
from PyQt5 import QtCore, QtGui, QtWidgets

from .engine import dual_path_analysis
from .errors import ForensicsError
from .loader import load_dataset
from .params import AnalysisParams, VersionControl
from .report import (
    build_report,
    heatmap_to_rgb,
    LatencyTracker,
)
from .states import PENDING, RUNNING, COMMITTED, FAILED


STATE_COLORS = {
    PENDING: QtGui.QColor(120, 120, 120),
    RUNNING: QtGui.QColor(80, 160, 255),
    COMMITTED: QtGui.QColor(80, 200, 120),
    FAILED: QtGui.QColor(230, 80, 80),
}


def numpy_to_qpixmap(rgb_uint8, cell=16):
    height, width, _ = rgb_uint8.shape
    image = QtGui.QImage(
        rgb_uint8.tobytes(),
        width,
        height,
        3 * width,
        QtGui.QImage.Format_RGB888,
    ).copy()
    return QtGui.QPixmap.fromImage(image).scaled(
        width * cell,
        height * cell,
        QtCore.Qt.KeepAspectRatio,
        QtCore.Qt.FastTransformation,
    )


class ForensicsMainWindow(QtWidgets.QMainWindow):
    def __init__(self, sample_dir, report_dir, checkpoint_dir):
        super().__init__()
        self.setWindowTitle("图像取证与完整性工作台")
        self.sample_dir = sample_dir
        self.report_dir = report_dir
        self.checkpoint_dir = checkpoint_dir
        self.vc = VersionControl(AnalysisParams())
        self.tracker = LatencyTracker()
        self.samples = load_dataset(sample_dir, block_size=self.vc.latest.params.block_size)
        self.sample_names = [s.name for s in self.samples]
        self._committed_versions = {}
        self._build_ui()
        self._debounce = QtCore.QTimer(self)
        self._debounce.setSingleShot(True)
        self._debounce.timeout.connect(self._on_preview_tick)
        self.version_label.setText(
            "参数版本: v%d (算法 %s)"
            % (self.vc.latest.version, self.vc.latest.algorithm_version)
        )
        self._load_sample(0)

    # -- UI construction -----------------------------------------------------

    def _build_ui(self):
        central = QtWidgets.QWidget()
        self.setCentralWidget(central)
        root = QtWidgets.QHBoxLayout(central)

        control = QtWidgets.QVBoxLayout()
        root.addLayout(control, 0)

        self.sample_box = QtWidgets.QComboBox()
        self.sample_box.addItems(self.sample_names)
        self.sample_box.currentIndexChanged.connect(self._load_sample)
        control.addWidget(QtWidgets.QLabel("样本"))
        control.addWidget(self.sample_box)

        self.block_size = self._spin(control, "分块尺寸", 32, 8, 128, 8, int)
        self.granularity = self._spin(control, "指纹粒度", 8, 1, 128, 1, int)
        self.noise_window = self._spin(control, "噪声窗口", 3, 1, 11, 2, int)
        self.edge_window = self._spin(control, "边缘窗口", 3, 1, 11, 2, int)
        self.contrast_window = self._spin(control, "对比度窗口", 3, 1, 11, 2, int)
        self.preview_scale = self._spin(control, "预览降采样", 2, 1, 4, 1, int)

        self.noise_threshold = self._spin(
            control, "噪声阈值", 100.0, 0.0, 5000.0, 10.0, float
        )
        self.edge_threshold = self._spin(
            control, "边缘阈值", 0.30, 0.0, 1.0, 0.01, float
        )
        self.contrast_threshold = self._spin(
            control, "对比度阈值", 20.0, 0.0, 400.0, 1.0, float
        )

        self.version_label = QtWidgets.QLabel()
        control.addWidget(self.version_label)
        self.invalid_label = QtWidgets.QLabel()
        self.invalid_label.setWordWrap(True)
        control.addWidget(self.invalid_label)
        self.latency_label = QtWidgets.QLabel()
        control.addWidget(self.latency_label)
        self.status_label = QtWidgets.QLabel()
        self.status_label.setWordWrap(True)
        control.addWidget(self.status_label)

        self.export_btn = QtWidgets.QPushButton("导出取证报告与热力图")
        self.export_btn.clicked.connect(self._export_report)
        control.addWidget(self.export_btn)

        self.inject_combo = QtWidgets.QComboBox()
        self.inject_combo.addItems(
            ["无注入", "窗口尺寸为零", "指纹粒度非法", "强制双路径分歧"]
        )
        control.addWidget(QtWidgets.QLabel("异常注入"))
        control.addWidget(self.inject_combo)

        control.addStretch(1)

        views = QtWidgets.QVBoxLayout()
        root.addLayout(views, 1)
        grid = QtWidgets.QGridLayout()
        views.addLayout(grid)
        self.preview_canvas = QtWidgets.QLabel()
        self.precise_canvas = QtWidgets.QLabel()
        self.diff_canvas = QtWidgets.QLabel()
        self.state_canvas = QtWidgets.QLabel()
        for col, (title, canvas) in enumerate(
            (
                ("预览热力图", self.preview_canvas),
                ("精确热力图", self.precise_canvas),
                ("差异热力图", self.diff_canvas),
                ("分块状态矩阵", self.state_canvas),
            )
        ):
            wrap = QtWidgets.QVBoxLayout()
            wrap.addWidget(QtWidgets.QLabel(title))
            canvas.setMinimumSize(220, 220)
            canvas.setStyleSheet("border:1px solid #555; background:#111;")
            wrap.addWidget(canvas)
            grid.addLayout(wrap, 0, col)

        self.report_view = QtWidgets.QPlainTextEdit()
        self.report_view.setReadOnly(True)
        views.addWidget(self.report_view, 1)

    def _spin(self, layout, label, value, lo, hi, step, kind):
        layout.addWidget(QtWidgets.QLabel(label))
        if kind is int:
            widget = QtWidgets.QSpinBox()
            widget.setRange(int(lo), int(hi))
            widget.setSingleStep(int(step))
            widget.setValue(int(value))
            widget.valueChanged.connect(self._schedule_preview)
        else:
            widget = QtWidgets.QDoubleSpinBox()
            widget.setRange(lo, hi)
            widget.setSingleStep(step)
            widget.setDecimals(3)
            widget.setValue(value)
            widget.valueChanged.connect(self._schedule_preview)
        layout.addWidget(widget)
        return widget

    # -- interaction ---------------------------------------------------------

    def _current_params(self):
        return AnalysisParams(
            block_size=self.block_size.value(),
            fingerprint_granularity=min(
                self.granularity.value(), self.block_size.value()
            ),
            noise_window=_odd(self.noise_window.value()),
            edge_window=_odd(self.edge_window.value()),
            contrast_window=_odd(self.contrast_window.value()),
            preview_scale=self.preview_scale.value(),
            noise_threshold=float(self.noise_threshold.value()),
            edge_threshold=float(self.edge_threshold.value()),
            contrast_threshold=float(self.contrast_threshold.value()),
        ).validate()

    def _schedule_preview(self):
        # Each edit bumps the version snapshot; the preview only reads latest.
        try:
            params = self._current_params()
        except ForensicsError as exc:
            self.status_label.setText("%s: %s" % (exc.code, exc.message))
            return
        snapshot = self.vc.replace_params(params)
        self.version_label.setText(
            "参数版本: v%d (算法 %s)"
            % (snapshot.version, snapshot.algorithm_version)
        )
        self._debounce.start(120)

    def _on_preview_tick(self):
        # Stop of interaction -> run both paths under the locked snapshot.
        self._run_analysis(self.vc.latest)

    def _load_sample(self, index):
        if not hasattr(self, "samples") or index >= len(self.samples):
            return
        self.current_sample = self.samples[index]
        self._run_analysis(self.vc.latest)

    def _run_analysis(self, snapshot):
        sample = self.current_sample
        # Rebuild the block index if the block size changed.
        if sample.block_index.block_size != snapshot.params.block_size:
            sample.block_index = type(sample.block_index)(
                sample.height, sample.width, snapshot.params.block_size
            )
        injection = self.inject_combo.currentText()
        force_diverge = injection == "强制双路径分歧"
        try:
            if injection == "窗口尺寸为零":
                from .hashing import median_filter2d

                median_filter2d(np.zeros((4, 4)), 0)
            if injection == "指纹粒度非法":
                from .hashing import fingerprint

                fingerprint(sample.block_pixels(0), 0)
            dual = dual_path_analysis(
                sample, snapshot.params, force_divergence=force_diverge
            )
        except ForensicsError as exc:
            self.status_label.setText("错误 %s — %s" % (exc.code, exc))
            return

        self.tracker.record(dual["preview_ms"])
        self.latency_label.setText(
            "预览延迟 p95=%.1f ms (本帧 %.1f ms)"
            % (self.tracker.quantiles().get("p95_ms", 0.0), dual["preview_ms"])
        )
        self._render_heatmaps(dual)
        self._render_state_matrix(snapshot)
        self._render_summary(dual, snapshot)
        self._last_dual = dual
        self._last_snapshot = snapshot

    def _render_heatmaps(self, dual):
        self.preview_canvas.setPixmap(
            numpy_to_qpixmap(heatmap_to_rgb(dual["preview"]["heatmap"]))
        )
        self.precise_canvas.setPixmap(
            numpy_to_qpixmap(heatmap_to_rgb(dual["precise"]["heatmap"]))
        )
        diff = dual["diff_heatmap"]
        normalized = diff / max(float(diff.max()), 1e-9)
        self.diff_canvas.setPixmap(
            numpy_to_qpixmap(heatmap_to_rgb(normalized))
        )

    def _render_state_matrix(self, snapshot):
        sample = self.current_sample
        rows, cols = sample.block_index.grid_shape()
        cell = 14
        image = QtGui.QImage(cols, rows, QtGui.QImage.Format_RGB888)
        image.fill(QtGui.QColor(40, 40, 40))
        painter = QtGui.QPainter(image)
        reused = self._committed_versions.get(sample.name, {})
        invalidated = 0
        for bid in range(rows * cols):
            r, c = divmod(bid, cols)
            if bid in reused and reused[bid] == snapshot.version:
                color = STATE_COLORS[COMMITTED]
            elif bid in reused:
                color = QtGui.QColor(200, 150, 60)  # stale -> invalidated
                invalidated += 1
            else:
                color = STATE_COLORS[PENDING]
            painter.fillRect(c, r, 1, 1, color)
        painter.end()
        self.state_canvas.setPixmap(
            QtGui.QPixmap.fromImage(image).scaled(
                cols * cell,
                rows * cell,
                QtCore.Qt.KeepAspectRatio,
                QtCore.Qt.FastTransformation,
            )
        )
        reused_count = sum(
            1 for v in reused.values() if v == snapshot.version
        )
        self.invalid_label.setText(
            "失效分块: %d | 复用分块: %d (橙色=旧版本失效, 绿色=当前版本复用)"
            % (invalidated, reused_count)
        )

    def _render_summary(self, dual, snapshot):
        precise = dual["precise"]
        suspicious = [
            bid for bid, flag in precise["decisions"].items() if flag
        ]
        self.status_label.setText(
            "双路径一致: %s | 可疑分块: %s | 版本 v%d"
            % (dual["consistent"], suspicious, snapshot.version)
        )
        self.report_view.setPlainText(
            "样本: %s\n版本: v%d\n预览帧耗时: %.2f ms\n可疑分块: %s\n"
            "延迟分位数: %s"
            % (
                self.current_sample.name,
                snapshot.version,
                dual["preview_ms"],
                suspicious,
                self.tracker.quantiles(),
            )
        )

    def _export_report(self):
        if not getattr(self, "_last_dual", None):
            return
        report = build_report(
            self.current_sample,
            self._last_dual,
            self._last_snapshot,
            self.report_dir,
        )
        self.status_label.setText(
            "报告已导出: %s (%d 个分块指标)"
            % (report["sample"], len(report["metrics"]))
        )


def _odd(value):
    value = int(value)
    return value if value % 2 == 1 else value + 1


def run_gui(sample_dir, report_dir, checkpoint_dir):
    import sys

    app = QtWidgets.QApplication.instance() or QtWidgets.QApplication(sys.argv)
    window = ForensicsMainWindow(sample_dir, report_dir, checkpoint_dir)
    window.resize(1280, 820)
    window.show()
    return app.exec_()
