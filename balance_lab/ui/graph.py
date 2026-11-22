from __future__ import annotations

from math import atan2, cos, pi, sin

from PySide6.QtCore import QPointF, QRectF, Qt
from PySide6.QtGui import (
    QBrush,
    QColor,
    QFont,
    QFontMetrics,
    QPainter,
    QPainterPath,
    QPen,
    QPolygonF,
)
from PySide6.QtWidgets import QGraphicsPathItem, QGraphicsPolygonItem, QGraphicsScene, QGraphicsView

from ..models import Project
from .theme import CYAN, MUTED, PURPLE, TEAL, TEXT, UI_FONT


class _GraphNode(QGraphicsPathItem):
    def __init__(
        self,
        *,
        x: float,
        y: float,
        width: float,
        height: float,
        title: str,
        subtitle: str,
        accent: str,
        tooltip: str,
    ) -> None:
        super().__init__()
        path = QPainterPath()
        path.addRoundedRect(QRectF(0, 0, width, height), 6, 6)
        self.setPath(path)
        self.setPos(x, y)
        self.setBrush(QBrush(QColor("#13233a")))
        self.setPen(QPen(QColor(accent), 1.2))
        self.setToolTip(tooltip)
        self.rect_size = (width, height)
        self._title = title
        self._subtitle = subtitle
        self._accent = QColor(accent)

    def paint(self, painter: QPainter, option, widget=None) -> None:  # type: ignore[override]
        super().paint(painter, option, widget)
        width, height = self.rect_size
        painter.setRenderHint(QPainter.RenderHint.Antialiasing)
        painter.setPen(Qt.PenStyle.NoPen)
        painter.setBrush(self._accent)
        painter.drawRoundedRect(QRectF(0, 0, 3, height), 1.5, 1.5)
        title_font = QFont(UI_FONT, 9)
        title_font.setWeight(QFont.Weight.DemiBold)
        painter.setFont(title_font)
        painter.setPen(QColor(TEXT))
        metrics = QFontMetrics(title_font)
        title = metrics.elidedText(self._title, Qt.TextElideMode.ElideMiddle, int(width - 16))
        painter.drawText(QRectF(10, 4, width - 14, height / 2), Qt.AlignmentFlag.AlignVCenter, title)
        if self._subtitle:
            sub_font = QFont("Cascadia Mono", 7)
            painter.setFont(sub_font)
            painter.setPen(QColor(MUTED))
            metrics = QFontMetrics(sub_font)
            subtitle = metrics.elidedText(
                self._subtitle, Qt.TextElideMode.ElideRight, int(width - 16)
            )
            painter.drawText(
                QRectF(10, height / 2 - 1, width - 14, height / 2 - 3),
                Qt.AlignmentFlag.AlignVCenter,
                subtitle,
            )


