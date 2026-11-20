from __future__ import annotations

import os
import sys
import threading
from pathlib import Path
from typing import Any

import numpy as np
from PySide6.QtCore import QObject, QRectF, Qt, QThread, QTimer, Signal
from PySide6.QtGui import QColor, QFont, QImage, QPainter, QPen, QPixmap
from PySide6.QtWidgets import (
    QApplication,
    QComboBox,
    QDoubleSpinBox,
    QFileDialog,
    QFormLayout,
    QHBoxLayout,
    QHeaderView,
    QLabel,
    QListWidget,
    QListWidgetItem,
    QMainWindow,
    QMessageBox,
    QPushButton,
    QSlider,
    QSplitter,
    QTableWidget,
    QTableWidgetItem,
    QTextEdit,
    QVBoxLayout,
    QWidget,
)

from .color import decode_transfer
from .errors import ErrorCode, HDRError
from .model import (
    ColorSpace,
    ConversionParams,
    ExposureSegment,
    ToneMap,
    TransferFunction,
    VersionRecord,
)
from .session import ConversionSession


class TimelineWidget(QWidget):
    frame_clicked = Signal(int)

    def __init__(self, parent: QWidget | None = None) -> None:
        super().__init__(parent)
        self.setMinimumHeight(96)
        self.frame_count = 0
        self.intervals: list[tuple[int, int]] = []
        self.current_frame = 0
        self.version_label = "源素材"
        self.setMouseTracking(True)

    def set_state(
        self,
        *,
        frame_count: int,
        intervals: list[tuple[int, int]],
        current_frame: int,
        version_label: str,
    ) -> None:
        self.frame_count = max(0, frame_count)
        self.intervals = intervals
        self.current_frame = current_frame
        self.version_label = version_label
        self.update()

    def _track_rect(self) -> QRectF:
        return QRectF(22.0, 35.0, max(1.0, self.width() - 44.0), 28.0)

    def paintEvent(self, event: object) -> None:
        del event
        painter = QPainter(self)
        painter.setRenderHint(QPainter.RenderHint.Antialiasing, True)
        painter.fillRect(self.rect(), QColor("#171a20"))
        painter.setPen(QColor("#8b94a7"))
        painter.setFont(QFont("Sans Serif", 9))
        painter.drawText(22, 23, self.version_label)
        track = self._track_rect()
        painter.setPen(Qt.PenStyle.NoPen)
        painter.setBrush(QColor("#2a303b"))
        painter.drawRoundedRect(track, 4.0, 4.0)
        if self.frame_count <= 0:
            return
        painter.setBrush(QColor("#5b9cf5"))
        for start, end in self.intervals:
            left = track.left() + track.width() * (start / self.frame_count)
            right = track.left() + track.width() * ((end + 1) / self.frame_count)
            painter.drawRect(QRectF(left, track.top(), max(1.0, right - left), track.height()))
        painter.setPen(QPen(QColor("#394252"), 1.0))
        step = max(1, self.frame_count // 24)
        for frame in range(0, self.frame_count, step):
            x = track.left() + track.width() * (frame / self.frame_count)
            painter.drawLine(x, track.bottom() + 3, x, track.bottom() + 9)
        marker_x = track.left() + track.width() * (
            (min(self.current_frame, self.frame_count - 1) + 0.5) / self.frame_count
        )
        painter.setPen(QPen(QColor("#ffd166"), 2.0))
        painter.drawLine(marker_x, track.top() - 5, marker_x, track.bottom() + 5)
        painter.setPen(QColor("#c7cfdd"))
        painter.drawText(22, 88, f"帧 {self.current_frame:06d} / {self.frame_count - 1:06d}")

    def mousePressEvent(self, event: Any) -> None:
        if self.frame_count <= 0:
            return
        track = self._track_rect()
        x = min(max(float(event.position().x()), track.left()), track.right())
        frame = int((x - track.left()) / track.width() * self.frame_count)
        self.frame_clicked.emit(min(self.frame_count - 1, max(0, frame)))


class ConversionWorker(QObject):
    progress = Signal(int, int)
    published = Signal(object)
    failed = Signal(str, str, object)
    cancelled = Signal()
    finished = Signal()

    def __init__(
        self,
        engine: ConversionSession,
        params: ConversionParams | None = None,
        rollback_target: int | None = None,
    ) -> None:
        super().__init__()
        self.engine = engine
        self.params = params
        self.rollback_target = rollback_target
        self.cancel_event = threading.Event()

    def run(self) -> None:
        try:
            if self.rollback_target is not None:
                record = self.engine.rollback(self.rollback_target)
            else:
                assert self.params is not None
                record = self.engine.submit(
                    self.params,
                    cancel_event=self.cancel_event,
                    progress=self.progress.emit,
                )
            self.published.emit(record)
        except HDRError as exc:
            if exc.code is ErrorCode.CANCELLED:
                self.cancelled.emit()
            else:
                self.failed.emit(
                    exc.code.value,
                    exc.message,
                    {
                        "conflict_params": exc.conflict_params,
                        "details": exc.details,
                    },
                )
        except Exception as exc:  # noqa: BLE001  # pragma: no cover - UI boundary
            self.failed.emit("E_INTERNAL", str(exc), {})
        finally:
            self.finished.emit()


class WorkbenchWindow(QMainWindow):
    def __init__(self, workspace: Path, source: Path | None = None) -> None:
        super().__init__()
        self.workspace = workspace
        self.engine = ConversionSession(workspace, source)
        self._thread: QThread | None = None
        self._worker: ConversionWorker | None = None
        self._selected_version: int | None = None
        self._frame_index = 0
        self._play_timer = QTimer(self)
        self._play_timer.setInterval(120)
        self._play_timer.timeout.connect(self._advance_playback)
        self._build_ui()
        self._apply_header_defaults()
        self._refresh_versions()
        self._load_preview_state()

    def _build_ui(self) -> None:
        self.setWindowTitle("HDR Delivery Studio")
        self.resize(1420, 880)
        central = QWidget(self)
        self.setCentralWidget(central)
        root = QVBoxLayout(central)
        splitter = QSplitter(Qt.Orientation.Horizontal)
        root.addWidget(splitter, 1)

        left = QWidget()
        left_layout = QVBoxLayout(left)
        source_group = QWidget()
        source_form = QFormLayout(source_group)
        self.source_label = QLabel("-")
        self.source_label.setWordWrap(True)
        source_form.addRow("源素材", self.source_label)
        left_layout.addWidget(source_group)

        param_group = QWidget()
        form = QFormLayout(param_group)
        self.color_combo = QComboBox()
        for value in ColorSpace:
            self.color_combo.addItem(value.value, value)
        self.transfer_combo = QComboBox()
        for value in TransferFunction:
            self.transfer_combo.addItem(value.value, value)
        self.peak_spin = QDoubleSpinBox()
        self.peak_spin.setRange(1.0, 10000.0)
        self.peak_spin.setDecimals(2)
        self.peak_spin.setSingleStep(10.0)
        self.tone_combo = QComboBox()
        for value in ToneMap:
            self.tone_combo.addItem(value.value, value)
        form.addRow("目标色彩空间", self.color_combo)
        form.addRow("目标传递函数", self.transfer_combo)
        form.addRow("目标峰值 (nit)", self.peak_spin)
        form.addRow("色调映射", self.tone_combo)
        left_layout.addWidget(param_group)

        left_layout.addWidget(QLabel("逐段曝光调整"))
        self.exposure_table = QTableWidget(0, 3)
        self.exposure_table.setHorizontalHeaderLabels(["起始帧", "结束帧", "EV"])
        self.exposure_table.horizontalHeader().setSectionResizeMode(
            QHeaderView.ResizeMode.Stretch
        )
        left_layout.addWidget(self.exposure_table, 1)
        segment_buttons = QHBoxLayout()
        add_segment = QPushButton("添加段")
        remove_segment = QPushButton("删除段")
        add_segment.clicked.connect(self._add_exposure_segment)
        remove_segment.clicked.connect(self._remove_exposure_segment)
        segment_buttons.addWidget(add_segment)
        segment_buttons.addWidget(remove_segment)
        left_layout.addLayout(segment_buttons)
        self.submit_button = QPushButton("提交转换")
        self.submit_button.clicked.connect(self._start_conversion)
        self.cancel_button = QPushButton("取消任务")
        self.cancel_button.setEnabled(False)
        self.cancel_button.clicked.connect(self._cancel_conversion)
        left_layout.addWidget(self.submit_button)
        left_layout.addWidget(self.cancel_button)
        self.status_label = QLabel("就绪")
        self.status_label.setWordWrap(True)
        left_layout.addWidget(self.status_label)
        splitter.addWidget(left)

        center = QWidget()
        center_layout = QVBoxLayout(center)
        self.preview_label = QLabel("无预览")
        self.preview_label.setAlignment(Qt.AlignmentFlag.AlignCenter)
        self.preview_label.setMinimumSize(560, 315)
        self.preview_label.setStyleSheet(
            "background:#0c0e12;color:#7c879a;border:1px solid #2a303b;"
        )
        center_layout.addWidget(self.preview_label, 1)
        playback_row = QHBoxLayout()
        self.play_button = QPushButton("播放")
        self.play_button.clicked.connect(self._toggle_playback)
        self.frame_slider = QSlider(Qt.Orientation.Horizontal)
        self.frame_slider.valueChanged.connect(self._frame_slider_changed)
        self.frame_label = QLabel("0")
        playback_row.addWidget(self.play_button)
        playback_row.addWidget(self.frame_slider, 1)
        playback_row.addWidget(self.frame_label)
        center_layout.addLayout(playback_row)
        splitter.addWidget(center)

        right = QWidget()
        right_layout = QVBoxLayout(right)
        right_layout.addWidget(QLabel("转换版本"))
        self.version_list = QListWidget()
        self.version_list.setSelectionMode(
            QListWidget.SelectionMode.ExtendedSelection
        )
        self.version_list.itemSelectionChanged.connect(self._version_selection_changed)
        right_layout.addWidget(self.version_list, 1)
        self.version_detail = QTextEdit()
        self.version_detail.setReadOnly(True)
        self.version_detail.setMinimumHeight(220)
        right_layout.addWidget(self.version_detail)
        version_buttons = QHBoxLayout()
        self.compare_button = QPushButton("对比所选")
        self.rollback_button = QPushButton("回退到所选")
        self.compare_button.clicked.connect(self._compare_versions)
        self.rollback_button.clicked.connect(self._rollback_selected)
        version_buttons.addWidget(self.compare_button)
        version_buttons.addWidget(self.rollback_button)
        right_layout.addLayout(version_buttons)
        splitter.addWidget(right)
        splitter.setSizes([330, 720, 370])

        self.timeline = TimelineWidget()
        self.timeline.frame_clicked.connect(self._set_frame)
        root.addWidget(self.timeline)

        menu = self.menuBar().addMenu("文件")
        open_action = menu.addAction("打开 HDR 序列…")
        open_action.triggered.connect(self._choose_source)
        quit_action = menu.addAction("退出")
        quit_action.triggered.connect(self.close)

    def _apply_header_defaults(self) -> None:
        header = self.engine.header
        source = f"{self.engine.source_path}\n"
        source += (
            f"{header.width}×{header.height}, {header.frame_count} 帧, "
            f"{header.bit_depth}-bit, {header.color_space.value}, "
            f"{header.transfer.value}, {header.peak_luminance:g} nit"
        )
        self.source_label.setText(source)
        record = self.engine.latest_record
        params = record.params if record is not None else ConversionParams(
            target_color_space=header.color_space,
            target_transfer=header.transfer,
            peak_luminance=header.peak_luminance,
            tone_map=ToneMap.NONE,
        )
        self._set_param_controls(params)
        self.frame_slider.setRange(0, header.frame_count - 1)

    def _set_param_controls(self, params: ConversionParams) -> None:
        self.color_combo.setCurrentIndex(
            self.color_combo.findData(params.target_color_space)
        )
        self.transfer_combo.setCurrentIndex(
            self.transfer_combo.findData(params.target_transfer)
        )
        self.tone_combo.setCurrentIndex(self.tone_combo.findData(params.tone_map))
        self.peak_spin.setValue(params.peak_luminance)
        self.exposure_table.setRowCount(0)
        for segment in params.exposure_segments:
            self._append_segment_row(segment)

    def _append_segment_row(self, segment: ExposureSegment) -> None:
        row = self.exposure_table.rowCount()
        self.exposure_table.insertRow(row)
        for column, value in enumerate((segment.start, segment.end, segment.ev)):
            item = QTableWidgetItem(str(value))
            self.exposure_table.setItem(row, column, item)

    def _add_exposure_segment(self) -> None:
        row = self.exposure_table.rowCount()
        start = 0
        end = max(0, self.engine.header.frame_count - 1)
        if row:
            previous_end = self._table_int(self.exposure_table.item(row - 1, 1), -1)
            start = min(self.engine.header.frame_count - 1, previous_end + 1)
        self._append_segment_row(ExposureSegment(start, end, 0.0))

    def _remove_exposure_segment(self) -> None:
        row = self.exposure_table.currentRow()
        if row >= 0:
            self.exposure_table.removeRow(row)

    @staticmethod
    def _table_int(item: QTableWidgetItem | None, default: int) -> int:
        if item is None:
            return default
        try:
            return int(item.text())
        except ValueError:
            return default

    @staticmethod
    def _table_float(item: QTableWidgetItem | None, default: float) -> float:
        if item is None:
            return default
        try:
            return float(item.text())
        except ValueError:
            return default

    def _read_params(self) -> ConversionParams:
        segments: list[ExposureSegment] = []
        for row in range(self.exposure_table.rowCount()):
            start = self._table_int(self.exposure_table.item(row, 0), -1)
            end = self._table_int(self.exposure_table.item(row, 1), -1)
            ev = self._table_float(self.exposure_table.item(row, 2), 0.0)
            try:
                segments.append(ExposureSegment(start=start, end=end, ev=ev))
            except ValueError as exc:
                raise HDRError(
                    ErrorCode.PARAM_CONFLICT,
                    f"曝光段第 {row + 1} 行无效: {exc}",
                    conflict_params=[f"exposure_segments[{row}]"],
                ) from exc
        return ConversionParams(
            target_color_space=self.color_combo.currentData(),
            target_transfer=self.transfer_combo.currentData(),
            peak_luminance=self.peak_spin.value(),
            tone_map=self.tone_combo.currentData(),
            exposure_segments=tuple(segments),
        )

    def _refresh_versions(self) -> None:
        selected = self._selected_version
        self.version_list.blockSignals(True)
        self.version_list.clear()
        for record in reversed(self.engine.versions):
            item = QListWidgetItem(
                f"v{record.version:06d}  {record.operation:<8}  "
                f"影响 {record.affected_count:>3}  重算 {record.recomputed_count:>3}  "
                f"复用 {record.reused_count:>3}"
            )
            item.setData(Qt.ItemDataRole.UserRole, record.version)
            self.version_list.addItem(item)
        self.version_list.blockSignals(False)
        if selected is None and self.engine.versions:
            selected = self.engine.versions[-1].version
        if selected is not None:
            self._select_version_in_list(selected)
            self._selected_version = selected
            self._update_detail(selected)
        self._refresh_timeline()

    def _select_version_in_list(self, version: int) -> None:
        for row in range(self.version_list.count()):
            item = self.version_list.item(row)
            if item.data(Qt.ItemDataRole.UserRole) == version:
                self.version_list.setCurrentItem(item)
                return

    def _load_preview_state(self) -> None:
        preview = self.engine.preview_state
        if preview.selected_version is not None:
            self._selected_version = preview.selected_version
            self._frame_index = preview.frame_index
            self._select_version_in_list(preview.selected_version)
            self._update_detail(preview.selected_version)
        if preview.playing and self.engine.header.frame_count > 1:
            self._play_timer.start()
            self.play_button.setText("暂停")
        self.frame_slider.setValue(self._frame_index)
        self._refresh_timeline()
        self._render_preview()

    def _version_selection_changed(self) -> None:
        items = self.version_list.selectedItems()
        if not items:
            return
        current = self.version_list.currentItem()
        if current is None:
            current = items[0]
        version = int(current.data(Qt.ItemDataRole.UserRole))
        self._selected_version = version
        self._update_detail(version)
        self._refresh_timeline()
        self._render_preview()
        try:
            self.engine.set_preview(
                selected_version=version,
                frame_index=self._frame_index,
                playing=self._play_timer.isActive(),
            )
        except HDRError as exc:
            self.status_label.setText(str(exc))

    def _selected_record(self) -> VersionRecord | None:
        if self._selected_version is None:
            return None
        try:
            return self.engine.get_version(self._selected_version)
        except HDRError:
            return None

    def _refresh_timeline(self) -> None:
        record = self._selected_record()
        intervals = list(record.affected_intervals) if record is not None else []
        label = (
            f"源素材 / {self.engine.source_path.name}"
            if record is None
            else f"v{record.version:06d} 受影响区间 {record.affected_intervals}"
        )
        self.timeline.set_state(
            frame_count=self.engine.header.frame_count,
            intervals=intervals,
            current_frame=self._frame_index,
            version_label=label,
        )

    def _frame_slider_changed(self, value: int) -> None:
        if value == self._frame_index:
            return
        self._frame_index = value
        self._on_frame_changed()

    def _set_frame(self, frame: int) -> None:
        self.frame_slider.setValue(frame)

    def _on_frame_changed(self) -> None:
        self.frame_label.setText(str(self._frame_index))
        self._refresh_timeline()
        self._render_preview()
        if not self._play_timer.isActive():
            self._persist_preview()

    def _persist_preview(self) -> None:
        if self._selected_version is None:
            return
        try:
            self.engine.set_preview(
                selected_version=self._selected_version,
                frame_index=self._frame_index,
                playing=self._play_timer.isActive(),
            )
        except HDRError as exc:
            self.status_label.setText(str(exc))

    def _render_preview(self) -> None:
        try:
            record = self._selected_record()
            if record is None:
                codes = self.engine.read_source_frame(self._frame_index)
                transfer = self.engine.header.transfer
                peak = self.engine.header.peak_luminance
            else:
                codes = self.engine.read_version_frame(record.version, self._frame_index)
                transfer = record.params.target_transfer
                peak = record.params.peak_luminance
            image = self._codes_to_image(codes, transfer, peak)
            self.preview_label.setPixmap(
                QPixmap.fromImage(image).scaled(
                    self.preview_label.size(),
                    Qt.AspectRatioMode.KeepAspectRatio,
                    Qt.TransformationMode.SmoothTransformation,
                )
            )
            self.frame_label.setText(str(self._frame_index))
        except HDRError as exc:
            self.status_label.setText(str(exc))

    def _codes_to_image(
        self, codes: np.ndarray, transfer: TransferFunction, peak: float
    ) -> QImage:
        max_code = float((1 << self.engine.header.bit_depth) - 1)
        normalized = codes.astype(np.float64) / max_code
        nits = decode_transfer(normalized, transfer, peak)
        display_linear = np.clip(nits / max(peak, 1.0), 0.0, 1.0)
        srgb = np.power(display_linear, 1.0 / 2.2)
        rgb = np.rint(srgb * 255.0).astype(np.uint8)
        rgb = np.ascontiguousarray(rgb)
        height, width, _ = rgb.shape
        return QImage(
            rgb.data, width, height, width * 3, QImage.Format.Format_RGB888
        ).copy()

    def _update_detail(self, version: int) -> None:
        try:
            record = self.engine.get_version(version)
        except HDRError:
            return
        peak_deviation = max(
            (float(item.get("max_code_deviation", 0.0)) for item in record.frame_summaries),
            default=0.0,
        )
        detail = [
            f"版本: v{record.version:06d}",
            f"父版本: {record.parent_version}",
            f"操作: {record.operation}"
            + (f" -> v{record.rollback_target:06d}" if record.rollback_target else ""),
            (
                f"影响/重算/复用: {record.affected_count} / "
                f"{record.recomputed_count} / {record.reused_count}"
            ),
            f"受影响区间: {record.affected_intervals}",
            f"输出哈希: {record.output_hash}",
            f"记录哈希: {record.record_hash}",
            f"父记录哈希: {record.parent_record_hash}",
            f"最大码值偏差: {peak_deviation:.12g}",
            "",
            "参数:",
            (
                f"  目标={record.params.target_color_space.value}, "
                f"传递={record.params.target_transfer.value}, "
                f"峰值={record.params.peak_luminance:g}, "
                f"曲线={record.params.tone_map.value}"
            ),
            f"  曝光段={[segment.to_dict() for segment in record.params.exposure_segments]}",
            "",
            "中间链:",
            f"  精度={record.intermediate_summary.get('intermediate_precision')}",
            f"  矩阵哈希={record.intermediate_summary.get('color_matrix_hash')}",
            f"  白点适应={record.intermediate_summary.get('white_adaptation')}",
        ]
        self.version_detail.setPlainText("\n".join(detail))

    def _toggle_playback(self) -> None:
        if self._play_timer.isActive():
            self._play_timer.stop()
            self.play_button.setText("播放")
            self._persist_preview()
        else:
            self._play_timer.start()
            self.play_button.setText("暂停")
            self._persist_preview()

    def _advance_playback(self) -> None:
        if self.engine.header.frame_count <= 0:
            return
        self._frame_index = (self._frame_index + 1) % self.engine.header.frame_count
        self.frame_slider.blockSignals(True)
        self.frame_slider.setValue(self._frame_index)
        self.frame_slider.blockSignals(False)
        self._refresh_timeline()
        self._render_preview()

    def _set_busy(self, busy: bool) -> None:
        self.submit_button.setEnabled(not busy)
        self.cancel_button.setEnabled(busy)
        self.rollback_button.setEnabled(not busy)
        self.version_list.setEnabled(not busy)
        self.color_combo.setEnabled(not busy)
        self.transfer_combo.setEnabled(not busy)
        self.tone_combo.setEnabled(not busy)
        self.peak_spin.setEnabled(not busy)
        self.exposure_table.setEnabled(not busy)

    def _start_conversion(self) -> None:
        if self.engine.is_busy or self._thread is not None:
            self._show_error("E_BUSY", "当前会话已有转换任务", {})
            return
        try:
            params = self._read_params()
        except HDRError as exc:
            self._show_error(exc.code.value, exc.message, exc.to_dict())
            return
        self._start_worker(ConversionWorker(self.engine, params=params))

    def _start_worker(self, worker: ConversionWorker) -> None:
        thread = QThread(self)
        worker.moveToThread(thread)
        thread.started.connect(worker.run)
        worker.progress.connect(self._on_progress)
        worker.published.connect(self._on_published)
        worker.failed.connect(self._on_worker_failed)
        worker.cancelled.connect(self._on_cancelled)
        worker.finished.connect(worker.deleteLater)
        worker.finished.connect(thread.quit)
        thread.finished.connect(self._on_worker_finished)
        self._thread = thread
        self._worker = worker
        self._set_busy(True)
        self.status_label.setText("转换中…")
        thread.start()

    def _on_progress(self, done: int, total: int) -> None:
        self.status_label.setText(f"转换中… {done}/{total}")

    def _on_published(self, record: VersionRecord) -> None:
        self._play_timer.stop()
        self.play_button.setText("播放")
        self._selected_version = record.version
        self._frame_index = 0
        self.status_label.setText(f"已发布 v{record.version:06d}")
        self._refresh_versions()
        self._render_preview()

    def _on_worker_failed(self, code: str, message: str, details: object) -> None:
        self.status_label.setText(f"{code}: {message}")
        self._show_error(code, message, details if isinstance(details, dict) else {})

    def _on_cancelled(self) -> None:
        self.status_label.setText("任务已取消，最后发布版本保持不变")

    def _on_worker_finished(self) -> None:
        if self._thread is not None:
            self._thread.deleteLater()
        self._thread = None
        self._worker = None
        self._set_busy(False)

    def _cancel_conversion(self) -> None:
        if self.engine.cancel_active():
            self.status_label.setText("正在取消…")

    def _show_error(self, code: str, message: str, details: dict[str, Any]) -> None:
        text = f"{code}: {message}"
        conflict = details.get("conflict_params") if isinstance(details, dict) else None
        if conflict:
            text += f"\n冲突参数: {', '.join(conflict)}"
        QMessageBox.warning(self, "转换未发布", text)

    def _compare_versions(self) -> None:
        versions = sorted(
            int(item.data(Qt.ItemDataRole.UserRole))
            for item in self.version_list.selectedItems()
        )
        if len(versions) != 2:
            QMessageBox.information(self, "版本对比", "请选择两个版本。")
            return
        diff = self.engine.diff_versions(versions[0], versions[1])
        text = (
            f"v{versions[0]:06d} -> v{versions[1]:06d}\n"
            f"参数变化: {', '.join(diff.changed_params) or '无'}\n"
            f"变化帧数: {len(diff.changed_frames)}\n"
            f"变化帧: {list(diff.changed_frames)}\n"
            f"内容相同: {diff.same_content}\n"
            f"左输出哈希: {diff.left_output_hash}\n"
            f"右输出哈希: {diff.right_output_hash}"
        )
        QMessageBox.information(self, "版本差异", text)

    def _rollback_selected(self) -> None:
        record = self._selected_record()
        if record is None:
            return
        if self.engine.is_busy or self._thread is not None:
            self._show_error("E_BUSY", "当前会话已有任务", {})
            return
        self._start_worker(ConversionWorker(self.engine, rollback_target=record.version))

    def _choose_source(self) -> None:
        filename, _ = QFileDialog.getOpenFileName(
            self, "打开 HDR 序列", str(self.workspace / "samples"), "HDR Sequence (*.hdrseq)"
        )
        if not filename:
            return
        try:
            new_engine = ConversionSession(self.workspace, Path(filename))
        except HDRError as exc:
            self._show_error(exc.code.value, exc.message, exc.to_dict())
            return
        self.engine.close()
        self.engine = new_engine
        self._selected_version = None
        self._frame_index = 0
        self._apply_header_defaults()
        self._refresh_versions()
        self._load_preview_state()

    def closeEvent(self, event: Any) -> None:
        self._play_timer.stop()
        self._persist_preview()
        if self._thread is not None:
            self.engine.cancel_active()
            self._thread.quit()
            self._thread.wait(5000)
        self.engine.close()
        event.accept()


def run_workbench(workspace: Path | str, source: Path | str | None = None) -> int:
    app = QApplication.instance() or QApplication(sys.argv[:1])
    try:
        window = WorkbenchWindow(Path(workspace), Path(source) if source else None)
    except HDRError as exc:
        QMessageBox.critical(None, "启动失败", str(exc))
        return 1
    window.show()
    autoclose = os.environ.get("HDR_STUDIO_AUTOCLOSE_MS")
    if autoclose:
        QTimer.singleShot(int(autoclose), window.close)
    return app.exec()

