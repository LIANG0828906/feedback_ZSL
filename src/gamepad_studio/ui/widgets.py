"""Custom-painted dashboard widgets."""

from __future__ import annotations

from collections import defaultdict
from typing import Any

import numpy as np
from PySide6.QtCore import QPointF, QRectF, QSize, Qt, Signal
from PySide6.QtGui import (
    QColor,
    QFont,
    QPainter,
    QPainterPath,
    QPen,
)
from PySide6.QtWidgets import (
    QCheckBox,
    QComboBox,
    QDialog,
    QDialogButtonBox,
    QDoubleSpinBox,
    QFormLayout,
    QLabel,
    QLineEdit,
    QSpinBox,
    QVBoxLayout,
    QWidget,
)

from ..models import AxisSpec, BindingRule, CurveSpec, DeviceSpec, InputEvent
from ..replay import ReplayResult
from ..validation import HOLD_BOUNDS, WINDOW_BOUNDS
from .theme import COLORS


def _font(size: int, weight: int = 400) -> QFont:
    font = QFont("Inter")
    font.setPixelSize(size)
    font.setWeight(QFont.Weight(weight))
    return font


class DeviceLayoutWidget(QWidget):
    def __init__(self, parent: QWidget | None = None) -> None:
        super().__init__(parent)
        self.device: DeviceSpec | None = None
        self.axis_values: dict[str, float] = {}
        self.selected_axis: str | None = None
        self.setMinimumSize(300, 275)

    def sizeHint(self) -> QSize:
        return QSize(340, 290)

    def set_device(self, device: DeviceSpec) -> None:
        self.device = device
        self.update()

    def set_axis_values(self, values: dict[str, float]) -> None:
        self.axis_values = dict(values)
        self.update()

    def set_selected_axis(self, axis_id: str | None) -> None:
        self.selected_axis = axis_id
        self.update()

    def paintEvent(self, event: Any) -> None:
        painter = QPainter(self)
        painter.setRenderHint(QPainter.RenderHint.Antialiasing)
        rect = QRectF(self.rect()).adjusted(4, 4, -4, -4)
        painter.setPen(QPen(QColor(COLORS["border"]), 1))
        painter.setBrush(QColor("#0f171d"))
        painter.drawRoundedRect(rect, 12, 12)
        if self.device is None:
            painter.setPen(QColor(COLORS["muted"]))
            painter.drawText(rect, Qt.AlignmentFlag.AlignCenter, "No device loaded")
            return
        body = rect.adjusted(16, 18, -16, -45)
        painter.setBrush(QColor("#17252e"))
        painter.setPen(QPen(QColor("#2f4857"), 1.2))
        painter.drawRoundedRect(body, 34, 34)

        sticks = [axis for axis in self.device.axes if axis.kind == "stick"]
        positions = [
            (body.left() + body.width() * 0.26, body.top() + body.height() * 0.43),
            (body.left() + body.width() * 0.74, body.top() + body.height() * 0.43),
            (body.left() + body.width() * 0.26, body.top() + body.height() * 0.70),
            (body.left() + body.width() * 0.74, body.top() + body.height() * 0.70),
        ]
        pairs = [sticks[index : index + 2] for index in range(0, len(sticks), 2)]
        for pair_index, pair in enumerate(pairs[:2]):
            center = QPointF(*positions[pair_index])
            self._draw_stick(painter, center, pair)

        triggers = [axis for axis in self.device.axes if axis.kind == "trigger"]
        for index, axis in enumerate(triggers[:2]):
            x = body.left() + 12 + index * (body.width() / 2 + 2)
            y = body.top() + 10
            self._draw_trigger(
                painter,
                QRectF(x, y, body.width() / 2 - 20, 10),
                axis,
            )

        gyros = [axis for axis in self.device.axes if axis.kind == "gyro"]
        if gyros:
            self._draw_gyro(painter, QRectF(body.right() - 78, body.bottom() - 66, 62, 50), gyros)

        button_y = body.bottom() + 18
        button_x = rect.left() + 12
        for button in self.device.buttons[:9]:
            painter.setPen(QPen(QColor("#324957"), 1))
            painter.setBrush(QColor("#1c2d37"))
            painter.drawEllipse(QRectF(button_x, button_y, 25, 20))
            painter.setFont(_font(8, 600))
            painter.setPen(QColor(COLORS["muted"]))
            painter.drawText(
                QRectF(button_x, button_y, 25, 20),
                Qt.AlignmentFlag.AlignCenter,
                button[:3].upper(),
            )
            button_x += 29
        painter.end()

    def _draw_stick(
        self,
        painter: QPainter,
        center: QPointF,
        pair: list[AxisSpec],
    ) -> None:
        radius = 31.0
        painter.setBrush(QColor("#0f171d"))
        painter.setPen(QPen(QColor("#3b5564"), 1.2))
        painter.drawEllipse(center, radius, radius)
        painter.setPen(QPen(QColor("#233640"), 1))
        painter.drawLine(
            QPointF(center.x() - radius, center.y()),
            QPointF(center.x() + radius, center.y()),
        )
        painter.drawLine(
            QPointF(center.x(), center.y() - radius),
            QPointF(center.x(), center.y() + radius),
        )
        if pair:
            deadzone = max(axis.deadzone for axis in pair)
            painter.setPen(QPen(QColor("#2c4d48"), 1, Qt.PenStyle.DashLine))
            painter.setBrush(Qt.BrushStyle.NoBrush)
            painter.drawEllipse(center, radius * deadzone, radius * deadzone)
            x_value = self.axis_values.get(pair[0].id, 0.0)
            y_value = self.axis_values.get(pair[1].id, 0.0) if len(pair) > 1 else 0.0
            dot = QPointF(
                center.x() + x_value * (radius - 5),
                center.y() - y_value * (radius - 5),
            )
            painter.setBrush(QColor(COLORS["accent"]))
            painter.setPen(QPen(QColor("#b8fff2"), 1))
            painter.drawEllipse(dot, 5, 5)
            for axis in pair:
                selected = axis.id == self.selected_axis
                painter.setPen(
                    QColor(COLORS["orange"] if selected else COLORS["muted"])
                )
                painter.setFont(_font(8, 600 if selected else 400))
                label_y = center.y() + radius + (11 if axis is pair[0] else 22)
                painter.drawText(
                    QRectF(center.x() - 50, label_y, 100, 11),
                    Qt.AlignmentFlag.AlignCenter,
                    axis.id,
                )

    def _draw_trigger(self, painter: QPainter, rect: QRectF, axis: AxisSpec) -> None:
        value = self.axis_values.get(axis.id, 0.0)
        selected = axis.id == self.selected_axis
        painter.setBrush(QColor("#0f171d"))
        painter.setPen(
            QPen(QColor(COLORS["orange"] if selected else "#324957"), 1.2)
        )
        painter.drawRoundedRect(rect, 4, 4)
        fill = QRectF(rect.left(), rect.top(), rect.width() * max(0.0, min(1.0, value)), rect.height())
        painter.setBrush(QColor(COLORS["orange"] if selected else COLORS["accent_dim"]))
        painter.setPen(Qt.PenStyle.NoPen)
        painter.drawRoundedRect(fill, 3, 3)
        painter.setPen(QColor(COLORS["muted"]))
        painter.setFont(_font(8, 600))
        painter.drawText(rect.translated(0, -12), Qt.AlignmentFlag.AlignLeft, axis.id)

    def _draw_gyro(self, painter: QPainter, rect: QRectF, gyros: list[AxisSpec]) -> None:
        center = rect.center()
        painter.setBrush(QColor("#0f171d"))
        painter.setPen(QPen(QColor("#324957"), 1))
        painter.drawEllipse(center, 20, 20)
        painter.drawEllipse(center, 9, 9)
        x = self.axis_values.get(gyros[0].id, 0.0) if gyros else 0.0
        y = self.axis_values.get(gyros[1].id, 0.0) if len(gyros) > 1 else 0.0
        painter.setBrush(QColor(COLORS["purple"]))
        painter.drawEllipse(QPointF(center.x() + x * 18, center.y() - y * 18), 3, 3)
        painter.setPen(QColor(COLORS["muted"]))
        painter.setFont(_font(8))
        painter.drawText(rect.translated(0, 26), Qt.AlignmentFlag.AlignCenter, "GYRO")


