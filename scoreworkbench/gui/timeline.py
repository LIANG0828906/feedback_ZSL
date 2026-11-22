"""Timeline widget rendering velocity, legato and pedal expression lanes."""

from __future__ import annotations

from PySide6.QtCore import Qt, QPointF, Signal
from PySide6.QtGui import QColor, QPainter, QPen
from PySide6.QtWidgets import QSizePolicy, QWidget


class TimelineWidget(QWidget):
    seek_requested = Signal(float)

    LANE_HEIGHT = 70
    PADDING = 24

    def __init__(self, parent=None):
        super().__init__(parent)
        self._engine = None
        self._playhead_beat = 0.0
        self.setMinimumHeight(self.LANE_HEIGHT * 3 + self.PADDING * 2)
        self.setSizePolicy(QSizePolicy.Expanding, QSizePolicy.Expanding)
        self.setMouseTracking(True)

    def set_engine(self, engine) -> None:
        self._engine = engine
        self.update()

    def set_playhead(self, beat: float) -> None:
        self._playhead_beat = float(beat)
        self.update()

    def mousePressEvent(self, event) -> None:
        if self._engine is None:
            return
        total = max(self._engine.total_beats, 1e-9)
        ratio = (event.position().x() - self.PADDING) / max(
            self.width() - 2 * self.PADDING, 1
        )
        beat = min(max(ratio, 0.0), 1.0) * total
        self.seek_requested.emit(beat)

    def _x_for_beat(self, beat: float, total: float) -> float:
        return self.PADDING + beat / max(total, 1e-9) * (
            self.width() - 2 * self.PADDING
        )

    def paintEvent(self, event) -> None:
        painter = QPainter(self)
        painter.setRenderHint(QPainter.Antialiasing)
        painter.fillRect(self.rect(), QColor("#1e1f26"))
        if self._engine is None:
            painter.setPen(QColor("#9aa0b4"))
            painter.drawText(self.rect(), Qt.AlignCenter, "No score loaded")
            return
        total = max(self._engine.total_beats, 1e-9)
        lanes = [
            ("Velocity", self._engine.velocity_curve(), QColor("#5db1ff")),
            ("Pedal", self._engine.pedal_curve(), QColor("#7ee08b")),
        ]
        for lane_index, (label, curve, color) in enumerate(lanes):
            top = self.PADDING + lane_index * self.LANE_HEIGHT
            self._paint_lane(painter, label, top, color, curve, total)
        legato_top = self.PADDING + 2 * self.LANE_HEIGHT
        self._paint_legato(painter, legato_top, total)
        playhead_x = self._x_for_beat(self._playhead_beat, total)
        pen = QPen(QColor("#ffd166"), 2)
        painter.setPen(pen)
        painter.drawLine(
            QPointF(playhead_x, self.PADDING - 6),
            QPointF(playhead_x, self.height() - self.PADDING + 6),
        )

    def _paint_lane(self, painter, label, top, color, curve, total) -> None:
        painter.setPen(QColor("#c8cdda"))
        painter.drawText(4, top + 16, label)
        painter.setPen(QPen(QColor("#3a3d4a"), 1))
        painter.drawLine(
            self.PADDING, top + self.LANE_HEIGHT - 18,
            self.width() - self.PADDING, top + self.LANE_HEIGHT - 18,
        )
        pen = QPen(color, 2)
        painter.setPen(pen)
        previous = None
        for index, value in enumerate(curve):
            beat = total * index / max(len(curve) - 1, 1)
            x = self._x_for_beat(beat, total)
            y = top + self.LANE_HEIGHT - 18 - float(value) * (
                self.LANE_HEIGHT - 30
            )
            point = QPointF(x, y)
            if previous is not None:
                painter.drawLine(previous, point)
            previous = point

    def _paint_legato(self, painter, top, total) -> None:
        painter.setPen(QColor("#c8cdda"))
        painter.drawText(4, top + 16, "Legato")
        painter.setPen(QPen(QColor("#c792ea"), 6))
        for start, end, _event_id in self._engine.legato_spans():
            x1 = self._x_for_beat(start, total)
            x2 = self._x_for_beat(end, total)
            y = top + self.LANE_HEIGHT - 18
            painter.drawLine(QPointF(x1, y), QPointF(x2, y))
