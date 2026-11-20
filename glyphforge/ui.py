from __future__ import annotations

import sys
import threading
from dataclasses import replace
from pathlib import Path
from typing import Any

import numpy as np
from PySide6.QtCore import QRectF, Qt, QThread, Signal
from PySide6.QtGui import QColor, QFont, QImage, QPainter, QPen
from PySide6.QtWidgets import (
    QApplication,
    QComboBox,
    QDoubleSpinBox,
    QFileDialog,
    QFormLayout,
    QHBoxLayout,
    QLabel,
    QLineEdit,
    QMainWindow,
    QMessageBox,
    QPlainTextEdit,
    QPushButton,
    QSpinBox,
    QSplitter,
    QTabWidget,
    QVBoxLayout,
    QWidget,
)

from .config import WorkbenchConfig
from .errors import GlyphForgeError, RunCancelled
from .parser import load_font
from .pipeline import GlyphRunResult, PipelineResult, run_pipeline


def _qimage_from_rgba(rgba: np.ndarray) -> QImage:
    contiguous = np.ascontiguousarray(rgba, dtype=np.uint8)
    height, width, _ = contiguous.shape
    return QImage(
        contiguous.data,
        width,
        height,
        width * 4,
        QImage.Format.Format_RGBA8888,
    ).copy()


def _qimage_from_glyph(glyph: GlyphRunResult) -> QImage | None:
    if glyph.coverage is None:
        return None
    height, width = glyph.coverage.shape
    if width == 0 or height == 0:
        return None
    rgba = np.zeros((height, width, 4), dtype=np.uint8)
    rgba[:, :, 0:3] = 35
    rgba[:, :, 3] = glyph.coverage
    return _qimage_from_rgba(rgba)


class AtlasView(QWidget):
    def __init__(self) -> None:
        super().__init__()
        self.result: PipelineResult | None = None
        self.setMinimumSize(420, 280)

    def set_result(self, result: PipelineResult | None) -> None:
        self.result = result
        self.update()

    def paintEvent(self, event) -> None:
        painter = QPainter(self)
        painter.fillRect(self.rect(), QColor("#171b22"))
        painter.setRenderHint(QPainter.RenderHint.SmoothPixmapTransform, False)
        if self.result is None:
            painter.setPen(QColor("#9aa4b2"))
            painter.drawText(
                self.rect(),
                Qt.AlignmentFlag.AlignCenter,
                "尚无已发布图集",
            )
            return
        image = _qimage_from_rgba(self.result.atlas.rgba)
        available = self.rect().adjusted(16, 16, -16, -16)
        scaled = image.size().scaled(
            available.size(), Qt.AspectRatioMode.KeepAspectRatio
        )
        target = QRectF(
            available.center().x() - scaled.width() / 2,
            available.center().y() - scaled.height() / 2,
            scaled.width(),
            scaled.height(),
        )
        painter.setPen(QPen(QColor("#354052"), 1))
        painter.setBrush(QColor("#0b0e13"))
        painter.drawRect(target)
        painter.drawImage(target, image)
        painter.setPen(QColor("#cbd5e1"))
        painter.drawText(
            14,
            self.height() - 10,
            f"{self.result.atlas.width}×{self.result.atlas.height}  "
            f"{len(self.result.atlas.metrics)} 字形  "
            f"alpha {self.result.atlas.alpha_hash[:12]}…",
        )


