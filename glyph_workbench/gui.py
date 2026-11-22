from __future__ import annotations

import math
import sys
from pathlib import Path
from typing import Any

from PySide6.QtCore import QPointF, QRectF, QSize, Qt, QThread, QTime, QTimer, Signal
from PySide6.QtGui import (
    QColor,
    QFont,
    QFontDatabase,
    QImage,
    QPainter,
    QPainterPath,
    QPen,
    QPolygonF,
)
from PySide6.QtWidgets import (
    QApplication,
    QCheckBox,
    QComboBox,
    QDoubleSpinBox,
    QFormLayout,
    QFrame,
    QGridLayout,
    QGroupBox,
    QHBoxLayout,
    QLabel,
    QMainWindow,
    QMessageBox,
    QPlainTextEdit,
    QProgressBar,
    QPushButton,
    QSpinBox,
    QSplitter,
    QTableWidget,
    QTableWidgetItem,
    QVBoxLayout,
    QWidget,
)

from .engine import CrashInjected, EngineResult
from .errors import WorkbenchError
from .export import export_result
from .geometry import glyph_bbox
from .model_store import ensure_model_copy
from .models import Glyph
from .models import load_document as reload_document
from .params import RenderParams
from .samplegen import generate_samples
from .session import EditSession
from .state import STATE_LABELS, SegmentState

BG = "#0d1117"
PANEL = "#151c26"
PANEL_ALT = "#1b2430"
BORDER = "#283444"
TEXT = "#e6edf3"
MUTED = "#8292a6"
ACCENT = "#45d6c3"
ACCENT_DARK = "#1f8f84"
BLUE = "#63a9ff"
ORANGE = "#ffb454"
RED = "#ff6b7a"
GREEN = "#6ee7a8"
GRID = "#1d2733"
_BUNDLED_FONT_INSTALLED = False


def install_bundled_font() -> None:
    global _BUNDLED_FONT_INSTALLED
    if _BUNDLED_FONT_INSTALLED:
        return
    app = QApplication.instance()
    if app is None:
        return
    font_path = Path(__file__).resolve().parent / "assets" / "fonts" / "wqy-microhei.ttc"
    if font_path.exists():
        font_id = QFontDatabase.addApplicationFont(str(font_path))
        if font_id >= 0:
            families = QFontDatabase.applicationFontFamilies(font_id)
            if families:
                app.setFont(QFont(families[0], 10))
    _BUNDLED_FONT_INSTALLED = True


class WorkbenchWorker(QThread):
    progress = Signal(object)
    committed = Signal(int)
    succeeded = Signal(object, str)
    failed = Signal(object)

    def __init__(
        self,
        session: EditSession,
        mode: str,
        *,
        force_full: bool = False,
        parent: QWidget | None = None,
    ) -> None:
        super().__init__(parent)
        self.session = session
        self.mode = mode
        self.force_full = force_full
        self.pause_event = __import__("threading").Event()

    def request_pause(self) -> None:
        self.pause_event.set()

    def run(self) -> None:
        try:
            if self.mode == "commit":
                info = self.session.commit()
                if info.result is None:
                    raise WorkbenchError(
                        "E_STATE", "commit finished without a raster result"
                    )
                self.succeeded.emit(info.result, self.mode)
                return
            result = self.session.process_working(
                force_full=self.force_full or self.mode == "full",
                pause_event=self.pause_event,
                on_commit=lambda index, result: self.committed.emit(index),
                on_progress=self.progress.emit,
            )
            self.succeeded.emit(result, self.mode)
        except CrashInjected:
            raise
        except WorkbenchError as exc:
            self.failed.emit(exc.as_dict())
        except Exception as exc:  # noqa: BLE001 - report worker failures to UI.
            self.failed.emit(
                {
                    "ok": False,
                    "code": "E_STATE",
                    "message": f"{type(exc).__name__}: {exc}",
                    "context": {},
                }
            )


