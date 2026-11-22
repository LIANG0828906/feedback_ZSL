"""PyQt5 preview UI.

PyQt5 is imported only by this module so the renderer, CLI and tests remain
usable in headless environments without Qt.
"""

from __future__ import annotations

import sys
import threading
from dataclasses import replace
from typing import Any

from PyQt5.QtCore import QPointF, QRectF, QThread, Qt, pyqtSignal
from PyQt5.QtGui import QColor, QFont, QImage, QPainter, QPen
from PyQt5.QtWidgets import (
    QApplication,
    QCheckBox,
    QComboBox,
    QFormLayout,
    QHBoxLayout,
    QLabel,
    QMainWindow,
    QMessageBox,
    QPushButton,
    QSpinBox,
    QVBoxLayout,
    QWidget,
)

from .config import RenderConfig, validate_config
from .errors import BusinessError, ErrorCode
from .model import Scene
from .renderer import RenderResult, render_scene


def _qimage_from_ppm(data: bytes) -> QImage:
    first = data.find(b"\n")
    second = data.find(b"\n", first + 1)
    third = data.find(b"\n", second + 1)
    dimensions = data[first + 1 : second].split()
    width, height = int(dimensions[0]), int(dimensions[1])
    pixels = data[third + 1 :]
    image = QImage(pixels, width, height, width * 3, QImage.Format_RGB888)
    return image.copy()


class PreviewCanvas(QWidget):
    def __init__(self) -> None:
        super().__init__()
        self.setMinimumSize(480, 300)
        self._image: QImage | None = None
        self._tiles: list[dict[str, Any]] = []
        self._first_stage = "S0_FULL"

    def set_render(
        self,
        image: QImage,
        tiles: list[dict[str, Any]],
        first_stage: str,
    ) -> None:
        self._image = image
        self._tiles = tiles
        self._first_stage = first_stage
        self.update()

    def paintEvent(self, _event) -> None:  # noqa: N802 - Qt API
        painter = QPainter(self)
        painter.fillRect(self.rect(), QColor(18, 21, 27))
        if self._image is None:
            painter.setPen(QColor(180, 188, 200))
            painter.drawText(self.rect(), Qt.AlignCenter, "点击 Render 生成预览")
            return
        available = self.rect().adjusted(12, 12, -12, -12)
        scaled = self._image.size()
        scaled.scale(available.size(), Qt.KeepAspectRatio)
        x = available.x() + (available.width() - scaled.width()) // 2
        y = available.y() + (available.height() - scaled.height()) // 2
        target = QRectF(x, y, scaled.width(), scaled.height())
        painter.drawImage(target, self._image)
        sx = target.width() / self._image.width()
        sy = target.height() / self._image.height()
        font = QFont()
        font.setPointSize(7)
        painter.setFont(font)
        for tile in self._tiles:
            rect = QRectF(
                x + tile["x"] * sx,
                y + tile["y"] * sy,
                tile["width"] * sx,
                tile["height"] * sy,
            )
            degraded = tile["stage"] != self._first_stage
            painter.setPen(QPen(QColor(255, 172, 64, 210) if degraded else QColor(80, 190, 255, 120), 1.0))
            painter.drawRect(rect)
            if degraded:
                label = f"{tile['tile_id']}  {tile['stage']}"
                painter.fillRect(
                    QRectF(rect.x(), rect.y(), min(rect.width(), 130.0), 14.0),
                    QColor(10, 10, 10, 180),
                )
                painter.setPen(QColor(255, 190, 90))
                painter.drawText(
                    QPointF(rect.x() + 2, rect.y() + 10),
                    label[:28],
                )


