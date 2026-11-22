from __future__ import annotations

from typing import Any

import numpy as np
from PySide6.QtCore import QPointF, QRectF, Qt
from PySide6.QtGui import (
    QColor,
    QFont,
    QImage,
    QLinearGradient,
    QPainter,
    QPen,
)
from PySide6.QtWidgets import (
    QFrame,
    QLabel,
    QSizePolicy,
    QToolTip,
    QVBoxLayout,
    QWidget,
)

from ..models import PHASE_SEQUENCE, InputEvent, Phase
from .theme import PHASE_COLORS, STATUS_COLORS


def qcolor(value: str, alpha: int = 255) -> QColor:
    color = QColor(value)
    color.setAlpha(alpha)
    return color


class MirrorView(QWidget):
    def __init__(self, parent: QWidget | None = None) -> None:
        super().__init__(parent)
        self._image: QImage | None = None
        self._frame_seq = 0
        self._phase = Phase.NEGOTIATING
        self._status = ""
        self._resolution = ""
        self._codec = ""
        self.setMinimumSize(640, 360)
        self.setSizePolicy(
            QSizePolicy.Policy.Expanding, QSizePolicy.Policy.Expanding
        )

    def set_frame(
        self,
        frame: np.ndarray | None,
        frame_seq: int = 0,
        phase: str = "negotiating",
        status: str = "",
        resolution: str = "",
        codec: str = "",
    ) -> None:
        if frame is not None:
            contiguous = np.ascontiguousarray(frame)
            height, width = contiguous.shape[:2]
            image = QImage(
                contiguous.data,
                width,
                height,
                width * 4,
                QImage.Format.Format_RGBA8888,
            )
            self._image = image.copy()
        self._frame_seq = frame_seq
        self._phase = Phase(phase)
        self._status = status
        self._resolution = resolution
        self._codec = codec
        self.update()

    def paintEvent(self, event: Any) -> None:
        painter = QPainter(self)
        painter.setRenderHint(QPainter.RenderHint.Antialiasing, True)
        rect = QRectF(self.rect()).adjusted(1, 1, -1, -1)
        gradient = QLinearGradient(rect.topLeft(), rect.bottomRight())
        gradient.setColorAt(0, QColor("#050b13"))
        gradient.setColorAt(1, QColor("#0b1a2a"))
        painter.fillRect(rect, gradient)
        painter.setPen(QPen(QColor("#1a344b"), 1))
        painter.drawRoundedRect(rect, 10, 10)
        if self._image is None:
            painter.setPen(QColor("#47637b"))
            painter.setFont(QFont("Segoe UI", 13, QFont.Weight.DemiBold))
            painter.drawText(
                rect,
                Qt.AlignmentFlag.AlignCenter,
                "WAITING FOR REASSEMBLED FRAME",
            )
            painter.setPen(QColor("#263f55"))
            painter.setFont(QFont("Segoe UI", 9))
            painter.drawText(
                rect.adjusted(0, 30, 0, 0),
                Qt.AlignmentFlag.AlignCenter,
                "协商完成 → 媒体包 → 有序重组 → 渲染",
            )
            return
        available = rect.adjusted(12, 12, -12, -12)
        image_size = self._image.size()
        scaled = image_size.scaled(
            available.size().toSize(), Qt.AspectRatioMode.KeepAspectRatio
        )
        target = QRectF(
            available.center().x() - scaled.width() / 2,
            available.center().y() - scaled.height() / 2,
            scaled.width(),
            scaled.height(),
        )
        painter.fillRect(target.adjusted(-3, -3, 3, 3), QColor("#020609"))
        painter.drawImage(target, self._image)
        painter.setPen(QPen(qcolor(PHASE_COLORS.get(self._phase.value, "#49d5ff"), 150), 1))
        painter.drawRect(target)

        badge = QRectF(target.left() + 12, target.top() + 12, 102, 24)
        painter.fillRect(badge, QColor(4, 12, 20, 205))
        painter.setPen(qcolor(PHASE_COLORS.get(self._phase.value, "#49d5ff")))
        painter.setFont(QFont("Segoe UI", 9, QFont.Weight.Bold))
        painter.drawText(
            badge,
            Qt.AlignmentFlag.AlignCenter,
            f"● {self._phase.label.upper() if self._phase.value == 'mirroring' else self._phase.label}",
        )
        info = QRectF(
            target.left() + 12,
            target.bottom() - 33,
            max(280.0, target.width() - 24),
            22,
        )
        painter.fillRect(info, QColor(4, 12, 20, 190))
        painter.setPen(QColor("#a9c4d9"))
        painter.setFont(QFont("Consolas", 8))
        painter.drawText(
            info.adjusted(8, 0, -8, 0),
            Qt.AlignmentFlag.AlignVCenter | Qt.AlignmentFlag.AlignLeft,
            f"FRAME {self._frame_seq:03d}   {self._resolution}   {self._codec}"
            + (f"   {self._status.upper()}" if self._status else ""),
        )
        if self._status == "missing":
            painter.setPen(QColor("#ff8db0"))
            painter.setFont(QFont("Segoe UI", 10, QFont.Weight.Bold))
            painter.drawText(
                target.adjusted(0, 40, 0, 0),
                Qt.AlignmentFlag.AlignHCenter | Qt.AlignmentFlag.AlignTop,
                "MISSING FRAME MARKER",
            )


