from __future__ import annotations

from typing import Any

from PySide6.QtCore import QPointF, QRectF, Qt
from PySide6.QtGui import QBrush, QColor, QFont, QPainter, QPainterPath, QPen
from PySide6.QtWidgets import QWidget

from .theme import BORDER, CYAN, MUTED, SURFACE_2, TEXT, UI_FONT


class WinRateChart(QWidget):
    def __init__(self, parent=None) -> None:
        super().__init__(parent)
        self._rows: list[dict[str, Any]] = []
        self.setMinimumHeight(240)
        self.setToolTip("柱：观测胜率　竖线：95% 置信区间　半透明带：解析/校准真值区间")

    def set_data(self, rows: list[dict[str, Any]]) -> None:
        self._rows = rows
        self.update()

    def paintEvent(self, event) -> None:  # type: ignore[override]
        painter = QPainter(self)
        painter.setRenderHint(QPainter.RenderHint.Antialiasing)
        painter.fillRect(self.rect(), QColor(SURFACE_2))
        painter.setPen(QPen(QColor(BORDER), 1))
        painter.drawRoundedRect(self.rect().adjusted(0, 0, -1, -1), 7, 7)
        title_font = QFont(UI_FONT, 10)
        title_font.setWeight(QFont.Weight.DemiBold)
        painter.setFont(title_font)
        painter.setPen(QColor(TEXT))
        painter.drawText(14, 22, "胜率 · 置信区间 · 真值带")
        if not self._rows:
            painter.setPen(QColor(MUTED))
            painter.setFont(QFont(UI_FONT, 9))
            painter.drawText(self.rect(), Qt.AlignmentFlag.AlignCenter, "运行验证后显示逐场景胜率")
            return
        plot = QRectF(44, 38, self.width() - 62, self.height() - 76)
        painter.setFont(QFont("Cascadia Mono", 7))
        for tick in (0, 2500, 5000, 7500, 10000):
            y = plot.bottom() - plot.height() * tick / 10000
            painter.setPen(QPen(QColor("#203551"), 1, Qt.PenStyle.DotLine))
            painter.drawLine(QPointF(plot.left(), y), QPointF(plot.right(), y))
            painter.setPen(QColor(MUTED))
            painter.drawText(QRectF(2, y - 8, 38, 16), Qt.AlignmentFlag.AlignRight, f"{tick // 100}%")
        count = len(self._rows)
        slot = plot.width() / max(1, count)
        bar_width = min(34.0, slot * 0.42)
        for index, row in enumerate(self._rows):
            center = plot.left() + slot * (index + 0.5)
            low = int(row.get("truth_low_bp", 0))
            high = int(row.get("truth_high_bp", 10000))
            truth_top = plot.bottom() - plot.height() * high / 10000
            truth_bottom = plot.bottom() - plot.height() * low / 10000
            painter.setPen(Qt.PenStyle.NoPen)
            painter.setBrush(QColor(62, 224, 176, 24))
            painter.drawRect(QRectF(center - slot * 0.38, truth_top, slot * 0.76, truth_bottom - truth_top))
            rate = int(row.get("win_rate_bp", 0))
            y = plot.bottom() - plot.height() * rate / 10000
            bar_rect = QRectF(center - bar_width / 2, y, bar_width, plot.bottom() - y)
            color = QColor(CYAN)
            if rate > high:
                color = QColor("#ffbf69")
            elif rate < low:
                color = QColor("#ff6b7a")
            painter.setBrush(QBrush(color))
            painter.drawRoundedRect(bar_rect, 2, 2)
            ci_low = int(row.get("ci_low_bp", 0))
            ci_high = int(row.get("ci_high_bp", 10000))
            ci_top = plot.bottom() - plot.height() * ci_high / 10000
            ci_bottom = plot.bottom() - plot.height() * ci_low / 10000
            painter.setPen(QPen(QColor("#d7e8ff"), 1.3))
            painter.drawLine(QPointF(center, ci_top), QPointF(center, ci_bottom))
            painter.drawLine(QPointF(center - 4, ci_top), QPointF(center + 4, ci_top))
            painter.drawLine(QPointF(center - 4, ci_bottom), QPointF(center + 4, ci_bottom))
            painter.setPen(QColor(TEXT))
            painter.setFont(QFont("Cascadia Mono", 7))
            painter.drawText(
                QRectF(center - slot / 2, y - 16, slot, 14),
                Qt.AlignmentFlag.AlignCenter,
                f"{rate / 100:.1f}%",
            )
            label = str(row.get("scenario_id", "")).replace("scene.", "")
            painter.setPen(QColor(MUTED))
            painter.drawText(
                QRectF(center - slot / 2, plot.bottom() + 5, slot, 22),
                Qt.AlignmentFlag.AlignHCenter | Qt.AlignmentFlag.AlignTop,
                label,
            )