class OutlineView(QWidget):
    selection_changed = Signal(int, int)
    edited = Signal()

    def __init__(self, parent: QWidget | None = None) -> None:
        super().__init__(parent)
        self.setMinimumSize(360, 300)
        self.setMouseTracking(True)
        self.session: EditSession | None = None
        self.flat_contours: list[Any] = []
        self.show_flattened = True
        self.selected_contour = 0
        self.selected_segment = 0
        self._drag: tuple[int, int, str] | None = None
        self._transform_cache: tuple[float, float, float, float] | None = None

    def sizeHint(self) -> QSize:
        return QSize(620, 500)

    def set_session(
        self,
        session: EditSession,
        result: EngineResult | None = None,
    ) -> None:
        self.session = session
        self.flat_contours = [
            flat for flat in (result.flat_contours if result else []) if flat is not None
        ]
        glyph = session.working_glyph
        if glyph.contours:
            self.selected_contour = min(self.selected_contour, len(glyph.contours) - 1)
            self.selected_segment = min(
                self.selected_segment,
                len(glyph.contours[self.selected_contour].segments) - 1,
            )
        self.update()

    def set_result(self, result: EngineResult | None) -> None:
        self.flat_contours = [
            flat for flat in (result.flat_contours if result else []) if flat is not None
        ]
        self.update()

    def select_contour(self, position: int) -> None:
        if self.session is None:
            return
        contours = self.session.working_glyph.contours
        if not (0 <= position < len(contours)):
            return
        self.selected_contour = position
        self.selected_segment = min(
            self.selected_segment, len(contours[position].segments) - 1
        )
        self.selection_changed.emit(self.selected_contour, self.selected_segment)
        self.update()

    def _current_transform(self) -> tuple[float, float, float, float]:
        if self.session is None:
            return 1.0, 0.0, 0.0, 0.0
        glyph = self.session.working_glyph
        bbox = glyph_bbox(glyph)
        world_width = max(bbox[2] - bbox[0], 1.0)
        world_height = max(bbox[3] - bbox[1], 1.0)
        padding = 42.0
        available_width = max(1.0, self.width() - padding * 2)
        available_height = max(1.0, self.height() - padding * 2)
        scale = min(available_width / world_width, available_height / world_height)
        center_x = (bbox[0] + bbox[2]) * 0.5
        center_y = (bbox[1] + bbox[3]) * 0.5
        origin_x = self.width() * 0.5
        origin_y = self.height() * 0.5
        self._transform_cache = scale, origin_x, origin_y, center_x, center_y  # type: ignore[assignment]
        return scale, origin_x, origin_y, center_x, center_y  # type: ignore[return-value]

    def _to_screen(self, point: tuple[float, float] | QPointF) -> QPointF:
        if self.session is None:
            return QPointF(0, 0)
        scale, origin_x, origin_y, center_x, center_y = self._current_transform()
        if isinstance(point, QPointF):
            x, y = point.x(), point.y()
        else:
            x, y = point
        return QPointF(
            origin_x + (x - center_x) * scale,
            origin_y - (y - center_y) * scale,
        )

    def _to_world(self, point: QPointF) -> tuple[float, float]:
        if self.session is None:
            return 0.0, 0.0
        scale, origin_x, origin_y, center_x, center_y = self._current_transform()
        return (
            center_x + (point.x() - origin_x) / scale,
            center_y - (point.y() - origin_y) / scale,
        )

    def _exact_path(self, glyph: Glyph) -> QPainterPath:
        path = QPainterPath()
        path.setFillRule(Qt.FillRule.OddEvenFill)
        for contour in glyph.contours:
            path.moveTo(self._to_screen(contour.start))
            for segment in contour.segments:
                if segment.kind == "line":
                    path.lineTo(self._to_screen(segment.p1))
                elif segment.kind == "quadratic":
                    path.quadTo(
                        self._to_screen(segment.p1),
                        self._to_screen(segment.p2),  # type: ignore[arg-type]
                    )
                else:
                    path.cubicTo(
                        self._to_screen(segment.p1),
                        self._to_screen(segment.p2),  # type: ignore[arg-type]
                        self._to_screen(segment.p3),  # type: ignore[arg-type]
                    )
            path.closeSubpath()
        return path

    def paintEvent(self, event) -> None:  # type: ignore[no-untyped-def]
        painter = QPainter(self)
        painter.setRenderHint(QPainter.RenderHint.Antialiasing, True)
        painter.fillRect(self.rect(), QColor(BG))
        painter.fillRect(self.rect().adjusted(1, 1, -1, -1), QColor("#101720"))
        if self.session is None:
            painter.setPen(QColor(MUTED))
            painter.drawText(self.rect(), Qt.AlignmentFlag.AlignCenter, "未加载字形")
            return
        glyph = self.session.working_glyph
        bbox = glyph_bbox(glyph)

        painter.setPen(QPen(QColor(GRID), 1))
        step = 100
        left = int(math.floor(bbox[0] / step) * step)
        right = int(math.ceil(bbox[2] / step) * step)
        bottom = int(math.floor(bbox[1] / step) * step)
        top = int(math.ceil(bbox[3] / step) * step)
        for x in range(left, right + 1, step):
            screen_x = self._to_screen((x, 0)).x()
            painter.drawLine(QPointF(screen_x, 10), QPointF(screen_x, self.height() - 10))
        for y in range(bottom, top + 1, step):
            screen_y = self._to_screen((0, y)).y()
            painter.drawLine(QPointF(10, screen_y), QPointF(self.width() - 10, screen_y))

        baseline_y = self._to_screen((bbox[0], glyph.baseline)).y()
        painter.setPen(QPen(QColor("#3a4a5f"), 1, Qt.PenStyle.DashLine))
        painter.drawLine(QPointF(18, baseline_y), QPointF(self.width() - 18, baseline_y))
        painter.setPen(QColor(MUTED))
        painter.setFont(QFont(self.font().family(), 8))
        painter.drawText(QPointF(20, baseline_y - 5), "BASELINE")

        bbox_path = QPainterPath()
        bbox_path.addRect(
            QRectF(
                self._to_screen((bbox[0], bbox[3])),
                self._to_screen((bbox[2], bbox[1])),
            )
        )
        painter.setPen(QPen(QColor("#35465a"), 1, Qt.PenStyle.DotLine))
        painter.setBrush(Qt.BrushStyle.NoBrush)
        painter.drawPath(bbox_path)

        path = self._exact_path(glyph)
        painter.setPen(QPen(QColor(ACCENT), 1.6))
        painter.setBrush(QColor(69, 214, 195, 35))
        painter.drawPath(path)

        if self.show_flattened and self.flat_contours:
            painter.setPen(QPen(QColor(99, 169, 255, 180), 1.0, Qt.PenStyle.DashLine))
            painter.setBrush(Qt.BrushStyle.NoBrush)
            ppu = max(self.session.working_parameters.pixels_per_unit, 1e-9)
            for flat in self.flat_contours:
                polygon = QPolygonF(
                    [self._to_screen((float(x) / ppu, float(y) / ppu)) for x, y in flat.points]
                )
                if polygon.size() > 2:
                    painter.drawPolygon(polygon)

        for contour_position, contour in enumerate(glyph.contours):
            selected_contour = contour_position == self.selected_contour
            for segment_position, segment in enumerate(contour.segments):
                selected = selected_contour and segment_position == self.selected_segment
                controls = segment.control_points()
                if len(controls) > 2:
                    painter.setPen(
                        QPen(
                            QColor("#6a7b90"),
                            1,
                            Qt.PenStyle.DotLine,
                        )
                    )
                    control_polygon = QPolygonF(
                        [self._to_screen(point) for _, point in controls]
                    )
                    painter.drawPolyline(control_polygon)
                if not selected:
                    continue
                painter.setPen(QPen(QColor(BLUE), 2))
                selected_path = QPainterPath()
                selected_path.moveTo(self._to_screen(segment.p0))
                if segment.kind == "line":
                    selected_path.lineTo(self._to_screen(segment.p1))
                elif segment.kind == "quadratic":
                    selected_path.quadTo(
                        self._to_screen(segment.p1),
                        self._to_screen(segment.p2),  # type: ignore[arg-type]
                    )
                else:
                    selected_path.cubicTo(
                        self._to_screen(segment.p1),
                        self._to_screen(segment.p2),  # type: ignore[arg-type]
                        self._to_screen(segment.p3),  # type: ignore[arg-type]
                    )
                painter.setBrush(Qt.BrushStyle.NoBrush)
                painter.drawPath(selected_path)

                for name, point in controls:
                    screen = self._to_screen(point)
                    endpoint = name == "p0" or name in ("p1", "p2", "p3") and (
                        (segment.kind == "line" and name == "p1")
                        or (segment.kind == "quadratic" and name == "p2")
                        or (segment.kind == "cubic" and name == "p3")
                    )
                    radius = 4.8 if endpoint else 3.3
                    if endpoint:
                        painter.setBrush(QColor(ORANGE))
                        painter.setPen(QPen(QColor("#26303d"), 1))
                        painter.drawEllipse(screen, radius, radius)
                    else:
                        painter.setBrush(QColor("#17202b"))
                        painter.setPen(QPen(QColor(BLUE), 1.4))
                        painter.drawRect(
                            QRectF(
                                screen.x() - radius,
                                screen.y() - radius,
                                radius * 2,
                                radius * 2,
                            )
                        )

        painter.setPen(QColor(MUTED))
        painter.setFont(QFont(self.font().family(), 9))
        contour = glyph.contours[self.selected_contour]
        segment = contour.segments[self.selected_segment]
        selected_text = (
            f"轮廓 {contour.index} / 曲线段 {self.selected_segment} / {segment.kind}"
        )
        painter.drawText(16, 24, selected_text)
        painter.setPen(QColor(ACCENT))
        painter.drawText(
            16,
            self.height() - 16,
            f"bbox  {bbox[0]:.1f}, {bbox[1]:.1f}  →  {bbox[2]:.1f}, {bbox[3]:.1f}",
        )

    def _hit_control(self, position: QPointF) -> tuple[int, int, str] | None:
        if self.session is None:
            return None
        best: tuple[float, int, int, str] | None = None
        for contour_position, contour in enumerate(self.session.working_glyph.contours):
            for segment_position, segment in enumerate(contour.segments):
                for name, point in segment.control_points():
                    screen = self._to_screen(point)
                    distance = math.hypot(
                        screen.x() - position.x(), screen.y() - position.y()
                    )
                    if best is None or distance < best[0]:
                        best = distance, contour_position, segment_position, name
        if best is not None and best[0] <= 11.0:
            return best[1], best[2], best[3]
        return None

    def _hit_segment(self, position: QPointF) -> tuple[int, int] | None:
        if self.session is None:
            return None
        best: tuple[float, int, int] | None = None
        glyph = self.session.working_glyph
        for contour_position, contour in enumerate(glyph.contours):
            for segment_position, segment in enumerate(contour.segments):
                for step in range(9):
                    from .geometry import segment_point

                    point = segment_point(segment, step / 8.0)
                    screen = self._to_screen((float(point[0]), float(point[1])))
                    distance = math.hypot(
                        screen.x() - position.x(), screen.y() - position.y()
                    )
                    if best is None or distance < best[0]:
                        best = distance, contour_position, segment_position
        if best is not None and best[0] <= 22.0:
            return best[1], best[2]
        return None

    def mousePressEvent(self, event) -> None:  # type: ignore[no-untyped-def]
        if event.button() != Qt.MouseButton.LeftButton:
            return
        hit = self._hit_control(event.position())
        if hit is not None:
            self._drag = hit
            self.selected_contour, self.selected_segment, _ = hit
            self.selection_changed.emit(self.selected_contour, self.selected_segment)
            self.setCursor(Qt.CursorShape.ClosedHandCursor)
            self.update()
            return
        segment = self._hit_segment(event.position())
        if segment is not None:
            self.selected_contour, self.selected_segment = segment
            self.selection_changed.emit(*segment)
            self.update()

    def mouseMoveEvent(self, event) -> None:  # type: ignore[no-untyped-def]
        if self._drag is None or self.session is None:
            hit = self._hit_control(event.position())
            self.setCursor(
                Qt.CursorShape.OpenHandCursor
                if hit is not None
                else Qt.CursorShape.CrossCursor
            )
            return
        contour_position, segment_position, control_name = self._drag
        world_x, world_y = self._to_world(event.position())
        try:
            self.session.set_control_point(
                contour_position,
                segment_position,
                control_name,
                (round(world_x), round(world_y)),
            )
        except WorkbenchError:
            return
        self.edited.emit()
        self.update()

    def mouseReleaseEvent(self, event) -> None:  # type: ignore[no-untyped-def]
        if self._drag is not None:
            self._drag = None
            self.setCursor(Qt.CursorShape.CrossCursor)
            self.update()


