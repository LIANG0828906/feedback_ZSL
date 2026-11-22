"""Custom waveform and before/after spectrogram widgets."""

from __future__ import annotations

import numpy as np
from PySide6.QtCore import QPointF, QRectF, Qt
from PySide6.QtGui import QColor, QFont, QImage, QPainter, QPen
from PySide6.QtWidgets import QSizePolicy, QWidget

from ..config import RepairConfig
from ..dsp import analysis_frames


def _as_2d(samples: np.ndarray | None) -> np.ndarray | None:
    if samples is None:
        return None
    data = np.asarray(samples, dtype=np.float64)
    if data.ndim == 1:
        data = data[:, None]
    return data


def _envelope(data: np.ndarray, width: int) -> tuple[np.ndarray, np.ndarray]:
    flat = np.mean(data, axis=1) if data.shape[1] > 1 else data[:, 0]
    if flat.size <= width:
        indexes = np.linspace(0, flat.size - 1, width)
        values = np.interp(indexes, np.arange(flat.size), flat)
        return values, values
    edges = np.linspace(0, flat.size, width + 1).astype(np.int64)
    minima = np.empty(width, dtype=np.float64)
    maxima = np.empty(width, dtype=np.float64)
    for index in range(width):
        chunk = flat[edges[index] : max(edges[index] + 1, edges[index + 1])]
        minima[index] = float(np.min(chunk))
        maxima[index] = float(np.max(chunk))
    return minima, maxima


class WaveformView(QWidget):
    def __init__(self, parent: QWidget | None = None) -> None:
        super().__init__(parent)
        self.setMinimumHeight(190)
        self.setSizePolicy(QSizePolicy.Policy.Expanding, QSizePolicy.Policy.Expanding)
        self._original: np.ndarray | None = None
        self._repaired: np.ndarray | None = None
        self._title = "Select a segment"
        self._sample_rate = 0
        self._range_text = ""

    def set_audio(
        self,
        original: np.ndarray | None,
        repaired: np.ndarray | None,
        sample_rate: int,
        *,
        title: str = "",
        range_text: str = "",
    ) -> None:
        self._original = _as_2d(original)
        self._repaired = _as_2d(repaired)
        self._sample_rate = int(sample_rate)
        self._title = title or "Segment"
        self._range_text = range_text
        self.update()

    def paintEvent(self, event: object) -> None:  # noqa: N802 - Qt API
        del event
        painter = QPainter(self)
        painter.setRenderHint(QPainter.RenderHint.Antialiasing, False)
        rect = QRectF(self.rect()).adjusted(14, 12, -14, -14)
        painter.fillRect(self.rect(), QColor("#0d1219"))
        painter.setPen(QPen(QColor("#222c38"), 1))
        for fraction in (0.25, 0.5, 0.75):
            x = rect.left() + rect.width() * fraction
            painter.drawLine(QPointF(x, rect.top()), QPointF(x, rect.bottom()))
        for fraction in (0.25, 0.5, 0.75):
            y = rect.top() + rect.height() * fraction
            painter.drawLine(QPointF(rect.left(), y), QPointF(rect.right(), y))

        painter.setPen(QPen(QColor("#435164"), 1))
        painter.drawRect(rect)
        baseline = rect.center().y()
        painter.setPen(QPen(QColor("#2b3746"), 1))
        painter.drawLine(QPointF(rect.left(), baseline), QPointF(rect.right(), baseline))

        painter.setFont(QFont("DejaVu Sans", 9, QFont.Weight.DemiBold))
        painter.setPen(QColor("#dce7f3"))
        painter.drawText(rect.adjusted(6, 5, -6, 0), Qt.AlignmentFlag.AlignLeft, self._title)
        painter.setFont(QFont("DejaVu Sans", 8))
        painter.setPen(QColor("#7f91a6"))
        if self._range_text:
            painter.drawText(
                rect.adjusted(6, 5, -6, 0),
                Qt.AlignmentFlag.AlignRight,
                self._range_text,
            )

        data_rect = rect.adjusted(0, 24, 0, -4)
        width = max(1, int(data_rect.width()))
        if self._original is not None and self._original.size:
            self._draw_envelope(
                painter, data_rect, self._original, QColor(119, 135, 153, 135), width, 1.2
            )
        if self._repaired is not None and self._repaired.size:
            self._draw_envelope(
                painter, data_rect, self._repaired, QColor(54, 221, 187, 230), width, 1.4
            )

        painter.setFont(QFont("DejaVu Sans", 8))
        painter.setPen(QColor("#8da1b6"))
        label_y = rect.bottom() - 3
        painter.drawText(
            QRectF(rect.left() + 6, label_y - 14, 74, 14),
            Qt.AlignmentFlag.AlignLeft,
            "ORIGINAL",
        )
        painter.setPen(QColor("#36ddbb"))
        painter.drawText(
            QRectF(rect.left() + 70, label_y - 14, 90, 14),
            Qt.AlignmentFlag.AlignLeft,
            "REPAIRED",
        )
        if self._sample_rate:
            painter.setPen(QColor("#637387"))
            painter.drawText(
                QRectF(rect.right() - 220, label_y - 14, 214, 14),
                Qt.AlignmentFlag.AlignRight,
                f"{self._sample_rate:,} Hz",
            )


    @staticmethod
    def _draw_envelope(
        painter: QPainter,
        rect: QRectF,
        data: np.ndarray,
        color: QColor,
        width: int,
        line_width: float,
    ) -> None:
        minima, maxima = _envelope(data, width)
        peak = max(
            float(np.max(np.abs(minima))),
            float(np.max(np.abs(maxima))),
            1e-6,
        )
        peak = max(peak, 0.05)
        center = rect.center().y()
        scale = rect.height() * 0.45 / peak
        painter.setPen(QPen(color, line_width))
        for index in range(width):
            x = rect.left() + index + 0.5
            top = center - maxima[index] * scale
            bottom = center - minima[index] * scale
            if bottom - top < 1.0:
                bottom = top + 1.0
            painter.drawLine(QPointF(x, top), QPointF(x, bottom))


