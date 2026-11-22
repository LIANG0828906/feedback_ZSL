"""Grid, path, waypoint and tick-state renderer."""

from __future__ import annotations

from typing import Callable

from PySide6.QtCore import QPointF, QRectF, Qt, Signal
from PySide6.QtGui import (
    QBrush,
    QColor,
    QFont,
    QPainter,
    QPen,
    QPolygonF,
)
from PySide6.QtWidgets import QWidget

from ..engine import FormationEngine
from ..fixed import COORD_SCALE, angle_difference
from ..models import Coord

MEMBER_COLORS = {
    "alpha": QColor("#4cc9f0"),
    "bravo": QColor("#f72585"),
    "charlie": QColor("#ffd166"),
    "delta": QColor("#80ed99"),
    "echo": QColor("#c77dff"),
    "foxtrot": QColor("#f9844a"),
}


def _member_color(member_id: str) -> QColor:
    if member_id in MEMBER_COLORS:
        return MEMBER_COLORS[member_id]
    palette = (
        "#4cc9f0",
        "#f72585",
        "#ffd166",
        "#80ed99",
        "#c77dff",
        "#f9844a",
    )
    return QColor(palette[sum(ord(char) for char in member_id) % len(palette)])


class TacticalCanvas(QWidget):
    cell_selected = Signal(int, int)
    member_selected = Signal(str)

    def __init__(self, engine: FormationEngine, parent: QWidget | None = None) -> None:
        super().__init__(parent)
        self._engine = engine
        self._selected_cell: Coord | None = None
        self._selected_member: str | None = None
        self._tick = 0
        self._cell_size = 24.0
        self._origin_x = 20.0
        self._origin_y = 20.0
        self._invalidated_cells: set[Coord] = set()
        self._invalidated_members: set[str] = set()
        self.setMinimumSize(680, 500)
        self.setMouseTracking(True)
        self.setFocusPolicy(Qt.FocusPolicy.StrongFocus)

    def set_tick(self, tick: int) -> None:
        self._tick = tick
        self.update()

    def set_selected_member(self, member_id: str | None) -> None:
        self._selected_member = member_id
        self.update()

    def set_selected_cell(self, coord: Coord | None) -> None:
        self._selected_cell = coord
        self.update()

    def set_invalidated_cells(self, coords: set[Coord]) -> None:
        self._invalidated_cells = coords
        self.update()

    def set_invalidated_members(self, member_ids: set[str]) -> None:
        self._invalidated_members = member_ids
        self.update()

    def _layout(self) -> tuple[float, float, float]:
        scene = self._engine.scene
        width = self.width() - 36
        height = self.height() - 36
        self._cell_size = max(
            4.0, min(width / scene.map.width, height / scene.map.height)
        )
        total_width = self._cell_size * scene.map.width
        total_height = self._cell_size * scene.map.height
        self._origin_x = (self.width() - total_width) / 2
        self._origin_y = (self.height() - total_height) / 2
        return self._origin_x, self._origin_y, self._cell_size

    def _cell_rect(self, x: int, y: int) -> QRectF:
        return QRectF(
            self._origin_x + x * self._cell_size,
            self._origin_y + y * self._cell_size,
            self._cell_size,
            self._cell_size,
        )

    def _world_to_screen(self, point: tuple[int, int]) -> QPointF:
        return QPointF(
            self._origin_x + (point[0] / COORD_SCALE) * self._cell_size,
            self._origin_y + (point[1] / COORD_SCALE) * self._cell_size,
        )

    def _cell_at(self, position: QPointF) -> Coord | None:
        scene = self._engine.scene
        x = int((position.x() - self._origin_x) // self._cell_size)
        y = int((position.y() - self._origin_y) // self._cell_size)
        if 0 <= x < scene.map.width and 0 <= y < scene.map.height:
            return x, y
        return None

    def _cost_color(self, cost: int) -> QColor:
        if cost < 0:
            return QColor("#191b20")
        if cost <= 1:
            return QColor("#33454d")
        if cost <= 2:
            return QColor("#3c4d40")
        if cost <= 4:
            return QColor("#4a4b3d")
        if cost <= 6:
            return QColor("#4a3d3a")
        return QColor("#4d3235")

    def paintEvent(self, _event) -> None:  # type: ignore[override]
        scene = self._engine.scene
        grid = scene.map
        self._layout()
        painter = QPainter(self)
        painter.setRenderHint(QPainter.RenderHint.Antialiasing, True)
        painter.fillRect(self.rect(), QColor("#0d1117"))

        for y in range(grid.height):
            for x in range(grid.width):
                rect = self._cell_rect(x, y)
                cost = grid.costs[y][x]
                painter.fillRect(rect, self._cost_color(cost))
                terrain_name = grid.terrain[y][x]
                terrain = grid.terrain_types.get(terrain_name)
                if terrain is not None and terrain.passable and cost >= 0:
                    tint = QColor(terrain.color)
                    tint.setAlpha(36)
                    painter.fillRect(rect, tint)
                if cost < 0:
                    painter.setPen(QPen(QColor("#4b5563"), 1))
                    painter.drawLine(
                        rect.topLeft() + QPointF(3, 3),
                        rect.bottomRight() - QPointF(3, 3),
                    )
                    painter.drawLine(
                        rect.topRight() + QPointF(-3, 3),
                        rect.bottomLeft() + QPointF(3, -3),
                    )
                if (x, y) in self._invalidated_cells:
                    painter.fillRect(rect, QColor(255, 190, 80, 85))
                painter.setPen(QPen(QColor(10, 15, 20, 90), 0.6))
                painter.drawRect(rect)

        for blockade in grid.blockades:
            if not blockade.active:
                continue
            painter.setPen(QPen(QColor("#ff5d73"), 1.5, Qt.PenStyle.DashLine))
            for x, y in blockade.cells:
                painter.drawRect(self._cell_rect(x, y).adjusted(2, 2, -2, -2))
            if blockade.cells:
                x, y = blockade.cells[0]
                rect = self._cell_rect(x, y)
                painter.setPen(QColor("#ff8fa3"))
                font = QFont("Segoe UI", max(7, int(self._cell_size * 0.32)))
                painter.setFont(font)
                painter.drawText(
                    rect.adjusted(2, 1, -2, -1),
                    Qt.AlignmentFlag.AlignLeft | Qt.AlignmentFlag.AlignTop,
                    blockade.blockade_id,
                )

        paths = self._engine.paths()
        for member_id, path in sorted(paths.items()):
            color = _member_color(member_id)
            selected = self._selected_member == member_id
            invalidated = member_id in self._invalidated_members
            pen = QPen(color, 3.8 if selected else (2.8 if invalidated else 2.0))
            if invalidated:
                pen.setStyle(Qt.PenStyle.DashLine)
            pen.setCapStyle(Qt.PenCapStyle.RoundCap)
            pen.setJoinStyle(Qt.PenJoinStyle.RoundJoin)
            painter.setPen(pen)
            polygon = QPolygonF(
                [self._world_to_screen(point) for point in path.points]
            )
            painter.drawPolyline(polygon)
            for waypoint in path.waypoints:
                center = self._world_to_screen(waypoint.world)
                painter.setPen(QPen(QColor("#0d1117"), 2))
                painter.setBrush(QBrush(color))
                radius = 4.5 if selected else 3.2
                painter.drawEllipse(center, radius, radius)
            if path.points:
                start = self._world_to_screen(path.points[0])
                painter.setBrush(Qt.BrushStyle.NoBrush)
                painter.setPen(QPen(color, 1.5))
                painter.drawEllipse(start, 8, 8)

        traces = self._engine.traces()
        for member_id, trace in sorted(traces.items()):
            if self._tick >= len(trace.ticks):
                continue
            record = trace.ticks[self._tick]
            center = self._world_to_screen(record.position)
            color = _member_color(member_id)
            painter.setPen(QPen(QColor("#ffffff"), 1.6 if self._selected_member == member_id else 1.0))
            painter.setBrush(QBrush(color))
            heading = record.heading / 1000.0
            import math

            radians = math.radians(heading)
            forward = QPointF(math.cos(radians), math.sin(radians))
            right = QPointF(-forward.y(), forward.x())
            tip = center + forward * 10
            back_left = center - forward * 5 + right * 5
            back_right = center - forward * 5 - right * 5
            painter.drawPolygon(QPolygonF([tip, back_left, back_right]))
            painter.setPen(QColor("#e8eef5"))
            painter.setFont(QFont("Consolas", 8))
            painter.drawText(center + QPointF(9, -8), member_id)

        self._draw_slots(painter)
        if self._selected_cell is not None:
            x, y = self._selected_cell
            painter.setPen(QPen(QColor("#ffffff"), 2))
            painter.setBrush(Qt.BrushStyle.NoBrush)
            painter.drawRect(self._cell_rect(x, y).adjusted(1, 1, -1, -1))

        painter.setPen(QColor("#8b98a8"))
        painter.setFont(QFont("Consolas", 8))
        painter.drawText(
            16,
            self.height() - 10,
            (
                f"{scene.name}  {grid.width}x{grid.height}  "
                f"tick {self._tick}/{scene.simulation.max_ticks}  "
                f"rev {self._engine.revision}"
            ),
        )

    def _draw_slots(self, painter: QPainter) -> None:
        scene = self._engine.scene
        paths = self._engine.paths()
        traces = self._engine.traces()
        for member in scene.members:
            parent_id = scene.formation.parents.get(member.member_id)
            if parent_id is None or parent_id not in paths:
                continue
            path = paths[parent_id]
            if not path.points:
                continue
            parent_trace = traces.get(parent_id)
            if parent_trace is not None and parent_trace.ticks:
                parent_anchor = parent_trace.ticks[
                    min(self._tick, len(parent_trace.ticks) - 1)
                ].position
            else:
                parent_anchor = path.points[-1]
            own_slot = scene.formation.slots[member.slot_id]
            parent_member = scene.member(parent_id)
            parent_slot = scene.formation.slots[parent_member.slot_id]
            expected = (
                parent_anchor[0] + own_slot[0] - parent_slot[0],
                parent_anchor[1] + own_slot[1] - parent_slot[1],
            )
            center = self._world_to_screen(expected)
            color = _member_color(member.member_id)
            color.setAlpha(150)
            painter.setPen(QPen(color, 1, Qt.PenStyle.DashLine))
            painter.setBrush(Qt.BrushStyle.NoBrush)
            painter.drawEllipse(center, 7, 7)

    def mousePressEvent(self, event) -> None:  # type: ignore[override]
        coord = self._cell_at(event.position())
        if coord is not None:
            self._selected_cell = coord
            self.cell_selected.emit(coord[0], coord[1])
            self.update()
        super().mousePressEvent(event)