class DamageCurveChart(QWidget):
    def __init__(self, parent=None) -> None:
        super().__init__(parent)
        self._scenario = ""
        self._a: tuple[int, ...] = ()
        self._b: tuple[int, ...] = ()
        self.setMinimumHeight(240)

    def set_series(self, scenario: str, a: tuple[int, ...], b: tuple[int, ...]) -> None:
        self._scenario = scenario
        self._a = a
        self._b = b
        self.update()

    def paintEvent(self, event) -> None:  # type: ignore[override]
        painter = QPainter(self)
        painter.setRenderHint(QPainter.RenderHint.Antialiasing)
        painter.fillRect(self.rect(), QColor(SURFACE_2))
        painter.setPen(QPen(QColor(BORDER), 1))
        painter.drawRoundedRect(self.rect().adjusted(0, 0, -1, -1), 7, 7)
        title_font = QFont(UI_FONT, 10)
        title_font.setWeight(QFont.Weight.DemiBold)
        painter.setFont(title_font)
        painter.setPen(QColor(TEXT))
        painter.drawText(14, 22, "逐回合伤害曲线")
        if not self._a and not self._b:
            painter.setPen(QColor(MUTED))
            painter.setFont(QFont(UI_FONT, 9))
            painter.drawText(self.rect(), Qt.AlignmentFlag.AlignCenter, "运行验证后显示平均伤害曲线")
            return
        plot = QRectF(46, 38, self.width() - 64, self.height() - 76)
        max_rounds = max(len(self._a), len(self._b), 1)
        max_damage = max(max(self._a, default=1), max(self._b, default=1), 1)
        for tick in range(5):
            y = plot.bottom() - plot.height() * tick / 4
            painter.setPen(QPen(QColor("#203551"), 1, Qt.PenStyle.DotLine))
            painter.drawLine(QPointF(plot.left(), y), QPointF(plot.right(), y))
            painter.setPen(QColor(MUTED))
            painter.setFont(QFont("Cascadia Mono", 7))
            painter.drawText(
                QRectF(0, y - 8, 41, 16),
                Qt.AlignmentFlag.AlignRight,
                f"{max_damage * tick // 4 / 1000:.1f}k",
            )
        for index in range(max_rounds):
            x = plot.left() + plot.width() * (index + 0.5) / max_rounds
            painter.setPen(QColor(MUTED))
            painter.setFont(QFont("Cascadia Mono", 7))
            painter.drawText(
                QRectF(x - 12, plot.bottom() + 5, 24, 14),
                Qt.AlignmentFlag.AlignCenter,
                str(index + 1),
            )

        def points(values: tuple[int, ...]) -> list[QPointF]:
            result: list[QPointF] = []
            for index, value in enumerate(values):
                x = plot.left() + plot.width() * (index + 0.5) / max_rounds
                y = plot.bottom() - plot.height() * value / max_damage
                result.append(QPointF(x, y))
            return result

        self._draw_series(painter, points(self._a), QColor(CYAN), QColor(71, 215, 255, 38))
        self._draw_series(painter, points(self._b), QColor("#ff9f68"), QColor(255, 159, 104, 30))
        painter.setFont(QFont(UI_FONT, 8))
        painter.setPen(QColor(CYAN))
        painter.drawText(QRectF(plot.left(), 24, 110, 14), "A → B 伤害")
        painter.setPen(QColor("#ff9f68"))
        painter.drawText(QRectF(plot.left() + 105, 24, 110, 14), "B → A 伤害")
        painter.setPen(QColor(MUTED))
        painter.drawText(
            QRectF(plot.right() - 240, 24, 232, 14),
            Qt.AlignmentFlag.AlignRight,
            self._scenario,
        )

    @staticmethod
    def _draw_series(
        painter: QPainter,
        points: list[QPointF],
        line_color: QColor,
        fill_color: QColor,
    ) -> None:
        if not points:
            return
        if len(points) > 1:
            fill = QPainterPath(points[0])
            for point in points[1:]:
                fill.lineTo(point)
            fill.lineTo(QPointF(points[-1].x(), painter.viewport().height() - 38))
            fill.lineTo(QPointF(points[0].x(), painter.viewport().height() - 38))
            fill.closeSubpath()
            painter.setPen(Qt.PenStyle.NoPen)
            painter.setBrush(fill_color)
            painter.drawPath(fill)
        path = QPainterPath(points[0])
        for point in points[1:]:
            path.lineTo(point)
        painter.setBrush(Qt.BrushStyle.NoBrush)
        painter.setPen(QPen(line_color, 2.0))
        painter.drawPath(path)
        painter.setBrush(line_color)
        painter.setPen(Qt.PenStyle.NoPen)
        for point in points:
            painter.drawEllipse(point, 2.6, 2.6)
