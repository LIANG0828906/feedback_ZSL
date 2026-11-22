"""Reusable image, heat-map and dependency-graph widgets."""

from __future__ import annotations

from typing import Any

import numpy as np
from PySide6.QtCore import QPointF, QRectF, Qt
from PySide6.QtGui import (
    QColor,
    QFont,
    QImage,
    QPainter,
    QPainterPath,
    QPen,
)
from PySide6.QtWidgets import QWidget


STATUS_LABELS = {
    "empty": "空",
    "invalid": "已失效",
    "recomputing": "重算中",
    "computed": "已重算",
    "cached": "缓存命中",
    "valid": "有效",
    "failed": "失败",
    "blocked": "被阻塞",
    "cancelled": "已取消",
    "pending": "待处理",
}

STATUS_COLORS = {
    "empty": "#596174",
    "invalid": "#c58445",
    "recomputing": "#4d9dff",
    "computed": "#4ed197",
    "cached": "#70e0b0",
    "valid": "#70e0b0",
    "failed": "#ef5d72",
    "blocked": "#d97757",
    "cancelled": "#a4a9b8",
    "pending": "#8790a4",
}


def array_to_qimage(array: np.ndarray) -> QImage:
    contiguous = np.ascontiguousarray(array)
    height, width = contiguous.shape[:2]
    if contiguous.ndim == 2:
        return QImage(
            contiguous.data,
            width,
            height,
            contiguous.strides[0],
            QImage.Format.Format_Grayscale8,
        ).copy()
    channels = contiguous.shape[2]
    if channels == 1:
        gray = np.ascontiguousarray(contiguous[..., 0])
        return QImage(
            gray.data,
            width,
            height,
            gray.strides[0],
            QImage.Format.Format_Grayscale8,
        ).copy()
    if channels == 2:
        rgba = np.empty((height, width, 4), dtype=np.uint8)
        rgba[..., 0] = contiguous[..., 0]
        rgba[..., 1] = contiguous[..., 0]
        rgba[..., 2] = contiguous[..., 0]
        rgba[..., 3] = contiguous[..., 1]
        return QImage(
            rgba.data,
            width,
            height,
            rgba.strides[0],
            QImage.Format.Format_RGBA8888,
        ).copy()
    if channels == 3:
        return QImage(
            contiguous.data,
            width,
            height,
            contiguous.strides[0],
            QImage.Format.Format_RGB888,
        ).copy()
    return QImage(
        contiguous.data,
        width,
        height,
        contiguous.strides[0],
        QImage.Format.Format_RGBA8888,
    ).copy()


class ImageCanvas(QWidget):
    """Aspect-preserving image preview with optional geometry truth overlay."""

    def __init__(self, title: str, parent: QWidget | None = None) -> None:
        super().__init__(parent)
        self._title = title
        self._image: QImage | None = None
        self._truth: dict[str, Any] | None = None
        self._show_truth = False
        self._empty_message = "等待图像"
        self.setMinimumSize(300, 220)
        self.setSizePolicy(
            self.sizePolicy().Policy.Expanding,
            self.sizePolicy().Policy.Expanding,
        )

    def set_image(self, array: np.ndarray | None) -> None:
        self._image = array_to_qimage(array) if array is not None else None
        self.update()

    def set_title(self, title: str) -> None:
        self._title = title
        self.update()

    def set_truth(self, truth: dict[str, Any] | None) -> None:
        self._truth = truth
        self.update()

    def set_show_truth(self, show: bool) -> None:
        self._show_truth = show
        self.update()

    def set_empty_message(self, message: str) -> None:
        self._empty_message = message
        self.update()

    def paintEvent(self, event: Any) -> None:
        del event
        painter = QPainter(self)
        painter.setRenderHint(QPainter.RenderHint.Antialiasing, True)
        painter.setRenderHint(QPainter.RenderHint.SmoothPixmapTransform, True)
        painter.fillRect(self.rect(), QColor("#101522"))
        painter.setPen(QColor("#aab3c8"))
        title_font = QFont(self.font())
        title_font.setPointSizeF(max(9.0, title_font.pointSizeF()))
        title_font.setBold(True)
        painter.setFont(title_font)
        painter.drawText(14, 22, self._title)

        target = QRectF(14, 34, max(1, self.width() - 28), max(1, self.height() - 48))
        if self._image is None:
            painter.setPen(QColor("#596174"))
            painter.drawText(target, Qt.AlignmentFlag.AlignCenter, self._empty_message)
            self._draw_border(painter, target)
            return

        scale = min(
            target.width() / self._image.width(),
            target.height() / self._image.height(),
        )
        draw_width = self._image.width() * scale
        draw_height = self._image.height() * scale
        draw_rect = QRectF(
            target.center().x() - draw_width / 2,
            target.center().y() - draw_height / 2,
            draw_width,
            draw_height,
        )
        self._draw_checkerboard(painter, draw_rect)
        painter.drawImage(draw_rect, self._image)
        if self._show_truth and self._truth:
            self._draw_truth(painter, draw_rect)
        self._draw_border(painter, draw_rect)

    def _draw_checkerboard(self, painter: QPainter, rect: QRectF) -> None:
        painter.save()
        painter.setClipRect(rect)
        cell = 12
        start_x = int(rect.x())
        start_y = int(rect.y())
        end_x = int(rect.right()) + cell
        end_y = int(rect.bottom()) + cell
        colors = (QColor("#202533"), QColor("#2a3040"))
        for y in range(start_y, end_y, cell):
            row = (y - start_y) // cell
            for x in range(start_x, end_x, cell):
                column = (x - start_x) // cell
                painter.fillRect(
                    x,
                    y,
                    cell,
                    cell,
                    colors[(row + column) % 2],
                )
        painter.restore()

    def _draw_border(self, painter: QPainter, rect: QRectF) -> None:
        painter.setPen(QPen(QColor("#333b52"), 1.0))
        painter.setBrush(Qt.BrushStyle.NoBrush)
        painter.drawRect(rect)

    def _draw_truth(self, painter: QPainter, draw_rect: QRectF) -> None:
        assert self._truth is not None
        truth_width = float(self._truth.get("width", 1))
        truth_height = float(self._truth.get("height", 1))
        scale_x = draw_rect.width() / max(truth_width, 1.0)
        scale_y = draw_rect.height() / max(truth_height, 1.0)

        def point(x: float, y: float) -> QPointF:
            return QPointF(draw_rect.x() + x * scale_x, draw_rect.y() + y * scale_y)

        painter.save()
        painter.setClipRect(draw_rect)
        outline = QPen(QColor(255, 236, 126, 225), 1.5, Qt.PenStyle.DashLine)
        painter.setPen(outline)
        painter.setBrush(Qt.BrushStyle.NoBrush)
        for obj in self._truth.get("objects", []):
            object_type = obj.get("type")
            if object_type == "circle":
                center = point(float(obj["cx"]), float(obj["cy"]))
                radius = float(obj["radius"]) * (scale_x + scale_y) / 2.0
                painter.drawEllipse(center, radius, radius)
            elif object_type == "rect":
                start = point(float(obj["x0"]), float(obj["y0"]))
                end = point(float(obj["x1"]), float(obj["y1"]))
                painter.drawRect(QRectF(start, end).normalized())
            elif object_type == "line":
                painter.drawLine(
                    point(float(obj["x0"]), float(obj["y0"])),
                    point(float(obj["x1"]), float(obj["y1"])),
                )
        painter.restore()


