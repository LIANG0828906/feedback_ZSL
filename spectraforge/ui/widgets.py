"""Custom image and analytical chart widgets."""

from __future__ import annotations

import math
from typing import Any

import numpy as np
from PySide6.QtCore import QPointF, QRectF, Qt
from PySide6.QtGui import QColor, QFont, QImage, QLinearGradient, QPainter, QPainterPath, QPen, QPixmap, QPolygonF
from PySide6.QtWidgets import QFrame, QLabel, QSizePolicy, QVBoxLayout, QWidget

from ..color import CIE_1931_2DEG_400_700_10, GAMUT_PRIMARIES_XY, rgb_to_xyz_matrix
from ..tone import tone_curve_luminance


def numpy_rgb_to_qimage(array: np.ndarray) -> QImage:
    rgb = np.ascontiguousarray(np.clip(array, 0, 255).astype(np.uint8))
    if rgb.ndim == 2:
        rgb = np.repeat(rgb[..., np.newaxis], 3, axis=2)
    height, width, _ = rgb.shape
    return QImage(rgb.data, width, height, width * 3, QImage.Format_RGB888).copy()


def _lut() -> np.ndarray:
    stops = np.array(
        [
            [6, 13, 22],
            [14, 46, 70],
            [22, 120, 130],
            [38, 191, 169],
            [147, 226, 122],
            [255, 198, 74],
            [245, 94, 70],
            [255, 236, 220],
        ],
        dtype=np.float64,
    )
    x = np.linspace(0, len(stops) - 1, 256)
    low = np.floor(x).astype(int)
    high = np.minimum(low + 1, len(stops) - 1)
    frac = (x - low)[:, np.newaxis]
    return (stops[low] * (1 - frac) + stops[high] * frac).astype(np.uint8)


HEAT_LUT = _lut()


def heatmap_rgb(values: np.ndarray, robust: bool = True) -> tuple[np.ndarray, float, float]:
    data = np.asarray(values, dtype=np.float64)
    finite = data[np.isfinite(data)]
    if finite.size == 0:
        return np.zeros(data.shape + (3,), dtype=np.uint8), 0.0, 0.0
    high = float(np.percentile(finite, 99.5)) if robust else float(finite.max())
    high = max(high, 1e-9)
    normalized = np.clip(data / high, 0.0, 1.0)
    indices = np.rint(np.power(normalized, 0.55) * 255).astype(np.int32)
    return HEAT_LUT[indices], 0.0, high


def tone_to_uint8(values: np.ndarray, gamma: float = 2.2) -> np.ndarray:
    data = np.asarray(values, dtype=np.float64)
    positive = data[data > 0]
    if positive.size == 0:
        normalized = np.zeros_like(data)
    else:
        low = float(np.percentile(positive, 1))
        high = float(np.percentile(positive, 99.7))
        normalized = np.clip((data - low) / max(high - low, 1e-9), 0.0, 1.0)
    return np.rint(np.power(normalized, 1.0 / gamma) * 255).astype(np.uint8)


class ImagePanel(QFrame):
    def __init__(self, title: str, subtitle: str = "", parent: QWidget | None = None):
        super().__init__(parent)
        self.setObjectName("card")
        self.setMinimumSize(190, 145)
        layout = QVBoxLayout(self)
        layout.setContentsMargins(10, 8, 10, 10)
        layout.setSpacing(4)
        self.title = QLabel(title)
        self.title.setObjectName("sectionTitle")
        self.subtitle = QLabel(subtitle)
        self.subtitle.setProperty("class", "muted")
        self.subtitle.setStyleSheet("color:#718397;font-size:10px;")
        self.image = QLabel()
        self.image.setAlignment(Qt.AlignCenter)
        self.image.setStyleSheet("background:#070b0f;border:1px solid #182633;border-radius:3px;")
        self.image.setSizePolicy(QSizePolicy.Expanding, QSizePolicy.Expanding)
        layout.addWidget(self.title)
        layout.addWidget(self.subtitle)
        layout.addWidget(self.image, 1)
        self._pixmap = None

    def set_array(self, array: np.ndarray, caption: str | None = None) -> None:
        self._pixmap = QImage()
        self._source = numpy_rgb_to_qimage(array)
        if caption is not None:
            self.subtitle.setText(caption)
        self._rescale()

    def set_message(self, text: str) -> None:
        self._source = None
        self._pixmap = None
        self.image.setPixmap(QPixmap())
        self.image.setText(text)

    def resizeEvent(self, event: Any) -> None:
        super().resizeEvent(event)
        self._rescale()

    def _rescale(self) -> None:
        if getattr(self, "_source", None) is None or self._source.isNull():
            return
        target = self.image.size()
        if target.width() < 2 or target.height() < 2:
            return
        self._pixmap = QPixmap.fromImage(
            getattr(self, "_source", QImage()).scaled(
                target,
                Qt.KeepAspectRatio,
                Qt.SmoothTransformation,
            )
        )
        self.image.setPixmap(self._pixmap)


