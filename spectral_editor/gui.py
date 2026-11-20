from __future__ import annotations

import json
from dataclasses import replace
from pathlib import Path
from typing import Any

from PySide6.QtCore import QTimer, Qt
from PySide6.QtGui import QBrush, QColor, QFont
from PySide6.QtWidgets import (
    QApplication,
    QComboBox,
    QDialog,
    QDialogButtonBox,
    QDockWidget,
    QDoubleSpinBox,
    QFileDialog,
    QFormLayout,
    QHBoxLayout,
    QLabel,
    QMainWindow,
    QMessageBox,
    QPushButton,
    QSplitter,
    QTableWidget,
    QTableWidgetItem,
    QTextEdit,
    QVBoxLayout,
    QWidget,
)

from .color import WHITE_POINTS, max_channel_delta
from .errors import BusinessError, busy
from .library import MaterialLibrary, SnapshotHandle
from .models import (
    IlluminantSpec,
    PendingJob,
    PipelineConfig,
    TransitionRequest,
    VersionRecord,
    thaw_value,
)
from .runtime import RecomputeManager


def _qcolor(values: list[float] | tuple[float, ...]) -> QColor:
    channels = [max(0, min(255, round(float(value) * 255.0))) for value in values[:3]]
    while len(channels) < 3:
        channels.append(0)
    return QColor(channels[0], channels[1], channels[2])


def _values_text(values: list[float] | tuple[float, ...]) -> str:
    return "  ".join(f"{float(value):.6f}" for value in values)


class DiffDialog(QDialog):
    def __init__(self, record: VersionRecord, compare: VersionRecord, parent: QWidget | None = None):
        super().__init__(parent)
        self.setWindowTitle(
            f"Version diff: {record.material_id} v{compare.version} -> v{record.version}"
        )
        self.resize(760, 420)
        layout = QVBoxLayout(self)
        text = QTextEdit()
        text.setReadOnly(True)
        deltas = max_channel_delta(compare.colors, record.colors)
        lines = [
            f"material: {record.material_id}",
            f"versions: {compare.version} -> {record.version}",
            f"max channel deltas: {json.dumps(deltas, sort_keys=True)}",
            "",
        ]
        for target in ("srgb", "display-p3"):
            lines.append(f"{target}:")
            lines.append(f"  old: {_values_text(compare.colors[target])}")
            lines.append(f"  new: {_values_text(record.colors[target])}")
            lines.append("")
        changed: list[str] = []
        if compare.dependencies != record.dependencies:
            changed.append("dependencies")
        if compare.params != record.params:
            changed.append("material params")
        if compare.intermediates != record.intermediates:
            changed.append("pipeline intermediates")
        lines.append("changed: " + (", ".join(changed) if changed else "none"))
        text.setPlainText("\n".join(lines))
        layout.addWidget(text)
        buttons = QDialogButtonBox(QDialogButtonBox.Close)
        buttons.rejected.connect(self.reject)
        buttons.accepted.connect(self.accept)
        layout.addWidget(buttons)


