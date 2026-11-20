from __future__ import annotations

from dataclasses import replace

import numpy as np
from PySide6.QtCore import QPointF, QRectF, Qt, Signal
from PySide6.QtGui import QColor, QFont, QKeyEvent, QMouseEvent, QPainter, QPen, QWheelEvent
from PySide6.QtWidgets import QWidget

from .models import Interval


class TimelineWidget(QWidget):
    intervalsEdited = Signal(list)
    selectionChanged = Signal(int)
    message = Signal(str)

    def __init__(self) -> None:
        super().__init__()
        self.setMinimumHeight(300)
        self.setFocusPolicy(Qt.FocusPolicy.StrongFocus)
        self.setMouseTracking(True)
        self.samples: np.ndarray | None = None
        self.sample_rate = 48_000
        self.intervals: list[Interval] = []
        self.selected_id: int | None = None
        self.pixels_per_second = 220.0
        self.view_start = 0.0
        self._drag_mode: str | None = None
        self._drag_origin: tuple[QPointF, list[Interval], int] | None = None
        self._pan_origin: tuple[float, float] | None = None
        self._drag_changed = False
        self._progress_block: int | None = None
        self._progress_total = 0
        self._block_size = 4096

    @property
    def duration(self) -> float:
        if self.samples is None or self.sample_rate <= 0:
            return 0.0
        return self.samples.shape[0] / float(self.sample_rate)

    def set_audio(self, samples: np.ndarray | None, sample_rate: int, block_size: int = 4096) -> None:
        self.samples = samples
        self.sample_rate = sample_rate
        self._block_size = block_size
        self.view_start = 0.0
        self._progress_block = None
        self.update()

    def set_intervals(self, intervals: list[Interval], selected_id: int | None = None) -> None:
        self.intervals = sorted(intervals, key=lambda item: (item.start_frame, item.end_frame, item.id))
        if selected_id is not None and not any(item.id == selected_id for item in self.intervals):
            selected_id = None
        self.selected_id = selected_id
        self.update()

    def set_selected(self, interval_id: int | None) -> None:
        self.selected_id = interval_id
        self.update()

    def set_zoom(self, pixels_per_second: float) -> None:
        anchor_time = self.view_start + (self.width() / max(1.0, self.pixels_per_second)) / 2.0
        self.pixels_per_second = max(20.0, min(5000.0, pixels_per_second))
        self.view_start = self._clamp_view_start(
            anchor_time - (self.width() / max(1.0, self.pixels_per_second)) / 2.0
        )
        self.update()

    def set_progress(self, block_index: int | None, total: int = 0) -> None:
        self._progress_block = block_index
        self._progress_total = total
        self.update()

    def _clamp_view_start(self, value: float) -> float:
        visible = self.width() / max(1.0, self.pixels_per_second)
        return max(0.0, min(value, max(0.0, self.duration - visible)))

    def _time_to_x(self, seconds: float) -> float:
        return (seconds - self.view_start) * self.pixels_per_second

    def _x_to_time(self, x: float) -> float:
        return max(0.0, min(self.duration, self.view_start + x / max(1.0, self.pixels_per_second)))

    def _lane_rects(self) -> list[QRectF]:
        top = 28.0
        height = max(20.0, self.height() - top - 8.0)
        channels = 1 if self.samples is None else self.samples.shape[1]
        lane_height = height / channels
        return [
            QRectF(0.0, top + lane * lane_height, self.width(), lane_height)
            for lane in range(channels)
        ]

    def paintEvent(self, event) -> None:  # noqa: N802
        painter = QPainter(self)
        painter.fillRect(self.rect(), QColor("#11161d"))
        painter.setRenderHint(QPainter.RenderHint.Antialiasing, False)
        lanes = self._lane_rects()
        for index, lane in enumerate(lanes):
            painter.fillRect(lane, QColor("#161d26") if index % 2 == 0 else QColor("#131a22"))

        self._draw_ruler(painter)
        self._draw_intervals(painter, lanes)
        self._draw_waveform(painter, lanes)
        self._draw_intervals_outline(painter, lanes)
        self._draw_progress(painter, lanes)

        if self.samples is None:
            painter.setPen(QColor("#8593a5"))
            font = QFont()
            font.setPointSize(12)
            painter.setFont(font)
            painter.drawText(self.rect(), Qt.AlignmentFlag.AlignCenter, "打开 WAV 或会话以显示波形")
        painter.end()

    def _draw_ruler(self, painter: QPainter) -> None:
        painter.fillRect(QRectF(0, 0, self.width(), 28), QColor("#0d1218"))
        painter.setPen(QPen(QColor("#3c4b5d"), 1))
        painter.drawLine(0, 27, self.width(), 27)
        visible = self.width() / max(1.0, self.pixels_per_second)
        candidates = [0.05, 0.1, 0.2, 0.5, 1.0, 2.0, 5.0, 10.0, 20.0, 30.0, 60.0]
        step = candidates[-1]
        for candidate in candidates:
            if candidate * self.pixels_per_second >= 72.0:
                step = candidate
                break
        start_tick = np.ceil(self.view_start / step) * step
        tick = start_tick
        painter.setPen(QColor("#93a3b7"))
        font = QFont()
        font.setPointSize(8)
        painter.setFont(font)
        while tick <= self.view_start + visible + step * 0.5:
            x = self._time_to_x(float(tick))
            painter.setPen(QPen(QColor("#344252"), 1))
            painter.drawLine(int(x), 18, int(x), 27)
            painter.setPen(QColor("#93a3b7"))
            painter.drawText(int(x) + 3, 13, f"{tick:g}s")
            tick += step

    def _draw_waveform(self, painter: QPainter, lanes: list[QRectF]) -> None:
        if self.samples is None or self.samples.size == 0:
            return
        visible = self.width() / max(1.0, self.pixels_per_second)
        frame_start = int(self.view_start * self.sample_rate)
        frame_end = min(
            self.samples.shape[0], int((self.view_start + visible) * self.sample_rate) + 2
        )
        if frame_end <= frame_start:
            return
        frames_per_pixel = (frame_end - frame_start) / max(1, self.width())
        for channel, lane in enumerate(lanes[: self.samples.shape[1]]):
            center = lane.center().y()
            amplitude = max(1.0, lane.height() * 0.44)
            painter.setPen(QPen(QColor("#56a8df"), 1))
            for x in range(max(0, self.width())):
                start = frame_start + int(x * frames_per_pixel)
                end = frame_start + int((x + 1) * frames_per_pixel) + 1
                if start >= self.samples.shape[0]:
                    break
                end = min(end, self.samples.shape[0])
                if end <= start:
                    continue
                stride = max(1, (end - start) // 96)
                chunk = self.samples[start:end:stride, channel]
                if chunk.size == 0:
                    continue
                low = float(chunk.min()) / 32768.0
                high = float(chunk.max()) / 32768.0
                y1 = center - high * amplitude
                y2 = center - low * amplitude
                if abs(y2 - y1) < 1.0:
                    y2 = y1 + 1.0
                painter.drawLine(int(x), int(y1), int(x), int(y2))

    def _draw_intervals(self, painter: QPainter, lanes: list[QRectF]) -> None:
        if not lanes:
            return
        top = lanes[0].top()
        bottom = lanes[-1].bottom()
        for interval in self.intervals:
            start_x = self._time_to_x(interval.start_frame / self.sample_rate)
            end_x = self._time_to_x(interval.end_frame / self.sample_rate)
            if end_x < 0 or start_x > self.width():
                continue
            color = QColor(66, 148, 111, 55)
            if interval.id == self.selected_id:
                color = QColor(72, 177, 224, 75)
            painter.fillRect(QRectF(start_x, top, max(1.0, end_x - start_x), bottom - top), color)

    def _draw_intervals_outline(self, painter: QPainter, lanes: list[QRectF]) -> None:
        if not lanes:
            return
        top = lanes[0].top()
        bottom = lanes[-1].bottom()
        font = QFont()
        font.setPointSize(8)
        painter.setFont(font)
        for interval in self.intervals:
            start_x = self._time_to_x(interval.start_frame / self.sample_rate)
            end_x = self._time_to_x(interval.end_frame / self.sample_rate)
            if end_x < 0 or start_x > self.width():
                continue
            selected = interval.id == self.selected_id
            pen = QPen(
                QColor("#6ed3a3") if not selected else QColor("#61c7ff"),
                2 if selected else 1,
            )
            painter.setPen(pen)
            painter.drawRect(QRectF(start_x, top, max(1.0, end_x - start_x), bottom - top))
            label = f"#{interval.id}  {interval.gain_db:+.1f} dB"
            painter.fillRect(QRectF(start_x + 2, top + 2, 94, 17), QColor(10, 16, 22, 210))
            painter.drawText(int(start_x) + 5, int(top) + 15, label)
            if selected:
                painter.fillRect(QRectF(start_x - 3, top, 6, bottom - top), QColor("#61c7ff"))
                painter.fillRect(QRectF(end_x - 3, top, 6, bottom - top), QColor("#61c7ff"))

    def _draw_progress(self, painter: QPainter, lanes: list[QRectF]) -> None:
        if self._progress_block is None or self.samples is None:
            return
        frame = self._progress_block * self._block_size
        x = self._time_to_x(frame / self.sample_rate)
        if 0 <= x <= self.width():
            painter.setPen(QPen(QColor("#f3b95f"), 1, Qt.PenStyle.DashLine))
            painter.drawLine(int(x), 28, int(x), int(lanes[-1].bottom()) if lanes else self.height())

    def _hit_test(self, position: QPointF) -> tuple[int, str] | None:
        if not self.intervals:
            return None
        tolerance = 6.0
        for index in range(len(self.intervals) - 1, -1, -1):
            interval = self.intervals[index]
            start_x = self._time_to_x(interval.start_frame / self.sample_rate)
            end_x = self._time_to_x(interval.end_frame / self.sample_rate)
            if abs(position.x() - start_x) <= tolerance:
                return index, "left"
            if abs(position.x() - end_x) <= tolerance:
                return index, "right"
            if start_x <= position.x() <= end_x and position.y() >= 28:
                return index, "move"
        return None

    def mousePressEvent(self, event: QMouseEvent) -> None:  # noqa: N802
        self.setFocus()
        if event.button() == Qt.MouseButton.MiddleButton:
            self._pan_origin = (event.position().x(), self.view_start)
            event.accept()
            return
        if event.button() != Qt.MouseButton.LeftButton:
            return
        hit = self._hit_test(event.position())
        if hit is None:
            self.selected_id = None
            self.selectionChanged.emit(-1)
            self.update()
            return
        index, mode = hit
        interval = self.intervals[index]
        self.selected_id = interval.id
        self.selectionChanged.emit(interval.id)
        self._drag_mode = mode
        self._drag_origin = (event.position(), [replace(item) for item in self.intervals], index)
        self._drag_changed = False
        self.update()

    def mouseMoveEvent(self, event: QMouseEvent) -> None:  # noqa: N802
        if self._pan_origin is not None:
            origin_x, origin_start = self._pan_origin
            delta_seconds = (event.position().x() - origin_x) / max(1.0, self.pixels_per_second)
            self.view_start = self._clamp_view_start(origin_start - delta_seconds)
            self.update()
            return
        if self._drag_mode is None or self._drag_origin is None or self.samples is None:
            return
        origin_position, original_intervals, index = self._drag_origin
        delta_frames = int(
            round(
                (event.position().x() - origin_position.x())
                / max(1.0, self.pixels_per_second)
                * self.sample_rate
            )
        )
        original = original_intervals[index]
        total_frames = self.samples.shape[0]
        previous_end = original_intervals[index - 1].end_frame if index > 0 else 0
        next_start = (
            original_intervals[index + 1].start_frame
            if index + 1 < len(original_intervals)
            else total_frames
        )
        updated = original
        if self._drag_mode == "move":
            minimum_delta = previous_end - original.start_frame
            maximum_delta = next_start - original.end_frame
            applied = max(minimum_delta, min(maximum_delta, delta_frames))
            updated = replace(
                original,
                start_frame=original.start_frame + applied,
                end_frame=original.end_frame + applied,
            )
        elif self._drag_mode == "left":
            start = max(previous_end, min(original.end_frame - 1, original.start_frame + delta_frames))
            updated = replace(original, start_frame=start)
        elif self._drag_mode == "right":
            end = min(next_start, max(original.start_frame + 1, original.end_frame + delta_frames))
            updated = replace(original, end_frame=end)
        if updated != original:
            self._drag_changed = True
        self.intervals[index] = updated
        self.update()

    def mouseReleaseEvent(self, event: QMouseEvent) -> None:  # noqa: N802
        if self._pan_origin is not None and event.button() == Qt.MouseButton.MiddleButton:
            self._pan_origin = None
            return
        if event.button() != Qt.MouseButton.LeftButton:
            return
        if self._drag_mode is not None and self._drag_changed:
            self.intervalsEdited.emit(list(self.intervals))
        self._drag_mode = None
        self._drag_origin = None
        self._drag_changed = False

    def mouseDoubleClickEvent(self, event: QMouseEvent) -> None:  # noqa: N802
        if event.button() != Qt.MouseButton.LeftButton or self.samples is None:
            return
        if self._hit_test(event.position()) is not None:
            return
        total_frames = self.samples.shape[0]
        clicked = int(round(self._x_to_time(event.position().x()) * self.sample_rate))
        duration = max(1, int(round(0.5 * self.sample_rate)))
        start = max(0, min(total_frames - 1, clicked - duration // 2))
        end = min(total_frames, start + duration)
        for existing in self.intervals:
            if start < existing.end_frame and end > existing.start_frame:
                self.message.emit("该位置已有区间，请拖动或双击空白处添加")
                return
        next_id = max((item.id for item in self.intervals), default=0) + 1
        interval = Interval(id=next_id, start_frame=start, end_frame=end)
        self.intervals = sorted(self.intervals + [interval], key=lambda item: item.start_frame)
        self.selected_id = next_id
        self.selectionChanged.emit(next_id)
        self.intervalsEdited.emit(list(self.intervals))
        self.update()

    def wheelEvent(self, event: QWheelEvent) -> None:  # noqa: N802
        if self.samples is None:
            return
        delta = event.angleDelta().y()
        if event.modifiers() & Qt.KeyboardModifier.ShiftModifier:
            self.view_start = self._clamp_view_start(
                self.view_start - delta / 120.0 * 40.0 / max(1.0, self.pixels_per_second)
            )
        else:
            cursor_time = self._x_to_time(event.position().x())
            factor = 1.2 if delta > 0 else 1.0 / 1.2
            self.pixels_per_second = max(20.0, min(5000.0, self.pixels_per_second * factor))
            self.view_start = self._clamp_view_start(
                cursor_time - event.position().x() / max(1.0, self.pixels_per_second)
            )
        self.update()
        event.accept()

    def keyPressEvent(self, event: QKeyEvent) -> None:  # noqa: N802
        if (
            event.key() in (Qt.Key.Key_Delete, Qt.Key.Key_Backspace)
            and self.selected_id is not None
        ):
            remaining = [item for item in self.intervals if item.id != self.selected_id]
            self.selected_id = None
            self.intervals = remaining
            self.selectionChanged.emit(-1)
            self.intervalsEdited.emit(list(remaining))
            self.update()
            event.accept()
            return
        super().keyPressEvent(event)

