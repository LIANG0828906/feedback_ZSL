from __future__ import annotations

import os
from typing import Any

from .params import RenderParams
from .preview import PreviewService, PreviewSnapshot
from .scene import Scene
from .storage import Store
from .strokes import LAYER_DRAW_ORDER


def _paint_snapshot(
    painter: Any,
    snapshot: PreviewSnapshot | None,
    *,
    width: int,
    height: int,
) -> None:
    from PySide6.QtCore import QLineF, Qt
    from PySide6.QtGui import QColor, QPen

    painter.fillRect(0, 0, width, height, QColor("#f2efe8"))
    if snapshot is None:
        painter.setPen(QColor("#5b5b5b"))
        painter.drawText(
            0,
            0,
            width,
            height,
            Qt.AlignmentFlag.AlignCenter,
            "No committed preview",
        )
        return
    grouped: dict[tuple[int, int, int, int, int], list[Any]] = {}
    for layer in LAYER_DRAW_ORDER:
        for batch in snapshot.batches_for_layer(layer):
            for stroke in batch.strokes:
                points = stroke.get("points", [])
                if len(points) < 2:
                    continue
                values = stroke.get("color", [0, 0, 0, 255])
                red = int(values[0])
                green = int(values[1])
                blue = int(values[2])
                alpha = max(0, min(255, int(round(int(values[3]) / 16.0) * 16)))
                width_bucket = max(1, int(round(float(stroke.get("width", 1.0)))))
                key = (red, green, blue, alpha, width_bucket)
                segments = grouped.setdefault(key, [])
                for index in range(1, len(points)):
                    segments.append(
                        QLineF(
                            float(points[index - 1][0]),
                            float(points[index - 1][1]),
                            float(points[index][0]),
                            float(points[index][1]),
                        )
                    )
    for (red, green, blue, alpha, width_bucket), segments in grouped.items():
        pen = QPen(QColor(red, green, blue, alpha))
        pen.setWidthF(float(width_bucket))
        pen.setCapStyle(Qt.PenCapStyle.RoundCap)
        pen.setJoinStyle(Qt.PenJoinStyle.RoundJoin)
        painter.setPen(pen)
        painter.drawLines(segments)


def run_ui(
    *,
    scene: Scene,
    params: RenderParams,
    store: Store,
    refresh_ms: int = 200,
) -> dict[str, Any]:
    os.environ.setdefault("QT_QPA_PLATFORM", "offscreen")
    from PySide6.QtCore import QTimer, Qt
    from PySide6.QtGui import QColor, QPainter
    from PySide6.QtWidgets import QApplication, QLabel, QMainWindow, QWidget

    class PreviewCanvas(QWidget):
        def __init__(self, preview: PreviewService):
            super().__init__()
            self.preview = preview
            self.snapshot: PreviewSnapshot | None = None
            self.last_error: str | None = None
            self.setMinimumSize(640, 420)

        def paintEvent(self, _event: Any) -> None:
            painter = QPainter(self)
            painter.setRenderHint(QPainter.RenderHint.Antialiasing, True)
            snapshot = self.snapshot
            margin = 18.0
            usable_width = max(1.0, self.width() - margin * 2)
            usable_height = max(1.0, self.height() - margin * 2)
            scale = min(usable_width / scene.width, usable_height / scene.height)
            offset_x = (self.width() - scene.width * scale) * 0.5
            offset_y = (self.height() - scene.height * scale) * 0.5
            if snapshot is None:
                _paint_snapshot(
                    painter,
                    None,
                    width=self.width(),
                    height=self.height(),
                )
                return
            painter.fillRect(self.rect(), QColor("#f2efe8"))
            painter.translate(offset_x, offset_y)
            painter.scale(scale, scale)
            _paint_snapshot(
                painter,
                snapshot,
                width=int(scene.width),
                height=int(scene.height),
            )

    class PreviewWindow(QMainWindow):
        def __init__(self) -> None:
            super().__init__()
            self.setWindowTitle("NPR Stroke Preview")
            self.preview = PreviewService(store)
            self.canvas = PreviewCanvas(self.preview)
            self.status = QLabel("warming up")
            self.status.setStyleSheet("padding: 4px; color: #333;")
            self.setCentralWidget(self.canvas)
            self.statusBar().addWidget(self.status)
            self.timer = QTimer(self)
            self.timer.setInterval(max(16, refresh_ms))
            self.timer.timeout.connect(self.refresh)
            self.timer.start()
            self.resize(int(scene.width), int(scene.height) + 40)
            self.refresh()

        def refresh(self) -> None:
            try:
                snapshot = self.preview.refresh()
                self.canvas.snapshot = snapshot
                self.canvas.update()
                if snapshot is None:
                    self.status.setText("no committed version")
                else:
                    self.status.setText(
                        f"committed v{snapshot.version}  {snapshot.content_hash[:16]}"
                    )
            except Exception as exc:  # Keep the last complete frame visible.
                self.canvas.last_error = str(exc)
                self.status.setText(f"preview read failed: {exc}")

    app = QApplication.instance() or QApplication([])
    window = PreviewWindow()
    window.show()
    app.exec()
    return {"exit": 0, "version": window.canvas.snapshot.version if window.canvas.snapshot else None}


def render_preview_image(
    preview: PreviewService,
    scene: Scene,
    width: int,
    height: int,
    output_path: str,
) -> PreviewSnapshot | None:
    """Offscreen helper used by tests and documentation checks."""
    os.environ.setdefault("QT_QPA_PLATFORM", "offscreen")
    from PySide6.QtGui import QColor, QImage, QPainter

    snapshot = preview.refresh()
    if snapshot is None:
        return None
    image = QImage(width, height, QImage.Format.Format_ARGB32)
    image.fill(QColor("#f2efe8"))
    painter = QPainter(image)
    painter.setRenderHint(QPainter.RenderHint.Antialiasing, True)
    scale = min(width / scene.width, height / scene.height)
    painter.scale(scale, scale)
    _paint_snapshot(
        painter,
        snapshot,
        width=int(scene.width),
        height=int(scene.height),
    )
    painter.end()
    if not image.save(output_path):
        raise OSError(f"failed to write preview image: {output_path}")
    return snapshot


__all__ = ["render_preview_image", "run_ui"]