class TimelineWidget(QWidget):
    def __init__(self, parent: QWidget | None = None) -> None:
        super().__init__(parent)
        self.events: tuple[InputEvent, ...] = ()
        self.frames: tuple[Any, ...] = ()
        self.device: DeviceSpec | None = None
        self.selected_seq: int | None = None
        self.setMinimumHeight(165)

    def set_data(
        self,
        device: DeviceSpec,
        events: tuple[InputEvent, ...],
        result: ReplayResult | None = None,
    ) -> None:
        self.device = device
        self.events = events
        self.frames = result.frames if result else ()
        self.update()

    def set_selected_sequence(self, seq: int | None) -> None:
        self.selected_seq = seq
        self.update()

    def paintEvent(self, event: Any) -> None:
        painter = QPainter(self)
        painter.setRenderHint(QPainter.RenderHint.Antialiasing)
        rect = QRectF(self.rect()).adjusted(1, 1, -1, -1)
        painter.setBrush(QColor("#0f171d"))
        painter.setPen(QPen(QColor(COLORS["border"]), 1))
        painter.drawRoundedRect(rect, 8, 8)
        if not self.events or self.device is None:
            painter.setPen(QColor(COLORS["muted"]))
            painter.drawText(rect, Qt.AlignmentFlag.AlignCenter, "No events")
            return
        left = rect.left() + 76
        right = rect.right() - 14
        top = rect.top() + 24
        bottom = rect.bottom() - 12
        start_t = self.events[0].t_ms
        end_t = max(self.events[-1].t_ms, start_t + 1)

        def x_for(t_ms: int) -> float:
            return left + (t_ms - start_t) / (end_t - start_t) * (right - left)

        painter.setPen(QPen(QColor("#22323d"), 1))
        for index in range(6):
            ratio = index / 5
            x = left + ratio * (right - left)
            painter.drawLine(QPointF(x, top - 8), QPointF(x, bottom))
            painter.setPen(QColor(COLORS["muted"]))
            painter.setFont(_font(8))
            painter.drawText(
                QRectF(x - 30, rect.top() + 3, 60, 12),
                Qt.AlignmentFlag.AlignCenter,
                f"{int(start_t + ratio * (end_t - start_t))}ms",
            )
            painter.setPen(QPen(QColor("#22323d"), 1))

        axis_ids = [
            event.axis
            for event in self.events
            if event.type == "axis" and event.axis is not None
        ]
        axis_order = list(dict.fromkeys(axis_ids))
        button_order = list(
            dict.fromkeys(
                event.button
                for event in self.events
                if event.type == "button" and event.button is not None
            )
        )
        axis_lane_h = max(6.0, (bottom - top) * 0.55 / max(1, len(axis_order)))
        button_lane_h = max(6.0, (bottom - top) * 0.45 / max(1, len(button_order)))
        axis_colors = [
            COLORS["accent"],
            COLORS["blue"],
            COLORS["purple"],
            COLORS["green"],
            COLORS["orange"],
        ]
        for index, axis_id in enumerate(axis_order):
            y = top + axis_lane_h * (index + 0.5)
            painter.setPen(QColor(COLORS["muted"]))
            painter.setFont(_font(8))
            painter.drawText(
                QRectF(rect.left() + 6, y - 6, 64, 12),
                Qt.AlignmentFlag.AlignRight | Qt.AlignmentFlag.AlignVCenter,
                axis_id,
            )
            painter.setPen(QPen(QColor(COLORS["grid"]), 1))
            painter.drawLine(QPointF(left, y), QPointF(right, y))
            for input_event in self.events:
                if input_event.type != "axis" or input_event.axis != axis_id:
                    continue
                x = x_for(input_event.t_ms)
                painter.setPen(Qt.PenStyle.NoPen)
                painter.setBrush(QColor(axis_colors[index % len(axis_colors)]))
                painter.drawEllipse(QPointF(x, y), 2.5, 2.5)
        button_top = top + (bottom - top) * 0.58
        for index, button in enumerate(button_order):
            y = button_top + button_lane_h * (index + 0.5)
            painter.setPen(QColor(COLORS["muted"]))
            painter.setFont(_font(8))
            painter.drawText(
                QRectF(rect.left() + 6, y - 6, 64, 12),
                Qt.AlignmentFlag.AlignRight | Qt.AlignmentFlag.AlignVCenter,
                button,
            )
            state = False
            last_x = left
            for input_event in self.events:
                if input_event.type != "button" or input_event.button != button:
                    continue
                x = x_for(input_event.t_ms)
                if state:
                    painter.setPen(Qt.PenStyle.NoPen)
                    painter.setBrush(QColor(COLORS["accent_dim"]))
                    painter.drawRect(QRectF(last_x, y - 2.5, max(1.0, x - last_x), 5))
                state = bool(input_event.pressed)
                last_x = x
                painter.setPen(QPen(QColor(COLORS["orange"] if input_event.repeat else "#5a7280"), 1))
                painter.drawLine(QPointF(x, y - 4), QPointF(x, y + 4))

        for frame in self.frames:
            if frame.tick_kind == "event":
                continue
            x = x_for(frame.t_ms)
            painter.setPen(QPen(QColor(COLORS["orange"]), 1, Qt.PenStyle.DotLine))
            painter.drawLine(QPointF(x, top - 6), QPointF(x, bottom))

        if self.selected_seq is not None:
            selected = next(
                (event for event in self.events if event.seq == self.selected_seq),
                None,
            )
            if selected is not None:
                x = x_for(selected.t_ms)
                painter.setPen(QPen(QColor(COLORS["red"]), 1.2))
                painter.drawLine(QPointF(x, top - 8), QPointF(x, bottom))
                painter.setBrush(QColor(COLORS["red"]))
                painter.drawEllipse(QPointF(x, top - 8), 3, 3)
        painter.end()