class SpectrogramView(QWidget):
    def __init__(self, parent: QWidget | None = None) -> None:
        super().__init__(parent)
        self.setMinimumHeight(250)
        self.setSizePolicy(QSizePolicy.Policy.Expanding, QSizePolicy.Policy.Expanding)
        self._original_image: QImage | None = None
        self._repaired_image: QImage | None = None
        self._title = "Spectrum comparison"
        self._dB_floor = -78.0

    def clear(self) -> None:
        self._original_image = None
        self._repaired_image = None
        self.update()

    def set_audio(
        self,
        original: np.ndarray | None,
        repaired: np.ndarray | None,
        sample_rate: int,
        *,
        title: str = "",
        config: RepairConfig | None = None,
    ) -> None:
        self._title = title or "Spectrum comparison"
        if original is None or np.asarray(original).size == 0:
            self.clear()
            return
        display_config = config or RepairConfig(frame_size=512, hop_size=128)
        frame_size = min(512, max(128, display_config.frame_size // 2))
        hop_size = max(32, frame_size // 4)
        original_db = self._spectrum_db(original, frame_size, hop_size)
        reference = self._reference_max(original_db)
        repaired_db = (
            self._spectrum_db(repaired, frame_size, hop_size)
            if repaired is not None and np.asarray(repaired).size
            else None
        )
        self._original_image = self._to_image(original_db, reference)
        self._repaired_image = (
            self._to_image(repaired_db, reference) if repaired_db is not None else None
        )
        self._sample_rate = sample_rate
        self.update()

    @staticmethod
    def _spectrum_db(data: np.ndarray, frame_size: int, hop_size: int) -> np.ndarray:
        array = _as_2d(data)
        assert array is not None
        mono = np.mean(array, axis=1)
        frames, _pad = analysis_frames(mono, frame_size, hop_size)
        window = np.hanning(frame_size)
        spectrum = np.abs(np.fft.rfft(frames[:, :, 0] * window, axis=1))
        power = spectrum * spectrum
        return 10.0 * np.log10(np.maximum(power, 1e-12)).T

    @staticmethod
    def _reference_max(db: np.ndarray) -> float:
        return float(np.percentile(db, 99.5))

    def _to_image(self, db: np.ndarray, reference: float) -> QImage:
        normalized = np.clip((db - (reference + self._dB_floor)) / abs(self._dB_floor), 0.0, 1.0)
        # Dark blue -> teal -> amber.  Frequencies are flipped so low bins sit
        # at the bottom of the widget.
        normalized = normalized[::-1, :]
        red = np.clip(1.65 * normalized - 0.55, 0.0, 1.0)
        green = np.clip(1.55 * normalized - 0.08, 0.0, 1.0)
        blue = np.clip(0.78 * (1.0 - normalized) + 0.18 * normalized, 0.0, 1.0)
        alpha = np.full_like(red, 1.0)
        rgba = np.stack(
            [
                (blue * 255).astype(np.uint8),
                (green * 255).astype(np.uint8),
                (red * 255).astype(np.uint8),
                (alpha * 255).astype(np.uint8),
            ],
            axis=2,
        )
        rgba = np.ascontiguousarray(rgba)
        height, width = rgba.shape[:2]
        image = QImage(
            rgba.data,
            width,
            height,
            int(rgba.strides[0]),
            QImage.Format.Format_RGB32,
        )
        return image.copy()

    def paintEvent(self, event: object) -> None:  # noqa: N802 - Qt API
        del event
        painter = QPainter(self)
        painter.fillRect(self.rect(), QColor("#0d1219"))
        outer = QRectF(self.rect()).adjusted(14, 12, -14, -14)
        painter.setFont(QFont("DejaVu Sans", 9, QFont.Weight.DemiBold))
        painter.setPen(QColor("#dce7f3"))
        painter.drawText(
            QRectF(outer.left(), outer.top(), outer.width(), 18),
            Qt.AlignmentFlag.AlignLeft,
            self._title,
        )
        image_area = outer.adjusted(0, 24, 0, -18)
        gap = 10.0
        panel_width = (image_area.width() - gap) / 2.0
        left_rect = QRectF(image_area.left(), image_area.top(), panel_width, image_area.height())
        right_rect = QRectF(
            image_area.left() + panel_width + gap,
            image_area.top(),
            panel_width,
            image_area.height(),
        )
        for panel, image, label in (
            (left_rect, self._original_image, "ORIGINAL SPECTRUM"),
            (right_rect, self._repaired_image, "REPAIRED SPECTRUM"),
        ):
            painter.fillRect(panel, QColor("#070a0e"))
            if image is not None:
                painter.drawImage(panel, image)
            else:
                painter.setPen(QColor("#4d5c6d"))
                painter.drawText(panel, Qt.AlignmentFlag.AlignCenter, "NO PREVIEW")
            painter.setPen(QPen(QColor("#2a3644"), 1))
            painter.drawRect(panel)
            painter.setFont(QFont("DejaVu Sans", 8))
            painter.setPen(QColor("#91a3b7"))
            painter.drawText(
                QRectF(panel.left() + 6, panel.bottom() - 17, panel.width() - 12, 14),
                Qt.AlignmentFlag.AlignLeft,
                label,
            )
        painter.setFont(QFont("DejaVu Sans", 8))
        painter.setPen(QColor("#66778b"))
        painter.drawText(
            QRectF(outer.left(), outer.bottom() - 14, outer.width(), 14),
            Qt.AlignmentFlag.AlignRight,
            f"dB floor {self._dB_floor:.0f} · shared reference",
        )

