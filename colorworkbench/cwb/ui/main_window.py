"""PySide6 工作台界面。

展示原图/结果对照、颜色域剖面、逐像素差异热力图、像素批状态与缓存命中，
支持调整参数触发增量重算与导出。无显示环境时使用 offscreen 平台插件。
"""

from __future__ import annotations

import os

import numpy as np

from PySide6.QtCore import Qt
from PySide6.QtGui import QImage, QPixmap
from PySide6.QtWidgets import (
    QComboBox,
    QHBoxLayout,
    QLabel,
    QMainWindow,
    QMessageBox,
    QPushButton,
    QTableWidget,
    QTableWidgetItem,
    QVBoxLayout,
    QWidget,
)

from ..engine import Workbench
from ..errors import CwbError
from ..model import INTENTS


def _to_qpixmap(image: np.ndarray) -> QPixmap:
    if image.ndim == 2:
        image = image[..., None]
    channels = image.shape[2]
    h, w = image.shape[:2]
    if image.dtype == np.uint16:
        image = (image // 257).astype(np.uint8)
    if channels == 1:
        data = np.repeat(image, 3, axis=2)
    elif channels == 2:
        data = np.repeat(image[..., :1], 3, axis=2)
    elif channels == 4:
        data = image[..., :3]
    else:
        data = image
    rgb = np.ascontiguousarray(data)
    qimg = QImage(rgb.data, w, h, 3 * w, QImage.Format.Format_RGB888)
    return QPixmap.fromImage(qimg.copy())


def _heatmap_pixmap(heat: np.ndarray) -> QPixmap:
    h, w = heat.shape
    base = np.zeros((h, w, 3), dtype=np.uint8)
    base[..., 0] = np.clip(heat * 255, 0, 255).astype(np.uint8)
    base[..., 1] = (40 * (1 - heat)).astype(np.uint8)
    base[..., 2] = (255 * (1 - heat)).astype(np.uint8)
    qimg = QImage(base.data, w, h, 3 * w, QImage.Format.Format_RGB888)
    return QPixmap.fromImage(qimg.copy())


class WorkbenchWindow(QMainWindow):
    def __init__(self, workbench: Workbench):
        super().__init__()
        self.wb = workbench
        self.setWindowTitle("颜色空间转换编辑工作台")
        self.resize(1280, 820)

        central = QWidget()
        self.setCentralWidget(central)
        layout = QVBoxLayout(central)

        controls = QHBoxLayout()
        self.intent_box = QComboBox()
        self.intent_box.addItems(list(INTENTS))
        self.intent_box.setCurrentText(self.wb.config.intent)
        controls.addWidget(QLabel("渲染意图:"))
        controls.addWidget(self.intent_box)
        self.run_btn = QPushButton("增量重算")
        self.export_btn = QPushButton("导出结果")
        controls.addWidget(self.run_btn)
        controls.addWidget(self.export_btn)
        layout.addLayout(controls)

        images = QHBoxLayout()
        self.source_label = QLabel("原图")
        self.result_label = QLabel("转换结果")
        self.heat_label = QLabel("差异热力图")
        for lab in (self.source_label, self.result_label, self.heat_label):
            lab.setAlignment(Qt.AlignmentFlag.AlignCenter)
            images.addWidget(lab)
        layout.addLayout(images)

        self.profile_label = QLabel("颜色域剖面: 待计算")
        layout.addWidget(self.profile_label)

        self.batch_table = QTableWidget(0, 4)
        self.batch_table.setHorizontalHeaderLabels(
            ["像素批", "行范围", "状态", "缓存命中"])
        layout.addWidget(self.batch_table)

        self.status = QLabel("就绪")
        layout.addWidget(self.status)

        self.run_btn.clicked.connect(self._on_recompute)
        self.export_btn.clicked.connect(self._on_export)
        self._output = None

    def load_and_run(self, image_path: str, expected_depth: int) -> None:
        self.image_path = image_path
        self.wb.load_image(image_path, expected_depth)
        self._output = self.wb.run_full(parallel=True)
        self._refresh()

    def _on_recompute(self) -> None:
        try:
            intent = self.intent_box.currentText()

            def edit(editor):
                if intent != self.wb.config.intent:
                    editor.edit_intent(intent)
            self._output = self.wb.edit_and_recompute(edit)
            self._refresh()
            self.status.setText(
                f"增量重算完成，累计缓存命中 "
                f"{sum(b.cache_hits for b in self.wb.pipeline.batches)}")
        except CwbError as exc:
            QMessageBox.critical(self, "转换失败",
                                 f"{exc.code}: {exc.message}")

    def _on_export(self) -> None:
        if self._output is None:
            return
        from ..image_io import encode_image
        target = os.path.join(os.getcwd(), "export_result.cimg")
        encode_image(target, self._output, self.wb.config.output_depth)
        self.status.setText(f"已导出 {target}")

    def _refresh(self) -> None:
        self.source_label.setPixmap(
            _to_qpixmap(self.wb.image).scaled(360, 360,
                                              Qt.AspectRatioMode.KeepAspectRatio))
        self.result_label.setPixmap(
            _to_qpixmap(self._output).scaled(360, 360,
                                             Qt.AspectRatioMode.KeepAspectRatio))
        heat = self.wb.heatmap(self._output)
        self.heat_label.setPixmap(
            _heatmap_pixmap(heat).scaled(360, 360,
                                         Qt.AspectRatioMode.KeepAspectRatio))
        stats, hist = self.wb.stats_and_histogram(self._output)
        self.profile_label.setText(
            "颜色域剖面: " +
            " | ".join(f"{k}峰={max(v) if isinstance(v, list) else v}"
                       for k, v in hist.items() if k.startswith("ch")) +
            f" | mean_abs={stats['mean_abs']:.5f} max_abs={stats['max_abs']:.5f}")
        self._refresh_batch_table()

    def _refresh_batch_table(self) -> None:
        batches = self.wb.pipeline.batches
        self.batch_table.setRowCount(len(batches))
        for row, batch in enumerate(batches):
            values = [str(batch.batch_id),
                      f"{batch.row_start}-{batch.row_end}",
                      batch.status, str(batch.cache_hits)]
            for col, value in enumerate(values):
                self.batch_table.setItem(row, col, QTableWidgetItem(value))