class MainWindow(QMainWindow):
    def __init__(
        self,
        library: MaterialLibrary,
        pending: PendingJob | None = None,
        parent: QWidget | None = None,
    ) -> None:
        super().__init__(parent)
        self.library = library
        self.manager = RecomputeManager(library)
        self.pending = pending
        self._last_job_state = "idle"
        self._selected_material: str | None = None
        self._selected_version: int | None = None
        self.setWindowTitle("Spectral Material Color Management Lab")
        self.resize(1440, 900)
        self._build_ui()
        self._install_controls()
        self._poll_timer = QTimer(self)
        self._poll_timer.setInterval(100)
        self._poll_timer.timeout.connect(self._poll_job)
        self._poll_timer.start()
        self._restore_selection_from_ui()
        if pending is not None:
            self.manager.resume_pending(pending)

    def _build_ui(self) -> None:
        splitter = QSplitter(Qt.Vertical)
        material_panel = QWidget()
        material_layout = QVBoxLayout(material_panel)
        title = QLabel("Published material comparison")
        title.setFont(QFont(title.font().family(), 15, QFont.Bold))
        material_layout.addWidget(title)
        self.status_label = QLabel("")
        self.status_label.setWordWrap(True)
        material_layout.addWidget(self.status_label)
        self.material_table = QTableWidget(0, 6)
        self.material_table.setHorizontalHeaderLabels(
            ["Material", "Version", "sRGB", "sRGB values", "Display-P3", "Display-P3 values"]
        )
        self.material_table.setSelectionBehavior(QTableWidget.SelectRows)
        self.material_table.setSelectionMode(QTableWidget.SingleSelection)
        self.material_table.setEditTriggers(QTableWidget.NoEditTriggers)
        self.material_table.setColumnWidth(0, 220)
        self.material_table.setColumnWidth(2, 80)
        self.material_table.setColumnWidth(3, 250)
        self.material_table.setColumnWidth(4, 80)
        self.material_table.setColumnWidth(5, 250)
        material_layout.addWidget(self.material_table)
        splitter.addWidget(material_panel)

        timeline_panel = QWidget()
        timeline_layout = QVBoxLayout(timeline_panel)
        timeline_title = QLabel("Published version timeline")
        timeline_title.setFont(QFont(timeline_title.font().family(), 14, QFont.Bold))
        timeline_layout.addWidget(timeline_title)
        self.timeline_table = QTableWidget(0, 7)
        self.timeline_table.setHorizontalHeaderLabels(
            [
                "Version",
                "Parent",
                "Changed dependency factors",
                "sRGB",
                "Display-P3",
                "Content hash",
                "Chain hash",
            ]
        )
        self.timeline_table.setSelectionBehavior(QTableWidget.SelectRows)
        self.timeline_table.setSelectionMode(QTableWidget.SingleSelection)
        self.timeline_table.setEditTriggers(QTableWidget.NoEditTriggers)
        self.timeline_table.setColumnWidth(0, 75)
        self.timeline_table.setColumnWidth(1, 75)
        self.timeline_table.setColumnWidth(2, 270)
        self.timeline_table.setColumnWidth(3, 190)
        self.timeline_table.setColumnWidth(4, 190)
        self.timeline_table.setColumnWidth(5, 270)
        self.timeline_table.setColumnWidth(6, 270)
        timeline_layout.addWidget(self.timeline_table)
        splitter.addWidget(timeline_panel)
        splitter.setStretchFactor(0, 3)
        splitter.setStretchFactor(1, 2)
        self.setCentralWidget(splitter)
        self.material_table.itemSelectionChanged.connect(self._material_selection_changed)
        self.timeline_table.itemSelectionChanged.connect(self._timeline_selection_changed)

    def _install_controls(self) -> None:
        dock = QDockWidget("Color pipeline controls", self)
        dock.setAllowedAreas(Qt.LeftDockWidgetArea | Qt.RightDockWidgetArea)
        panel = QWidget()
        layout = QFormLayout(panel)
        self.white_point_combo = QComboBox()
        self.white_point_combo.addItems(["D50", "D55", "D60", "D65", "D75", "E"])
        self.observer_combo = QComboBox()
        self.observer_combo.addItems(["2deg", "10deg"])
        self.adaptation_combo = QComboBox()
        self.adaptation_combo.addItems(["bradford", "von-kries", "none"])
        self.temperature_spin = QDoubleSpinBox()
        self.temperature_spin.setRange(1000.0, 20000.0)
        self.temperature_spin.setDecimals(1)
        self.temperature_spin.setSingleStep(100.0)
        self.temperature_spin.setValue(6504.0)
        layout.addRow("White point", self.white_point_combo)
        layout.addRow("Observer", self.observer_combo)
        layout.addRow("Adaptation", self.adaptation_combo)
        layout.addRow("Illuminant CCT (K)", self.temperature_spin)
        load_button = QPushButton("Load illuminant JSON...")
        load_button.clicked.connect(self._load_illuminant_file)
        layout.addRow(load_button)
        button_row = QHBoxLayout()
        self.apply_button = QPushButton("Apply and publish")
        self.cancel_button = QPushButton("Cancel")
        self.apply_button.clicked.connect(self._apply_pipeline)
        self.cancel_button.clicked.connect(self._cancel_job)
        button_row.addWidget(self.apply_button)
        button_row.addWidget(self.cancel_button)
        layout.addRow(button_row)
        self.diff_button = QPushButton("Compare selected version")
        self.rollback_button = QPushButton("Rollback selected version")
        self.diff_button.clicked.connect(self._show_diff)
        self.rollback_button.clicked.connect(self._rollback_selected)
        layout.addRow(self.diff_button)
        layout.addRow(self.rollback_button)
        dock.setWidget(panel)
        self.addDockWidget(Qt.RightDockWidgetArea, dock)
        self._sync_controls(self.library.current_snapshot.pipeline)

    def _restore_selection_from_ui(self) -> None:
        snapshot = self.library.current_snapshot
        state = dict(snapshot.ui_state)
        selected = state.get("selected_material")
        if not isinstance(selected, str) or selected not in snapshot.histories:
            selected = sorted(snapshot.histories)[0]
        version = state.get("selected_version")
        if not isinstance(version, int):
            version = snapshot.current_record(selected).version
        self._selected_material = selected
        self._selected_version = version
        self._refresh_from_snapshot()

    def _sync_controls(self, config: PipelineConfig) -> None:
        white_index = self.white_point_combo.findText(config.white_point_name)
        self.white_point_combo.setCurrentIndex(max(0, white_index))
        observer_index = self.observer_combo.findText(config.observer)
        self.observer_combo.setCurrentIndex(max(0, observer_index))
        adaptation_index = self.adaptation_combo.findText(config.adaptation)
        self.adaptation_combo.setCurrentIndex(max(0, adaptation_index))
        if config.illuminant.temperature_k:
            self.temperature_spin.setValue(float(config.illuminant.temperature_k))

    def _refresh_from_snapshot(self) -> None:
        with self.library.acquire_snapshot() as handle:
            self._render_snapshot(handle)

    def _render_snapshot(self, handle: SnapshotHandle) -> None:
        snapshot = handle.ensure_valid()
        materials = snapshot.current_materials()
        self.material_table.setRowCount(len(materials))
        selected_row = -1
        for row, material_id in enumerate(sorted(materials)):
            material = materials[material_id]
            colors = material["colors"]
            self.material_table.setItem(row, 0, QTableWidgetItem(material_id))
            self.material_table.setItem(row, 1, QTableWidgetItem(str(material["version"])))
            for target, swatch_column, value_column in (
                ("srgb", 2, 3),
                ("display-p3", 4, 5),
            ):
                values = colors[target]
                swatch = QTableWidgetItem("")
                swatch.setBackground(QBrush(_qcolor(values)))
                self.material_table.setItem(row, swatch_column, swatch)
                self.material_table.setItem(row, value_column, QTableWidgetItem(_values_text(values)))
            if material_id == self._selected_material:
                selected_row = row
        if selected_row < 0 and len(materials):
            selected_row = 0
            self._selected_material = sorted(materials)[0]
        if selected_row >= 0:
            self.material_table.blockSignals(True)
            self.material_table.selectRow(selected_row)
            self.material_table.blockSignals(False)
        self._render_timeline(handle)
        self.status_label.setText(
            f"generation {snapshot.generation} | publication {snapshot.publication_hash[:16]}... | "
            f"published-{len(materials)} materials | read lock max "
            f"{self.library.max_observed_pointer_wait_ms:.3f} ms"
        )

    def _render_timeline(self, handle: SnapshotHandle) -> None:
        snapshot = handle.ensure_valid()
        if self._selected_material not in snapshot.histories:
            return
        records = snapshot.histories[self._selected_material].versions
        self.timeline_table.setRowCount(len(records))
        select_row = 0
        previous: VersionRecord | None = None
        for row, record in enumerate(records):
            changed: list[str] = []
            if previous is None:
                changed = sorted(record.dependencies)
            else:
                changed = sorted(
                    key
                    for key in set(previous.dependencies) | set(record.dependencies)
                    if previous.dependencies.get(key) != record.dependencies.get(key)
                )
            cells = [
                str(record.version),
                "" if record.parent_version is None else str(record.parent_version),
                ", ".join(changed) if changed else "cached/reused",
                _values_text(record.colors["srgb"]),
                _values_text(record.colors["display-p3"]),
                record.content_hash,
                record.chain_hash,
            ]
            for column, text in enumerate(cells):
                item = QTableWidgetItem(text)
                if changed and column == 2:
                    item.setBackground(QBrush(QColor("#ffe6a8")))
                    item.setForeground(QBrush(QColor("#7a4300")))
                    item.setFont(QFont(item.font().family(), item.font().pointSize() + 1, QFont.Bold))
                self.timeline_table.setItem(row, column, item)
            if record.version == self._selected_version:
                select_row = row
            previous = record
        self.timeline_table.blockSignals(True)
        self.timeline_table.selectRow(select_row)
        self.timeline_table.blockSignals(False)
        self._selected_version = records[select_row].version

    def _material_selection_changed(self) -> None:
        rows = self.material_table.selectionModel().selectedRows()
        if not rows:
            return
        item = self.material_table.item(rows[0].row(), 0)
        if item is None:
            return
        self._selected_material = item.text()
        self._selected_version = self.library.current_snapshot.current_record(
            self._selected_material
        ).version
        self._refresh_from_snapshot()
        self._persist_ui_state()

    def _timeline_selection_changed(self) -> None:
        rows = self.timeline_table.selectionModel().selectedRows()
        if not rows:
            return
        item = self.timeline_table.item(rows[0].row(), 0)
        if item is not None:
            self._selected_version = int(item.text())
            self._persist_ui_state()

    def _persist_ui_state(self) -> None:
        if self._selected_material is None:
            return
        try:
            self.library.update_ui_state(
                selected_material=self._selected_material,
                selected_version=self._selected_version or 1,
                timeline_material=self._selected_material,
            )
        except BusinessError as exc:
            self.status_label.setText(str(exc))

    def _current_config_from_controls(self) -> PipelineConfig:
        config = self.library.current_snapshot.pipeline
        white_name = self.white_point_combo.currentText()
        temperature = float(self.temperature_spin.value())
        return replace(
            config,
            observer=self.observer_combo.currentText(),
            adaptation=self.adaptation_combo.currentText(),
            white_point_name=white_name,
            white_point_xyz=WHITE_POINTS[white_name],
            illuminant=IlluminantSpec(
                name=f"Blackbody-{temperature:.1f}K",
                temperature_k=temperature,
                wavelengths_nm=None,
                values=None,
            ),
        )

    def _apply_pipeline(self) -> None:
        try:
            config = self._current_config_from_controls()
            self.manager.submit(
                TransitionRequest(
                    operation="gui_pipeline_update",
                    pipeline=config,
                )
            )
            self.status_label.setText("recompute running in background; old snapshot remains active")
        except BusinessError as exc:
            QMessageBox.warning(self, exc.code, str(exc))

    def _cancel_job(self) -> None:
        if not self.manager.cancel():
            self.status_label.setText("no recompute task is running")
        else:
            self.status_label.setText("cancellation requested at next checkpoint")

    def _poll_job(self) -> None:
        state = self.manager.snapshot()
        if state.state != self._last_job_state:
            if state.state == "published":
                self._refresh_from_snapshot()
                self.status_label.setText(
                    f"published generation "
                    f"{self.library.current_snapshot.generation}"
                )
            elif state.state == "failed" and state.error is not None:
                QMessageBox.warning(self, state.error.code, str(state.error))
            elif state.state == "cancelled":
                self.status_label.setText("recompute cancelled; last published snapshot retained")
            self._last_job_state = state.state
        enabled = not state.active
        self.apply_button.setEnabled(enabled)
        self.rollback_button.setEnabled(enabled)

    def _selected_record(self) -> VersionRecord | None:
        if self._selected_material is None or self._selected_version is None:
            return None
        snapshot = self.library.current_snapshot
        try:
            return snapshot.histories[self._selected_material].get(self._selected_version)
        except KeyError:
            return None

    def _show_diff(self) -> None:
        record = self._selected_record()
        if record is None or self._selected_material is None:
            return
        current = self.library.current_snapshot.current_record(self._selected_material)
        DiffDialog(current, record, self).exec()

    def _rollback_selected(self) -> None:
        record = self._selected_record()
        if record is None or self._selected_material is None:
            return
        current = self.library.current_snapshot.current_record(self._selected_material)
        try:
            self.manager.submit(
                TransitionRequest(
                    operation=f"rollback_{self._selected_material}_to_v{record.version}",
                    rollback={
                        "material_id": self._selected_material,
                        "target_version": record.version,
                        "expected_head_hash": current.content_hash,
                    },
                )
            )
            self.status_label.setText(
                f"rollback to v{record.version} running; history will be preserved"
            )
        except BusinessError as exc:
            QMessageBox.warning(self, exc.code, str(exc))

    def _load_illuminant_file(self) -> None:
        filename, _ = QFileDialog.getOpenFileName(
            self, "Load illuminant spectrum", "", "JSON (*.json)"
        )
        if not filename:
            return
        try:
            document = json.loads(Path(filename).read_text(encoding="utf-8"))
            spec = IlluminantSpec.from_dict(document, "illuminant")
            config = replace(
                self.library.current_snapshot.pipeline,
                illuminant=spec,
            )
            self.manager.submit(
                TransitionRequest(operation="gui_custom_illuminant", pipeline=config)
            )
            self.status_label.setText(f"custom illuminant {spec.name} is recomputing")
        except (OSError, json.JSONDecodeError, BusinessError) as exc:
            code = exc.code if isinstance(exc, BusinessError) else "E_MATERIAL_INVALID"
            QMessageBox.warning(self, code, str(exc))

    def closeEvent(self, event: Any) -> None:
        self._persist_ui_state()
        super().closeEvent(event)


def run_gui(
    snapshot_path: str | Path,
    *,
    screenshot: str | Path | None = None,
    exit_after_ms: int = 0,
) -> int:
    from .library import load_library

    library, pending = load_library(snapshot_path)
    app = QApplication.instance() or QApplication([])
    app.setStyle("Fusion")
    window = MainWindow(library, pending)
    window.show()
    if screenshot is not None:
        target = Path(screenshot)
        target.parent.mkdir(parents=True, exist_ok=True)

        def capture() -> None:
            window.grab().save(str(target))
            app.quit()

        QTimer.singleShot(max(500, exit_after_ms), capture)
    elif exit_after_ms > 0:
        QTimer.singleShot(exit_after_ms, app.quit)
    code = app.exec()
    library.close()
    return code
