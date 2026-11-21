"""Optional PySide6 desktop workbench."""

from __future__ import annotations

import sys
from pathlib import Path
from typing import Any

from .models import (
    CompensationConfig,
    MarkerConfig,
    PipelineConfig,
    SegmentationConfig,
)
from .pipeline import SyncPipeline, export_result
from .resample import compare_interpolation_modes
from .wavio import read_audio


def _pyside() -> tuple[Any, Any, Any, Any, Any, Any]:
    try:
        from PySide6 import QtCore, QtGui, QtWidgets
    except ImportError as exc:
        raise SystemExit(
            "PySide6 is not installed; install it with: pip install PySide6"
        ) from exc
    return QtCore, QtGui, QtWidgets, QtCore.Qt, QtWidgets, QtGui


def create_window() -> Any:
    QtCore, QtGui, QtWidgets, Qt, _, _ = _pyside()

    class WorkbenchWindow(QtWidgets.QMainWindow):
        def __init__(self) -> None:
            super().__init__()
            self.setWindowTitle("mcSync Workbench")
            self.resize(1180, 760)
            self.audio = None
            self.result = None
            self.input_path: Path | None = None
            self.output_directory: Path | None = None
            self._build()

        def _build(self) -> None:
            central = QtWidgets.QWidget()
            layout = QtWidgets.QVBoxLayout(central)
            toolbar = QtWidgets.QHBoxLayout()
            self.open_button = QtWidgets.QPushButton("Open WAV / channel dir")
            self.analyze_button = QtWidgets.QPushButton("Analyze")
            self.export_button = QtWidgets.QPushButton("Export v3")
            self.compare_button = QtWidgets.QPushButton("Compare modes")
            self.output_button = QtWidgets.QPushButton("Output directory")
            for button in (
                self.open_button,
                self.analyze_button,
                self.export_button,
                self.compare_button,
                self.output_button,
            ):
                toolbar.addWidget(button)
            toolbar.addStretch(1)
            layout.addLayout(toolbar)
            form = QtWidgets.QHBoxLayout()
            self.reference = QtWidgets.QSpinBox()
            self.reference.setRange(0, 63)
            self.segment_seconds = QtWidgets.QDoubleSpinBox()
            self.segment_seconds.setRange(0.1, 600.0)
            self.segment_seconds.setValue(1.0)
            self.mode = QtWidgets.QComboBox()
            self.mode.addItems(["windowed_sinc", "linear"])
            self.tolerance = QtWidgets.QDoubleSpinBox()
            self.tolerance.setRange(0.01, 1000.0)
            self.tolerance.setValue(2.0)
            self.export_version = QtWidgets.QComboBox()
            self.export_version.addItems(["v3", "v2", "v1"])
            form.addWidget(QtWidgets.QLabel("Reference"))
            form.addWidget(self.reference)
            form.addWidget(QtWidgets.QLabel("Segment seconds"))
            form.addWidget(self.segment_seconds)
            form.addWidget(QtWidgets.QLabel("Interpolation"))
            form.addWidget(self.mode)
            form.addWidget(QtWidgets.QLabel("Tolerance samples"))
            form.addWidget(self.tolerance)
            form.addWidget(QtWidgets.QLabel("Export version"))
            form.addWidget(self.export_version)
            form.addStretch(1)
            layout.addLayout(form)
            self.table = QtWidgets.QTableWidget(0, 8)
            self.table.setHorizontalHeaderLabels(
                [
                    "Channel",
                    "Conclusion",
                    "Confidence",
                    "Drift ppm",
                    "Residual RMS",
                    "Residual max",
                    "Within tolerance",
                    "Missing markers",
                ]
            )
            self.table.horizontalHeader().setStretchLastSection(True)
            layout.addWidget(self.table, 3)
            self.log = QtWidgets.QPlainTextEdit()
            self.log.setReadOnly(True)
            layout.addWidget(self.log, 2)
            self.setCentralWidget(central)
            self.open_button.clicked.connect(self.open_audio)
            self.analyze_button.clicked.connect(self.analyze)
            self.export_button.clicked.connect(self.export)
            self.compare_button.clicked.connect(self.compare_modes)
            self.output_button.clicked.connect(self.choose_output)
            self.statusBar().showMessage("Ready")

        def choose_output(self) -> None:
            directory = QtWidgets.QFileDialog.getExistingDirectory(
                self, "Output directory"
            )
            if directory:
                self.output_directory = Path(directory)

        def open_audio(self) -> None:
            path, _ = QtWidgets.QFileDialog.getOpenFileName(
                self, "Open multichannel WAV", "", "WAV files (*.wav)"
            )
            if not path:
                return
            try:
                self.audio = read_audio(path)
            except Exception as exc:
                QtWidgets.QMessageBox.critical(self, "Open failed", str(exc))
                return
            self.input_path = Path(path)
            self.statusBar().showMessage(
                f"{self.audio.channel_count} channels, "
                f"{self.audio.duration_seconds:.2f} s, {self.audio.sample_rate} Hz"
            )

        def _config(self) -> PipelineConfig:
            return PipelineConfig(
                reference_channel=self.reference.value(),
                segmentation=SegmentationConfig(
                    segment_length_samples=int(
                        round(
                            self.segment_seconds.value()
                            * self.audio.sample_rate
                        )
                    )
                ),
                compensation=CompensationConfig(
                    mode=self.mode.currentText(),
                    tolerance_samples=self.tolerance.value(),
                ),
            )

        def analyze(self) -> None:
            if self.audio is None:
                QtWidgets.QMessageBox.information(
                    self, "No input", "Open a WAV file first."
                )
                return
            self.statusBar().showMessage("Analyzing...")
            QtWidgets.QApplication.setOverrideCursor(Qt.WaitCursor)
            try:
                self.result = SyncPipeline(self._config()).analyze(self.audio)
            except Exception as exc:
                QtWidgets.QApplication.restoreOverrideCursor()
                QtWidgets.QMessageBox.critical(self, "Analysis failed", str(exc))
                return
            QtWidgets.QApplication.restoreOverrideCursor()
            self._populate()
            self.statusBar().showMessage(
                f"Revision ready: {self.result.parameter_version}"
            )

        def _populate(self) -> None:
            report = self.result.report
            diagnoses = {item.channel: item for item in report.diagnoses}
            residuals = {
                item.channel: item
                for item in (
                    report.residual_summary.channels
                    if report.residual_summary
                    else ()
                )
            }
            missing: dict[int, list[int]] = {}
            for issue in self.result.issues:
                if issue.marker_index is not None and issue.channel is not None:
                    missing.setdefault(issue.channel, []).append(issue.marker_index)
            self.table.setRowCount(len(report.drift_fits))
            for row, fit in enumerate(report.drift_fits):
                diagnosis = diagnoses.get(fit.channel)
                residual = residuals.get(fit.channel)
                values = [
                    str(fit.channel),
                    diagnosis.conclusion if diagnosis else "n/a",
                    f"{diagnosis.confidence:.3f}" if diagnosis else "n/a",
                    f"{fit.drift_ppm:.3f}",
                    (
                        f"{residual.residual_rms_samples:.3f}"
                        if residual
                        else "n/a"
                    ),
                    (
                        f"{residual.residual_max_samples:.3f}"
                        if residual
                        else "n/a"
                    ),
                    (
                        str(residual.within_tolerance)
                        if residual
                        else "n/a"
                    ),
                    ",".join(str(value) for value in missing.get(fit.channel, [])),
                ]
                for column, value in enumerate(values):
                    self.table.setItem(
                        row, column, QtWidgets.QTableWidgetItem(value)
                    )
            self.log.setPlainText(
                "\n".join(
                    f"{item['stage']}: "
                    + ", ".join(
                        f"{key}={value}"
                        for key, value in item.items()
                        if key != "stage"
                    )
                    for item in self.result.trace
                )
            )

        def export(self) -> None:
            if self.result is None:
                return
            directory = self.output_directory
            if directory is None:
                selected = QtWidgets.QFileDialog.getExistingDirectory(
                    self, "Export directory"
                )
                if not selected:
                    return
                directory = Path(selected)
                self.output_directory = directory
            try:
                version = self.export_version.currentText()
                exported = export_result(
                    self.result,
                    directory / "compensated.wav",
                    directory / f"report_{version}.json",
                    target_version=version,
                )
            except Exception as exc:
                QtWidgets.QMessageBox.critical(self, "Export failed", str(exc))
                return
            self.log.appendPlainText(
                f"Exported {exported.wav_path}\n{exported.report_path}"
            )

        def compare_modes(self) -> None:
            if self.result is None:
                return
            directory = self.output_directory
            if directory is None:
                selected = QtWidgets.QFileDialog.getExistingDirectory(
                    self, "Comparison output directory"
                )
                if not selected:
                    return
                directory = Path(selected)
                self.output_directory = directory
            comparison = compare_interpolation_modes(
                self.audio,
                self.result.report.drift_fits,
                reference_channel=self._config().reference_channel,
                marker_config=self._config().marker,
                tolerance_samples=self.tolerance.value(),
            )
            path = directory / "interpolation_comparison.json"
            from .atomic import atomic_write_json

            atomic_write_json(path, comparison)
            self.log.appendPlainText(f"Comparison written to {path}")

    return WorkbenchWindow()


def main() -> int:
    QtCore, _, QtWidgets, _, _, _ = _pyside()
    app = QtWidgets.QApplication.instance() or QtWidgets.QApplication(sys.argv)
    window = create_window()
    window.show()
    return int(app.exec())


if __name__ == "__main__":
    raise SystemExit(main())