class BudgetCurveWidget(QWidget):
    def __init__(self) -> None:
        super().__init__()
        self.setMinimumHeight(130)
        self._tiles: list[dict[str, Any]] = []
        self._budget = 0
        self._first_stage = "S0_FULL"

    def set_tiles(
        self, tiles: list[dict[str, Any]], budget: int, first_stage: str
    ) -> None:
        self._tiles = sorted(tiles, key=lambda item: item["submit_seq"])
        self._budget = max(budget, 1)
        self._first_stage = first_stage
        self.update()

    def paintEvent(self, _event) -> None:  # noqa: N802 - Qt API
        painter = QPainter(self)
        painter.fillRect(self.rect(), QColor(18, 21, 27))
        area = self.rect().adjusted(42, 18, -14, -28)
        painter.setPen(QPen(QColor(90, 100, 115), 1))
        painter.drawLine(area.left(), area.bottom(), area.right(), area.bottom())
        painter.drawLine(area.left(), area.top(), area.left(), area.bottom())
        painter.setPen(QColor(180, 188, 200))
        painter.drawText(4, 13, "预算占用")
        painter.drawText(4, area.bottom(), "0")
        painter.drawText(4, area.top() + 8, "1")
        if not self._tiles:
            painter.drawText(area, Qt.AlignCenter, "尚无分块轨迹")
            return
        count = len(self._tiles)
        step = area.width() / max(count, 1)
        points: list[QPointF] = []
        for index, tile in enumerate(self._tiles):
            ratio = min(1.0, float(tile.get("sample_count", 0)) / self._budget)
            px = area.left() + (index + 0.5) * step
            py = area.bottom() - ratio * area.height()
            points.append(QPointF(px, py))
            color = (
                QColor(90, 205, 255)
                if tile.get("stage") == self._first_stage
                else QColor(255, 172, 64)
            )
            painter.setPen(QPen(color, 2))
            painter.drawLine(QPointF(px, area.bottom()), QPointF(px, py))
            painter.setBrush(color)
            painter.drawEllipse(QPointF(px, py), 2.2, 2.2)
        painter.setBrush(Qt.NoBrush)
        painter.setPen(QPen(QColor(120, 220, 255), 1.2))
        for left, right in zip(points[:-1], points[1:]):
            painter.drawLine(left, right)
        painter.setPen(QColor(150, 158, 170))
        painter.drawText(
            area.adjusted(0, 0, 0, 18),
            Qt.AlignRight | Qt.AlignBottom,
            f"{count} 个分块，预算 {self._budget}",
        )


class RenderThread(QThread):
    result_ready = pyqtSignal(object)
    error_ready = pyqtSignal(object)

    def __init__(
        self,
        scene: Scene,
        config: RenderConfig,
        validate_seams: bool,
        compare_precision: bool,
        cancel_event: threading.Event,
    ) -> None:
        super().__init__()
        self.scene = scene
        self.config = config
        self.validate_seams = validate_seams
        self.compare_precision = compare_precision
        self.cancel_event = cancel_event

    def run(self) -> None:
        try:
            result = render_scene(
                self.scene,
                self.config,
                validate_seams=self.validate_seams,
                compare_precision=self.compare_precision,
                cancel_event=self.cancel_event,
            )
            self.result_ready.emit(result)
        except BusinessError as exc:
            self.error_ready.emit(exc.to_dict())
        except Exception as exc:  # pragma: no cover - defensive UI boundary
            self.error_ready.emit(
                {
                    "ok": False,
                    "code": ErrorCode.INTERNAL,
                    "message": str(exc),
                    "type": type(exc).__name__,
                }
            )