class ActionWaveformWidget(QWidget):
    def __init__(self, parent: QWidget | None = None) -> None:
        super().__init__(parent)
        self.result: ReplayResult | None = None
        self.action_kinds: dict[str, str] = {}
        self.setMinimumHeight(260)
        self._lane_height = 32
        self._top = 34

    def sizeHint(self) -> QSize:
        lanes = len(self.result.action_ids) if self.result else 6
        return QSize(520, max(280, self._top + lanes * self._lane_height + 18))

    def set_result(
        self,
        result: ReplayResult,
        rules: Any,
    ) -> None:
        self.result = result
        self.action_kinds = {rule.action: rule.kind for rule in rules}
        self.updateGeometry()
        self.update()

    def paintEvent(self, event: Any) -> None:
        painter = QPainter(self)
        painter.setRenderHint(QPainter.RenderHint.Antialiasing)
        rect = QRectF(self.rect()).adjusted(1, 1, -1, -1)
        painter.setBrush(QColor("#0f171d"))
        painter.setPen(QPen(QColor(COLORS["border"]), 1))
        painter.drawRoundedRect(rect, 8, 8)
        if self.result is None or not self.result.action_ids:
            painter.setPen(QColor(COLORS["muted"]))
            painter.drawText(rect, Qt.AlignmentFlag.AlignCenter, "No action output")
            return
        frames = self.result.frames
        start_t = frames[0].t_ms
        end_t = max(frames[-1].t_ms, start_t + 1)
        left = rect.left() + 112
        right = rect.right() - 18
        lane_h = self._lane_height
        top = rect.top() + self._top

        def x_for(t_ms: int) -> float:
            return left + (t_ms - start_t) / (end_t - start_t) * (right - left)

        painter.setPen(QPen(QColor("#22323d"), 1))
        for index in range(5):
            x = left + index / 4 * (right - left)
            painter.drawLine(QPointF(x, top - 10), QPointF(x, rect.bottom() - 6))
        for action_index, action_id in enumerate(self.result.action_ids):
            lane_top = top + action_index * lane_h
            lane_bottom = lane_top + lane_h - 6
            lane_rect = QRectF(rect.left() + 6, lane_top, rect.width() - 12, lane_h - 6)
            painter.setPen(QPen(QColor(COLORS["grid"] if action_index % 2 == 0 else "#1c2a33"), 1))
            painter.setBrush(QColor("#121c23" if action_index % 2 == 0 else "#101920"))
            painter.drawRoundedRect(lane_rect, 4, 4)
            painter.setPen(QColor(COLORS["text"]))
            painter.setFont(_font(9, 600))
            painter.drawText(
                QRectF(rect.left() + 8, lane_top, 96, lane_h - 6),
                Qt.AlignmentFlag.AlignLeft | Qt.AlignmentFlag.AlignVCenter,
                action_id,
            )
            kind = self.action_kinds.get(action_id, "button")
            if kind == "axis":
                painter.setPen(QPen(QColor(COLORS["accent"]), 1.8))
                path = QPainterPath()
                for frame_index, frame in enumerate(frames):
                    snapshot = frame.actions.get(action_id)
                    if snapshot is None:
                        continue
                    x = x_for(frame.t_ms)
                    value = max(-1.0, min(1.0, snapshot.value))
                    y = lane_top + (lane_h - 8) * (0.5 - value * 0.42)
                    if frame_index == 0:
                        path.moveTo(x, y)
                    else:
                        path.lineTo(x, y)
                painter.drawPath(path)
            else:
                previous_t = start_t
                previous_pressed = False
                for frame in frames:
                    snapshot = frame.actions.get(action_id)
                    pressed = bool(snapshot and snapshot.pressed)
                    if previous_pressed and not pressed:
                        painter.setPen(Qt.PenStyle.NoPen)
                        painter.setBrush(QColor(COLORS["accent_dim"]))
                        painter.drawRoundedRect(
                            QRectF(
                                x_for(previous_t),
                                lane_top + 7,
                                max(1.0, x_for(frame.t_ms) - x_for(previous_t)),
                                lane_h - 19,
                            ),
                            3,
                            3,
                        )
                    if not previous_pressed and pressed:
                        previous_t = frame.t_ms
                    previous_pressed = pressed
                if previous_pressed:
                    painter.setPen(Qt.PenStyle.NoPen)
                    painter.setBrush(QColor(COLORS["accent_dim"]))
                    painter.drawRoundedRect(
                        QRectF(
                            x_for(previous_t),
                            lane_top + 7,
                            max(1.0, right - x_for(previous_t)),
                            lane_h - 19,
                        ),
                        3,
                        3,
                    )
            marker_times: list[tuple[int, bool]] = []
            for frame in frames:
                snapshot = frame.actions.get(action_id)
                if snapshot is not None and snapshot.triggered_now:
                    marker_times.append(
                        (frame.t_ms, snapshot.last_reason == "button_auto_repeat")
                    )
            for marker_t, repeat in marker_times:
                x = x_for(marker_t)
                painter.setPen(
                    QPen(
                        QColor(COLORS["red"] if repeat else COLORS["orange"]),
                        1.2 if repeat else 1,
                    )
                )
                painter.drawLine(QPointF(x, lane_top + 2), QPointF(x, lane_bottom - 1))
            snapshot = frames[-1].actions.get(action_id)
            if snapshot is not None:
                painter.setPen(QColor(COLORS["muted"]))
                painter.setFont(_font(8))
                if kind == "axis":
                    text = f"v={snapshot.value:+.3f}"
                else:
                    text = f"trig×{snapshot.trigger_count}"
                painter.drawText(
                    QRectF(right - 74, lane_top, 72, lane_h - 6),
                    Qt.AlignmentFlag.AlignRight | Qt.AlignmentFlag.AlignVCenter,
                    text,
                )
        painter.end()