class RasterView(QWidget):
    def __init__(self, parent: QWidget | None = None) -> None:
        super().__init__(parent)
        self.setMinimumSize(360, 300)
        self.result: EngineResult | None = None
        self.show_grid = True
        self.stale = False

    def sizeHint(self) -> QSize:
        return QSize(520, 500)

    def set_result(self, result: EngineResult | None, stale: bool = False) -> None:
        self.result = result
        self.stale = stale
        self.update()

    def paintEvent(self, event) -> None:  # type: ignore[no-untyped-def]
        painter = QPainter(self)
        painter.setRenderHint(QPainter.RenderHint.Antialiasing, False)
        painter.fillRect(self.rect(), QColor(BG))
        painter.fillRect(self.rect().adjusted(1, 1, -1, -1), QColor("#0f151d"))
        if self.result is None:
            painter.setPen(QColor(MUTED))
            painter.drawText(
                self.rect(), Qt.AlignmentFlag.AlignCenter, "尚无覆盖位图\n运行增量处理或全量重算"
            )
            return
        result = self.result
        width = result.raster.width
        height = result.raster.height
        image = QImage(
            result.raster.coverage_u8.tobytes(),
            width,
            height,
            width,
            QImage.Format.Format_Grayscale8,
        ).copy()
        available = self.rect().adjusted(34, 34, -34, -54)
        scale = min(available.width() / width, available.height() / height)
        display_width = max(1.0, width * scale)
        display_height = max(1.0, height * scale)
        target = QRectF(
            available.x() + (available.width() - display_width) * 0.5,
            available.y() + (available.height() - display_height) * 0.5,
            display_width,
            display_height,
        )
        painter.fillRect(target.adjusted(-1, -1, 1, 1), QColor("#2a3747"))
        painter.drawImage(target, image)
        if self.show_grid and scale >= 5.0:
            painter.setPen(QPen(QColor(90, 110, 130, 45), 1))
            for x in range(width + 1):
                screen_x = target.left() + x * scale
                painter.drawLine(
                    QPointF(screen_x, target.top()),
                    QPointF(screen_x, target.bottom()),
                )
            for y in range(height + 1):
                screen_y = target.top() + y * scale
                painter.drawLine(
                    QPointF(target.left(), screen_y),
                    QPointF(target.right(), screen_y),
                )
        painter.setPen(QColor(MUTED))
        painter.setFont(QFont(self.font().family(), 9))
        paintable = (
            f"{width}×{height} px   grid {result.raster.params.grid}   "
            f"area {result.raster.area:.1f} units²"
        )
        painter.drawText(
            QRectF(16, 10, self.width() - 32, 22),
            Qt.AlignmentFlag.AlignLeft | Qt.AlignmentFlag.AlignVCenter,
            paintable,
        )
        if self.stale:
            painter.fillRect(
                QRectF(16, self.height() - 34, 154, 22), QColor(255, 180, 84, 28)
            )
            painter.setPen(QColor(ORANGE))
            painter.drawText(
                QRectF(22, self.height() - 34, 160, 22),
                Qt.AlignmentFlag.AlignVCenter,
                "工作副本未提交 · 光栅已过期",
            )


class StateTable(QTableWidget):
    retry_requested = Signal(int)

    def __init__(self, parent: QWidget | None = None) -> None:
        super().__init__(parent)
        self.setColumnCount(6)
        self.setHorizontalHeaderLabels(
            ["段", "轮廓", "状态", "已提交", "折线点", "缓存摘要"]
        )
        self.verticalHeader().setVisible(False)
        self.setSelectionBehavior(QTableWidget.SelectionBehavior.SelectRows)
        self.setSelectionMode(QTableWidget.SelectionMode.SingleSelection)
        self.setEditTriggers(QTableWidget.EditTrigger.NoEditTriggers)
        self.setAlternatingRowColors(True)
        self.setShowGrid(False)
        self.horizontalHeader().setStretchLastSection(True)
        self.setColumnWidth(0, 42)
        self.setColumnWidth(1, 52)
        self.setColumnWidth(2, 104)
        self.setColumnWidth(3, 64)
        self.setColumnWidth(4, 72)
        self.cellDoubleClicked.connect(lambda row, column: self.retry_requested.emit(row))

    def set_states(self, states: dict[str, Any]) -> None:
        segments = states.get("segments", [])
        self.setRowCount(len(segments))
        palette = {
            "PENDING_FLATTEN": QColor("#8292a6"),
            "FLATTENED": QColor(BLUE),
            "FILLED": QColor(ORANGE),
            "COMMITTED": QColor(GREEN),
            "FAILED": QColor(RED),
        }
        for row, record in enumerate(segments):
            state = record.get("state", "PENDING_FLATTEN")
            values = [
                str(record.get("index", row)),
                str(record.get("contour_index", row)),
                STATE_LABELS.get(SegmentState(state), state),
                "是" if record.get("committed") else "否",
                str(record.get("flat_count", 0)),
                (record.get("cache_hash") or "—")[:12],
            ]
            for column, value in enumerate(values):
                item = QTableWidgetItem(value)
                if column == 2:
                    item.setForeground(palette.get(state, QColor(TEXT)))
                    if state == "FAILED":
                        item.setToolTip(
                            f"{record.get('error_code') or ''}\n{record.get('error_message') or ''}"
                        )
                if column in (0, 3, 4):
                    item.setTextAlignment(Qt.AlignmentFlag.AlignCenter)
                self.setItem(row, column, item)


