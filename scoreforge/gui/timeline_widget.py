"""演奏表达时间轴控件：力度曲线、连奏、踏板与播放头可视化."""

from __future__ import annotations

from PySide6.QtCore import Qt
from PySide6.QtGui import QColor, QPainter, QPen, QPolygonF
from PySide6.QtCore import QPointF
from PySide6.QtWidgets import QWidget

LANE_DYNAMICS_TOP = 20
LANE_DYNAMICS_H = 120
LANE_LEGATO_TOP = 150
LANE_LEGATO_H = 30
LANE_PEDAL_TOP = 190
LANE_PEDAL_H = 30
WIDGET_H = 240


class TimelineWidget(QWidget):
    def __init__(self, parent=None):
        super().__init__(parent)
        self.score = None
        self.part_id = None
        self.position_ms = 0.0
        self.px_per_tick = 0.08
        self.setMinimumHeight(WIDGET_H)

    def set_score(self, score, part_id=None):
        self.score = score
        self.part_id = part_id or (score.part_ids[0] if score else None)
        if score is not None:
            self.setMinimumWidth(int(score.total_ticks * self.px_per_tick) + 40)
        self.update()

    def set_position_ms(self, ms: float):
        self.position_ms = ms
        self.update()

    def _x(self, tick: float) -> float:
        return 10 + tick * self.px_per_tick

    def paintEvent(self, event):  # noqa: N802 - Qt 约定
        painter = QPainter(self)
        painter.fillRect(self.rect(), QColor("#1b1e24"))
        if self.score is None:
            painter.setPen(QColor("#888"))
            painter.drawText(self.rect(), Qt.AlignCenter, "未加载工程")
            painter.end()
            return
        score = self.score
        tempo = score.tempo_map

        # 小节网格
        painter.setPen(QPen(QColor("#3a3f4a"), 1))
        for mi in range(len(score.measure_structure)):
            x = self._x(score.measure_start(mi))
            painter.drawLine(x, 5, x, WIDGET_H - 5)
            painter.setPen(QColor("#7a828e"))
            painter.drawText(x + 3, 12, f"M{mi}")
            painter.setPen(QPen(QColor("#3a3f4a"), 1))

        # 泳道标签
        painter.setPen(QColor("#9aa2ad"))
        painter.drawText(2, LANE_DYNAMICS_TOP + 10, "力度")
        painter.drawText(2, LANE_LEGATO_TOP + 15, "连奏")
        painter.drawText(2, LANE_PEDAL_TOP + 15, "踏板")

        pid = self.part_id
        # 力度曲线（0..127 映射到泳道高度）
        ticks, values = score.dynamics_curve(pid)
        painter.setPen(QPen(QColor("#4fc3f7"), 2))
        total = score.total_ticks
        poly = QPolygonF()
        step = max(1, int(total // 400))
        for t in range(0, total + 1, step):
            v = score.dynamics_value_at(pid, t)
            y = LANE_DYNAMICS_TOP + LANE_DYNAMICS_H * (1.0 - v / 127.0)
            poly.append(QPointF(self._x(t), y))
        painter.drawPolyline(poly)
        for t, v in zip(ticks, values):
            y = LANE_DYNAMICS_TOP + LANE_DYNAMICS_H * (1.0 - float(v) / 127.0)
            painter.drawEllipse(QPointF(self._x(float(t)), y), 3.0, 3.0)

        # 连奏与踏板事件
        for ev in score.events.values():
            if ev.part_id != pid:
                continue
            x = self._x(score.event_abs_tick(ev))
            if ev.type == "legato":
                painter.setPen(QPen(QColor("#aed581"), 2))
                painter.drawLine(x, LANE_LEGATO_TOP + LANE_LEGATO_H,
                                 x, LANE_LEGATO_TOP + 4)
            elif ev.type == "pedal":
                on = float(ev.value) > 0
                painter.setPen(QPen(QColor("#ffb74d"), 2))
                y = LANE_PEDAL_TOP + (4 if on else LANE_PEDAL_H - 4)
                painter.drawLine(x, LANE_PEDAL_TOP + LANE_LEGATO_H, x, y)
                painter.drawEllipse(QPointF(x, y), 2.5, 2.5)

        # 播放头（毫秒 -> tick -> x，与模型统一时间单位）
        playhead_tick = tempo.tick_at_ms(self.position_ms)
        painter.setPen(QPen(QColor("#ef5350"), 2))
        x = self._x(playhead_tick)
        painter.drawLine(x, 0, x, WIDGET_H)
        painter.end()