class DependencyGraphWidget(QWidget):
    nodeClicked = Signal(str)

    def __init__(self, parent: QWidget | None = None) -> None:
        super().__init__(parent)
        self.nodes: dict[str, dict[str, Any]] = {}
        self.order: list[str] = []
        self.invalidations: dict[str, dict[str, Any]] = {}
        self._rects: dict[str, QRectF] = {}
        self.setMinimumSize(920, 230)

    def set_graph(
        self,
        nodes: dict[str, dict[str, Any]],
        order: list[str],
        invalidations: tuple[Any, ...],
    ) -> None:
        self.nodes = nodes
        self.order = order
        self.invalidations = {entry.node_id: entry.to_dict() for entry in invalidations}
        self.update()

    def _depth(self, node_id: str) -> int:
        node = self.nodes.get(node_id)
        if not node or not node.get("deps"):
            return 0
        return 1 + max(self._depth(dependency) for dependency in node["deps"])

    def paintEvent(self, event: Any) -> None:
        painter = QPainter(self)
        painter.setRenderHint(QPainter.RenderHint.Antialiasing)
        rect = QRectF(self.rect()).adjusted(4, 4, -4, -4)
        painter.setBrush(QColor("#0f171d"))
        painter.setPen(QPen(QColor(COLORS["border"]), 1))
        painter.drawRoundedRect(rect, 8, 8)
        if not self.nodes:
            painter.setPen(QColor(COLORS["muted"]))
            painter.drawText(rect, Qt.AlignmentFlag.AlignCenter, "No dependency graph")
            return
        layers: dict[int, list[str]] = defaultdict(list)
        for node_id in self.order:
            layers[self._depth(node_id)].append(node_id)
        max_depth = max(layers) if layers else 0
        max_rows = max(len(values) for values in layers.values()) if layers else 1
        col_width = (rect.width() - 30) / (max_depth + 1)
        row_height = min(34.0, (rect.height() - 34) / max(1, max_rows))
        self._rects = {}
        for depth, node_ids in layers.items():
            node_ids.sort()
            for row, node_id in enumerate(node_ids):
                x = rect.left() + 15 + depth * col_width
                y = rect.top() + 24 + row * row_height
                self._rects[node_id] = QRectF(
                    x,
                    y,
                    max(110, col_width - 22),
                    min(27, row_height - 6),
                )
        painter.setPen(QPen(QColor(52, 80, 95, 130), 1))
        for node_id, node in self.nodes.items():
            target = self._rects.get(node_id)
            if target is None:
                continue
            for dependency in node.get("deps", []):
                source = self._rects.get(dependency)
                if source is None:
                    continue
                start = QPointF(source.right(), source.center().y())
                end = QPointF(target.left(), target.center().y())
                mid = (start.x() + end.x()) / 2
                path = QPainterPath(start)
                path.cubicTo(
                    QPointF(mid, start.y()),
                    QPointF(mid, end.y()),
                    end,
                )
                painter.drawPath(path)
                painter.setBrush(QColor(52, 80, 95, 150))
                painter.drawEllipse(end, 2.5, 2.5)
        for node_id in self.order:
            node_rect = self._rects.get(node_id)
            if node_rect is None:
                continue
            invalidation = self.invalidations.get(node_id, {})
            if invalidation.get("recomputed"):
                fill = QColor("#4b321b")
                border = QColor(COLORS["orange"])
            elif invalidation.get("reused"):
                fill = QColor("#143328")
                border = QColor("#2d725f")
            elif invalidation.get("invalidated"):
                fill = QColor("#3f1c22")
                border = QColor(COLORS["red"])
            else:
                fill = QColor("#192630")
                border = QColor(COLORS["border"])
            painter.setBrush(fill)
            painter.setPen(QPen(border, 1.2))
            painter.drawRoundedRect(node_rect, 5, 5)
            short_id = node_id if len(node_id) <= 24 else node_id[:21] + "…"
            painter.setPen(QColor(COLORS["text"]))
            painter.setFont(_font(8, 600))
            painter.drawText(
                node_rect.adjusted(6, 0, -4, 0),
                Qt.AlignmentFlag.AlignLeft | Qt.AlignmentFlag.AlignVCenter,
                short_id,
            )
        painter.end()

    def mousePressEvent(self, event: Any) -> None:
        position = QPointF(event.position())
        for node_id, rect in self._rects.items():
            if rect.contains(position):
                self.nodeClicked.emit(node_id)
                return
        super().mousePressEvent(event)


