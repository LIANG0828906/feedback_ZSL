"""PyQt5 workbench for published crosstalk analysis snapshots."""

from __future__ import annotations

import math
import sys
from pathlib import Path
from typing import Any

from PyQt5.QtCore import Qt, QTimer
from PyQt5.QtGui import QColor
from PyQt5.QtWidgets import (
    QAbstractItemView,
    QApplication,
    QComboBox,
    QDoubleSpinBox,
    QFileDialog,
    QGroupBox,
    QHBoxLayout,
    QHeaderView,
    QLabel,
    QMainWindow,
    QMessageBox,
    QPushButton,
    QSplitter,
    QTabWidget,
    QTableWidget,
    QTableWidgetItem,
    QVBoxLayout,
    QWidget,
)

from .config import load_config, save_config
from .controller import JobEvent, RecomputeController
from .errors import E_BUSY, VenueError
from .models import load_workspace
from .utils import read_json


def _table(headers: list[str]) -> QTableWidget:
    widget = QTableWidget(0, len(headers))
    widget.setHorizontalHeaderLabels(headers)
    widget.setEditTriggers(QAbstractItemView.NoEditTriggers)
    widget.setSelectionBehavior(QAbstractItemView.SelectRows)
    widget.setAlternatingRowColors(True)
    widget.horizontalHeader().setSectionResizeMode(QHeaderView.ResizeToContents)
    widget.horizontalHeader().setStretchLastSection(True)
    return widget


def _item(value: Any) -> QTableWidgetItem:
    if isinstance(value, float):
        text = f"{value:.3f}"
    elif value is None:
        text = ""
    elif isinstance(value, bool):
        text = "yes" if value else "no"
    else:
        text = str(value)
    item = QTableWidgetItem(text)
    return item