class MainWindow(QMainWindow):
    def __init__(
        self,
        *,
        model_path: str | Path,
        persist_root: str | Path,
        state_model: str | Path,
        glyph_id: str | None,
        params: RenderParams,
    ) -> None:
        super().__init__()
        install_bundled_font()
        self.setWindowTitle("Glyph Forge · 字形光栅工作台")
        self.resize(1540, 930)
        self.setMinimumSize(1180, 760)
        self.bootstrap_model = Path(model_path)
        self.persist_root = Path(persist_root)
        self.state_model_path = Path(state_model)
        self.persist_root.mkdir(parents=True, exist_ok=True)
        if not self.bootstrap_model.exists():
            generate_samples("samples")
        ensure_model_copy(self.bootstrap_model, self.state_model_path)
        self.document = reload_document(self.state_model_path)
        selected = glyph_id or self.document.glyphs[0].glyph_id
        if selected not in [glyph.glyph_id for glyph in self.document.glyphs]:
            selected = self.document.glyphs[0].glyph_id
        self.session = EditSession(
            self.document,
            selected,
            params,
            self.persist_root,
            self.state_model_path,
        )
        self.worker: WorkbenchWorker | None = None
        self._worker_mode: str | None = None
        self.last_result: EngineResult | None = None
        self._syncing = False
        self._build_ui()
        self._apply_style()
        self._populate_glyph_combo(selected)
        self._sync_controls(params)
        self._refresh_all("就绪")
        self._set_busy(False)
        QTimer.singleShot(180, self.resume_processing)

    def _build_ui(self) -> None:
        central = QWidget()
        self.setCentralWidget(central)
        root = QVBoxLayout(central)
        root.setContentsMargins(18, 14, 18, 14)
        root.setSpacing(12)

        header = QFrame()
        header.setObjectName("Header")
        header_layout = QHBoxLayout(header)
        header_layout.setContentsMargins(18, 12, 18, 12)
        title_column = QVBoxLayout()
        title_column.setSpacing(0)
        title = QLabel("GLYPH FORGE")
        title.setObjectName("Title")
        subtitle = QLabel("游戏 UI / 互动媒体 · 轮廓编辑与确定性文字光栅化")
        subtitle.setObjectName("Subtitle")
        title_column.addWidget(title)
        title_column.addWidget(subtitle)
        header_layout.addLayout(title_column)
        header_layout.addStretch(1)
        self.header_status = QLabel("INITIALIZING")
        self.header_status.setObjectName("StatusPill")
        self.header_status.setAlignment(Qt.AlignmentFlag.AlignCenter)
        self.header_status.setMinimumWidth(150)
        header_layout.addWidget(self.header_status)
        root.addWidget(header)

        splitter = QSplitter(Qt.Orientation.Horizontal)
        splitter.setChildrenCollapsible(False)
        splitter.addWidget(self._build_left_panel())
        splitter.addWidget(self._build_center_panel())
        splitter.addWidget(self._build_log_panel())
        splitter.setSizes([310, 900, 300])
        root.addWidget(splitter, 1)

        footer = QFrame()
        footer.setObjectName("Footer")
        footer_layout = QHBoxLayout(footer)
        footer_layout.setContentsMargins(12, 7, 12, 7)
        self.footer_status = QLabel("就绪")
        self.footer_status.setObjectName("FooterText")
        self.progress = QProgressBar()
        self.progress.setRange(0, 1)
        self.progress.setValue(0)
        self.progress.setTextVisible(True)
        self.progress.setFixedWidth(220)
        self.progress.setFormat("%v / %m 段已提交")
        self.algorithm_label = QLabel("adaptive-bezier + scanline supersample")
        self.algorithm_label.setObjectName("FooterText")
        footer_layout.addWidget(self.footer_status)
        footer_layout.addStretch(1)
        footer_layout.addWidget(self.algorithm_label)
        footer_layout.addWidget(self.progress)
        root.addWidget(footer)

    def _build_left_panel(self) -> QWidget:
        panel = QFrame()
        panel.setObjectName("Panel")
        panel.setMinimumWidth(285)
        panel.setMaximumWidth(360)
        layout = QVBoxLayout(panel)
        layout.setContentsMargins(14, 14, 14, 14)
        layout.setSpacing(11)

        glyph_label = QLabel("GLYPH")
        glyph_label.setObjectName("SectionLabel")
        self.glyph_combo = QComboBox()
        self.glyph_combo.currentTextChanged.connect(self._on_glyph_changed)
        layout.addWidget(glyph_label)
        layout.addWidget(self.glyph_combo)

        params_group = QGroupBox("光栅参数")
        form = QFormLayout(params_group)
        form.setContentsMargins(10, 14, 10, 10)
        form.setVerticalSpacing(7)
        self.ppu_spin = QDoubleSpinBox()
        self.ppu_spin.setRange(0.01, 64.0)
        self.ppu_spin.setDecimals(3)
        self.ppu_spin.setSingleStep(0.01)
        self.ppu_spin.valueChanged.connect(self._on_parameters_changed)
        self.tolerance_spin = QDoubleSpinBox()
        self.tolerance_spin.setRange(0.0001, 16.0)
        self.tolerance_spin.setDecimals(4)
        self.tolerance_spin.setSingleStep(0.05)
        self.tolerance_spin.valueChanged.connect(self._on_parameters_changed)
        self.grid_spin = QSpinBox()
        self.grid_spin.setRange(1, 16)
        self.grid_spin.valueChanged.connect(self._on_parameters_changed)
        self.workers_spin = QSpinBox()
        self.workers_spin.setRange(1, 8)
        self.workers_spin.valueChanged.connect(self._on_parameters_changed)
        self.buffer_spin = QDoubleSpinBox()
        self.buffer_spin.setRange(1, 512)
        self.buffer_spin.setDecimals(0)
        self.buffer_spin.setSuffix(" MB")
        self.buffer_spin.valueChanged.connect(self._on_parameters_changed)
        form.addRow("像素/单位", self.ppu_spin)
        form.addRow("扁平化容差", self.tolerance_spin)
        form.addRow("光栅网格", self.grid_spin)
        form.addRow("并行线程", self.workers_spin)
        form.addRow("线程缓冲上限", self.buffer_spin)
        layout.addWidget(params_group)

        self.truth_label = QLabel()
        self.truth_label.setObjectName("InfoCard")
        self.truth_label.setWordWrap(True)
        layout.addWidget(self.truth_label)

        process_group = QGroupBox("处理与恢复")
        process_grid = QGridLayout(process_group)
        process_grid.setContentsMargins(9, 14, 9, 9)
        process_grid.setSpacing(6)
        self.incremental_btn = QPushButton("增量提交")
        self.incremental_btn.setProperty("role", "primary")
        self.full_btn = QPushButton("全量重算")
        self.pause_btn = QPushButton("暂停")
        self.resume_btn = QPushButton("恢复检查点")
        self.retry_btn = QPushButton("重试选中段")
        self.validate_btn = QPushButton("结构校验")
        process_grid.addWidget(self.incremental_btn, 0, 0)
        process_grid.addWidget(self.full_btn, 0, 1)
        process_grid.addWidget(self.pause_btn, 1, 0)
        process_grid.addWidget(self.resume_btn, 1, 1)
        process_grid.addWidget(self.retry_btn, 2, 0)
        process_grid.addWidget(self.validate_btn, 2, 1)
        layout.addWidget(process_group)

        edit_group = QGroupBox("编辑事务")
        edit_grid = QGridLayout(edit_group)
        edit_grid.setContentsMargins(9, 14, 9, 9)
        edit_grid.setSpacing(6)
        self.split_btn = QPushButton("细分新增曲线段")
        self.delete_btn = QPushButton("删除选中曲线段")
        self.commit_btn = QPushButton("提交批次")
        self.commit_btn.setProperty("role", "success")
        self.rollback_btn = QPushButton("回滚工作副本")
        self.export_btn = QPushButton("导出已提交 PNG")
        edit_grid.addWidget(self.split_btn, 0, 0)
        edit_grid.addWidget(self.delete_btn, 0, 1)
        edit_grid.addWidget(self.commit_btn, 1, 0)
        edit_grid.addWidget(self.rollback_btn, 1, 1)
        edit_grid.addWidget(self.export_btn, 2, 0, 1, 2)
        layout.addWidget(edit_group)

        self.dirty_label = QLabel("工作副本干净")
        self.dirty_label.setObjectName("DirtyClean")
        layout.addWidget(self.dirty_label)
        layout.addStretch(1)

        self.incremental_btn.clicked.connect(self.incremental_processing)
        self.full_btn.clicked.connect(self.full_recompute)
        self.pause_btn.clicked.connect(self.pause_processing)
        self.resume_btn.clicked.connect(self.resume_processing)
        self.retry_btn.clicked.connect(self.retry_processing)
        self.validate_btn.clicked.connect(self.validate_structure)
        self.split_btn.clicked.connect(self.split_selected)
        self.delete_btn.clicked.connect(self.delete_selected)
        self.commit_btn.clicked.connect(self.commit_batch)
        self.rollback_btn.clicked.connect(self.rollback_working)
        self.export_btn.clicked.connect(self.export_png)
        return panel

    def _build_center_panel(self) -> QWidget:
        splitter = QSplitter(Qt.Orientation.Vertical)
        splitter.setChildrenCollapsible(False)
        visual_splitter = QSplitter(Qt.Orientation.Horizontal)
        visual_splitter.setChildrenCollapsible(False)

        outline_group = QGroupBox("轮廓 / 控制点 / 扁平化折线")
        outline_layout = QVBoxLayout(outline_group)
        outline_layout.setContentsMargins(8, 10, 8, 8)
        outline_bar = QHBoxLayout()
        self.flat_check = QCheckBox("显示扁平化折线")
        self.flat_check.setChecked(True)
        self.flat_check.toggled.connect(self._toggle_flat)
        self.outline_info = QLabel("拖动控制点编辑工作副本")
        self.outline_info.setObjectName("Muted")
        outline_bar.addWidget(self.flat_check)
        outline_bar.addStretch(1)
        outline_bar.addWidget(self.outline_info)
        self.outline = OutlineView()
        self.outline.selection_changed.connect(self._on_selection_changed)
        self.outline.edited.connect(self._on_outline_edited)
        outline_layout.addLayout(outline_bar)
        outline_layout.addWidget(self.outline, 1)

        raster_group = QGroupBox("灰度覆盖 / 抗锯齿位图")
        raster_layout = QVBoxLayout(raster_group)
        raster_layout.setContentsMargins(8, 10, 8, 8)
        raster_bar = QHBoxLayout()
        self.raster_grid_check = QCheckBox("像素网格")
        self.raster_grid_check.setChecked(True)
        self.raster_grid_check.toggled.connect(self._toggle_raster_grid)
        self.raster_info = QLabel("覆盖值按网格面积归一")
        self.raster_info.setObjectName("Muted")
        raster_bar.addWidget(self.raster_grid_check)
        raster_bar.addStretch(1)
        raster_bar.addWidget(self.raster_info)
        self.raster = RasterView()
        raster_layout.addLayout(raster_bar)
        raster_layout.addWidget(self.raster, 1)
        visual_splitter.addWidget(outline_group)
        visual_splitter.addWidget(raster_group)
        visual_splitter.setSizes([620, 520])

        state_group = QGroupBox("分段状态矩阵")
        state_layout = QVBoxLayout(state_group)
        state_layout.setContentsMargins(8, 10, 8, 8)
        state_bar = QHBoxLayout()
        self.state_summary = QLabel("0 / 0 committed")
        self.state_summary.setObjectName("Muted")
        self.state_hint = QLabel("双击失败段触发重试")
        self.state_hint.setObjectName("Muted")
        state_bar.addWidget(self.state_summary)
        state_bar.addStretch(1)
        state_bar.addWidget(self.state_hint)
        self.state_table = StateTable()
        self.state_table.retry_requested.connect(self._retry_row)
        self.state_table.itemSelectionChanged.connect(self._on_state_selection)
        state_layout.addLayout(state_bar)
        state_layout.addWidget(self.state_table, 1)
        splitter.addWidget(visual_splitter)
        splitter.addWidget(state_group)
        splitter.setSizes([570, 220])
        return splitter

    def _build_log_panel(self) -> QWidget:
        panel = QFrame()
        panel.setObjectName("Panel")
        panel.setMinimumWidth(260)
        panel.setMaximumWidth(390)
        layout = QVBoxLayout(panel)
        layout.setContentsMargins(12, 14, 12, 12)
        label = QLabel("事件与诊断")
        label.setObjectName("SectionLabel")
        self.log = QPlainTextEdit()
        self.log.setReadOnly(True)
        self.log.setMaximumBlockCount(600)
        self.log.setObjectName("Log")
        self.render_info = QLabel("尚无渲染结果")
        self.render_info.setObjectName("InfoCard")
        self.render_info.setWordWrap(True)
        self.commit_info = QLabel("提交边界：未提交编辑不参与导出")
        self.commit_info.setObjectName("Muted")
        self.commit_info.setWordWrap(True)
        layout.addWidget(label)
        layout.addWidget(self.log, 1)
        layout.addWidget(self.render_info)
        layout.addWidget(self.commit_info)
        return panel

    def _apply_style(self) -> None:
        self.setStyleSheet(
            f"""
            QWidget {{
                background: {BG};
                color: {TEXT};
                font-family: "WenQuanYi Micro Hei", "Segoe UI", sans-serif;
                font-size: 12px;
            }}
            QFrame#Header {{
                background: qlineargradient(x1:0, y1:0, x2:1, y2:0,
                    stop:0 #141d27, stop:0.62 #17222d, stop:1 #13202a);
                border: 1px solid {BORDER};
                border-radius: 10px;
            }}
            QFrame#Panel, QFrame#Footer {{
                background: {PANEL};
                border: 1px solid {BORDER};
                border-radius: 9px;
            }}
            QFrame#Footer {{
                background: #111821;
            }}
            QLabel#Title {{
                color: {ACCENT};
                font-size: 22px;
                font-weight: 800;
                letter-spacing: 3px;
            }}
            QLabel#Subtitle {{
                color: {MUTED};
                font-size: 11px;
                padding-top: 2px;
            }}
            QLabel#StatusPill {{
                background: #0f2d2a;
                color: {ACCENT};
                border: 1px solid #245f59;
                border-radius: 11px;
                padding: 4px 12px;
                font-weight: 700;
                font-size: 10px;
                letter-spacing: 1px;
            }}
            QLabel#SectionLabel {{
                color: {MUTED};
                font-size: 10px;
                font-weight: 700;
                letter-spacing: 2px;
                margin-top: 2px;
            }}
            QLabel#Muted, QLabel#FooterText {{
                color: {MUTED};
                font-size: 10px;
            }}
            QLabel#InfoCard {{
                background: {PANEL_ALT};
                border: 1px solid {BORDER};
                border-radius: 7px;
                padding: 9px;
                color: #bdc9d7;
                font-family: "WenQuanYi Micro Hei Mono", "Cascadia Mono", monospace;
                font-size: 10px;
            }}
            QLabel#DirtyClean {{
                color: {GREEN};
                padding: 6px 8px;
                border-left: 3px solid {GREEN};
                background: #10241c;
            }}
            QLabel#DirtyDirty {{
                color: {ORANGE};
                padding: 6px 8px;
                border-left: 3px solid {ORANGE};
                background: #2a2114;
            }}
            QGroupBox {{
                background: #121922;
                border: 1px solid {BORDER};
                border-radius: 9px;
                margin-top: 9px;
                padding-top: 8px;
                font-size: 10px;
                font-weight: 700;
                color: #9fb0c4;
            }}
            QGroupBox::title {{
                subcontrol-origin: margin;
                left: 12px;
                padding: 0 5px;
                color: #aebed0;
            }}
            QPushButton {{
                background: #1c2733;
                border: 1px solid #304154;
                border-radius: 6px;
                padding: 7px 8px;
                color: #d7e2ee;
                font-weight: 600;
            }}
            QPushButton:hover {{
                background: #253344;
                border-color: #46617e;
            }}
            QPushButton:pressed {{
                background: #141d27;
            }}
            QPushButton:disabled {{
                color: #536275;
                border-color: #25303d;
                background: #141b23;
            }}
            QPushButton[role="primary"] {{
                background: #123b38;
                border-color: #2d8d83;
                color: {ACCENT};
            }}
            QPushButton[role="primary"]:hover {{
                background: #174c47;
            }}
            QPushButton[role="success"] {{
                background: #14341f;
                border-color: #397f4e;
                color: {GREEN};
            }}
            QComboBox, QDoubleSpinBox, QSpinBox {{
                background: #0f161f;
                border: 1px solid #2b3a4b;
                border-radius: 5px;
                padding: 5px 7px;
                min-height: 20px;
                selection-background-color: #1f5d56;
            }}
            QComboBox::drop-down, QDoubleSpinBox::up-button, QSpinBox::up-button {{
                border: 0;
                width: 18px;
            }}
            QCheckBox {{
                color: {MUTED};
                spacing: 6px;
                font-size: 10px;
            }}
            QCheckBox::indicator {{
                width: 13px;
                height: 13px;
                border: 1px solid #3b4d61;
                border-radius: 3px;
                background: #0f161f;
            }}
            QCheckBox::indicator:checked {{
                background: {ACCENT_DARK};
                border-color: {ACCENT};
            }}
            QTableWidget {{
                background: #0f151d;
                alternate-background-color: #121a24;
                border: 1px solid {BORDER};
                border-radius: 6px;
                gridline-color: #202b38;
                selection-background-color: #1d3d45;
                selection-color: #ffffff;
                font-family: "WenQuanYi Micro Hei Mono", "Cascadia Mono", monospace;
                font-size: 10px;
            }}
            QHeaderView::section {{
                background: #18222d;
                color: {MUTED};
                border: 0;
                border-bottom: 1px solid {BORDER};
                padding: 6px;
                font-size: 9px;
                font-weight: 700;
            }}
            QPlainTextEdit#Log {{
                background: #0c1219;
                border: 1px solid {BORDER};
                border-radius: 7px;
                color: #96a7ba;
                selection-background-color: #1d4d49;
                font-family: "WenQuanYi Micro Hei Mono", "Cascadia Mono", monospace;
                font-size: 10px;
                padding: 7px;
            }}
            QProgressBar {{
                background: #0c1219;
                border: 1px solid {BORDER};
                border-radius: 4px;
                color: {MUTED};
                text-align: center;
                font-size: 9px;
                min-height: 15px;
            }}
            QProgressBar::chunk {{
                background: {ACCENT_DARK};
                border-radius: 3px;
            }}
            QSplitter::handle {{
                background: transparent;
                width: 8px;
                height: 8px;
            }}
            QScrollBar:vertical {{
                background: #0f151d;
                width: 9px;
                margin: 0;
            }}
            QScrollBar::handle:vertical {{
                background: #2a3848;
                border-radius: 4px;
                min-height: 24px;
            }}
            QToolTip {{
                background: #1b2632;
                color: {TEXT};
                border: 1px solid #3b4d61;
            }}
            """
        )

    def _populate_glyph_combo(self, selected: str) -> None:
        self._syncing = True
        self.glyph_combo.clear()
        for glyph in self.document.glyphs:
            self.glyph_combo.addItem(glyph.glyph_id)
        self.glyph_combo.setCurrentText(selected)
        self._syncing = False

    def _sync_controls(self, params: RenderParams) -> None:
        self._syncing = True
        self.ppu_spin.setValue(params.pixels_per_unit)
        self.tolerance_spin.setValue(params.tolerance)
        self.grid_spin.setValue(params.grid)
        self.workers_spin.setValue(params.max_workers)
        self.buffer_spin.setValue(params.max_buffer_bytes / (1024 * 1024))
        self._syncing = False

    def _parameters_from_controls(self) -> RenderParams:
        return RenderParams(
            pixels_per_unit=self.ppu_spin.value(),
            tolerance=self.tolerance_spin.value(),
            grid=self.grid_spin.value(),
            max_workers=self.workers_spin.value(),
            max_buffer_bytes=int(self.buffer_spin.value() * 1024 * 1024),
        )

    def _log(self, message: str, tone: str = "info") -> None:
        prefix = QTime.currentTime().toString("HH:mm:ss")
        self.log.appendPlainText(f"[{prefix}] {message}")
        self.footer_status.setText(message)

    def _set_status(self, text: str, tone: str = "ready") -> None:
        colors = {
            "ready": ("#0f2d2a", ACCENT, "#245f59"),
            "busy": ("#2a2114", ORANGE, "#5e4520"),
            "error": ("#35171c", RED, "#7b2d38"),
            "ok": ("#10241c", GREEN, "#295e40"),
        }
        background, foreground, border = colors[tone]
        self.header_status.setText(text)
        self.header_status.setStyleSheet(
            f"background:{background};color:{foreground};border:1px solid {border};"
            "border-radius:11px;padding:4px 12px;font-weight:700;font-size:10px;"
            "letter-spacing:1px;"
        )

    def _refresh_all(self, message: str | None = None) -> None:
        self.outline.set_session(self.session, self.last_result)
        self.raster.set_result(
            self.last_result, stale=bool(self.session.dirty_contours)
        )
        self._refresh_state_table()
        self._refresh_info()
        if message:
            self._set_status(message.upper(), "ready")

    def _pending_states(self) -> dict[str, Any]:
        count = len(self.session.working_glyph.contours)
        segments = []
        for index in range(count):
            segments.append(
                {
                    "index": index,
                    "contour_index": self.session.working_glyph.contours[index].index,
                    "state": SegmentState.PENDING_FLATTEN.value,
                    "state_label": STATE_LABELS[SegmentState.PENDING_FLATTEN],
                    "committed": False,
                    "error_code": None,
                    "error_message": None,
                    "attempts": 0,
                    "flat_count": 0,
                    "cache_hash": None,
                }
            )
        return {
            "committed_count": 0,
            "total": count,
            "progress": f"0/{count}",
            "commit_sequence": [],
            "segments": segments,
        }

    def _refresh_state_table(self) -> None:
        states = (
            self.last_result.states
            if self.last_result is not None
            else self._pending_states()
        )
        self.state_table.set_states(states)
        committed = states.get("committed_count", 0)
        total = states.get("total", 0)
        self.progress.setRange(0, max(1, total))
        self.progress.setValue(committed)
        self.progress.setFormat(f"{committed} / {total} 段已提交")
        self.state_summary.setText(
            f"{committed} / {total} committed   "
            f"sequence {states.get('commit_sequence', [])}"
        )

    def _refresh_info(self) -> None:
        glyph = self.session.working_glyph
        bbox = glyph_bbox(glyph)
        segment_count = sum(len(contour.segments) for contour in glyph.contours)
        curve_types = sorted(
            {
                segment.kind
                for contour in glyph.contours
                for segment in contour.segments
            }
        )
        truth = glyph.truth
        lines = [
            f"glyph {glyph.glyph_id}   baseline {glyph.baseline}   advance {glyph.advance_width}",
            f"contours {len(glyph.contours)}   curves {segment_count}   {' / '.join(curve_types)}",
            f"bbox {bbox[0]:.1f}, {bbox[1]:.1f} → {bbox[2]:.1f}, {bbox[3]:.1f}",
        ]
        if truth:
            lines.append(f"truth area {truth.area:.2f} units²")
        self.truth_label.setText("\n".join(lines))
        dirty = self.session.dirty_contours
        if dirty:
            self.dirty_label.setObjectName("DirtyDirty")
            self.dirty_label.setText(f"工作副本已修改 · 脏轮廓 {sorted(dirty)}")
        else:
            self.dirty_label.setObjectName("DirtyClean")
            self.dirty_label.setText("工作副本干净 · 与模型一致")
        self.dirty_label.style().unpolish(self.dirty_label)
        self.dirty_label.style().polish(self.dirty_label)
        if self.last_result is not None:
            result = self.last_result
            info = [
                f"raster {result.raster.width}×{result.raster.height}   grid {result.raster.params.grid}   ppu {result.raster.params.pixels_per_unit}",
                f"coverage area {result.raster.area:.2f} units²",
            ]
            if result.deviation:
                info.append(
                    f"flatten deviation max {result.deviation['maximum']:.4f} px / p95 {result.deviation['p95']:.4f} px"
                )
            if result.loaded_from_checkpoint:
                info.append(
                    f"resumed {result.loaded_from_checkpoint}/{result.total_segments} committed segments"
                )
            if result.checkpoint_invalidated:
                info.append(f"checkpoint invalidated: {result.checkpoint_reason}")
            self.render_info.setText("\n".join(info))
        else:
            self.render_info.setText("尚无渲染结果")

    def _on_selection_changed(self, contour_position: int, segment_position: int) -> None:
        self.outline.selected_contour = contour_position
        self.outline.selected_segment = segment_position
        glyph = self.session.working_glyph
        segment = glyph.contours[contour_position].segments[segment_position]
        self.outline_info.setText(
            f"轮廓 {contour_position} · 段 {segment_position} · {segment.kind} · 可拖动控制点"
        )
        self.outline.update()

    def _on_state_selection(self) -> None:
        rows = self.state_table.selectionModel().selectedRows()
        if rows:
            self.outline.select_contour(rows[0].row())

    def _on_outline_edited(self) -> None:
        self.last_result = self.session.working_result
        if self.last_result is not None:
            self.raster.set_result(self.last_result, stale=True)
        self._refresh_info()
        self._set_status("EDITING", "busy")

    def _toggle_flat(self, enabled: bool) -> None:
        self.outline.show_flattened = enabled
        self.outline.update()

    def _toggle_raster_grid(self, enabled: bool) -> None:
        self.raster.show_grid = enabled
        self.raster.update()

    def _on_parameters_changed(self) -> None:
        if self._syncing:
            return
        try:
            params = self._parameters_from_controls()
            params.validate()
            self.session.set_parameters(params)
            self._refresh_info()
            self._set_status("PARAMETERS DIRTY", "busy")
            self._log("参数已作用于工作副本，等待处理或提交")
        except WorkbenchError as exc:
            self._log(str(exc), "error")

    def _on_glyph_changed(self, glyph_id: str) -> None:
        if self._syncing or not glyph_id or glyph_id == self.session.glyph_id:
            return
        if self.session.dirty_contours:
            answer = QMessageBox.question(
                self,
                "切换字形",
                "当前工作副本存在未提交编辑，切换将回滚这些编辑。继续？",
                QMessageBox.StandardButton.Yes | QMessageBox.StandardButton.No,
                QMessageBox.StandardButton.No,
            )
            if answer != QMessageBox.StandardButton.Yes:
                self._syncing = True
                self.glyph_combo.setCurrentText(self.session.glyph_id)
                self._syncing = False
                return
        self.document = reload_document(self.state_model_path)
        self.session = EditSession(
            self.document,
            glyph_id,
            self._parameters_from_controls(),
            self.persist_root,
            self.state_model_path,
        )
        self.last_result = None
        self.outline.selected_contour = 0
        self.outline.selected_segment = 0
        self._refresh_all(f"已加载 {glyph_id}")
        self._log(f"切换字形 {glyph_id}")
        QTimer.singleShot(80, self.resume_processing)

    def _busy(self) -> bool:
        return self.worker is not None and self.worker.isRunning()

    def _set_busy(self, busy: bool) -> None:
        for widget in (
            self.glyph_combo,
            self.incremental_btn,
            self.full_btn,
            self.resume_btn,
            self.commit_btn,
            self.rollback_btn,
            self.split_btn,
            self.delete_btn,
            self.ppu_spin,
            self.tolerance_spin,
            self.grid_spin,
        ):
            widget.setEnabled(not busy)
        self.outline.setEnabled(not busy)
        self.pause_btn.setEnabled(busy)

    def _start_worker(self, mode: str, *, force_full: bool = False) -> None:
        if self._busy():
            self._log("已有处理任务在运行", "error")
            return
        params = self._parameters_from_controls()
        params.validate()
        self.session.set_parameters(params)
        self._set_busy(True)
        self._set_status("RUNNING", "busy")
        self._worker_mode = mode
        self._log(
            f"启动 {mode} · {params.pixels_per_unit} ppu · tol {params.tolerance} · grid {params.grid} · {params.max_workers} workers"
        )
        self.worker = WorkbenchWorker(
            self.session, mode, force_full=force_full, parent=self
        )
        self.worker.progress.connect(self._on_worker_progress)
        self.worker.committed.connect(
            lambda index: self._log(f"segment {index} committed atomically")
        )
        self.worker.succeeded.connect(self._on_worker_succeeded)
        self.worker.failed.connect(self._on_worker_failed)
        self.worker.finished.connect(self._on_worker_finished)
        self.worker.start()

    def incremental_processing(self) -> None:
        self._start_worker("incremental")

    def full_recompute(self) -> None:
        self._start_worker("full", force_full=True)

    def resume_processing(self) -> None:
        if self._busy():
            return
        self._start_worker("resume")

    def pause_processing(self) -> None:
        if self.worker is not None and self.worker.isRunning():
            self.worker.request_pause()
            self._set_status("PAUSE REQUESTED", "busy")
            self._log("已请求在当前段提交边界暂停")

    def retry_processing(self) -> None:
        rows = self.state_table.selectionModel().selectedRows()
        row = rows[0].row() if rows else 0
        self._log(f"重试选中段 {row}：从持久化已提交前缀继续")
        self._start_worker("resume")

    def _retry_row(self, row: int) -> None:
        self.state_table.selectRow(row)
        self._log(f"重试段 {row}")
        self._start_worker("resume")

    def validate_structure(self) -> None:
        try:
            from .models import parse_document

            parse_document(self.session.working_document.to_dict(include_truth=True))  # type: ignore[attr-defined]
            self._set_status("SCHEMA OK", "ok")
            self._log("结构校验通过：闭合性、连续性、量化范围与段类型均有效")
        except WorkbenchError as exc:
            self._set_status(exc.code, "error")
            self._log(str(exc), "error")

    def split_selected(self) -> None:
        try:
            self.session.split_segment(
                self.outline.selected_contour, self.outline.selected_segment
            )
            self.last_result = self.session.working_result
            self._refresh_info()
            self.outline.update()
            self.raster.set_result(self.last_result, stale=True)
            self._set_status("EDITING", "busy")
            self._log("已在 t=0.5 细分选中曲线段")
        except WorkbenchError as exc:
            self._log(str(exc), "error")

    def delete_selected(self) -> None:
        try:
            self.session.delete_segment(
                self.outline.selected_contour, self.outline.selected_segment
            )
            self.last_result = self.session.working_result
            self._refresh_info()
            self.outline.update()
            self.raster.set_result(self.last_result, stale=True)
            self._set_status("EDITING", "busy")
            self._log("已删除选中曲线段并修复闭合连续性")
        except WorkbenchError as exc:
            self._log(str(exc), "error")

    def commit_batch(self) -> None:
        if self._busy():
            return
        self._set_status("COMMIT", "busy")
        self._start_worker("commit")

    def rollback_working(self) -> None:
        if self._busy():
            return
        self.session.rollback()
        self.last_result = self.session.working_result
        self._refresh_all("ROLLED BACK")
        self._log("工作副本已回滚到最近提交模型")

    def export_png(self) -> None:
        try:
            result = self.session.export_result()
            base = (
                Path("exports")
                / f"{self.session.glyph_id}_{result.raster.params.params_hash()[:10]}"
            )
            paths = export_result(result, base)
            path = paths.get("png") or paths["pgm"]
            self._log(f"已导出已提交覆盖位图：{path}")
            self.commit_info.setText(
                f"最近导出\n{path}\n像素哈希 {__import__('hashlib').sha256(result.raster.coverage_counts.tobytes()).hexdigest()[:20]}"
            )
            self._set_status("EXPORTED", "ok")
        except WorkbenchError as exc:
            self._log(str(exc), "error")

    def _on_worker_progress(self, result: EngineResult) -> None:
        self.last_result = result
        self.outline.set_result(result)
        self.raster.set_result(result)
        self.state_table.set_states(result.states)
        committed = result.states["committed_count"]
        total = result.states["total"]
        self.progress.setRange(0, max(1, total))
        self.progress.setValue(committed)
        self.state_summary.setText(
            f"{committed} / {total} committed   sequence {result.commit_sequence}"
        )

    def _on_worker_succeeded(self, result: EngineResult, mode: str) -> None:
        self.last_result = result
        self._set_busy(False)
        self.outline.set_session(self.session, result)
        self.raster.set_result(result, stale=False)
        self._refresh_state_table()
        self._refresh_info()
        if mode == "commit":
            self._set_status("COMMITTED", "ok")
            self._log("批次提交成功：模型、检查点与覆盖值已原子生效")
            self.commit_info.setText(
                "提交边界\n编辑仅作用于工作副本；本次批次已写回模型并重算依赖段。"
            )
        elif result.complete:
            self._set_status("PIXEL LOCKED", "ok")
            self._log(
                f"处理完成：{result.raster.width}×{result.raster.height}，commit {result.commit_sequence}"
            )
        elif result.paused:
            self._set_status("PAUSED", "busy")
            self._log(
                f"已暂停在提交边界：{result.states['committed_count']}/{result.states['total']}，重启可恢复"
            )
        else:
            self._set_status("FAILED SEGMENT", "error")
            self._log(
                f"处理未完成：failures={result.failures}，其他段结果保留且可重试"
            )

    def _on_worker_failed(self, payload: dict[str, Any]) -> None:
        self._set_busy(False)
        code = payload.get("code", "E_STATE")
        message = payload.get("message", "unknown")
        context = payload.get("context", {})
        self._set_status(code, "error")
        self._log(f"{code}: {message}  {context}", "error")
        if self._worker_mode == "commit":
            self.commit_info.setText(
                "提交失败\n"
                + message
                + "\n模型保持提交前状态，工作副本已回滚。"
            )

    def _on_worker_finished(self) -> None:
        self._set_busy(False)

    def closeEvent(self, event) -> None:  # type: ignore[no-untyped-def]
        if self.worker is not None and self.worker.isRunning():
            self.worker.request_pause()
            if not self.worker.wait(4000):
                self.worker.terminate()
                self.worker.wait(2000)
        super().closeEvent(event)


def launch_gui(
    *,
    model_path: str | Path = "samples/glyph_samples.json",
    persist_root: str | Path = "persist",
    state_model: str | Path = "persist/runtime/glyph_model.json",
    glyph_id: str | None = None,
    ppu: float = 0.25,
    tolerance: float = 0.35,
    grid: int = 8,
    workers: int = 2,
) -> int:
    app = QApplication.instance() or QApplication(sys.argv)
    app.setApplicationName("Glyph Forge")
    app.setStyle("Fusion")
    install_bundled_font()
    params = RenderParams(
        pixels_per_unit=ppu,
        tolerance=tolerance,
        grid=grid,
        max_workers=workers,
    )
    window = MainWindow(
        model_path=model_path,
        persist_root=persist_root,
        state_model=state_model,
        glyph_id=glyph_id,
        params=params,
    )
    window.show()
    return app.exec()


if __name__ == "__main__":
    raise SystemExit(launch_gui())

