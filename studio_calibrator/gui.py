from __future__ import annotations

import sys
from pathlib import Path
from typing import Dict, List, Optional

from PyQt5.QtCore import QThread, Qt, pyqtSignal
from PyQt5.QtGui import QColor
from PyQt5.QtWidgets import (
    QAbstractItemView,
    QApplication,
    QComboBox,
    QDoubleSpinBox,
    QFormLayout,
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

from .config import load_config, parse_config
from .engine import AnalysisEngine, RecomputeManager
from .errors import E_BUSY, E_IO, BusinessError
from .models import ScenarioConfig
from .snapshot import SnapshotStore
from .util import load_json


def _item(value: object) -> QTableWidgetItem:
    return QTableWidgetItem(str(value))


def _heat_color(value_db: float) -> QColor:
    normalized = max(0.0, min(1.0, (value_db + 60.0) / 60.0))
    red = int(35 + 210 * normalized)
    green = int(90 + 120 * (1.0 - abs(normalized - 0.5) * 2.0))
    blue = int(180 - 150 * normalized)
    return QColor(red, green, blue)


class RecomputeWorker(QThread):
    completed = pyqtSignal(object)
    failed = pyqtSignal(object)

    def __init__(
        self,
        manager: RecomputeManager,
        config: ScenarioConfig,
        trace_path: Optional[Path],
    ) -> None:
        super().__init__()
        self.manager = manager
        self.config = config
        self.trace_path = trace_path

    def run(self) -> None:
        try:
            state = self.manager.recompute(self.config, self.trace_path)
        except BusinessError as exc:
            self.failed.emit(exc)
        except Exception as exc:
            self.failed.emit(
                BusinessError("E_INTERNAL", f"{type(exc).__name__}: {exc}")
            )
        else:
            self.completed.emit(state)


class WorkbenchWindow(QMainWindow):
    def __init__(
        self,
        manifest_path: Path,
        geometry_path: Path,
        config_path: Path,
        snapshot_dir: Path,
        trace_path: Optional[Path] = None,
    ) -> None:
        super().__init__()
        self.manifest_path = manifest_path
        self.geometry_path = geometry_path
        self.config_path = config_path
        self.trace_path = trace_path
        self.raw_config = load_json(config_path)
        self.config = load_config(config_path)
        self.manager = RecomputeManager(
            AnalysisEngine(manifest_path, geometry_path),
            SnapshotStore(snapshot_dir),
        )
        self.worker: Optional[RecomputeWorker] = None
        self.state = None
        self.setWindowTitle("Studio Crosstalk Calibration Workbench")
        self.resize(1440, 900)
        self._build_ui()
        self._populate_bands()
        self._restore_committed_state()

    def _build_ui(self) -> None:
        splitter = QSplitter(Qt.Horizontal)
        controls = QWidget()
        controls_layout = QVBoxLayout(controls)
        paths = QGroupBox("Measurement")
        path_form = QFormLayout(paths)
        path_form.addRow("Manifest", QLabel(str(self.manifest_path)))
        path_form.addRow("Geometry", QLabel(str(self.geometry_path)))
        path_form.addRow("Layout", QLabel(self.config.measurement_layout))
        controls_layout.addWidget(paths)

        self.band_table = QTableWidget(0, 4)
        self.band_table.setHorizontalHeaderLabels(
            ["Band", "Low Hz", "High Hz", "Transition Hz"]
        )
        self.band_table.horizontalHeader().setSectionResizeMode(
            QHeaderView.Stretch
        )
        self.band_table.setSelectionBehavior(QAbstractItemView.SelectRows)
        self.band_table.setSelectionMode(QAbstractItemView.SingleSelection)
        controls_layout.addWidget(QLabel("Crossover boundaries"))
        controls_layout.addWidget(self.band_table)

        policy = QGroupBox("Calibration target")
        policy_form = QFormLayout(policy)
        self.target_spin = QDoubleSpinBox()
        self.target_spin.setRange(0.1, 120.0)
        self.target_spin.setDecimals(3)
        self.target_spin.setSuffix(" dB")
        self.target_spin.setValue(self.config.target_suppression_db)
        self.max_gain_spin = QDoubleSpinBox()
        self.max_gain_spin.setRange(0.0, 60.0)
        self.max_gain_spin.setDecimals(3)
        self.max_gain_spin.setSuffix(" dB")
        self.max_gain_spin.setValue(self.config.max_cancellation_gain_db)
        policy_form.addRow("Target suppression", self.target_spin)
        policy_form.addRow("Cancellation cap", self.max_gain_spin)
        controls_layout.addWidget(policy)

        buttons = QHBoxLayout()
        self.start_button = QPushButton("Recompute")
        self.cancel_button = QPushButton("Cancel")
        self.cancel_button.setEnabled(False)
        self.start_button.clicked.connect(self.start_recompute)
        self.cancel_button.clicked.connect(self.cancel_recompute)
        buttons.addWidget(self.start_button)
        buttons.addWidget(self.cancel_button)
        controls_layout.addLayout(buttons)
        self.status_label = QLabel("No committed state loaded")
        self.status_label.setWordWrap(True)
        controls_layout.addWidget(self.status_label)
        controls_layout.addStretch(1)
        splitter.addWidget(controls)

        tabs = QWidget()
        tabs_layout = QVBoxLayout(tabs)
        band_row = QHBoxLayout()
        band_row.addWidget(QLabel("Displayed band"))
        self.band_selector = QComboBox()
        self.band_selector.currentIndexChanged.connect(self.refresh_views)
        band_row.addWidget(self.band_selector)
        band_row.addStretch(1)
        tabs_layout.addLayout(band_row)
        self.tabs = QTabWidget()
        self.route_table = QTableWidget()
        self.budget_table = QTableWidget()
        self.phase_table = QTableWidget()
        self.calibration_table = QTableWidget()
        self.tabs.addTab(self.route_table, "Route heatmap")
        self.tabs.addTab(self.budget_table, "Residual budget")
        self.tabs.addTab(self.phase_table, "Phase continuity")
        self.tabs.addTab(self.calibration_table, "Calibration")
        tabs_layout.addWidget(self.tabs)
        splitter.addWidget(tabs)
        splitter.setStretchFactor(0, 0)
        splitter.setStretchFactor(1, 1)
        self.setCentralWidget(splitter)

    def _populate_bands(self) -> None:
        self.band_table.setRowCount(len(self.config.bands))
        self.band_selector.clear()
        for row, band in enumerate(self.config.bands):
            band_item = _item(band.id)
            band_item.setFlags(band_item.flags() & ~Qt.ItemIsEditable)
            self.band_table.setItem(row, 0, band_item)
            self.band_table.setItem(row, 1, _item(band.low_hz))
            self.band_table.setItem(row, 2, _item(band.high_hz))
            self.band_table.setItem(row, 3, _item(band.transition_hz))
            self.band_selector.addItem(band.id)

    def _restore_committed_state(self) -> None:
        try:
            self.manager.restore()
        except BusinessError as exc:
            if exc.code != E_IO:
                self.status_label.setText(str(exc))
            return
        self.state = self.manager.query()
        if self.state is not None:
            self.status_label.setText(
                f"Restored {self.state.status} snapshot "
                f"{self.state.content_hash_value[:16]}"
            )
            self.refresh_views()

    def _config_from_ui(self) -> ScenarioConfig:
        raw = dict(self.raw_config)
        bands: List[Dict[str, object]] = []
        for row in range(self.band_table.rowCount()):
            band_id_item = self.band_table.item(row, 0)
            bands.append(
                {
                    "id": band_id_item.text() if band_id_item is not None else f"B{row}",
                    "low_hz": float(self.band_table.item(row, 1).text()),
                    "high_hz": float(self.band_table.item(row, 2).text()),
                    "transition_hz": float(self.band_table.item(row, 3).text()),
                }
            )
        raw["bands"] = bands
        raw["target_suppression_db"] = self.target_spin.value()
        raw["max_cancellation_gain_db"] = self.max_gain_spin.value()
        return parse_config(raw)

    def start_recompute(self) -> None:
        if self.manager.busy:
            self.status_label.setText(f"{E_BUSY}: recompute already active")
            return
        try:
            config = self._config_from_ui()
        except (BusinessError, ValueError, TypeError) as exc:
            QMessageBox.warning(self, "Invalid configuration", str(exc))
            return
        self.worker = RecomputeWorker(self.manager, config, self.trace_path)
        self.worker.completed.connect(self._on_complete)
        self.worker.failed.connect(self._on_failed)
        self.worker.finished.connect(self._on_worker_finished)
        self.start_button.setEnabled(False)
        self.cancel_button.setEnabled(True)
        self.status_label.setText("Recomputing in background; last committed state remains visible")
        self.worker.start()

    def cancel_recompute(self) -> None:
        if self.manager.cancel():
            self.status_label.setText("Cancellation requested; waiting for the worker boundary")
        self.cancel_button.setEnabled(False)

    def _on_complete(self, state: object) -> None:
        self.state = state
        self.status_label.setText(
            f"Committed {state.status} state, hash {state.content_hash_value[:16]}"
        )
        self.refresh_views()

    def _on_failed(self, error: object) -> None:
        message = str(error)
        if isinstance(error, BusinessError) and error.code == "E_CANCELLED":
            message = "Recompute cancelled; last committed state retained"
        self.status_label.setText(message)

    def _on_worker_finished(self) -> None:
        self.start_button.setEnabled(True)
        self.cancel_button.setEnabled(False)
        self.worker = None

    def refresh_views(self) -> None:
        if self.state is None:
            return
        self._refresh_route()
        self._refresh_budget()
        self._refresh_phase()
        self._refresh_calibration()

    def _selected_band(self) -> Optional[str]:
        return self.band_selector.currentText() or None

    def _refresh_route(self) -> None:
        band_id = self._selected_band()
        route = next(
            (item for item in self.state.route_matrix if item["band"]["id"] == band_id),
            None,
        )
        if route is None:
            self.route_table.clear()
            return
        receivers = sorted(route["matrix"])
        sources = sorted(
            {source for row in route["matrix"].values() for source in row}
        )
        self.route_table.setRowCount(len(receivers))
        self.route_table.setColumnCount(len(sources))
        self.route_table.setHorizontalHeaderLabels(sources)
        self.route_table.setVerticalHeaderLabels(receivers)
        for row, receiver in enumerate(receivers):
            for column, source in enumerate(sources):
                value = route["matrix"][receiver].get(source, {})
                strength = float(value.get("strength_db", -120.0))
                cell = _item(f"{strength:.1f} dB")
                cell.setBackground(_heat_color(strength))
                cell.setForeground(QColor(10, 10, 10))
                self.route_table.setItem(row, column, cell)

    def _refresh_budget(self) -> None:
        band_id = self._selected_band()
        band = next(
            (item for item in self.state.budget if item["band_id"] == band_id),
            None,
        )
        if band is None:
            self.budget_table.clear()
            return
        rows = [entry for entry in band["entries"] if entry["source"] != entry["receiver"]]
        self.budget_table.setRowCount(len(rows))
        self.budget_table.setColumnCount(8)
        self.budget_table.setHorizontalHeaderLabels(
            [
                "Source",
                "Receiver",
                "Coupling dB",
                "Required gain dB",
                "Achieved dB",
                "Target dB",
                "Status",
                "Reason",
            ]
        )
        for row, entry in enumerate(rows):
            values = [
                entry["source"],
                entry["receiver"],
                f"{entry['coupling_db']:.3f}",
                f"{entry['required_gain_db']:.3f}",
                f"{entry['achievable_suppression_db']:.3f}",
                f"{entry['target_suppression_db']:.3f}",
                entry["status"],
                entry["reason"],
            ]
            for column, value in enumerate(values):
                cell = _item(value)
                if entry["status"] == "short":
                    cell.setBackground(QColor(255, 205, 205))
                self.budget_table.setItem(row, column, cell)
        self.budget_table.resizeColumnsToContents()

    def _refresh_phase(self) -> None:
        findings = list(self.state.phase_findings)
        self.phase_table.setRowCount(len(findings))
        self.phase_table.setColumnCount(9)
        self.phase_table.setHorizontalHeaderLabels(
            [
                "Kind",
                "Source",
                "Receiver",
                "Band from",
                "Band to",
                "Wrapped delta",
                "Raw delta",
                "Confidence",
                "Candidates",
            ]
        )
        for row, finding in enumerate(findings):
            values = [
                finding["kind"],
                finding["source"],
                finding["receiver"],
                finding["band_from"],
                finding["band_to"] or "",
                ""
                if finding["wrapped_delta_deg"] is None
                else f"{finding['wrapped_delta_deg']:.2f}",
                ""
                if finding["raw_delta_deg"] is None
                else f"{finding['raw_delta_deg']:.2f}",
                f"{finding['confidence']:.3f}",
                ", ".join(finding["candidate_causes"]),
            ]
            for column, value in enumerate(values):
                cell = _item(value)
                if finding["kind"] == "polarity_flip_candidate":
                    cell.setBackground(QColor(255, 225, 160))
                elif finding["kind"] == "boundary_phase_wrap":
                    cell.setBackground(QColor(210, 235, 255))
                self.phase_table.setItem(row, column, cell)
        self.phase_table.resizeColumnsToContents()

    def _refresh_calibration(self) -> None:
        settings = list(self.state.calibration["settings"])
        self.calibration_table.setRowCount(len(settings))
        self.calibration_table.setColumnCount(10)
        self.calibration_table.setHorizontalHeaderLabels(
            [
                "Band",
                "Source",
                "Receiver",
                "Enabled",
                "Gain dB",
                "Polarity",
                "Target dB",
                "Achieved dB",
                "Feasible",
                "Reason",
            ]
        )
        for row, setting in enumerate(settings):
            values = [
                setting["band_id"],
                setting["source"],
                setting["receiver"],
                setting["enabled"],
                f"{setting['gain_db']:.3f}",
                setting["polarity"],
                f"{setting['target_suppression_db']:.3f}",
                f"{setting['achieved_suppression_db']:.3f}",
                setting["feasible"],
                setting["reason"],
            ]
            for column, value in enumerate(values):
                cell = _item(value)
                if not setting["feasible"]:
                    cell.setBackground(QColor(255, 205, 205))
                self.calibration_table.setItem(row, column, cell)
        self.calibration_table.resizeColumnsToContents()

    def closeEvent(self, event) -> None:
        if self.manager.busy:
            self.manager.cancel()
        if self.worker is not None:
            self.worker.wait(10000)
        event.accept()


def launch(
    manifest: Path,
    geometry: Path,
    config: Path,
    snapshot_dir: Path,
    trace_path: Optional[Path],
) -> int:
    app = QApplication.instance() or QApplication(sys.argv)
    window = WorkbenchWindow(
        manifest_path=manifest,
        geometry_path=geometry,
        config_path=config,
        snapshot_dir=snapshot_dir,
        trace_path=trace_path,
    )
    window.show()
    return int(app.exec_())


def main(argv: Optional[List[str]] = None) -> int:
    import argparse

    parser = argparse.ArgumentParser(description="Studio calibration workbench")
    parser.add_argument("--manifest", required=True, type=Path)
    parser.add_argument("--geometry", required=True, type=Path)
    parser.add_argument("--config", required=True, type=Path)
    parser.add_argument("--snapshot-dir", required=True, type=Path)
    parser.add_argument("--trace", type=Path)
    args = parser.parse_args(argv)
    return launch(
        args.manifest,
        args.geometry,
        args.config,
        args.snapshot_dir,
        args.trace,
    )


if __name__ == "__main__":
    raise SystemExit(main())