class HistogramWidget(QWidget):
    def __init__(self, parent: QWidget | None = None):
        super().__init__(parent)
        self.setMinimumHeight(150)
        self.counts = np.zeros(48)
        self.edges = np.ones(49)
        self.title = "RADIANCE HISTOGRAM"

    def set_values(self, values: np.ndarray, bins: int = 48) -> None:
        data = np.asarray(values, dtype=np.float64).reshape(-1)
        finite = data[np.isfinite(data)]
        if finite.size == 0:
            self.counts = np.zeros(bins)
            self.edges = np.ones(bins + 1)
        else:
            low = max(float(finite.min()), 1e-6)
            high = max(float(finite.max()), low * 1.00001)
            self.counts, self.edges = np.histogram(finite, bins=np.geomspace(low, high, bins + 1))
        self.update()

    def paintEvent(self, _event: Any) -> None:
        painter = QPainter(self)
        painter.setRenderHint(QPainter.Antialiasing)
        painter.fillRect(self.rect(), QColor("#0d141c"))
        rect = QRectF(12, 26, self.width() - 24, self.height() - 47)
        painter.setPen(QColor("#718397"))
        painter.setFont(QFont("Segoe UI", 8, QFont.DemiBold))
        painter.drawText(12, 16, self.title)
        painter.setPen(QPen(QColor("#1c2a36"), 1))
        for index in range(5):
            y = rect.top() + rect.height() * index / 4
            painter.drawLine(QPointF(rect.left(), y), QPointF(rect.right(), y))
        if self.counts.size:
            log_counts = np.log1p(self.counts)
            high = max(float(log_counts.max()), 1e-9)
            bar_width = rect.width() / len(log_counts)
            for index, value in enumerate(log_counts):
                height = rect.height() * float(value) / high
                bar = QRectF(rect.left() + index * bar_width + 0.5, rect.bottom() - height, max(1.0, bar_width - 1), height)
                color = QColor("#26d0ce") if index < len(log_counts) * 0.7 else QColor("#ffb454")
                painter.fillRect(bar, color)
        painter.setPen(QColor("#66798d"))
        painter.setFont(QFont("Cascadia Mono", 7))
        if self.edges.size:
            painter.drawText(12, self.height() - 5, f"{self.edges[0]:.3g}")
            label = f"{self.edges[-1]:.3g}"
            painter.drawText(self.width() - 62, self.height() - 5, label)


