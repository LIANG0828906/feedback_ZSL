"""可视化控件：时间轴/状态、波形、频谱、响度曲线、增益曲线、差异热力图。

全部基于 QPainter 自绘，不依赖额外图表服务，可在 offscreen 平台测试。
"""
from __future__ import annotations

import numpy as np
from PySide6.QtCore import QPointF, Qt
from PySide6.QtGui import QColor, QPainter, QPen, QPolygonF
from PySide6.QtWidgets import QWidget


STATE_COLORS = {
    "pending": "#8d99ae",
    "processing": "#f4a261",
    "committed": "#2a9d8f",
    "failed": "#e63946",
}


class TimelineWidget(QWidget):
    """节目段时间轴与状态色块。"""

    def __init__(self, parent=None):
        super().__init__(parent)
        self.segments: list[dict] = []
        self.setMinimumHeight(90)

    def set_segments(self, segments: list[dict]) -> None:
        self.segments = segments
        self.update()

    def paintEvent(self, event) -> None:  # noqa: N802 (Qt 命名)
        painter = QPainter(self)
        painter.fillRect(self.rect(), QColor("#1d2433"))
        if not self.segments:
            return
        margin = 12
        width = self.width() - 2 * margin
        block_w = width / len(self.segments)
        for i, seg in enumerate(self.segments):
            color = QColor(STATE_COLORS.get(seg["state"], "#8d99ae"))
            rect_x = margin + i * block_w
            painter.fillRect(int(rect_x) + 2, 20,
                             int(block_w) - 4, self.height() - 38, color)
            painter.setPen(Qt.white)
            painter.drawText(int(rect_x) + 4, 15,
                             f"#{i} {seg['name'][:10]}")
            painter.drawText(int(rect_x) + 4, self.height() - 6,
                             seg["state"])


class WaveformWidget(QWidget):
    """波形（单声道/立体声）。"""

    def __init__(self, parent=None):
        super().__init__(parent)
        self.samples: np.ndarray | None = None
        self.setMinimumHeight(140)

    def set_samples(self, samples: np.ndarray) -> None:
        self.samples = samples
        self.update()

    def paintEvent(self, event) -> None:  # noqa: N802
        painter = QPainter(self)
        painter.fillRect(self.rect(), QColor("#10151f"))
        if self.samples is None or self.samples.size == 0:
            return
        x = self.samples
        mono = x if x.ndim == 1 else x.mean(axis=1)
        # 抽稀到像素宽度，保证大文件也流畅且确定性
        target = max(self.width(), 64)
        step = max(1, mono.size // target)
        points = mono[::step][:target]
        mid = self.height() / 2
        scale = mid * 0.9
        pen = QPen(QColor("#7bdff2"))
        pen.setWidth(1)
        painter.setPen(pen)
        polygon = QPolygonF()
        for i, value in enumerate(points):
            px = i / max(len(points) - 1, 1) * self.width()
            polygon.append(QPointF(px, mid - float(value) * scale))
        painter.drawPolyline(polygon)


class SpectrumWidget(QWidget):
    """幅度谱（FFT 对数幅度）。"""

    def __init__(self, parent=None):
        super().__init__(parent)
        self.spectrum: np.ndarray | None = None
        self.setMinimumHeight(140)

    def set_samples(self, samples: np.ndarray) -> None:
        mono = samples if samples.ndim == 1 else samples.mean(axis=1)
        windowed = mono * np.hanning(mono.size)
        spec = np.abs(np.fft.rfft(windowed))
        self.spectrum = 20 * np.log10(np.maximum(spec, 1e-12))
        self.update()

    def paintEvent(self, event) -> None:  # noqa: N802
        painter = QPainter(self)
        painter.fillRect(self.rect(), QColor("#10151f"))
        if self.spectrum is None:
            return
        spec = self.spectrum
        target = max(self.width(), 64)
        step = max(1, spec.size // target)
        values = spec[::step][:target]
        span = max(float(values.max() - values.min()), 1.0)
        painter.setPen(QPen(QColor("#f2b134")))
        for i, value in enumerate(values):
            x = int(i / max(len(values) - 1, 1) * self.width())
            h = int((float(value) - values.min()) / span *
                    (self.height() - 10))
            painter.drawLine(x, self.height(), x, self.height() - h)


class CurveWidget(QWidget):
    """通用曲线：逐段响度/真峰值或压缩增益曲线。"""

    def __init__(self, parent=None, title: str = ""):
        super().__init__(parent)
        self.title = title
        self.series: dict[str, list[float]] = {}
        self.setMinimumHeight(150)

    def set_series(self, series: dict[str, list[float]]) -> None:
        self.series = series
        self.update()

    def paintEvent(self, event) -> None:  # noqa: N802
        painter = QPainter(self)
        painter.fillRect(self.rect(), QColor("#10151f"))
        painter.setPen(Qt.white)
        painter.drawText(8, 14, self.title)
        if not self.series:
            return
        colors = ("#7bdff2", "#f28482", "#80ed99", "#ffd166")
        all_values = [v for vals in self.series.values()
                      for v in vals if np.isfinite(v)]
        if not all_values:
            return
        lo, hi = min(all_values), max(all_values)
        span = max(hi - lo, 1e-6)
        for color_idx, (name, values) in enumerate(self.series.items()):
            painter.setPen(QPen(QColor(colors[color_idx % len(colors)])))
            polygon = QPolygonF()
            finite = [v if np.isfinite(v) else lo for v in values]
            for i, value in enumerate(finite):
                px = i / max(len(finite) - 1, 1) * self.width()
                py = self.height() - 24 - (
                    (float(value) - lo) / span) * (self.height() - 40)
                polygon.append(QPointF(px, py))
            painter.drawPolyline(polygon)
            painter.drawText(8, 30 + color_idx * 14,
                             f"{name} [{lo:.1f}, {hi:.1f}]")


class DiffHeatmapWidget(QWidget):
    """差异热力图：两条路径的逐段/逐块绝对差。"""

    def __init__(self, parent=None):
        super().__init__(parent)
        self.matrix: np.ndarray | None = None
        self.setMinimumHeight(150)

    def set_diff(self, audio_a: np.ndarray, audio_b: np.ndarray,
                 bins: int = 64) -> None:
        mono_a = audio_a if audio_a.ndim == 1 else audio_a.mean(axis=1)
        mono_b = audio_b if audio_b.ndim == 1 else audio_b.mean(axis=1)
        n = min(mono_a.size, mono_b.size)
        diff = np.abs(mono_a[:n] - mono_b[:n])
        block = max(1, n // bins)
        trimmed = diff[:block * bins]
        self.matrix = trimmed.reshape(bins, -1).mean(axis=1)
        self.update()

    def paintEvent(self, event) -> None:  # noqa: N802
        painter = QPainter(self)
        painter.fillRect(self.rect(), QColor("#10151f"))
        if self.matrix is None or self.matrix.size == 0:
            painter.setPen(Qt.white)
            painter.drawText(8, 20, "差异热力图（无数据）")
            return
        peak = max(float(self.matrix.max()), 1e-12)
        cell_w = self.width() / self.matrix.size
        for i, value in enumerate(self.matrix):
            intensity = float(value) / peak
            color = QColor.fromHsvF(0.0, intensity, 1.0 if intensity else 0.3)
            painter.fillRect(int(i * cell_w), 10,
                             int(cell_w) + 1, self.height() - 20, color)
        painter.setPen(Qt.white)
        painter.drawText(8, self.height() - 4,
                         f"max |Δ|={peak:.3e}（全零表示逐样本一致）")