class DependencyGraphWidget(QWidget):
    """Paint the explicit input -> denoise -> sharpen -> contrast graph."""

    def __init__(self, parent: QWidget | None = None) -> None:
        super().__init__(parent)
        self._rows: list[dict[str, Any]] = []
        self.setMinimumHeight(190)
        self.setMinimumWidth(520)

    def set_rows(self, rows: list[dict[str, Any]]) -> None:
        self._rows = rows
        self.update()

    def paintEvent(self, event: Any) -> None:
        del event
        painter = QPainter(self)
        painter.setRenderHint(QPainter.RenderHint.Antialiasing, True)
        painter.fillRect(self.rect(), QColor("#101522"))
        nodes = [{"stage": "input", "display_name": "输入图像", "status": "valid"}]
        nodes.extend(self._rows)
        count = max(1, len(nodes))
        margin = 18.0
        gap = 22.0
        node_width = max(
            88.0,
            min(154.0, (self.width() - 2 * margin - gap * (count - 1)) / count),
        )
        node_height = 82.0
        total_width = node_width * count + gap * (count - 1)
        start_x = (self.width() - total_width) / 2.0
        center_y = self.height() / 2.0
        for index, node in enumerate(nodes):
            left = start_x + index * (node_width + gap)
            rect = QRectF(left, center_y - node_height / 2, node_width, node_height)
            status = str(node.get("status", "empty"))
            border = QColor(STATUS_COLORS.get(status, "#596174"))
            fill = QColor("#182033")
            painter.setBrush(fill)
            painter.setPen(QPen(border, 2.0))
            path = QPainterPath()
            path.addRoundedRect(rect, 10.0, 10.0)
            painter.drawPath(path)
            painter.setPen(QColor("#e6ebf7"))
            bold = QFont(self.font())
            bold.setBold(True)
            painter.setFont(bold)
            painter.drawText(
                rect.adjusted(8, 8, -8, -36),
                Qt.AlignmentFlag.AlignHCenter | Qt.AlignmentFlag.AlignVCenter,
                str(node.get("display_name", node.get("stage", ""))),
            )
            painter.setPen(border)
            small = QFont(self.font())
            small.setPointSizeF(max(8.0, small.pointSizeF() - 1))
            painter.setFont(small)
            painter.drawText(
                rect.adjusted(8, 42, -8, -7),
                Qt.AlignmentFlag.AlignHCenter | Qt.AlignmentFlag.AlignVCenter,
                STATUS_LABELS.get(status, status),
            )
            cache_key = node.get("cache_key")
            if cache_key:
                painter.setPen(QColor("#77829a"))
                painter.drawText(
                    rect.adjusted(8, 57, -8, -3),
                    Qt.AlignmentFlag.AlignHCenter | Qt.AlignmentFlag.AlignVCenter,
                    str(cache_key)[:10],
                )
            if index < len(nodes) - 1:
                arrow_start = QPointF(rect.right() + 4, center_y)
                arrow_end = QPointF(rect.right() + gap - 4, center_y)
                painter.setPen(QPen(QColor("#59657d"), 1.7))
                painter.drawLine(arrow_start, arrow_end)
                painter.setBrush(QColor("#59657d"))
                painter.setPen(Qt.PenStyle.NoPen)
                painter.drawPolygon(
                    [
                        QPointF(arrow_end.x() - 4, arrow_end.y() - 4),
                        QPointF(arrow_end.x(), arrow_end.y()),
                        QPointF(arrow_end.x() - 4, arrow_end.y() + 4),
                    ]
                )