class PhaseTimeline(QWidget):
    def __init__(self, parent: QWidget | None = None) -> None:
        super().__init__(parent)
        self.current_phase = Phase.NEGOTIATING
        self.visited: set[str] = {"negotiating"}
        self.setMinimumHeight(82)
        self.setMaximumHeight(90)

    def set_phase(self, phase: str) -> None:
        self.current_phase = Phase(phase)
        self.visited.add(phase)
        self.update()

    def paintEvent(self, event: Any) -> None:
        painter = QPainter(self)
        painter.setRenderHint(QPainter.RenderHint.Antialiasing, True)
        width = self.width()
        margin = 26
        line_y = 28
        usable = max(1, width - margin * 2)
        step = usable / (len(PHASE_SEQUENCE) - 1)
        painter.setPen(QPen(QColor("#1a3349"), 2))
        painter.drawLine(margin, line_y, width - margin, line_y)
        current_index = PHASE_SEQUENCE.index(self.current_phase)
        painter.setPen(QPen(QColor("#2d6a89"), 2))
        if current_index > 0:
            painter.drawLine(margin, line_y, margin + step * current_index, line_y)
        for index, phase in enumerate(PHASE_SEQUENCE):
            x = margin + step * index
            color = QColor(PHASE_COLORS[phase.value])
            if phase.value == self.current_phase.value:
                painter.setPen(QPen(color, 2))
                painter.setBrush(QColor(8, 20, 32))
                painter.drawEllipse(QPointF(x, line_y), 8, 8)
                painter.setBrush(color)
                painter.drawEllipse(QPointF(x, line_y), 4, 4)
            else:
                fill = QColor("#15324a") if phase.value in self.visited else QColor("#0c1928")
                painter.setPen(QPen(color.darker(120) if phase.value in self.visited else QColor("#234056"), 1))
                painter.setBrush(fill)
                painter.drawEllipse(QPointF(x, line_y), 6, 6)
            painter.setPen(
                color
                if phase.value == self.current_phase.value
                else QColor("#7890a6")
            )
            painter.setFont(QFont("Segoe UI", 8, QFont.Weight.DemiBold))
            label_rect = QRectF(x - step / 2, line_y + 13, step, 18)
            painter.drawText(label_rect, Qt.AlignmentFlag.AlignHCenter, phase.label)
            painter.setPen(QColor("#415c73"))
            painter.setFont(QFont("Consolas", 7))
            painter.drawText(
                QRectF(x - step / 2, line_y - 23, step, 14),
                Qt.AlignmentFlag.AlignHCenter,
                f"{index + 1:02d}",
            )