class MainWindow(QMainWindow):
    def __init__(self, scene: Scene, config: RenderConfig) -> None:
        super().__init__()
        self.scene = scene
        self.config = config
        self.thread: RenderThread | None = None
        self.cancel_event: threading.Event | None = None
        self.setWindowTitle("Volume Light Preview")
        self.resize(1040, 760)

        central = QWidget()
        self.setCentralWidget(central)
        root = QVBoxLayout(central)
        controls = QHBoxLayout()
        form = QFormLayout()
        self.tile_spin = QSpinBox()
        self.tile_spin.setRange(4, max(4, min(scene.width, scene.height)))
        self.tile_spin.setValue(config.tile_size)
        self.parallel_spin = QSpinBox()
        self.parallel_spin.setRange(1, config.limits.max_parallelism)
        self.parallel_spin.setValue(config.max_parallelism)
        self.budget_spin = QSpinBox()
        self.budget_spin.setRange(1, config.limits.max_sample_budget)
        self.budget_spin.setValue(config.sample_budget)
        self.budget_spin.setSingleStep(1000)
        self.precision_combo = QComboBox()
        self.precision_combo.addItems(["float", "fixed"])
        self.precision_combo.setCurrentText(config.precision)
        self.compare_box = QCheckBox("输出精度对比")
        self.seam_box = QCheckBox("整屏真值接缝校验")
        self.seam_box.setChecked(config.seam.enabled)
        form.addRow("分块尺寸", self.tile_spin)
        form.addRow("并行度", self.parallel_spin)
        form.addRow("单任务预算", self.budget_spin)
        form.addRow("精度模式", self.precision_combo)
        form.addRow("", self.compare_box)
        form.addRow("", self.seam_box)
        controls.addLayout(form)
        button_column = QVBoxLayout()
        self.render_button = QPushButton("Render")
        self.cancel_button = QPushButton("Cancel")
        self.cancel_button.setEnabled(False)
        self.status = QLabel("就绪")
        self.status.setWordWrap(True)
        button_column.addWidget(self.render_button)
        button_column.addWidget(self.cancel_button)
        button_column.addWidget(self.status)
        button_column.addStretch(1)
        controls.addLayout(button_column)
        root.addLayout(controls)
        self.preview = PreviewCanvas()
        root.addWidget(self.preview, 1)
        self.budget_curve = BudgetCurveWidget()
        root.addWidget(self.budget_curve)

        self.render_button.clicked.connect(self.start_render)
        self.cancel_button.clicked.connect(self.cancel_render)

    def _current_config(self) -> RenderConfig:
        config = replace(
            self.config,
            tile_size=self.tile_spin.value(),
            max_parallelism=self.parallel_spin.value(),
            sample_budget=self.budget_spin.value(),
            precision=self.precision_combo.currentText(),
        )
        validate_config(config)
        return config

    def start_render(self) -> None:
        if self.thread is not None and self.thread.isRunning():
            return
        try:
            config = self._current_config()
        except BusinessError as exc:
            QMessageBox.critical(self, "配置错误", exc.message)
            return
        self.cancel_event = threading.Event()
        self.thread = RenderThread(
            self.scene,
            config,
            validate_seams=self.seam_box.isChecked(),
            compare_precision=self.compare_box.isChecked(),
            cancel_event=self.cancel_event,
        )
        self.thread.result_ready.connect(self.render_finished)
        self.thread.error_ready.connect(self.render_failed)
        self.thread.finished.connect(self.render_thread_finished)
        self.render_button.setEnabled(False)
        self.cancel_button.setEnabled(True)
        self.status.setText(
            f"渲染中：tile={config.tile_size}, parallel={config.max_parallelism}, "
            f"budget={config.sample_budget}, precision={config.precision}"
        )
        self.thread.start()

    def cancel_render(self) -> None:
        if self.cancel_event is not None:
            self.cancel_event.set()
            self.status.setText("正在协作式取消…")

    def render_finished(self, result: RenderResult) -> None:
        image = _qimage_from_ppm(result.image_ppm)
        first_stage = result.summary["config"]["degrade_sequence"][0]["name"]
        self.preview.set_render(image, result.summary["tiles"], first_stage)
        self.budget_curve.set_tiles(
            result.summary["tiles"],
            result.summary["budget"]["per_tile"],
            first_stage,
        )
        stats = result.summary.get("precision_comparison")
        extra = ""
        if stats:
            extra = (
                f" | precision max_abs={stats['max_abs']:.3e}, "
                f"bound={stats['error_upper_bound']:.3e}"
            )
        self.status.setText(
            f"完成 | pixel={result.pixel_hash[:12]} | "
            f"budget={result.summary['budget']['total_consumed']}"
            f"{extra}"
        )

    def render_failed(self, error: dict[str, Any]) -> None:
        self.status.setText(f"{error.get('code')}: {error.get('message')}")
        QMessageBox.warning(
            self,
            "渲染失败",
            f"{error.get('code')}: {error.get('message')}",
        )

    def render_thread_finished(self) -> None:
        self.render_button.setEnabled(True)
        self.cancel_button.setEnabled(False)

    def closeEvent(self, event) -> None:  # noqa: N802 - Qt API
        self.cancel_render()
        if self.thread is not None:
            self.thread.wait(3000)
        event.accept()


def run_gui(scene: Scene, config: RenderConfig) -> int:
    app = QApplication.instance() or QApplication(sys.argv)
    window = MainWindow(scene, config)
    window.show()
    return int(app.exec_())


__all__ = ["BudgetCurveWidget", "MainWindow", "PreviewCanvas", "RenderThread", "run_gui"]