class GlyphView(QWidget):
    def __init__(self) -> None:
        super().__init__()
        self.result: PipelineResult | None = None
        self.glyph: GlyphRunResult | None = None
        self.tile_states: dict[tuple[str, int, int], str] = {}
        self.setMinimumSize(420, 280)

    def set_data(
        self,
        result: PipelineResult | None,
        glyph: GlyphRunResult | None,
        tile_states: dict[tuple[str, int, int], str] | None = None,
    ) -> None:
        self.result = result
        self.glyph = glyph
        if tile_states is not None:
            self.tile_states = dict(tile_states)
        self.update()

    def paintEvent(self, event) -> None:
        painter = QPainter(self)
        painter.fillRect(self.rect(), QColor("#171b22"))
        if self.glyph is None or self.result is None:
            painter.setPen(QColor("#9aa4b2"))
            painter.drawText(
                self.rect(),
                Qt.AlignmentFlag.AlignCenter,
                "运行后选择字形查看分块网格",
            )
            return

        glyph = self.glyph
        x0, y0, x1, y1 = glyph.pixel_bbox
        width = max(1, x1 - x0)
        height = max(1, y1 - y0)
        available = self.rect().adjusted(28, 28, -28, -42)
        scale = min(available.width() / width, available.height() / height)
        draw_width = width * scale
        draw_height = height * scale
        origin_x = available.center().x() - draw_width / 2
        origin_y = available.center().y() - draw_height / 2
        target = QRectF(origin_x, origin_y, draw_width, draw_height)

        painter.setPen(QPen(QColor("#3a4556"), 1))
        painter.setBrush(QColor("#0b0e13"))
        painter.drawRect(target)
        image = _qimage_from_glyph(glyph)
        if image is not None:
            painter.drawImage(target, image)

        tile_size = self.result.config.tile_size
        rows = (height + tile_size - 1) // tile_size
        cols = (width + tile_size - 1) // tile_size
        colors = {
            "computed": QColor(62, 196, 126, 95),
            "reused": QColor(55, 157, 210, 95),
            "failed": QColor(238, 88, 88, 120),
            "rejected": QColor(238, 88, 88, 120),
            "rejected_glyph_isolated": QColor(238, 88, 88, 120),
            "cancelled": QColor(230, 170, 50, 120),
            "missing": QColor(140, 140, 140, 70),
        }
        for row in range(rows):
            for col in range(cols):
                tile_rect = QRectF(
                    origin_x + col * tile_size * scale,
                    origin_y + row * tile_size * scale,
                    min(tile_size, width - col * tile_size) * scale,
                    min(tile_size, height - row * tile_size) * scale,
                )
                state = self.tile_states.get((glyph.name, row, col), "pending")
                color = colors.get(state)
                if color is not None:
                    painter.fillRect(tile_rect, color)
                pen = QPen(
                    QColor("#637083") if state == "pending" else QColor("#d7e0ea"),
                    0.7,
                )
                painter.setPen(pen)
                painter.setBrush(Qt.BrushStyle.NoBrush)
                painter.drawRect(tile_rect)

        status_color = "#68d391" if glyph.status == "ok" else "#f87171"
        painter.setPen(QColor(status_color))
        font = painter.font()
        font.setPointSize(10)
        painter.setFont(font)
        painter.drawText(
            16,
            self.height() - 14,
            f"{glyph.name}  {glyph.status}  块 {glyph.tile_count}  "
            f"复用 {glyph.reused_tiles}  重算 {glyph.recomputed_tiles}  "
            f"哈希 {(glyph.coverage_hash or '-')[:12]}",
        )


class RasterThread(QThread):
    progress_event = Signal(object)
    result_ready = Signal(object)
    business_error = Signal(object)
    cancelled = Signal(str)

    def __init__(
        self,
        font_path: str,
        text: str,
        config: WorkbenchConfig,
        output_dir: str,
        snapshot_path: str,
        resume: bool,
        previous_summaries: dict[str, dict[str, Any]] | None,
    ) -> None:
        super().__init__()
        self.font_path = font_path
        self.text = text
        self.config = config
        self.output_dir = output_dir
        self.snapshot_path = snapshot_path
        self.resume = resume
        self.previous_summaries = previous_summaries
        self.cancel_event = threading.Event()

    def run(self) -> None:
        try:
            font = load_font(
                self.font_path,
                flatten_tolerance=self.config.curve_tolerance,
            )
            result = run_pipeline(
                font,
                self.text,
                self.config,
                output_dir=self.output_dir,
                snapshot_path=self.snapshot_path,
                resume=self.resume,
                cancel_event=self.cancel_event,
                previous_summaries=self.previous_summaries,
                progress=self.progress_event.emit,
            )
        except RunCancelled as exc:
            self.cancelled.emit(exc.snapshot_path or "")
            return
        except GlyphForgeError as exc:
            self.business_error.emit(exc.to_dict())
            return
        except Exception as exc:  # noqa: BLE001 - defensive UI boundary
            self.business_error.emit(
                {
                    "code": "E_UI_INTERNAL",
                    "message": repr(exc),
                    "details": {},
                }
            )
            return
        self.result_ready.emit(result)

    def cancel(self) -> None:
        self.cancel_event.set()