class DiffHeatmapWidget(QWidget):
    def __init__(self, title: str, parent: QWidget | None = None) -> None:
        super().__init__(parent)
        self.title = title
        self.matrix = np.zeros((1, 1), dtype=float)
        self.action_ids: list[str] = []
        self.edges: list[int] = []
        self.badge = "NO DATA"
        self.setMinimumSize(390, 240)

    def set_matrix(
        self,
        matrix: np.ndarray,
        action_ids: list[str],
        edges: list[int],
        *,
        badge: str,
    ) -> None:
        self.matrix = np.asarray(matrix, dtype=float)
        self.action_ids = list(action_ids)
        self.edges = list(edges)
        self.badge = badge
        self.update()

    def sizeHint(self) -> QSize:
        return QSize(480, 270)

    def paintEvent(self, event: Any) -> None:
        painter = QPainter(self)
        painter.setRenderHint(QPainter.RenderHint.Antialiasing)
        rect = QRectF(self.rect()).adjusted(4, 4, -4, -4)
        painter.setBrush(QColor("#0f171d"))
        painter.setPen(QPen(QColor(COLORS["border"]), 1))
        painter.drawRoundedRect(rect, 8, 8)
        painter.setPen(QColor(COLORS["text"]))
        painter.setFont(_font(10, 600))
        painter.drawText(QRectF(rect.left() + 12, rect.top() + 8, rect.width() - 140, 18), Qt.AlignmentFlag.AlignLeft, self.title)
        badge_color = COLORS["green"] if self.badge == "IDENTICAL" else COLORS["orange"]
        painter.setPen(QPen(QColor(badge_color), 1))
        painter.setBrush(QColor("#13231e" if self.badge == "IDENTICAL" else "#2b2317"))
        painter.drawRoundedRect(
            QRectF(rect.right() - 112, rect.top() + 8, 100, 19),
            5,
            5,
        )
        painter.setPen(QColor(badge_color))
        painter.setFont(_font(8, 700))
        painter.drawText(
            QRectF(rect.right() - 112, rect.top() + 8, 100, 19),
            Qt.AlignmentFlag.AlignCenter,
            self.badge,
        )
        if self.matrix.size == 0 or not self.action_ids:
            painter.setPen(QColor(COLORS["muted"]))
            painter.drawText(rect, Qt.AlignmentFlag.AlignCenter, "No comparison data")
            return
        left = rect.left() + 102
        top = rect.top() + 44
        right = rect.right() - 12
        bottom = rect.bottom() - 26
        rows, columns = self.matrix.shape
        cell_w = (right - left) / max(1, columns)
        cell_h = (bottom - top) / max(1, rows)
        maximum = max(float(np.nanmax(self.matrix)), 1e-9)
        for row in range(rows):
            painter.setPen(QColor(COLORS["muted"]))
            painter.setFont(_font(8))
            label = self.action_ids[row] if row < len(self.action_ids) else ""
            painter.drawText(
                QRectF(rect.left() + 6, top + row * cell_h, 90, cell_h),
                Qt.AlignmentFlag.AlignRight | Qt.AlignmentFlag.AlignVCenter,
                label[:14],
            )
            for column in range(columns):
                value = float(self.matrix[row, column])
                if value <= 1e-12:
                    color = QColor("#15372d")
                else:
                    ratio = min(1.0, value / maximum)
                    if ratio < 0.5:
                        color = QColor(
                            int(33 + ratio * 320),
                            int(82 + ratio * 120),
                            int(66 - ratio * 40),
                        )
                    else:
                        color = QColor(
                            int(193 + (ratio - 0.5) * 100),
                            int(142 - (ratio - 0.5) * 130),
                            int(46 - (ratio - 0.5) * 20),
                        )
                painter.setPen(QPen(QColor("#0f171d"), 1))
                painter.setBrush(color)
                painter.drawRect(
                    QRectF(
                        left + column * cell_w,
                        top + row * cell_h,
                        max(1.0, cell_w - 1),
                        max(1.0, cell_h - 1),
                    )
                )
        if self.edges:
            painter.setPen(QColor(COLORS["muted"]))
            painter.setFont(_font(7))
            for index in range(0, len(self.edges), max(1, len(self.edges) // 5)):
                ratio = index / max(1, len(self.edges) - 1)
                x = left + ratio * (right - left)
                painter.drawText(
                    QRectF(x - 26, bottom + 5, 52, 12),
                    Qt.AlignmentFlag.AlignCenter,
                    f"{self.edges[index]}",
                )
        painter.end()


class RuleDialog(QDialog):
    def __init__(
        self,
        rule: BindingRule,
        device: DeviceSpec,
        parent: QWidget | None = None,
    ) -> None:
        super().__init__(parent)
        self.rule = rule
        self.device = device
        self.setWindowTitle(f"Edit binding · {rule.id}")
        self.setMinimumWidth(430)
        layout = QVBoxLayout(self)
        header = QLabel(f"{rule.id}  →  {rule.action}")
        header.setObjectName("SectionTitle")
        layout.addWidget(header)
        form = QFormLayout()
        self.kind_combo = QComboBox()
        self.kind_combo.addItems(["button", "combo", "long_press", "double_press", "axis"])
        self.kind_combo.setCurrentText(rule.kind)
        form.addRow("Kind", self.kind_combo)
        self.buttons_edit = QLineEdit(", ".join(rule.buttons))
        self.buttons_edit.setPlaceholderText("comma-separated button IDs")
        form.addRow("Buttons", self.buttons_edit)
        self.axis_combo = QComboBox()
        self.axis_combo.addItem("")
        self.axis_combo.addItems(list(device.axis_ids))
        if rule.source_axis:
            self.axis_combo.setCurrentText(rule.source_axis)
        form.addRow("Source axis", self.axis_combo)
        self.window_spin = QSpinBox()
        self.window_spin.setRange(0, 5000)
        self.window_spin.setSuffix(" ms")
        self.window_spin.setValue(rule.window_ms)
        form.addRow("Window", self.window_spin)
        self.hold_spin = QSpinBox()
        self.hold_spin.setRange(0, 30000)
        self.hold_spin.setSuffix(" ms")
        self.hold_spin.setValue(rule.hold_ms)
        form.addRow("Hold", self.hold_spin)
        self.curve_combo = QComboBox()
        self.curve_combo.addItems(["inherit", "linear", "power"])
        if rule.curve is None:
            self.curve_combo.setCurrentText("inherit")
        else:
            self.curve_combo.setCurrentText(rule.curve.kind)
        form.addRow("Curve", self.curve_combo)
        self.exponent_spin = QDoubleSpinBox()
        self.exponent_spin.setRange(0.1, 8.0)
        self.exponent_spin.setSingleStep(0.1)
        self.exponent_spin.setDecimals(2)
        self.exponent_spin.setValue(rule.curve.exponent if rule.curve else 1.0)
        form.addRow("Exponent", self.exponent_spin)
        self.enabled_check = QCheckBox("Enabled")
        self.enabled_check.setChecked(rule.enabled)
        form.addRow("", self.enabled_check)
        layout.addLayout(form)
        buttons = QDialogButtonBox(
            QDialogButtonBox.StandardButton.Cancel
            | QDialogButtonBox.StandardButton.Ok
        )
        buttons.accepted.connect(self.accept)
        buttons.rejected.connect(self.reject)
        layout.addWidget(buttons)
        self.kind_combo.currentTextChanged.connect(self._sync_controls)
        self._sync_controls(rule.kind)

    def _sync_controls(self, kind: str) -> None:
        self.axis_combo.setEnabled(kind == "axis")
        self.buttons_edit.setEnabled(kind != "axis")
        self.window_spin.setEnabled(kind in WINDOW_BOUNDS)
        self.hold_spin.setEnabled(kind == "long_press")
        if kind in WINDOW_BOUNDS and self.window_spin.value() == 0:
            self.window_spin.setValue(WINDOW_BOUNDS[kind][0])
        if kind == "long_press" and self.hold_spin.value() == 0:
            self.hold_spin.setValue(HOLD_BOUNDS[0])

    def result_rule(self) -> BindingRule:
        kind = self.kind_combo.currentText()
        buttons = tuple(
            item.strip()
            for item in self.buttons_edit.text().split(",")
            if item.strip()
        )
        source_axis = self.axis_combo.currentText() or None
        curve_mode = self.curve_combo.currentText()
        curve = None
        if curve_mode != "inherit":
            curve = CurveSpec(
                kind=curve_mode,
                exponent=self.exponent_spin.value(),
            )
        return BindingRule(
            id=self.rule.id,
            action=self.rule.action,
            kind=kind,
            buttons=buttons,
            source_axis=source_axis,
            window_ms=self.window_spin.value() if kind in WINDOW_BOUNDS else 0,
            hold_ms=self.hold_spin.value() if kind == "long_press" else 0,
            curve=curve,
            enabled=self.enabled_check.isChecked(),
        )


class HotSwitchDialog(QDialog):
    def __init__(
        self,
        targets: list[tuple[str, str]],
        default_time: int,
        parent: QWidget | None = None,
    ) -> None:
        super().__init__(parent)
        self.setWindowTitle("Hot-switch device layout")
        self.setMinimumWidth(390)
        layout = QVBoxLayout(self)
        title = QLabel("Deterministic reconnect boundary")
        title.setObjectName("SectionTitle")
        layout.addWidget(title)
        form = QFormLayout()
        self.target_combo = QComboBox()
        for device_id, name in targets:
            self.target_combo.addItem(f"{name} · {device_id}", device_id)
        form.addRow("Target device", self.target_combo)
        self.time_spin = QSpinBox()
        self.time_spin.setRange(0, 3_600_000)
        self.time_spin.setSuffix(" ms")
        self.time_spin.setValue(default_time)
        form.addRow("Switch at logical time", self.time_spin)
        layout.addLayout(form)
        note = QLabel(
            "Source frames at or before the boundary are frozen. Target frames after "
            "the boundary are replayed independently."
        )
        note.setWordWrap(True)
        note.setObjectName("AppSubtitle")
        layout.addWidget(note)
        buttons = QDialogButtonBox(
            QDialogButtonBox.StandardButton.Cancel
            | QDialogButtonBox.StandardButton.Ok
        )
        buttons.accepted.connect(self.accept)
        buttons.rejected.connect(self.reject)
        layout.addWidget(buttons)

    def selection(self) -> tuple[str, int]:
        return self.target_combo.currentData(), self.time_spin.value()


__all__ = [
    "ActionWaveformWidget",
    "DependencyGraphWidget",
    "DeviceLayoutWidget",
    "DiffHeatmapWidget",
    "HotSwitchDialog",
    "RuleDialog",
    "TimelineWidget",
]