class FragmentStrip(QWidget):
    def __init__(self, parent: QWidget | None = None) -> None:
        super().__init__(parent)
        self.statuses: list[str] = []
        self.frame_seq = 0
        self.setMinimumHeight(72)
        self.setMaximumHeight(78)
        self.setMouseTracking(True)

    def set_fragments(self, statuses: list[str], frame_seq: int = 0) -> None:
        self.statuses = list(statuses)
        self.frame_seq = frame_seq
        counts: dict[str, int] = {}
        for status in self.statuses:
            counts[status] = counts.get(status, 0) + 1
        summary = " · ".join(f"{key}:{value}" for key, value in sorted(counts.items()))
        self.setToolTip(f"Frame {frame_seq} · {len(self.statuses)} fragments · {summary}")
        self.update()

    def paintEvent(self, event: Any) -> None:
        painter = QPainter(self)
        painter.setRenderHint(QPainter.RenderHint.Antialiasing, True)
        painter.setPen(QColor("#6e879e"))
        painter.setFont(QFont("Segoe UI", 8, QFont.Weight.DemiBold))
        painter.drawText(0, 11, "FRAGMENT REASSEMBLY")
        if not self.statuses:
            painter.setPen(QColor("#30495f"))
            painter.drawText(
                QRectF(0, 20, self.width(), 34),
                Qt.AlignmentFlag.AlignVCenter,
                "等待首帧媒体包…",
            )
            return
        columns = min(24, len(self.statuses))
        gap = 3
        left = 0
        top = 22
        available = self.width()
        cell_width = max(4.0, (available - (columns - 1) * gap) / columns)
        cell_height = 27
        for index, status in enumerate(self.statuses[:24]):
            row = index // columns
            column = index % columns
            x = left + column * (cell_width + gap)
            y = top + row * (cell_height + gap)
            color = QColor(STATUS_COLORS.get(status, "#284861"))
            painter.setPen(QPen(color.lighter(125), 1))
            painter.setBrush(color)
            painter.drawRoundedRect(QRectF(x, y, cell_width, cell_height), 2, 2)
        if len(self.statuses) > 24:
            painter.setPen(QColor("#6e879e"))
            painter.setFont(QFont("Consolas", 7))
            painter.drawText(
                QRectF(0, 52, self.width(), 16),
                Qt.AlignmentFlag.AlignRight,
                f"+{len(self.statuses) - 24} fragments",
            )