class WorkbenchWindow(QMainWindow):
    def __init__(
        self,
        *,
        font_path: str = "samples/fonts/mixed.json",
        config_path: str = "samples/configs/mixed-24-nonzero-q255.json",
        text: str = "AOB\u00c1",
    ) -> None:
        super().__init__()
        self.setWindowTitle("GlyphForge · 字形分块光栅化工作台")
        self.resize(1180, 760)
        self.config_path = config_path
        self.loaded_config = self._load_config(config_path)
        self.last_result: PipelineResult | None = None
        self.worker: RasterThread | None = None
        self.tile_states: dict[tuple[str, int, int], str] = {}
        self.output_dir = "artifacts/ui-last"
        self.snapshot_path = "artifacts/ui-last/snapshot.json"

        self.font_edit = QLineEdit(font_path)
        browse = QPushButton("浏览…")
        browse.clicked.connect(self._browse_font)
        font_row = QHBoxLayout()
        font_row.addWidget(self.font_edit, 1)
        font_row.addWidget(browse)

        self.text_edit = QLineEdit(text)
        self.text_edit.textChanged.connect(self._refresh_glyph_selector)
        self.size_combo = QComboBox()
        for size in (12, 16, 20, 24, 32, 36, 48, 64):
            self.size_combo.addItem(str(size), float(size))
        self._set_combo_value(self.size_combo, float(self.loaded_config.pixel_size))
        self.subpixel_x = QDoubleSpinBox()
        self.subpixel_y = QDoubleSpinBox()
        for control, value in (
            (self.subpixel_x, self.loaded_config.subpixel_x),
            (self.subpixel_y, self.loaded_config.subpixel_y),
        ):
            control.setRange(-2.0, 2.0)
            control.setDecimals(3)
            control.setSingleStep(0.25)
            control.setValue(value)
        self.fill_combo = QComboBox()
        self.fill_combo.addItem("非零环绕 non-zero", "nonzero")
        self.fill_combo.addItem("奇偶 even-odd", "evenodd")
        self.fill_combo.setCurrentIndex(
            0 if self.loaded_config.fill_rule == "nonzero" else 1
        )
        self.quant_spin = QDoubleSpinBox()
        self.quant_spin.setRange(0.000001, 1.0)
        self.quant_spin.setDecimals(8)
        self.quant_spin.setSingleStep(1.0 / 256.0)
        self.quant_spin.setValue(self.loaded_config.quant_step)
        self.tile_spin = QSpinBox()
        self.tile_spin.setRange(1, 64)
        self.tile_spin.setValue(self.loaded_config.tile_size)
        self.parallel_label = QLabel(str(self.loaded_config.budget.parallelism))
        self.snapshot_label = QLabel("—")

        self.start_button = QPushButton("启动光栅化")
        self.start_button.setDefault(True)
        self.start_button.clicked.connect(lambda: self._start(resume=False))
        self.resume_button = QPushButton("恢复快照")
        self.resume_button.clicked.connect(lambda: self._start(resume=True))
        self.cancel_button = QPushButton("协作式取消")
        self.cancel_button.setEnabled(False)
        self.cancel_button.clicked.connect(self._cancel)
        action_row = QHBoxLayout()
        action_row.addWidget(self.start_button)
        action_row.addWidget(self.resume_button)
        action_row.addWidget(self.cancel_button)

        self.glyph_selector = QComboBox()
        self.glyph_selector.currentIndexChanged.connect(self._refresh_views)
        self.status_label = QLabel("就绪")
        self.status_label.setWordWrap(True)
        self.resource_label = QLabel("资源：—")
        self.resource_label.setWordWrap(True)

        form = QFormLayout()
        form.addRow("字体 JSON", font_row)
        form.addRow("文本", self.text_edit)
        form.addRow("像素尺寸", self.size_combo)
        form.addRow("子像素 X", self.subpixel_x)
        form.addRow("子像素 Y", self.subpixel_y)
        form.addRow("填充规则", self.fill_combo)
        form.addRow("量化步长", self.quant_spin)
        form.addRow("分块尺寸", self.tile_spin)
        form.addRow("并行度（配置）", self.parallel_label)
        form.addRow("字形", self.glyph_selector)

        controls = QWidget()
        control_layout = QVBoxLayout(controls)
        control_layout.addLayout(form)
        control_layout.addLayout(action_row)
        control_layout.addWidget(self.status_label)
        control_layout.addWidget(self.resource_label)
        control_layout.addWidget(QLabel("轨迹 / 事件"))
        self.log = QPlainTextEdit()
        self.log.setReadOnly(True)
        self.log.setMaximumBlockCount(800)
        self.log.setFont(QFont("Consolas", 9))
        control_layout.addWidget(self.log, 1)
        controls.setMinimumWidth(390)
        controls.setMaximumWidth(470)

        self.atlas_view = AtlasView()
        self.glyph_view = GlyphView()
        tabs = QTabWidget()
        tabs.addTab(self.atlas_view, "图集预览")
        tabs.addTab(self.glyph_view, "字形与分块")

        splitter = QSplitter(Qt.Orientation.Horizontal)
        splitter.addWidget(controls)
        splitter.addWidget(tabs)
        splitter.setStretchFactor(0, 0)
        splitter.setStretchFactor(1, 1)
        self.setCentralWidget(splitter)
        self._append_log("工作台就绪；分块检查点支持协作式取消。")

    def _load_config(self, path: str) -> WorkbenchConfig:
        try:
            return WorkbenchConfig.from_file(path)
        except (OSError, ValueError):
            return WorkbenchConfig()

    @staticmethod
    def _set_combo_value(combo: QComboBox, value: float) -> None:
        index = combo.findData(value)
        if index < 0:
            combo.addItem(str(int(value)), value)
            index = combo.count() - 1
        combo.setCurrentIndex(index)

    def _browse_font(self) -> None:
        filename, _ = QFileDialog.getOpenFileName(
            self,
            "选择字体描述",
            str(Path(self.font_edit.text()).parent),
            "JSON 字体描述 (*.json);;所有文件 (*)",
        )
        if filename:
            self.font_edit.setText(filename)

    def _build_config(self) -> WorkbenchConfig:
        return replace(
            self.loaded_config,
            pixel_size=float(self.size_combo.currentData()),
            subpixel_x=float(self.subpixel_x.value()),
            subpixel_y=float(self.subpixel_y.value()),
            fill_rule=str(self.fill_combo.currentData()),
            quant_step=float(self.quant_spin.value()),
            tile_size=int(self.tile_spin.value()),
        )

    def _start(self, *, resume: bool) -> None:
        if self.worker is not None and self.worker.isRunning():
            return
        if not self.font_edit.text().strip():
            QMessageBox.warning(self, "字体缺失", "请选择本地字体 JSON 文件。")
            return
        try:
            config = self._build_config()
            config.validate()
        except (ValueError, GlyphForgeError) as exc:
            QMessageBox.warning(self, "配置无效", str(exc))
            return
        self.tile_states.clear()
        self.status_label.setText("运行中…")
        self.start_button.setEnabled(False)
        self.resume_button.setEnabled(False)
        self.cancel_button.setEnabled(True)
        self._append_log(
            f"启动：{self.text_edit.text()!r}  {config.pixel_size:g}px  "
            f"tile={config.tile_size}  P={config.budget.parallelism}  "
            f"{'恢复' if resume else '新建'}"
        )
        previous_summaries = None
        if self.last_result is not None and (
            self.last_result.config.quant_step != config.quant_step
        ):
            previous_summaries = {
                glyph.name: glyph.summary.to_dict()
                for glyph in self.last_result.glyphs
                if glyph.summary is not None
            }
        self.worker = RasterThread(
            self.font_edit.text().strip(),
            self.text_edit.text(),
            config,
            self.output_dir,
            self.snapshot_path,
            resume,
            previous_summaries,
        )
        self.worker.progress_event.connect(self._on_progress)
        self.worker.result_ready.connect(self._on_result)
        self.worker.business_error.connect(self._on_error)
        self.worker.cancelled.connect(self._on_cancelled)
        self.worker.finished.connect(self._on_worker_finished)
        self.worker.start()

    def _cancel(self) -> None:
        if self.worker is not None and self.worker.isRunning():
            self.worker.cancel()
            self.cancel_button.setEnabled(False)
            self.status_label.setText("已请求取消，等待分块检查点…")
            self._append_log("取消请求已发出；保留最后一次已发布图集。")

    def _on_progress(self, event: dict[str, Any]) -> None:
        kind = event.get("event")
        if kind == "run_start":
            self._append_log(
                f"计划字形 {', '.join(event.get('glyphs', []))}；"
                f"并行度 {event.get('parallelism')}"
            )
        elif kind == "glyph_plan":
            self._append_log(
                f"字形 {event['glyph']}：复用 {event['reused_tiles']}，"
                f"待算 {event['tiles_to_compute']}，"
                f"{'可运行' if event['accepted'] else '预算拒绝'}"
            )
        elif kind == "tile":
            tile = event["tile"]
            key = (event["glyph"], int(tile["row"]), int(tile["col"]))
            self.tile_states[key] = event["status"]
            self.snapshot_label.setText(str(event.get("snapshot_completed_tiles", 0)))
            if event["status"] in ("failed", "rejected", "rejected_glyph_isolated"):
                self._append_log(
                    f"分块 {event['glyph']}[{tile['row']},{tile['col']}] "
                    f"{event['status']}"
                )
        elif kind == "run_complete":
            self._append_log(
                f"完成：成功 {event['successful_glyphs']} 字形，"
                f"复用 {event['reused_tiles']} 块，重算 {event['recomputed_tiles']} 块"
            )
        self._refresh_views()

    def _on_result(self, result: PipelineResult) -> None:
        self.last_result = result
        self.status_label.setText(
            f"已发布：{result.successful_glyph_count}/{len(result.glyphs)} 字形，"
            f"图集 alpha {result.atlas.alpha_hash[:12]}…"
        )
        self._populate_glyph_selector(result)
        self._update_resource_label(result)
        self.snapshot_label.setText(str(result.snapshot_completed_tiles))
        self.atlas_view.set_result(result)
        self._refresh_views()
        for error in result.errors:
            self._append_log(
                f"业务错误 {error.code.value}：{error.message} "
                f"{error.details.get('glyph', '')}"
            )
        for glyph in result.glyphs:
            if glyph.comparison is not None:
                comparison = glyph.comparison
                self._append_log(
                    f"量化 {glyph.name}: {comparison.previous_step:.8g} → "
                    f"{comparison.current_step:.8g}，误差 "
                    f"{comparison.previous_abs_error:.6f} → "
                    f"{comparison.current_abs_error:.6f} "
                    f"(Δ {comparison.abs_error_delta:+.6f})"
                )

    def _on_error(self, payload: dict[str, Any]) -> None:
        self.status_label.setText(f"失败：{payload.get('code')}")
        self._append_log(
            f"{payload.get('code')}: {payload.get('message')} "
            f"{payload.get('details', {})}"
        )
        QMessageBox.warning(self, "光栅化失败", str(payload.get("message")))

    def _on_cancelled(self, snapshot_path: str) -> None:
        self.status_label.setText("已取消；上次已发布图集保持不变")
        self._append_log(
            f"已在分块检查点取消。快照：{snapshot_path or '未配置'}；"
            "未生成退出报告。"
        )

    def _on_worker_finished(self) -> None:
        self.start_button.setEnabled(True)
        self.resume_button.setEnabled(True)
        self.cancel_button.setEnabled(False)
        self.worker = None

    def _populate_glyph_selector(self, result: PipelineResult) -> None:
        self.glyph_selector.blockSignals(True)
        self.glyph_selector.clear()
        for glyph in result.glyphs:
            self.glyph_selector.addItem(
                f"{glyph.name} · {glyph.status}", glyph.name
            )
        self.glyph_selector.blockSignals(False)
        if self.selector_name() not in {glyph.name for glyph in result.glyphs}:
            self.glyph_selector.setCurrentIndex(0)

    def selector_name(self) -> str | None:
        return self.glyph_selector.currentData()

    def _refresh_glyph_selector(self) -> None:
        self._refresh_views()

    def _selected_glyph(self) -> GlyphRunResult | None:
        if self.last_result is None:
            return None
        name = self.selector_name()
        for glyph in self.last_result.glyphs:
            if glyph.name == name:
                return glyph
        return None

    def _refresh_views(self) -> None:
        if self.last_result is not None:
            states = dict(self.tile_states)
            for glyph in self.last_result.glyphs:
                for event in glyph.tile_events:
                    tile = event.get("tile", {})
                    key = (glyph.name, int(tile.get("row", 0)), int(tile.get("col", 0)))
                    states[key] = str(event.get("status", "pending"))
            self.glyph_view.set_data(
                self.last_result, self._selected_glyph(), states
            )
            self.atlas_view.set_result(self.last_result)
        else:
            self.glyph_view.set_data(None, None, {})

    def _update_resource_label(self, result: PipelineResult) -> None:
        measurements = [
            event.get("measurement")
            for glyph in result.glyphs
            for event in glyph.tile_events
            if event.get("measurement")
        ]
        peak = max(
            (int(item.get("total_bytes", 0)) for item in measurements),
            default=0,
        )
        total = sum(int(item.get("total_bytes", 0)) for item in measurements)
        self.resource_label.setText(
            f"资源：峰值任务 {peak} B；轨迹计量合计 {total} B；"
            f"复用 {result.reuse_count} 块；重算 {result.recompute_count} 块；"
            f"快照 {result.snapshot_completed_tiles} 块"
        )

    def _append_log(self, text: str) -> None:
        self.log.appendPlainText(text)

    def closeEvent(self, event) -> None:
        if self.worker is not None and self.worker.isRunning():
            self.worker.cancel()
            self.worker.wait(3000)
        super().closeEvent(event)


def launch_ui(
    *,
    font_path: str,
    config_path: str,
    text: str,
) -> int:
    app = QApplication.instance() or QApplication(sys.argv)
    app.setApplicationName("GlyphForge")
    window = WorkbenchWindow(
        font_path=font_path,
        config_path=config_path,
        text=text,
    )
    window.show()
    return app.exec()