class ChromaticityWidget(QWidget):
    def __init__(self, parent: QWidget | None = None):
        super().__init__(parent)
        self.setMinimumHeight(255)
        self.chroma: np.ndarray | None = None
        self.gamut = "sRGB"
        self.title = "CHROMATICITY SCOPE"

    def set_data(self, chroma: np.ndarray | None, gamut: str) -> None:
        self.chroma = None if chroma is None else np.asarray(chroma)
        self.gamut = gamut
        self.update()

    @staticmethod
    def _xy_from_xyz(xyz: np.ndarray) -> np.ndarray:
        total = np.maximum(xyz.sum(axis=-1), 1e-12)
        return xyz[..., :2] / total[..., np.newaxis]

    def _point(self, x: float, y: float, rect: QRectF) -> QPointF:
        return QPointF(rect.left() + x / 0.8 * rect.width(), rect.bottom() - y / 0.9 * rect.height())

    def paintEvent(self, _event: Any) -> None:
        painter = QPainter(self)
        painter.setRenderHint(QPainter.Antialiasing)
        painter.fillRect(self.rect(), QColor("#0d141c"))
        painter.setPen(QColor("#718397"))
        painter.setFont(QFont("Segoe UI", 8, QFont.DemiBold))
        painter.drawText(12, 16, self.title)
        rect = QRectF(24, 28, self.width() - 38, self.height() - 54)
        painter.setPen(QPen(QColor("#182632"), 1))
        for value in np.linspace(0.1, 0.7, 7):
            start = self._point(value, 0.0, rect)
            end = self._point(value, 0.9, rect)
            painter.drawLine(start, end)
        for value in np.linspace(0.1, 0.8, 8):
            start = self._point(0.0, value, rect)
            end = self._point(0.8, value, rect)
            painter.drawLine(start, end)
        cmf = CIE_1931_2DEG_400_700_10
        locus_xy = self._xy_from_xyz(cmf)
        locus = QPolygonF([self._point(float(x), float(y), rect) for x, y in locus_xy])
        painter.setPen(QPen(QColor("#50667a"), 1.3))
        painter.setBrush(QColor(20, 34, 46, 90))
        painter.drawPolygon(locus)
        if self.gamut in GAMUT_PRIMARIES_XY:
            entry = GAMUT_PRIMARIES_XY[self.gamut]
            points = [
                self._point(*entry["red"], rect),
                self._point(*entry["green"], rect),
                self._point(*entry["blue"], rect),
                self._point(*entry["red"], rect),
            ]
            painter.setBrush(Qt.NoBrush)
            painter.setPen(QPen(QColor("#ffb454"), 1.3, Qt.DashLine))
            painter.drawPolyline(QPolygonF(points))
        if self.chroma is not None and self.chroma.size:
            sample = self.chroma.reshape(-1, 3)[:: max(1, self.chroma.reshape(-1, 3).shape[0] // 5000)]
            xyz = sample @ rgb_to_xyz_matrix(self.gamut).T
            xy = self._xy_from_xyz(xyz)
            painter.setPen(Qt.NoPen)
            painter.setBrush(QColor(38, 208, 206, 95))
            for x, y in xy:
                if np.isfinite(x) and np.isfinite(y) and 0 <= x <= 0.8 and 0 <= y <= 0.9:
                    painter.drawEllipse(self._point(float(x), float(y), rect), 1.6, 1.6)
        painter.setPen(QColor("#66798d"))
        painter.setFont(QFont("Cascadia Mono", 7))
        painter.drawText(int(rect.left()), self.height() - 6, "CIE 1931  x")
        painter.drawText(5, int(rect.top() + 6), "y")


class ToneCurveWidget(QWidget):
    def __init__(self, parent: QWidget | None = None):
        super().__init__(parent)
        self.setMinimumHeight(155)
        self.params: dict[str, Any] = {}
        self.title = "TONE RESPONSE"

    def set_params(self, params: dict[str, Any]) -> None:
        self.params = dict(params)
        self.update()

    def paintEvent(self, _event: Any) -> None:
        painter = QPainter(self)
        painter.setRenderHint(QPainter.Antialiasing)
        painter.fillRect(self.rect(), QColor("#0d141c"))
        painter.setPen(QColor("#718397"))
        painter.setFont(QFont("Segoe UI", 8, QFont.DemiBold))
        painter.drawText(12, 16, self.title)
        rect = QRectF(26, 28, self.width() - 40, self.height() - 48)
        painter.setPen(QPen(QColor("#1c2a36"), 1))
        for index in range(5):
            x = rect.left() + rect.width() * index / 4
            y = rect.top() + rect.height() * index / 4
            painter.drawLine(QPointF(x, rect.top()), QPointF(x, rect.bottom()))
            painter.drawLine(QPointF(rect.left(), y), QPointF(rect.right(), y))
        if self.params:
            white = float(self.params.get("tone_white", 8.0))
            luminance = np.geomspace(1e-4, max(white, 0.2), 220)
            mapped = tone_curve_luminance(luminance, self.params)
            path = QPainterPath()
            for index, (value, output) in enumerate(zip(luminance, mapped)):
                x = math.log10(max(float(value), 1e-6) / 1e-4) / math.log10(max(white, 0.2) / 1e-4)
                point = QPointF(rect.left() + x * rect.width(), rect.bottom() - float(output) * rect.height())
                if index == 0:
                    path.moveTo(point)
                else:
                    path.lineTo(point)
            painter.setPen(QPen(QColor("#26d0ce"), 2))
            painter.drawPath(path)
            painter.setPen(QColor("#ffb454"))
            painter.setFont(QFont("Cascadia Mono", 7))
            painter.drawText(12, self.height() - 6, "EV -  low")
            painter.drawText(self.width() - 60, self.height() - 6, f"white {white:g}")


class DifferenceHeatmapWidget(QWidget):
    def __init__(self, parent: QWidget | None = None):
        super().__init__(parent)
        self.setMinimumHeight(250)
        self.diff: np.ndarray | None = None
        self.mismatch: dict[str, Any] | None = None
        self.title = "PIXEL DIFFERENCE HEATMAP"

    def set_difference(self, diff: np.ndarray | None, mismatch: dict[str, Any] | None = None) -> None:
        self.diff = None if diff is None else np.asarray(diff)
        self.mismatch = mismatch
        self.update()

    def paintEvent(self, _event: Any) -> None:
        painter = QPainter(self)
        painter.setRenderHint(QPainter.Antialiasing)
        painter.fillRect(self.rect(), QColor("#0d141c"))
        painter.setPen(QColor("#718397"))
        painter.setFont(QFont("Segoe UI", 8, QFont.DemiBold))
        painter.drawText(12, 16, self.title)
        rect = QRectF(12, 26, self.width() - 24, self.height() - 61)
        if self.diff is None:
            painter.setPen(QColor("#5d7084"))
            painter.setFont(QFont("Segoe UI", 10))
            painter.drawText(rect, Qt.AlignCenter, "Run full-recompute comparison to populate heatmap")
        else:
            rgb, low, high = heatmap_rgb(self.diff)
            image = numpy_rgb_to_qimage(rgb)
            painter.drawImage(rect, image)
            painter.setPen(QPen(QColor("#253849"), 1))
            painter.drawRect(rect)
            if self.mismatch:
                x = float(self.mismatch.get("x", 0))
                y = float(self.mismatch.get("y", 0))
                height, width = self.diff.shape[:2]
                point = QPointF(rect.left() + x / max(width, 1) * rect.width(), rect.top() + y / max(height, 1) * rect.height())
                painter.setPen(QPen(QColor("#ffffff"), 2))
                painter.setBrush(Qt.NoBrush)
                painter.drawEllipse(point, 8, 8)
            gradient = QLinearGradient(rect.left(), self.height() - 25, rect.right(), self.height() - 25)
            gradient.setColorAt(0.0, QColor("#061019"))
            gradient.setColorAt(0.55, QColor("#26d0ce"))
            gradient.setColorAt(1.0, QColor("#f55e46"))
            painter.fillRect(QRectF(rect.left(), self.height() - 24, rect.width(), 7), gradient)
            painter.setPen(QColor("#66798d"))
            painter.setFont(QFont("Cascadia Mono", 7))
            painter.drawText(rect.left(), self.height() - 8, f"{low:.3g}")
            painter.drawText(rect.right() - 45, self.height() - 8, f"{high:.3g}")
        painter.setPen(QColor("#718397"))
        painter.setFont(QFont("Cascadia Mono", 8))
        note = "EXACT MATCH" if self.diff is not None and self.mismatch is None else (
            f"FIRST MISMATCH {self.mismatch.get('stage')} @ {self.mismatch.get('x')},{self.mismatch.get('y')}"
            if self.mismatch
            else ""
        )
        painter.drawText(12, self.height() - 30, note)


class SegmentDetailWidget(QWidget):
    def __init__(self, parent: QWidget | None = None):
        super().__init__(parent)
        from PySide6.QtWidgets import QGridLayout

        layout = QGridLayout(self)
        layout.setContentsMargins(0, 0, 0, 0)
        layout.setSpacing(8)
        self.fusion = ImagePanel("FUSED HDR", "radiance / log preview")
        self.chroma = ImagePanel("CHROMA BUFFER", "Y-normalized target RGB")
        self.composite = ImagePanel("SEGMENT COMPOSITE", "tone-mapped display")
        layout.addWidget(self.fusion, 0, 0)
        layout.addWidget(self.chroma, 0, 1)
        layout.addWidget(self.composite, 1, 0, 1, 2)
        self.stats = QLabel("Select a committed segment to inspect its locked buffers.")
        self.stats.setWordWrap(True)
        self.stats.setStyleSheet(
            "color:#8ea2b7;background:#0c131a;border:1px solid #1d2c39;"
            "border-radius:5px;padding:9px;font-family:'Cascadia Mono';font-size:10px;"
        )
        layout.addWidget(self.stats, 2, 0, 1, 2)