class InputTimeline(QWidget):
    def __init__(self, parent: QWidget | None = None) -> None:
        super().__init__(parent)
        self.events: tuple[InputEvent, ...] = ()
        self.results: dict[int, dict[str, Any]] = {}
        self._points: list[tuple[float, float, InputEvent, dict[str, Any] | None]] = []
        self.setMinimumHeight(152)
        self.setMaximumHeight(168)
        self.setMouseTracking(True)

    def set_data(
        self,
        events: tuple[InputEvent, ...],
        results: dict[int, dict[str, Any]],
    ) -> None:
        self.events = events
        self.results = results
        self.update()

    def paintEvent(self, event: Any) -> None:
        painter = QPainter(self)
        painter.setRenderHint(QPainter.RenderHint.Antialiasing, True)
        width = self.width()
        height = self.height()
        left = 58
        right = 18
        top = 24
        bottom = 28
        plot_w = max(1, width - left - right)
        plot_h = max(1, height - top - bottom)
        lanes = ["click", "drag", "key", "scroll"]
        colors = {
            "click": "#49d5ff",
            "drag": "#ffb84d",
            "key": "#65e6a5",
            "scroll": "#b18cff",
        }
        painter.setFont(QFont("Consolas", 7))
        for lane_index, lane in enumerate(lanes):
            y = top + plot_h * (lane_index + 0.5) / len(lanes)
            painter.setPen(QPen(QColor("#142a3e"), 1))
            painter.drawLine(left, y, width - right, y)
            painter.setPen(QColor(colors[lane]))
            painter.setFont(QFont("Segoe UI", 8))
            painter.drawText(2, int(y + 3), lane.upper())
        if not self.events:
            painter.setPen(QColor("#30495f"))
            painter.drawText(
                QRectF(left, top, plot_w, plot_h),
                Qt.AlignmentFlag.AlignCenter,
                "等待输入回传…",
            )
            return
        max_time = max(1, self.events[-1].timestamp_ms)
        for tick in range(0, 5):
            x = left + plot_w * tick / 4
            painter.setPen(QPen(QColor("#19344a"), 1, Qt.PenStyle.DotLine))
            painter.drawLine(x, top, x, height - bottom)
            painter.setPen(QColor("#4e687f"))
            painter.setFont(QFont("Consolas", 7))
            painter.drawText(
                QRectF(x - 20, height - bottom + 5, 40, 12),
                Qt.AlignmentFlag.AlignHCenter,
                f"{max_time * tick / 4000:.1f}s",
            )
        self._points = []
        for input_event in self.events:
            lane_index = lanes.index(input_event.kind)
            x = left + plot_w * input_event.timestamp_ms / max_time
            y = top + plot_h * (lane_index + 0.5) / len(lanes)
            result = self.results.get(input_event.seq)
            status = result.get("status") if result else None
            color = QColor(colors[input_event.kind])
            if status == "rejected":
                painter.setBrush(QColor("#081421"))
                painter.setPen(QPen(QColor("#ff567f"), 2))
                painter.drawEllipse(QPointF(x, y), 5, 5)
            elif status == "duplicate":
                painter.setPen(QPen(QColor("#f2b34c"), 2))
                painter.drawLine(x - 4, y - 4, x + 4, y + 4)
                painter.drawLine(x - 4, y + 4, x + 4, y - 4)
            elif status == "replayed":
                painter.setPen(QPen(color.lighter(135), 1))
                painter.setBrush(color)
                painter.drawEllipse(QPointF(x, y), 4, 4)
            else:
                painter.setPen(QPen(color.darker(130), 1))
                painter.setBrush(QColor("#0c1a29"))
                painter.drawEllipse(QPointF(x, y), 3, 3)
            self._points.append((x, y, input_event, result))
        painter.setPen(QColor("#6e879e"))
        painter.setFont(QFont("Segoe UI", 8, QFont.Weight.DemiBold))
        painter.drawText(0, 11, "REMOTE INPUT / REPLAY TIMELINE")

    def mouseMoveEvent(self, event: Any) -> None:
        position = event.position()
        nearest = None
        best = 144.0
        for x, y, input_event, result in self._points:
            distance = (position.x() - x) ** 2 + (position.y() - y) ** 2
            if distance < best ** 2:
                best = distance**0.5
                nearest = (input_event, result)
        if nearest is not None:
            input_event, result = nearest
            status = result.get("status") if result else "pending"
            code = result.get("code") if result else None
            QToolTip.showText(
                self.mapToGlobal(position.toPoint()),
                f"#{input_event.seq} {input_event.kind} @ {input_event.timestamp_ms}ms\n"
                f"status={status} code={code or '-'}",
                self,
            )
        else:
            QToolTip.hideText()


class MetricCard(QFrame):
    def __init__(self, caption: str, value: str = "0", parent: QWidget | None = None) -> None:
        super().__init__(parent)
        self.setObjectName("MetricCard")
        layout = QVBoxLayout(self)
        layout.setContentsMargins(10, 8, 10, 8)
        layout.setSpacing(1)
        self.value_label = QLabel(value)
        self.value_label.setObjectName("MetricValue")
        self.caption_label = QLabel(caption)
        self.caption_label.setObjectName("MetricCaption")
        layout.addWidget(self.value_label)
        layout.addWidget(self.caption_label)

    def set_value(self, value: Any, accent: str | None = None) -> None:
        self.value_label.setText(str(value))
        if accent:
            self.value_label.setStyleSheet(f"color: {accent}; background: transparent;")