class DependencyGraphView(QGraphicsView):
    """Native class -> skill -> formula dependency graph with pan and zoom."""

    def __init__(self, project: Project, class_filter: str | None = None, parent=None) -> None:
        super().__init__(parent)
        self.project = project
        self.class_filter = class_filter
        self._scene = QGraphicsScene(self)
        self.setScene(self._scene)
        self.setRenderHints(
            QPainter.RenderHint.Antialiasing
            | QPainter.RenderHint.TextAntialiasing
            | QPainter.RenderHint.SmoothPixmapTransform
        )
        self.setDragMode(QGraphicsView.DragMode.ScrollHandDrag)
        self.setTransformationAnchor(QGraphicsView.ViewportAnchor.AnchorUnderMouse)
        self.setResizeAnchor(QGraphicsView.ViewportAnchor.AnchorViewCenter)
        self.setBackgroundBrush(QBrush(QColor("#0b1627")))
        self.setFrameShape(QGraphicsView.Shape.NoFrame)
        self._first_fit = True
        self._build()

    def _build(self) -> None:
        scene = self._scene
        scene.clear()
        scene.setBackgroundBrush(QBrush(QColor("#0b1627")))
        legend = scene.addText("DEPENDENCY GRAPH   职业  →  技能  →  公式    ·    滚轮缩放 / 拖拽平移")
        legend.setDefaultTextColor(QColor(MUTED))
        legend.setFont(QFont(UI_FONT, 8))
        legend.setPos(18, 8)

        class_nodes: dict[str, _GraphNode] = {}
        skill_nodes: dict[str, _GraphNode] = {}
        formula_nodes: dict[str, _GraphNode] = {}

        class_ids = sorted(self.project.classes)
        if self.class_filter and self.class_filter in self.project.classes:
            class_ids = [self.class_filter]
        for index, class_id in enumerate(class_ids):
            item = self.project.classes[class_id]
            node = _GraphNode(
                x=24,
                y=72 + index * 112,
                width=175,
                height=58,
                title=item.name,
                subtitle=item.id,
                accent=CYAN,
                tooltip=f"{item.name}\nrole: {item.role}\nskills: {len(item.skill_ids)}",
            )
            scene.addItem(node)
            class_nodes[class_id] = node

        skills = sorted(
            (
                skill
                for skill in self.project.skills.values()
                if not self.class_filter or skill.class_id == self.class_filter
            ),
            key=lambda item: (item.class_id, -item.priority, item.id),
        )
        for index, skill in enumerate(skills):
            node = _GraphNode(
                x=292,
                y=54 + index * 61,
                width=205,
                height=45,
                title=skill.name,
                subtitle=f"{skill.id}  CD{skill.cooldown}",
                accent=TEAL,
                tooltip=(
                    f"{skill.name}\nbase_damage={skill.base_damage}\n"
                    f"growth_coeff={skill.growth_coeff}\ncooldown={skill.cooldown}\n"
                    f"resource_cost={skill.resource_cost}\nformula={skill.formula_id}"
                ),
            )
            scene.addItem(node)
            skill_nodes[skill.id] = node

        if self.class_filter:
            needed = {skill.formula_id for skill in skills}
            frontier = list(needed)
            while frontier:
                formula = self.project.formulas[frontier.pop()]
                for dependency in formula.formula_dependencies():
                    if dependency not in needed:
                        needed.add(dependency)
                        frontier.append(dependency)
            formula_ids = sorted(needed)
            formula_columns = 2
        else:
            formula_ids = sorted(self.project.formulas)
            formula_columns = 4
        for index, formula_id in enumerate(formula_ids):
            formula = self.project.formulas[formula_id]
            col = index % formula_columns
            row = index // formula_columns
            node = _GraphNode(
                x=590 + col * (238 if formula_columns == 4 else 245),
                y=54 + row * 61,
                width=220,
                height=44,
                title=formula.name,
                subtitle=f"{formula.id}  [{formula.op}]",
                accent=PURPLE,
                tooltip=f"{formula.name}\nop={formula.op}\nargs={len(formula.args)}",
            )
            scene.addItem(node)
            formula_nodes[formula_id] = node

        for class_id, class_node in class_nodes.items():
            for skill_id in self.project.classes[class_id].skill_ids:
                self._add_edge(class_node, skill_nodes[skill_id], "#2d5f83", 1.0)
        for skill in skills:
            self._add_edge(skill_nodes[skill.id], formula_nodes[skill.formula_id], "#2f7368", 1.0)
        for formula_id in formula_ids:
            formula = self.project.formulas[formula_id]
            for dependency in formula.formula_dependencies():
                self._add_edge(
                    formula_nodes[dependency],
                    formula_nodes[formula.id],
                    "#5b4e91",
                    0.8,
                )

        bounds = scene.itemsBoundingRect()
        scene.setSceneRect(bounds.adjusted(-20, -20, 30, 30))
        self._first_fit = True
        self.fit_graph()

    def _add_edge(self, source: _GraphNode, target: _GraphNode, color: str, width: float) -> None:
        source_rect = QRectF(source.pos(), source.path().boundingRect().size())
        target_rect = QRectF(target.pos(), target.path().boundingRect().size())
        start = QPointF(source_rect.right() + 2, source_rect.center().y())
        end = QPointF(target_rect.left() - 4, target_rect.center().y())
        dx = max(35.0, (end.x() - start.x()) * 0.48)
        path = QPainterPath(start)
        path.cubicTo(
            QPointF(start.x() + dx, start.y()),
            QPointF(end.x() - dx, end.y()),
            end,
        )
        edge = QGraphicsPathItem(path)
        edge.setPen(QPen(QColor(color), width))
        edge.setZValue(-1)
        self._scene.addItem(edge)
        angle = atan2(end.y() - start.y(), end.x() - start.x())
        size = 5.0
        tip = QPointF(end.x(), end.y())
        left = QPointF(
            end.x() - size * cos(angle - pi / 6),
            end.y() - size * sin(angle - pi / 6),
        )
        right = QPointF(
            end.x() - size * cos(angle + pi / 6),
            end.y() - size * sin(angle + pi / 6),
        )
        arrow = QGraphicsPolygonItem(QPolygonF([tip, left, right]))
        arrow.setBrush(QBrush(QColor(color)))
        arrow.setPen(QPen(Qt.PenStyle.NoPen))
        arrow.setZValue(1)
        self._scene.addItem(arrow)

    def fit_graph(self) -> None:
        if self._scene.sceneRect().isEmpty():
            return
        self.fitInView(self._scene.sceneRect(), Qt.AspectRatioMode.KeepAspectRatio)
        self._first_fit = False

    def resizeEvent(self, event) -> None:  # type: ignore[override]
        super().resizeEvent(event)
        if self._first_fit:
            self.fit_graph()

    def wheelEvent(self, event) -> None:  # type: ignore[override]
        factor = 1.12 if event.angleDelta().y() > 0 else 1 / 1.12
        current = self.transform().m11()
        if 0.35 <= current * factor <= 3.2:
            self.scale(factor, factor)
        event.accept()
