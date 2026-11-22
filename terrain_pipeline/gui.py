from __future__ import annotations

import math
from pathlib import Path
from typing import Any, ClassVar

import numpy as np

from .bake import CancellationToken
from .errors import E_STATE, PipelineError
from .model import BakeParams
from .pipeline import Pipeline


def run_gui(
    project_dir: str | Path,
    cache_dir: str | Path,
    *,
    params: BakeParams | None = None,
    edits_file: str | Path | None = None,
    smoke_test: bool = False,
    screenshot: str | Path | None = None,
) -> None:
    try:
        from PyQt5 import QtCore, QtGui, QtWidgets
    except ImportError as exc:
        raise PipelineError(
            E_STATE,
            "PyQt5 is not installed; run: pip install -r requirements-ui.txt",
        ) from exc

    class BakeWorker(QtCore.QThread):
        completed = QtCore.pyqtSignal(object)
        failed = QtCore.pyqtSignal(object)

        def __init__(self, pipeline: Pipeline, mode: str, token: CancellationToken) -> None:
            super().__init__()
            self.pipeline = pipeline
            self.mode = mode
            self.token = token

        def run(self) -> None:
            try:
                result = self.pipeline.run(mode=self.mode, cancel_token=self.token)
                self.completed.emit(result)
            except Exception as exc:
                self.failed.emit(exc)

    class TerrainGLView(QtWidgets.QOpenGLWidget):
        def __init__(self, pipeline: Pipeline) -> None:
            super().__init__()
            self.pipeline = pipeline
            self.setMinimumSize(620, 460)
            self._program: Any = None
            self._vaos: list[Any] = []
            self._buffers: list[Any] = []
            self._center = (0.0, 0.0, 0.0)
            self._radius = 32.0
            self._error_label = QtWidgets.QLabel(
                "ModernGL 3D view is unavailable; the 2D diagnostics view remains usable.",
                self,
            )
            self._error_label.setAlignment(QtCore.Qt.AlignCenter)
            self._error_label.setStyleSheet("color:#d6dde2; background:#1a2026; padding:24px;")
            self._error_label.hide()
            layout = QtWidgets.QVBoxLayout(self)
            layout.setContentsMargins(0, 0, 0, 0)
            layout.addWidget(self._error_label)

        def initializeGL(self) -> None:
            try:
                import moderngl

                self.ctx = moderngl.create_context(require=330)
                self._program = self.ctx.program(
                    vertex_shader="""
                        #version 330
                        in vec3 in_pos;
                        in vec3 in_normal;
                        uniform mat4 mvp;
                        out vec3 v_normal;
                        void main() {
                            gl_Position = mvp * vec4(in_pos, 1.0);
                            v_normal = in_normal;
                        }
                    """,
                    fragment_shader="""
                        #version 330
                        in vec3 v_normal;
                        uniform vec3 tint;
                        out vec4 frag_color;
                        void main() {
                            vec3 n = normalize(v_normal);
                            float light = 0.38 + 0.62 * abs(n.z);
                            frag_color = vec4(tint * light, 1.0);
                        }
                    """,
                )
                self._error_label.hide()
                self._rebuild_buffers()
            except Exception as exc:
                self._show_error(f"{type(exc).__name__}: {exc}")

        def _show_error(self, message: str) -> None:
            self._error_label.setText(
                "ModernGL 3D view is unavailable; the 2D diagnostics view remains usable.\n"
                + message
            )
            self._error_label.show()

        def _release_buffers(self) -> None:
            for vao in self._vaos:
                vao.release()
            for buffer in self._buffers:
                buffer.release()
            self._vaos = []
            self._buffers = []

        def _rebuild_buffers(self) -> None:
            if self._program is None:
                return
            self._release_buffers()
            snapshot = self.pipeline.snapshot
            if not isinstance(snapshot, dict):
                return
            cache_dir = self.pipeline.cache_dir
            bounds_all: list[list[float]] = []
            palette = [
                (0.30, 0.58, 0.37),
                (0.59, 0.60, 0.63),
                (0.60, 0.43, 0.27),
                (0.82, 0.86, 0.89),
            ]
            for entry in snapshot.get("tiles", []):
                block_dir = cache_dir / entry["block_dir"]
                try:
                    positions = np.frombuffer(
                        (block_dir / "positions.bin").read_bytes(), dtype="<f4"
                    ).reshape(-1, 3)
                    normals = np.frombuffer(
                        (block_dir / "normals.bin").read_bytes(), dtype="<f4"
                    ).reshape(-1, 3)
                    indices = np.frombuffer((block_dir / "indices.bin").read_bytes(), dtype="<u4")
                except OSError:
                    continue
                interleaved = np.ascontiguousarray(
                    np.concatenate([positions, normals], axis=1),
                    dtype="<f4",
                )
                vertex_buffer = self.ctx.buffer(interleaved.tobytes())
                index_buffer = self.ctx.buffer(indices.tobytes())
                vao = self.ctx.vertex_array(
                    self._program,
                    [(vertex_buffer, "3f 3f", "in_pos", "in_normal")],
                    index_buffer,
                )
                tile = self.pipeline.project.tiles.get(str(entry.get("tile_id")))
                grid_x = tile.grid_x if tile is not None else 0
                grid_y = tile.grid_y if tile is not None else 0
                tint = palette[(int(grid_x) + 2 * int(grid_y)) % len(palette)]
                self._vaos.append((vao, tint))
                self._buffers.extend([vertex_buffer, index_buffer])
                bounds_all.append(entry["bounds"])
            if bounds_all:
                minimum = np.min(np.asarray(bounds_all), axis=0)
                maximum = np.max(np.asarray(bounds_all), axis=0)
                self._center = (
                    float((minimum[0] + maximum[3]) * 0.5),
                    float((minimum[1] + maximum[4]) * 0.5),
                    float((minimum[2] + maximum[5]) * 0.5),
                )
                self._radius = max(
                    8.0,
                    float(
                        max(
                            maximum[3] - minimum[0],
                            maximum[4] - minimum[1],
                            maximum[5] - minimum[2],
                        )
                    ),
                )

        def reload(self) -> None:
            if self._program is None:
                return
            self.makeCurrent()
            try:
                self._rebuild_buffers()
            finally:
                self.doneCurrent()
            self.update()

        def resizeGL(self, width: int, height: int) -> None:
            if hasattr(self, "ctx"):
                self.ctx.viewport = (0, 0, max(1, width), max(1, height))

        def paintGL(self) -> None:
            if not hasattr(self, "ctx") or self._program is None:
                return
            self.ctx.clear(0.055, 0.07, 0.085, 1.0)
            self.ctx.enable(self.ctx.DEPTH_TEST)
            width = max(1, self.width())
            height = max(1, self.height())
            aspect = width / height
            center = QtGui.QVector3D(*self._center)
            eye = QtGui.QVector3D(
                self._center[0] + self._radius * 1.15,
                self._center[1] - self._radius * 1.5,
                self._center[2] + self._radius * 1.1,
            )
            projection = QtGui.QMatrix4x4()
            projection.perspective(
                42.0, aspect, max(0.1, self._radius * 0.001), self._radius * 12.0
            )
            view = QtGui.QMatrix4x4()
            view.lookAt(eye, center, QtGui.QVector3D(0.0, 0.0, 1.0))
            mvp = projection * view
            mvp_bytes = np.asarray(mvp.data(), dtype="<f4").tobytes()
            self._program["mvp"].write(mvp_bytes)
            for vao, tint in self._vaos:
                self._program["tint"].value = tint
                vao.render()

    class TerrainPreview(QtWidgets.QWidget):
        tile_clicked = QtCore.pyqtSignal(str)
        STATUS_COLORS: ClassVar[dict[str, Any]] = {
            "cache-hit": QtGui.QColor("#2e9f68"),
            "reused": QtGui.QColor("#2e9f68"),
            "recovered": QtGui.QColor("#2b84c6"),
            "rebuilt": QtGui.QColor("#3478d4"),
            "invalidated": QtGui.QColor("#e39b24"),
            "failed": QtGui.QColor("#d34848"),
            "unchanged": QtGui.QColor("#6f7b86"),
        }
        MATERIAL_COLORS: ClassVar[dict[str, tuple[int, int, int]]] = {
            "grass": (86, 146, 74),
            "rock": (126, 128, 132),
            "dirt": (142, 105, 68),
            "snow": (225, 231, 235),
        }

        def __init__(self, project: Any) -> None:
            super().__init__()
            self.project = project
            self.status: dict[str, str] = {}
            self.report: dict[str, Any] | None = None
            self.setMinimumSize(620, 460)
            self.setMouseTracking(True)
            self._tile_rects: dict[str, QtCore.QRectF] = {}
            self._image_cache: dict[tuple[str, int, int, str], Any] = {}

        def set_status(self, status: dict[str, str]) -> None:
            self.status = dict(status)
            self.update()

        def set_report(self, report: Any) -> None:
            self.report = report.to_dict() if hasattr(report, "to_dict") else report
            self.update()

        def paintEvent(self, event: Any) -> None:
            painter = QtGui.QPainter(self)
            painter.setRenderHint(QtGui.QPainter.Antialiasing, True)
            painter.fillRect(self.rect(), QtGui.QColor("#171b20"))
            margin = 28
            gap = 8
            gx_count, gy_count = self.project.grid_tiles
            available_w = max(1, self.width() - margin * 2)
            available_h = max(1, self.height() - margin * 2 - 28)
            cell_w = (available_w - gap * (gx_count - 1)) / gx_count
            cell_h = (available_h - gap * (gy_count - 1)) / gy_count
            self._tile_rects = {}
            for tile in self.project.tiles.values():
                rect = QtCore.QRectF(
                    margin + tile.grid_x * (cell_w + gap),
                    margin + tile.grid_y * (cell_h + gap),
                    cell_w,
                    cell_h,
                )
                self._tile_rects[tile.tile_id] = rect
                image = self._tile_image(tile, max(16, int(cell_w)), max(16, int(cell_h)))
                painter.drawImage(rect, image)
                state = self.status.get(tile.tile_id, "unchanged")
                color = self.STATUS_COLORS.get(state, self.STATUS_COLORS["unchanged"])
                pen = QtGui.QPen(
                    color, 3.0 if state in {"invalidated", "failed", "rebuilt"} else 1.5
                )
                painter.setPen(pen)
                painter.drawRect(rect)
                label_rect = QtCore.QRectF(
                    rect.left() + 4,
                    rect.top() + 4,
                    min(rect.width() - 8, 128),
                    21,
                )
                painter.fillRect(label_rect, QtGui.QColor(10, 14, 18, 165))
                painter.setPen(QtGui.QPen(QtGui.QColor("#f0f3f5")))
                font = QtGui.QFont("Segoe UI", 9, QtGui.QFont.Bold)
                painter.setFont(font)
                painter.drawText(
                    label_rect.adjusted(5, 1, -3, -1),
                    QtCore.Qt.AlignLeft | QtCore.Qt.AlignVCenter,
                    f"{tile.tile_id}  [{tile.grid_x},{tile.grid_y}]",
                )
            self._draw_edges(painter)
            self._draw_legend(painter)
            painter.end()

        def _tile_image(self, tile: Any, width: int, height: int) -> Any:
            cache_key = (tile.tile_id, width, height, tile.content_hash)
            cached = self._image_cache.get(cache_key)
            if cached is not None:
                return cached
            source = tile.height
            weights = tile.weights
            h, w = source.shape
            min_h = float(source.min())
            max_h = float(source.max())
            span = max(1e-9, max_h - min_h)
            material_names = self.project.material_names
            sy = np.minimum((np.arange(height) * h // max(1, height)), h - 1)
            sx = np.minimum((np.arange(width) * w // max(1, width)), w - 1)
            sampled_height = source[sy[:, None], sx[None, :]]
            sampled_weights = weights[sy[:, None], sx[None, :], :]
            layers = np.argmax(sampled_weights, axis=2)
            palette = np.asarray(
                [self.MATERIAL_COLORS.get(name, (120, 125, 130)) for name in material_names],
                dtype=np.float64,
            )
            shade = 0.55 + 0.55 * ((sampled_height - min_h) / span)
            rgb = np.clip(palette[layers] * shade[:, :, None], 0, 255).astype(np.uint8)
            rgb = np.ascontiguousarray(rgb)
            image = QtGui.QImage(
                rgb.data,
                width,
                height,
                3 * width,
                QtGui.QImage.Format_RGB888,
            ).copy()
            self._image_cache[cache_key] = image
            if len(self._image_cache) > 64:
                self._image_cache.clear()
                self._image_cache[cache_key] = image
            return image

        def _draw_edges(self, painter: Any) -> None:
            if not self.report:
                return
            affected = set(self.report.get("affected_tiles", []))
            for edge in self.report.get("graph_edges", []):
                source = edge.get("from")
                target = edge.get("to")
                if source not in self._tile_rects or target not in self._tile_rects:
                    continue
                a = self._tile_rects[source].center()
                b = self._tile_rects[target].center()
                hot = source in affected and target in affected
                color = QtGui.QColor(242, 166, 60, 210) if hot else QtGui.QColor(170, 185, 195, 80)
                painter.setPen(QtGui.QPen(color, 2.3 if hot else 1.0, QtCore.Qt.DashLine))
                painter.drawLine(a, b)
                dx = b.x() - a.x()
                dy = b.y() - a.y()
                length = max(1.0, math.hypot(dx, dy))
                ux, uy = dx / length, dy / length
                tip = QtCore.QPointF(b.x() - ux * 18, b.y() - uy * 18)
                p1 = QtCore.QPointF(tip.x() - ux * 8 - uy * 4, tip.y() - uy * 8 + ux * 4)
                p2 = QtCore.QPointF(tip.x() - ux * 8 + uy * 4, tip.y() - uy * 8 - ux * 4)
                arrow = QtGui.QPolygonF([tip, p1, p2])
                painter.setBrush(color)
                painter.drawPolygon(arrow)

        def _draw_legend(self, painter: Any) -> None:
            items = [
                ("cache-hit", "Cache hit"),
                ("rebuilt", "Rebuilt"),
                ("recovered", "Recovered"),
                ("invalidated", "Invalidated"),
                ("failed", "Failed"),
            ]
            x = 28
            y = self.height() - 22
            painter.setFont(QtGui.QFont("Microsoft YaHei UI", 8))
            for key, label in items:
                color = self.STATUS_COLORS[key]
                painter.fillRect(QtCore.QRectF(x, y, 11, 11), color)
                painter.setPen(QtGui.QColor("#c9d0d6"))
                painter.drawText(QtCore.QRectF(x + 16, y - 2, 72, 16), QtCore.Qt.AlignLeft, label)
                x += 94

        def mousePressEvent(self, event: Any) -> None:
            for tile_id, rect in self._tile_rects.items():
                if rect.contains(event.pos()):
                    self.tile_clicked.emit(tile_id)
                    break
            super().mousePressEvent(event)

    class MainWindow(QtWidgets.QMainWindow):
        def __init__(self) -> None:
            super().__init__()
            self.setWindowTitle("OpenWorld Terrain Bake Diagnostics")
            self.resize(1320, 780)
            self.pipeline = Pipeline(
                project_dir,
                cache_dir,
                params=params,
                edits_file=edits_file,
            )
            self.worker: Any = None
            self.token: CancellationToken | None = None
            self.last_report: Any = None
            self.last_result: Any = None
            self.statuses = self._initial_statuses()

            central = QtWidgets.QWidget()
            self.setCentralWidget(central)
            root = QtWidgets.QHBoxLayout(central)
            root.setContentsMargins(12, 12, 12, 12)
            root.setSpacing(12)

            self.preview = TerrainPreview(self.pipeline.project)
            self.preview.tile_clicked.connect(self._select_tile)
            self.gl_view = TerrainGLView(self.pipeline)
            preview_tabs = QtWidgets.QTabWidget()
            preview_tabs.addTab(self.preview, "2D invalidation and tile diagnostics")
            preview_tabs.addTab(self.gl_view, "ModernGL 3D terrain")
            root.addWidget(preview_tabs, 3)

            side = QtWidgets.QVBoxLayout()
            root.addLayout(side, 2)
            controls = QtWidgets.QGroupBox("Edit and bake")
            form = QtWidgets.QGridLayout(controls)
            self.tile_combo = QtWidgets.QComboBox()
            for tile_id in self.pipeline.project.tiles:
                self.tile_combo.addItem(tile_id)
            self.x_spin = QtWidgets.QSpinBox()
            self.x_spin.setRange(0, self.pipeline.project.tile_size)
            self.y_spin = QtWidgets.QSpinBox()
            self.y_spin.setRange(0, self.pipeline.project.tile_size)
            self.height_spin = QtWidgets.QDoubleSpinBox()
            self.height_spin.setDecimals(5)
            self.height_spin.setRange(-100000.0, 100000.0)
            self.material_combo = QtWidgets.QComboBox()
            self.material_combo.addItems(list(self.pipeline.project.material_names))
            self.material_spin = QtWidgets.QDoubleSpinBox()
            self.material_spin.setDecimals(3)
            self.material_spin.setRange(0.05, 8.0)
            self.material_spin.setSingleStep(0.05)
            self.apply_height_button = QtWidgets.QPushButton("Apply height + incremental bake")
            self.apply_material_button = QtWidgets.QPushButton("Apply material + incremental bake")
            self.bake_button = QtWidgets.QPushButton("Incremental bake")
            self.full_button = QtWidgets.QPushButton("Full rebuild")
            self.cancel_button = QtWidgets.QPushButton("Cancel")
            self.cancel_button.setEnabled(False)

            form.addWidget(QtWidgets.QLabel("Tile"), 0, 0)
            form.addWidget(self.tile_combo, 0, 1, 1, 3)
            form.addWidget(QtWidgets.QLabel("X / Y / height"), 1, 0)
            form.addWidget(self.x_spin, 1, 1)
            form.addWidget(self.y_spin, 1, 2)
            form.addWidget(self.height_spin, 1, 3)
            form.addWidget(self.apply_height_button, 2, 0, 1, 4)
            form.addWidget(QtWidgets.QLabel("Material / param"), 3, 0)
            form.addWidget(self.material_combo, 3, 1, 1, 2)
            form.addWidget(self.material_spin, 3, 3)
            form.addWidget(self.apply_material_button, 4, 0, 1, 4)
            form.addWidget(self.bake_button, 5, 0, 1, 2)
            form.addWidget(self.full_button, 5, 2, 1, 2)
            form.addWidget(self.cancel_button, 6, 0, 1, 4)
            side.addWidget(controls)

            self.summary = QtWidgets.QLabel("Ready")
            self.summary.setWordWrap(True)
            self.summary.setStyleSheet(
                "padding:8px; background:#20262d; color:#dfe6eb; border:1px solid #38424c;"
            )
            side.addWidget(self.summary)

            self.table = QtWidgets.QTableWidget(0, 6)
            self.table.setHorizontalHeaderLabels(
                ["Tile", "Status", "Vertices", "Instances", "Time ms", "Bounds"]
            )
            self.table.horizontalHeader().setStretchLastSection(True)
            self.table.verticalHeader().setVisible(False)
            self.table.setSelectionBehavior(QtWidgets.QAbstractItemView.SelectRows)
            self.table.setEditTriggers(QtWidgets.QAbstractItemView.NoEditTriggers)
            side.addWidget(self.table, 2)

            self.log = QtWidgets.QPlainTextEdit()
            self.log.setReadOnly(True)
            self.log.setMaximumBlockCount(600)
            side.addWidget(self.log, 1)

            self.tile_combo.currentTextChanged.connect(self._tile_changed)
            self.material_combo.currentTextChanged.connect(self._material_changed)
            self.x_spin.valueChanged.connect(self._coordinate_changed)
            self.y_spin.valueChanged.connect(self._coordinate_changed)
            self.apply_height_button.clicked.connect(self._apply_height)
            self.apply_material_button.clicked.connect(self._apply_material)
            self.bake_button.clicked.connect(self._start_incremental)
            self.full_button.clicked.connect(self._start_full)
            self.cancel_button.clicked.connect(self._cancel)

            self._tile_changed(self.tile_combo.currentText())
            self._refresh_table()
            self.preview.set_status(self.statuses)
            self._append("Project loaded; cache-hit state comes from the persisted snapshot.")
            QtCore.QTimer.singleShot(150, self._start_initial)

        def _initial_statuses(self) -> dict[str, str]:
            if self.pipeline.snapshot is None:
                return {tile_id: "invalidated" for tile_id in self.pipeline.project.tiles}
            return {tile_id: "cache-hit" for tile_id in self.pipeline.project.tiles}

        def _start_initial(self) -> None:
            if self.pipeline.snapshot is None:
                self._append("No complete snapshot found; starting auto build or recovery.")
                self._start_bake("auto")

        def _tile_changed(self, tile_id: str) -> None:
            if not tile_id or tile_id not in self.pipeline.project.tiles:
                return
            tile = self.pipeline.project.tiles[tile_id]
            x = min(self.x_spin.value(), tile.tile_size)
            y = min(self.y_spin.value(), tile.tile_size)
            self.height_spin.setValue(float(tile.height[y, x]))
            self.material_spin.setValue(
                float(tile.material_params[self.material_combo.currentText()])
            )

        def _coordinate_changed(self) -> None:
            tile_id = self.tile_combo.currentText()
            if tile_id in self.pipeline.project.tiles:
                tile = self.pipeline.project.tiles[tile_id]
                self.height_spin.setValue(
                    float(tile.height[self.y_spin.value(), self.x_spin.value()])
                )

        def _material_changed(self, layer: str) -> None:
            tile_id = self.tile_combo.currentText()
            if tile_id in self.pipeline.project.tiles and layer:
                tile = self.pipeline.project.tiles[tile_id]
                self.material_spin.setValue(float(tile.material_params[layer]))

        def _select_tile(self, tile_id: str) -> None:
            index = self.tile_combo.findText(tile_id)
            if index >= 0:
                self.tile_combo.setCurrentIndex(index)

        def _apply_height(self) -> None:
            if self.worker is not None:
                return
            tile_id = self.tile_combo.currentText()
            try:
                report = self.pipeline.apply_height_edit(
                    tile_id,
                    int(self.x_spin.value()),
                    int(self.y_spin.value()),
                    float(self.height_spin.value()),
                )
            except PipelineError as exc:
                self._show_error(exc)
                return
            self._accept_report(report)
            self._start_bake("incremental")

        def _apply_material(self) -> None:
            if self.worker is not None:
                return
            tile_id = self.tile_combo.currentText()
            try:
                report = self.pipeline.apply_material_edit(
                    tile_id,
                    self.material_combo.currentText(),
                    float(self.material_spin.value()),
                )
            except PipelineError as exc:
                self._show_error(exc)
                return
            self._accept_report(report)
            self._start_bake("incremental")

        def _accept_report(self, report: Any) -> None:
            self.last_report = report
            self.preview.set_report(report)
            statuses = {tile_id: "cache-hit" for tile_id in self.pipeline.project.tiles}
            for tile_id in report.affected:
                statuses[tile_id] = "invalidated"
            self.statuses = statuses
            self.preview.set_status(statuses)
            self._refresh_table()
            self.summary.setText(
                f"Invalidated {len(report.affected)}: {', '.join(report.affected) or '-'}; "
                f"reusable {len(report.reused)}: {', '.join(report.reused) or '-'}"
            )
            self._append(
                f"Invalidation: {len(report.affected)} affected, {len(report.reused)} reusable."
            )

        def _start_incremental(self) -> None:
            self._start_bake("incremental")

        def _start_full(self) -> None:
            answer = QtWidgets.QMessageBox.question(
                self,
                "Full rebuild",
                "Full rebuild discards the current snapshot and block cache. Continue?",
                QtWidgets.QMessageBox.Yes | QtWidgets.QMessageBox.No,
                QtWidgets.QMessageBox.No,
            )
            if answer == QtWidgets.QMessageBox.Yes:
                self._start_bake("full")

        def _start_bake(self, mode: str) -> None:
            if self.worker is not None:
                return
            self.token = CancellationToken()
            self.worker = BakeWorker(self.pipeline, mode, self.token)
            self.worker.completed.connect(self._bake_finished)
            self.worker.failed.connect(self._bake_exception)
            self.worker.finished.connect(self._worker_done)
            self._set_busy(True)
            self._append(f"Starting {mode} bake (worker cap {self.pipeline.params.max_workers}).")
            self.worker.start()

        def _bake_finished(self, result: Any) -> None:
            self.last_result = result
            self.last_report = result.invalidations
            self.preview.set_report(result.invalidations)
            self.gl_view.reload()
            statuses = {tile_id: "cache-hit" for tile_id in self.pipeline.project.tiles}
            if not result.success:
                for tile_id in result.invalidations.affected:
                    statuses[tile_id] = "invalidated"
                for tile_id in result.failed:
                    if tile_id in statuses:
                        statuses[tile_id] = "failed"
            else:
                for tile_id in result.reused:
                    statuses[tile_id] = "cache-hit"
                for tile_id in result.recovered:
                    statuses[tile_id] = "recovered"
                for tile_id in result.rebuilt:
                    statuses[tile_id] = "rebuilt"
            self.statuses = statuses
            self.preview.set_status(statuses)
            self._refresh_table()
            if result.success:
                self.summary.setText(
                    f"Complete: rebuilt {len(result.rebuilt)}, reused {len(result.reused)}, "
                    f"recovered {len(result.recovered)}, total {result.total_ms:.1f} ms."
                )
                self._append(self.summary.text())
            else:
                error = result.error or {}
                self.summary.setText(
                    f"Failed {error.get('code', 'E_BAKE')}: {error.get('message', '')}; "
                    "committed blocks remain in the pending ledger for retry."
                )
                self._append(self.summary.text())

        def _bake_exception(self, exc: Exception) -> None:
            if isinstance(exc, PipelineError):
                self._show_error(exc)
            else:
                self._append(f"Unhandled exception: {type(exc).__name__}: {exc}")

        def _worker_done(self) -> None:
            self.worker = None
            self.token = None
            self._set_busy(False)

        def _cancel(self) -> None:
            if self.token is not None:
                self.token.cancel()
                self._append("Cancellation requested; only atomically committed blocks remain.")

        def _set_busy(self, busy: bool) -> None:
            for widget in (
                self.apply_height_button,
                self.apply_material_button,
                self.bake_button,
                self.full_button,
            ):
                widget.setEnabled(not busy)
            self.cancel_button.setEnabled(busy)

        def _refresh_table(self) -> None:
            entries = {}
            if self.pipeline.snapshot is not None:
                entries = {
                    entry["tile_id"]: entry
                    for entry in self.pipeline.snapshot.get("tiles", [])
                    if isinstance(entry, dict) and "tile_id" in entry
                }
            timings = {}
            if self.last_result is not None:
                timings = self.last_result.timings_ms
            self.table.setRowCount(len(self.pipeline.project.tiles))
            for row, tile_id in enumerate(self.pipeline.project.tiles):
                state = self.statuses.get(tile_id, "unchanged")
                entry = entries.get(tile_id, {})
                values = [
                    tile_id,
                    state,
                    str(entry.get("vertex_count", "-")),
                    str(entry.get("instance_count", "-")),
                    f"{timings[tile_id]:.2f}" if tile_id in timings else "-",
                    _format_bounds(entry.get("bounds")),
                ]
                for column, value in enumerate(values):
                    item = QtWidgets.QTableWidgetItem(value)
                    if column == 1:
                        item.setForeground(
                            TerrainPreview.STATUS_COLORS.get(
                                state,
                                TerrainPreview.STATUS_COLORS["unchanged"],
                            )
                        )
                    self.table.setItem(row, column, item)
            self.table.resizeColumnsToContents()

        def _show_error(self, exc: PipelineError) -> None:
            self.summary.setText(f"{exc.code}: {exc.message}")
            self._append(str(exc))

        def _append(self, message: str) -> None:
            self.log.appendPlainText(message)
            self.log.verticalScrollBar().setValue(self.log.verticalScrollBar().maximum())

        def closeEvent(self, event: Any) -> None:
            if self.token is not None:
                self.token.cancel()
            if self.worker is not None:
                self.worker.wait(8000)
            event.accept()

    app = QtWidgets.QApplication.instance() or QtWidgets.QApplication([])
    app.setStyle("Fusion")
    palette = QtGui.QPalette()
    palette.setColor(QtGui.QPalette.Window, QtGui.QColor("#11161b"))
    palette.setColor(QtGui.QPalette.WindowText, QtGui.QColor("#dce3e8"))
    palette.setColor(QtGui.QPalette.Base, QtGui.QColor("#1c2228"))
    palette.setColor(QtGui.QPalette.AlternateBase, QtGui.QColor("#20262d"))
    palette.setColor(QtGui.QPalette.Text, QtGui.QColor("#dce3e8"))
    palette.setColor(QtGui.QPalette.Button, QtGui.QColor("#2b343d"))
    palette.setColor(QtGui.QPalette.ButtonText, QtGui.QColor("#eef3f6"))
    palette.setColor(QtGui.QPalette.Highlight, QtGui.QColor("#3478d4"))
    app.setPalette(palette)
    window = MainWindow()
    window.show()
    if smoke_test:

        def finish_smoke() -> None:
            if screenshot is not None:
                target = Path(screenshot).resolve()
                target.parent.mkdir(parents=True, exist_ok=True)
                window.grab().save(str(target))
            app.quit()

        QtCore.QTimer.singleShot(900, finish_smoke)
    app.exec_()


def _format_bounds(bounds: Any) -> str:
    if not isinstance(bounds, (list, tuple)) or len(bounds) != 6:
        return "-"
    return (
        f"({bounds[0]:.2f},{bounds[1]:.2f},{bounds[2]:.2f})"
        f"..({bounds[3]:.2f},{bounds[4]:.2f},{bounds[5]:.2f})"
    )