class VenueWorkbench(QMainWindow):
    def __init__(self, workspace: str | Path | None = None) -> None:
        super().__init__()
        self.setWindowTitle("Venue Crosstalk Workbench")
        self.resize(1440, 900)
        self.workspace_root: Path | None = None
        self.controller: RecomputeController | None = None
        self.snapshot: dict[str, Any] | None = None

        central = QWidget()
        self.setCentralWidget(central)
        outer = QVBoxLayout(central)

        controls = QHBoxLayout()
        self.open_button = QPushButton("Open workspace")
        self.open_button.clicked.connect(self.choose_workspace)
        self.workspace_label = QLabel("No workspace")
        self.band_selector = QComboBox()
        self.band_selector.setMinimumWidth(130)
        self.band_selector.currentIndexChanged.connect(self.render_snapshot)
        self.target_spin = QDoubleSpinBox()
        self.target_spin.setRange(0.0, 120.0)
        self.target_spin.setDecimals(2)
        self.target_spin.setSuffix(" dB")
        self.start_button = QPushButton("Start recompute")
        self.start_button.clicked.connect(self.start_recompute)
        self.cancel_button = QPushButton("Cancel")
        self.cancel_button.setEnabled(False)
        self.cancel_button.clicked.connect(self.cancel_recompute)
        controls.addWidget(self.open_button)
        controls.addWidget(self.workspace_label, 1)
        controls.addWidget(QLabel("Band"))
        controls.addWidget(self.band_selector)
        controls.addWidget(QLabel("Target suppression"))
        controls.addWidget(self.target_spin)
        controls.addWidget(self.start_button)
        controls.addWidget(self.cancel_button)
        outer.addLayout(controls)

        splitter = QSplitter(Qt.Vertical)
        outer.addWidget(splitter, 1)

        band_group = QGroupBox("Crossover band plan (editable)")
        band_layout = QVBoxLayout(band_group)
        self.band_table = QTableWidget(0, 3)
        self.band_table.setHorizontalHeaderLabels(["Band", "Low Hz", "High Hz"])
        self.band_table.horizontalHeader().setSectionResizeMode(QHeaderView.Stretch)
        self.band_table.setMaximumHeight(190)
        band_layout.addWidget(self.band_table)
        splitter.addWidget(band_group)

        self.tabs = QTabWidget()
        self.matrix_table = _table(["Pickup / Source"])
        self.budget_table = _table(
            [
                "Kind",
                "Pair or pickup",
                "Coupling",
                "Cancel gain",
                "Residual",
                "Achieved dB",
                "Target dB",
                "Gap dB",
                "Met",
            ]
        )
        self.phase_table = _table(
            [
                "Pair",
                "Verdict",
                "Code",
                "Inversion bands",
                "Wraps",
                "Jumps",
                "Group-delay discontinuities",
                "Evidence",
            ]
        )
        self.calibration_table = _table(
            [
                "Pair",
                "Band",
                "Polarity verdict",
                "Cancellation polarity",
                "Gain",
                "Gain dB",
                "Target dB",
                "Achieved dB",
                "Gap dB",
                "Primary atten. dB",
                "Feasible",
                "Constraint order",
            ]
        )
        self.alignment_table = _table(
            [
                "Pair",
                "Arrival sample",
                "Alignment offset",
                "Residual offset",
                "Distance m",
                "Delay samples",
                "Basis",
            ]
        )
        self.tabs.addTab(self.matrix_table, "Routing heatmap")
        self.tabs.addTab(self.budget_table, "Residual budget")
        self.tabs.addTab(self.phase_table, "Phase continuity")
        self.tabs.addTab(self.calibration_table, "Calibration")
        self.tabs.addTab(self.alignment_table, "Geometry alignment")
        splitter.addWidget(self.tabs)
        splitter.setSizes([180, 650])

        self.status = QLabel("Ready")
        self.statusBar().addWidget(self.status, 1)
        self.timer = QTimer(self)
        self.timer.timeout.connect(self.poll_controller)
        self.timer.start(100)
        if workspace:
            self.open_workspace(Path(workspace))

    def choose_workspace(self) -> None:
        selected = QFileDialog.getExistingDirectory(self, "Select workspace")
        if selected:
            self.open_workspace(Path(selected))

    def open_workspace(self, path: Path) -> None:
        try:
            workspace = load_workspace(path)
        except VenueError as exc:
            QMessageBox.critical(self, "Workspace error", str(exc))
            return
        if self.controller is not None:
            self.controller.cancel()
        self.workspace_root = workspace.root
        self.workspace_label.setText(str(workspace.root))
        self.controller = RecomputeController(
            workspace.root,
            cache_root=workspace.root / "cache",
            trace_path=workspace.root / "logs" / "trace.jsonl",
        )
        self._fill_band_plan(workspace.config)
        self.target_spin.setValue(workspace.config.target_suppression_db)
        self._load_last_published()

    def _fill_band_plan(self, config) -> None:
        self.band_selector.blockSignals(True)
        self.band_selector.clear()
        self.band_table.setRowCount(len(config.bands))
        for row, band in enumerate(config.bands):
            self.band_table.setItem(row, 0, QTableWidgetItem(band.name))
            self.band_table.setItem(row, 1, QTableWidgetItem(f"{band.low_hz:g}"))
            self.band_table.setItem(row, 2, QTableWidgetItem(f"{band.high_hz:g}"))
            self.band_selector.addItem(band.name)
        self.band_selector.blockSignals(False)

    def _read_band_plan(self) -> list[tuple[str, float, float]]:
        bands: list[tuple[str, float, float]] = []
        for row in range(self.band_table.rowCount()):
            name_item = self.band_table.item(row, 0)
            low_item = self.band_table.item(row, 1)
            high_item = self.band_table.item(row, 2)
            if not name_item or not low_item or not high_item:
                raise ValueError(f"band row {row + 1} is incomplete")
            bands.append(
                (
                    name_item.text().strip(),
                    float(low_item.text()),
                    float(high_item.text()),
                )
            )
        return bands

    def _current_band(self) -> str | None:
        value = self.band_selector.currentText()
        return value or None

    def _band_section(self, name: str | None) -> dict[str, Any] | None:
        if self.snapshot is None or name is None:
            return None
        for section in self.snapshot.get("bands", []):
            if section.get("spec", {}).get("name") == name:
                return section
        return None

    def _load_last_published(self) -> None:
        if self.workspace_root is None:
            return
        path = self.workspace_root / "publish" / "snapshot.json"
        if not path.exists():
            self.snapshot = None
            self.status.setText("No published result yet")
            return
        try:
            self.snapshot = read_json(path)
        except (OSError, ValueError) as exc:
            QMessageBox.critical(self, "Publish error", f"Cannot read snapshot: {exc}")
            return
        bands = self.snapshot.get("effective_config", {}).get("bands", [])
        names = [band["name"] for band in bands]
        if names and [
            self.band_selector.itemText(index)
            for index in range(self.band_selector.count())
        ] != names:
            self.band_selector.blockSignals(True)
            self.band_selector.clear()
            self.band_selector.addItems(names)
            self.band_selector.blockSignals(False)
        self.render_snapshot()
        self.status.setText(
            f"Published run {self.snapshot.get('run_id', '')[:12]} | "
            f"status={self.snapshot.get('status')}"
        )

    def render_snapshot(self) -> None:
        if self.snapshot is None:
            return
        section = self._band_section(self._current_band())
        self._render_matrix(section)
        self._render_budget(section)
        self._render_phase()
        self._render_calibration(section)
        self._render_alignment()

    def _render_matrix(self, section: dict[str, Any] | None) -> None:
        self.matrix_table.clear()
        if not section:
            self.matrix_table.setRowCount(0)
            return
        layout = section["matrix"]["layout"]
        sources = layout["sources"]
        pickups = layout["pickups"]
        magnitudes = layout["magnitude"]
        phases = layout["phase_deg"]
        self.matrix_table.setRowCount(len(pickups))
        self.matrix_table.setColumnCount(len(sources) + 1)
        self.matrix_table.setHorizontalHeaderLabels(["Pickup / Source", *sources])
        for row, pickup in enumerate(pickups):
            self.matrix_table.setItem(row, 0, _item(pickup))
            for column, _source in enumerate(sources, start=1):
                magnitude = float(magnitudes[row][column - 1])
                phase = float(phases[row][column - 1])
                db = 20.0 * math.log10(max(magnitude, 1e-12))
                item = _item(f"{db:.1f} dB / {phase:.1f} deg")
                normalized = min(1.0, max(0.0, (db + 60.0) / 60.0))
                red = int(40 + 200 * normalized)
                green = int(180 - 120 * normalized)
                blue = 70
                item.setBackground(QColor(red, green, blue))
                item.setForeground(QColor(255, 255, 255))
                item.setToolTip(
                    f"linear magnitude={magnitude:.7g}\nphase={phase:.3f} deg"
                )
                self.matrix_table.setItem(row, column, item)
        self.matrix_table.resizeColumnsToContents()

    def _render_budget(self, section: dict[str, Any] | None) -> None:
        self.budget_table.setRowCount(0)
        if not section:
            return
        budget = section["residual_budget"]
        pair_rows = budget["pairs"]
        pickup_rows = budget["pickups"]
        self.budget_table.setRowCount(len(pair_rows) + len(pickup_rows))
        for row, entry in enumerate(pair_rows):
            values = [
                "pair",
                entry["pair"],
                f"{20 * math.log10(max(entry['coupling_magnitude'], 1e-12)):.2f} dB",
                f"{entry['cancel_gain_linear']:.4f}",
                f"{entry['residual_magnitude']:.6g}",
                entry["achieved_suppression_db"],
                entry["target_suppression_db"],
                entry["gap_db"],
                entry["met"],
            ]
            for column, value in enumerate(values):
                item = _item(value)
                if not entry["met"]:
                    item.setBackground(QColor(120, 30, 30))
                self.budget_table.setItem(row, column, item)
        offset = len(pair_rows)
        for index, entry in enumerate(pickup_rows):
            row = offset + index
            values = [
                "pickup",
                entry["pickup"],
                "",
                "",
                f"{entry['residual_rms']:.6g}",
                entry["relative_suppression_db"],
                entry["target_suppression_db"],
                entry["gap_db"],
                entry["met"],
            ]
            for column, value in enumerate(values):
                item = _item(value)
                if not entry["met"]:
                    item.setBackground(QColor(120, 30, 30))
                self.budget_table.setItem(row, column, item)

    def _render_phase(self) -> None:
        phases = [] if self.snapshot is None else self.snapshot.get("phase_continuity", [])
        self.phase_table.setRowCount(len(phases))
        for row, phase in enumerate(phases):
            wraps = ", ".join(
                "->".join(item["bands"]) for item in phase.get("boundary_wraps", [])
            )
            jumps = ", ".join(
                "->".join(item["bands"]) for item in phase.get("phase_jumps", [])
            )
            delays = ", ".join(
                "->".join(item["bands"])
                for item in phase.get("group_delay_discontinuities", [])
            )
            values = [
                phase["pair"],
                phase["verdict"],
                phase.get("code") or "",
                ",".join(phase.get("inversion_bands", [])),
                wraps,
                jumps,
                delays,
                " | ".join(phase.get("evidence", [])),
            ]
            for column, value in enumerate(values):
                item = _item(value)
                if phase["verdict"] == "unknown":
                    item.setBackground(QColor(130, 105, 20))
                elif phase["verdict"] == "inverted":
                    item.setBackground(QColor(115, 35, 75))
                self.phase_table.setItem(row, column, item)

    def _render_calibration(self, section: dict[str, Any] | None) -> None:
        self.calibration_table.setRowCount(0)
        if not section:
            return
        rows = section.get("calibration", [])
        self.calibration_table.setRowCount(len(rows))
        for row, calibration in enumerate(rows):
            values = [
                calibration["pair"],
                calibration["band"],
                calibration["polarity_verdict"],
                calibration["polarity_setting"],
                f"{calibration['cancel_gain_linear']:.5f}",
                calibration["cancel_gain_db"],
                calibration["target_suppression_db"],
                calibration["achieved_suppression_db"],
                calibration["gap_db"],
                calibration["primary_suppression_db"],
                calibration["feasible"],
                " -> ".join(calibration["constraint_order"]),
            ]
            for column, value in enumerate(values):
                item = _item(value)
                if not calibration["feasible"]:
                    item.setBackground(QColor(120, 30, 30))
                self.calibration_table.setItem(row, column, item)

    def _render_alignment(self) -> None:
        alignments = [] if self.snapshot is None else self.snapshot.get("alignments", [])
        self.alignment_table.setRowCount(len(alignments))
        for row, alignment in enumerate(alignments):
            basis = alignment["distance_basis"]
            values = [
                alignment["pair"],
                alignment["arrival_samples"],
                alignment["alignment_offset_samples"],
                alignment["residual_offset_samples"],
                basis["distance_m"],
                basis["delay_samples"],
                basis["kind"],
            ]
            for column, value in enumerate(values):
                self.alignment_table.setItem(row, column, _item(value))

    def start_recompute(self) -> None:
        if self.workspace_root is None or self.controller is None:
            QMessageBox.warning(self, "No workspace", "Open a workspace first.")
            return
        try:
            bands = self._read_band_plan()
            target = float(self.target_spin.value())
            workspace = load_workspace(self.workspace_root)
            config_path = (
                self.workspace_root
                / str(workspace.manifest.get("config_file", "crossover.json"))
            )
            effective = load_config(config_path).with_bands(bands)
            effective = effective.with_target_suppression(target)
            save_config(config_path, effective)
        except (ValueError, VenueError) as exc:
            QMessageBox.critical(self, "Invalid band plan", str(exc))
            return
        response = self.controller.submit(
            operation="gui-recompute",
            target_suppression_db=target,
            bands=bands,
        )
        if not response.get("ok"):
            code = response.get("code", "ERROR")
            if code == E_BUSY:
                self.status.setText("E_BUSY: recompute already running")
            else:
                self.status.setText(f"{code}: {response.get('message', '')}")
            return
        self.start_button.setEnabled(False)
        self.cancel_button.setEnabled(True)
        self.status.setText(f"Recompute job {response['job_id']} running in background")

    def cancel_recompute(self) -> None:
        if self.controller is None or not self.controller.cancel():
            return
        self.cancel_button.setEnabled(False)
        self.status.setText("Cancellation requested; last published snapshot is preserved")

    def poll_controller(self) -> None:
        if self.controller is None:
            return
        for event in self.controller.poll():
            self._handle_event(event)

    def _handle_event(self, event: JobEvent) -> None:
        if event.event == "completed":
            self.start_button.setEnabled(True)
            self.cancel_button.setEnabled(False)
            self._load_last_published()
            self.status.setText(
                f"Published run {event.payload.get('run_id', '')[:12]} | "
                f"status={event.payload.get('status')}"
            )
        elif event.event in {"cancelled", "failed"}:
            self.start_button.setEnabled(True)
            self.cancel_button.setEnabled(False)
            self._load_last_published()
            self.status.setText(
                f"{event.event}: {event.payload.get('code', '')} "
                f"{event.payload.get('message', '')}; last publish retained"
            )
        elif event.event == "cancel_requested":
            self.status.setText("Cancellation requested")


def main(argv: list[str] | None = None) -> int:
    args = list(sys.argv[1:] if argv is None else argv)
    app = QApplication([sys.argv[0], *args])
    window = VenueWorkbench(args[0] if args else None)
    window.show()
    return app.exec_()


if __name__ == "__main__":
    raise SystemExit(main())
