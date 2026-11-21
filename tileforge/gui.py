"""Optional PySide6 workbench.

The conversion engine is intentionally dependency-free. PySide6 is imported
only when the workbench is requested, so headless/offline verification remains
fully functional when Qt bindings are unavailable.
"""

from __future__ import annotations

import json
import sys
from dataclasses import replace
from pathlib import Path
from typing import Any

from .binary import parse_sequence, unpack_samples
from .config import ColorSpaceConfig, JobConfig, MemoryConfig, ObservationSpec, VALID_TONE_MAPS
from .errors import E_CANCELLED, TileForgeError, error
from .model import PRIMARIES_NAMES, TRANSFER_NAMES, Primaries, TransferCode
from .pipeline import ConversionJob
from .preview import preview_frame
from .store import Snapshot, VersionStore


def run_gui(input_path: str, config_path: str, workspace: str) -> int:
    try:
        from PySide6.QtCore import Qt, QTimer, Signal
        from PySide6.QtGui import QColor, QFont, QPainter, QPen
        from PySide6.QtWidgets import (
            QApplication,
            QComboBox,
            QDoubleSpinBox,
            QFileDialog,
            QFormLayout,
            QGroupBox,
            QHBoxLayout,
            QLabel,
            QLineEdit,
            QMainWindow,
            QMessageBox,
            QPlainTextEdit,
            QPushButton,
            QScrollArea,
            QSpinBox,
            QSplitter,
            QVBoxLayout,
            QWidget,
        )
    except ImportError:
        print(
            "PySide6 is required for the workbench. Use the offline wheelhouse bootstrap "
            "or run the CLI commands.",
            file=sys.stderr,
        )
        return 2

    input_sequence = parse_sequence(input_path)
    initial_config = JobConfig.load(config_path)
    initial_config.validate_for_sequence(
        input_sequence.header.width,
        input_sequence.header.height,
        input_sequence.header.block_count,
        input_sequence.header.source_transfer,
        input_sequence.header.conversion_id,
    )
    store = VersionStore(workspace)

    _STAGE_LABELS = {"full": "F", "banded": "B", "streaming": "S", "reused": "R"}

    class HeatGrid(QWidget):
        tileSelected = Signal(int)

        def __init__(self) -> None:
            super().__init__()
            self.snapshot: Snapshot | None = None
            self.records: dict[int, dict[str, Any]] = {}
            self.selected_tile: int | None = None
            self.setMinimumSize(420, 320)

        def set_snapshot(self, snapshot: Snapshot) -> None:
            self.snapshot = snapshot
            self.records = {int(item["tile_id"]): item for item in snapshot.manifest.get("tile_records", [])}
            self.update()

        def select_tile(self, tile_id: int | None) -> None:
            self.selected_tile = tile_id
            self.update()

        def _geometry(self) -> tuple[float, float, float]:
            if self.snapshot is None:
                return 0.0, 0.0, 1.0
            header = self.snapshot.sequence.header
            margin = 20.0
            scale = min(
                (self.width() - 2 * margin) / max(1, header.width),
                (self.height() - 2 * margin) / max(1, header.height),
            )
            offset_x = (self.width() - header.width * scale) / 2
            offset_y = (self.height() - header.height * scale) / 2
            return offset_x, offset_y, max(scale, 0.1)

        def paintEvent(self, event) -> None:  # noqa: N802
            painter = QPainter(self)
            painter.fillRect(self.rect(), QColor("#11151c"))
            if self.snapshot is None:
                painter.setPen(QColor("#9aa4b2"))
                painter.drawText(self.rect(), Qt.AlignmentFlag.AlignCenter, "No published version")
                return
            sequence = self.snapshot.sequence
            header = sequence.header
            offset_x, offset_y, scale = self._geometry()
            max_code = max(1, (1 << header.bit_depth) - 1)
            painter.setFont(QFont("monospace", max(7, int(7 * scale / 3))))
            for block in sequence.ordered_blocks():
                values = unpack_samples(block.data, header.bit_depth)
                channels = header.channels
                mean_r = sum(values[0::channels]) / max(1, len(values[0::channels]))
                mean_g = sum(values[1::channels]) / max(1, len(values[1::channels])) if channels > 1 else mean_r
                mean_b = sum(values[2::channels]) / max(1, len(values[2::channels])) if channels > 2 else mean_r
                color = QColor.fromRgbF(
                    min(1.0, (mean_r / max_code) ** 0.4545),
                    min(1.0, (mean_g / max_code) ** 0.4545),
                    min(1.0, (mean_b / max_code) ** 0.4545),
                )
                record = self.records.get(block.tile_id, {})
                cell = (
                    offset_x + block.x * scale,
                    offset_y + block.y * scale,
                    block.width * scale,
                    block.height * scale,
                )
                painter.fillRect(*[int(value) for value in cell], color)
                stage = str(record.get("downgrade_stage", record.get("profile", "?")))
                painter.setPen(QColor("#071016"))
                painter.drawText(
                    int(cell[0] + 5),
                    int(cell[1] + 14),
                    _STAGE_LABELS.get(stage, stage[:1].upper()),
                )
                if float(record.get("max_code_delta", 0)) > initial_config.observation.tolerance_code:
                    painter.setPen(QPen(QColor("#ff4655"), 2))
                    painter.drawRect(*[int(value) for value in cell])
                painter.setPen(QPen(QColor("#4b5563"), 1))
                painter.drawRect(*[int(value) for value in cell])
                if self.selected_tile == block.tile_id:
                    painter.setPen(QPen(QColor("#f8fafc"), 3))
                    painter.drawRect(*[int(value) for value in cell])
            painter.setPen(QColor("#9aa4b2"))
            painter.drawText(
                16,
                self.height() - 12,
                f"version {self.snapshot.version_id} | {self.snapshot.manifest.get('sync_marker')} | "
                f"{header.bit_depth}-bit | heat=preview code, F/B/S=stage",
            )

        def mousePressEvent(self, event) -> None:  # noqa: N802
            if self.snapshot is None:
                return
            offset_x, offset_y, scale = self._geometry()
            x = int((event.position().x() - offset_x) / scale)
            y = int((event.position().y() - offset_y) / scale)
            header = self.snapshot.sequence.header
            if 0 <= x < header.width and 0 <= y < header.height:
                tile_id = (y // header.tile_height) * header.tiles_x + (x // header.tile_width)
                self.selected_tile = tile_id
                self.tileSelected.emit(tile_id)
                self.update()

    class Workbench(QMainWindow):
        def __init__(self) -> None:
            super().__init__()
            self.setWindowTitle("TileForge Workbench")
            self.resize(1280, 820)
            self.config = initial_config
            self.input_sequence = input_sequence
            self.job: ConversionJob | None = None
            self.selected_tile: int | None = 0
            self.last_summary: dict[str, Any] | None = None

            self.grid = HeatGrid()
            self.grid.tileSelected.connect(self._select_tile)

            sidebar = QWidget()
            sidebar_layout = QVBoxLayout(sidebar)
            sidebar_layout.setContentsMargins(8, 8, 8, 8)

            self.version_label = QLabel("No published version")
            self.version_label.setWordWrap(True)
            sidebar_layout.addWidget(self.version_label)

            params = QGroupBox("Conversion parameters")
            form = QFormLayout(params)
            self.source_primaries = QComboBox()
            self.target_primaries = QComboBox()
            for primaries in (Primaries.REC709, Primaries.DISPLAY_P3, Primaries.REC2020):
                self.source_primaries.addItem(PRIMARIES_NAMES[primaries], primaries)
                self.target_primaries.addItem(PRIMARIES_NAMES[primaries], primaries)
            self.source_transfer = QComboBox()
            self.target_transfer = QComboBox()
            for transfer in (TransferCode.SDR_GAMMA, TransferCode.PQ, TransferCode.HLG):
                self.source_transfer.addItem(TRANSFER_NAMES[transfer], transfer)
                self.target_transfer.addItem(TRANSFER_NAMES[transfer], transfer)
            self.source_peak = QDoubleSpinBox()
            self.source_peak.setRange(1.0, 10_000.0)
            self.source_peak.setSuffix(" nits")
            self.target_peak = QDoubleSpinBox()
            self.target_peak.setRange(1.0, 10_000.0)
            self.target_peak.setSuffix(" nits")
            self.curve = QComboBox()
            self.curve.addItems(list(VALID_TONE_MAPS))
            self.sync_marker = QLineEdit(self.config.observation.sync_marker)
            self.exposure = QDoubleSpinBox()
            self.exposure.setRange(-16.0, 16.0)
            self.exposure.setSingleStep(0.05)
            self.exposure.setDecimals(4)
            self.exposure.setSuffix(" stops")
            self.exposure_label = QLabel("tile 0")
            self.parallelism = QSpinBox()
            self.parallelism.setRange(1, 64)
            self.tile_budget = QSpinBox()
            self.tile_budget.setRange(1, 2_000_000_000)
            self.tile_budget.setSingleStep(1024)
            self.run_budget = QSpinBox()
            self.run_budget.setRange(1, 2_000_000_000)
            self.run_budget.setSingleStep(1024)
            form.addRow("Source primaries", self.source_primaries)
            form.addRow("Source transfer", self.source_transfer)
            form.addRow("Target primaries", self.target_primaries)
            form.addRow("Target transfer", self.target_transfer)
            form.addRow("Source peak", self.source_peak)
            form.addRow("Target peak", self.target_peak)
            form.addRow("Tone map", self.curve)
            form.addRow("Sync marker", self.sync_marker)
            form.addRow(self.exposure_label, self.exposure)
            form.addRow("Parallelism", self.parallelism)
            form.addRow("Tile budget", self.tile_budget)
            form.addRow("Run budget", self.run_budget)
            sidebar_layout.addWidget(params)

            buttons = QHBoxLayout()
            self.start_button = QPushButton("Apply && Start")
            self.pause_button = QPushButton("Pause")
            self.cancel_button = QPushButton("Cancel")
            buttons.addWidget(self.start_button)
            buttons.addWidget(self.pause_button)
            buttons.addWidget(self.cancel_button)
            sidebar_layout.addLayout(buttons)

            history = QGroupBox("Published history")
            history_layout = QVBoxLayout(history)
            self.version_combo = QComboBox()
            self.rollback_button = QPushButton("Rollback to selected")
            history_layout.addWidget(self.version_combo)
            history_layout.addWidget(self.rollback_button)
            sidebar_layout.addWidget(history)

            self.status = QLabel("Idle")
            self.status.setWordWrap(True)
            self.status.setStyleSheet("color:#93c5fd")
            sidebar_layout.addWidget(self.status)
            self.log = QPlainTextEdit()
            self.log.setReadOnly(True)
            self.log.setMaximumBlockCount(2000)
            sidebar_layout.addWidget(self.log, 1)

            scroll = QScrollArea()
            scroll.setWidgetResizable(True)
            scroll.setWidget(sidebar)
            splitter = QSplitter()
            splitter.addWidget(self.grid)
            splitter.addWidget(scroll)
            splitter.setStretchFactor(0, 4)
            splitter.setStretchFactor(1, 2)
            self.setCentralWidget(splitter)

            self.start_button.clicked.connect(self.start_conversion)
            self.pause_button.clicked.connect(self.toggle_pause)
            self.cancel_button.clicked.connect(self.cancel_conversion)
            self.rollback_button.clicked.connect(self.rollback)
            self.exposure.valueChanged.connect(self.update_exposure)

            self.preview_timer = QTimer(self)
            self.preview_timer.timeout.connect(self.refresh_published)
            self.preview_timer.start(self.config.preview_refresh_ms)
            self.job_timer = QTimer(self)
            self.job_timer.timeout.connect(self.poll_job)
            self.job_timer.start(100)
            self._load_controls()
            self._populate_history()
            self.refresh_published()

        def _load_controls(self) -> None:
            self.source_primaries.setCurrentIndex(self.source_primaries.findData(self.config.source.primaries))
            self.target_primaries.setCurrentIndex(self.target_primaries.findData(self.config.target.primaries))
            self.source_transfer.setCurrentIndex(self.source_transfer.findData(self.config.source.transfer))
            self.target_transfer.setCurrentIndex(self.target_transfer.findData(self.config.target.transfer))
            self.source_peak.setValue(self.config.source.peak_nits)
            self.target_peak.setValue(self.config.target.peak_nits)
            self.curve.setCurrentText(self.config.tone_map_curve)
            self.parallelism.setValue(self.config.memory.parallelism)
            self.tile_budget.setValue(self.config.memory.tile_budget_bytes)
            self.run_budget.setValue(self.config.memory.run_budget_bytes)
            self._select_tile(self.selected_tile or 0)

        def _edited_config(self) -> JobConfig:
            exposures = dict(self.config.block_exposure_stops)
            curves = dict(self.config.block_tone_map)
            if self.selected_tile is not None:
                block = self.input_sequence.block_map()[self.selected_tile]
                value = self.exposure.value()
                if abs(value - block.exposure) > 1e-12:
                    exposures[self.selected_tile] = value
                elif self.selected_tile in exposures:
                    del exposures[self.selected_tile]
            self.config = replace(
                self.config,
                source=replace(
                    self.config.source,
                    primaries=self.source_primaries.currentData(),
                    transfer=self.source_transfer.currentData(),
                ),
                target=replace(
                    self.config.target,
                    primaries=self.target_primaries.currentData(),
                    transfer=self.target_transfer.currentData(),
                ),
                tone_map_curve=self.curve.currentText(),
                block_exposure_stops=exposures,
                block_tone_map=curves,
                observation=replace(
                    self.config.observation,
                    sync_marker=self.sync_marker.text().strip() or self.config.observation.sync_marker,
                ),
                memory=MemoryConfig(
                    parallelism=self.parallelism.value(),
                    tile_budget_bytes=self.tile_budget.value(),
                    run_budget_bytes=self.run_budget.value(),
                    downgrade_profiles=self.config.memory.downgrade_profiles,
                    estimator_overhead_bytes=self.config.memory.estimator_overhead_bytes,
                ),
            )
            self.config = replace(
                self.config,
                source=replace(self.config.source, peak_nits=self.source_peak.value()),
                target=replace(self.config.target, peak_nits=self.target_peak.value()),
            )
            return self.config

        def _select_tile(self, tile_id: int) -> None:
            if tile_id not in self.input_sequence.block_map():
                return
            self.selected_tile = tile_id
            block = self.input_sequence.block_map()[tile_id]
            self.exposure_label.setText(f"tile {tile_id} exposure")
            self.exposure.blockSignals(True)
            self.exposure.setValue(self.config.block_exposure_stops.get(tile_id, block.exposure))
            self.exposure.blockSignals(False)
            self.grid.select_tile(tile_id)

        def update_exposure(self, value: float) -> None:
            if self.selected_tile is None:
                return
            exposures = dict(self.config.block_exposure_stops)
            block = self.input_sequence.block_map()[self.selected_tile]
            if abs(value - block.exposure) <= 1e-12:
                exposures.pop(self.selected_tile, None)
            else:
                exposures[self.selected_tile] = value
            self.config = replace(self.config, block_exposure_stops=exposures)

        def start_conversion(self) -> None:
            if self.job is not None and self.job.is_alive:
                return
            try:
                config = self._edited_config()
                config.validate_for_sequence(
                    self.input_sequence.header.width,
                    self.input_sequence.header.height,
                    self.input_sequence.header.block_count,
                    self.input_sequence.header.source_transfer,
                    self.input_sequence.header.conversion_id,
                )
                self.job = ConversionJob(self.input_path, config, self.workspace)
                self.job.start()
                self.status.setText("Converting...")
                self.log.appendPlainText(
                    f"start config={config.config_hash()[:12]} conversion={config.conversion_id()} "
                    f"parallelism={config.memory.parallelism}"
                )
            except TileForgeError as exc:
                QMessageBox.warning(self, exc.code, str(exc))

        def toggle_pause(self) -> None:
            if self.job is None or not self.job.is_alive:
                return
            if self.pause_button.text() == "Pause":
                self.job.pause()
                self.pause_button.setText("Resume")
                self.status.setText("Paused (running tiles finish)")
            else:
                self.job.resume()
                self.pause_button.setText("Pause")
                self.status.setText("Converting...")

        def cancel_conversion(self) -> None:
            if self.job is not None and self.job.is_alive:
                self.job.cancel()
                self.status.setText("Cancellation requested")

        def poll_job(self) -> None:
            if self.job is None or self.job.is_alive:
                return
            try:
                result = self.job.join(0)
                self.last_summary = result
                if result is not None:
                    self.status.setText(
                        f"Published version {result['version_id']} | affected {result['affected_count']} | "
                        f"reused {result['reused_count']}"
                    )
                    self.log.appendPlainText(json.dumps(result, sort_keys=True))
                    self._populate_history()
                    self.refresh_published()
            except TileForgeError as exc:
                self.status.setText(f"{exc.code}: {exc.message}")
                self.log.appendPlainText(json.dumps(exc.to_dict(), sort_keys=True))
            finally:
                self.pause_button.setText("Pause")
                self.job = None

        def refresh_published(self) -> None:
            try:
                snapshot = store.load_current()
            except TileForgeError:
                return
            self.grid.set_snapshot(snapshot)
            manifest = snapshot.manifest
            self.version_label.setText(
                f"version {snapshot.version_id}\n{snapshot.version_hash}\n"
                f"sync {manifest.get('sync_marker')} | affected {len(manifest.get('affected_tiles', []))} | "
                f"reused {len(manifest.get('reused_tiles', []))}"
            )

        def _populate_history(self) -> None:
            current_id = None
            try:
                current_id = store.load_current().version_id
            except TileForgeError:
                pass
            self.version_combo.clear()
            for manifest in reversed(store.list_versions()):
                label = (
                    f"v{manifest['version_id']}  {manifest['version_hash'][:12]}  "
                    f"A{len(manifest.get('affected_tiles', []))}/R{len(manifest.get('reused_tiles', []))}"
                )
                if manifest["version_id"] == current_id:
                    label += "  [current]"
                self.version_combo.addItem(label, int(manifest["version_id"]))

        def rollback(self) -> None:
            version_id = self.version_combo.currentData()
            if version_id is None:
                return
            try:
                manifest = store.rollback(int(version_id))
                self.status.setText(f"Rolled back to version {version_id}")
                self.log.appendPlainText(f"rollback -> v{version_id} {manifest['version_hash']}")
                self._populate_history()
                self.refresh_published()
            except TileForgeError as exc:
                QMessageBox.warning(self, exc.code, str(exc))

        @property
        def input_path(self) -> str:
            return input_path

        @property
        def workspace(self) -> str:
            return workspace

    app = QApplication(sys.argv[:1])
    window = Workbench()
    window.show()
    return int(app.exec())
