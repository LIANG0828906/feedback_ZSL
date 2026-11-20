"""PyQt5 example desktop application.

The widget layer only edits component state and displays NumPy results. It
never re-implements normalization, LUT lookup, lighting, or error semantics.
"""

from __future__ import annotations

import argparse
import sys
from pathlib import Path
from typing import Any, Callable

import numpy as np
from PyQt5.QtCore import QThread, Qt, pyqtSignal
from PyQt5.QtGui import QImage, QPixmap
from PyQt5.QtWidgets import (
    QApplication,
    QCheckBox,
    QComboBox,
    QDoubleSpinBox,
    QFileDialog,
    QFormLayout,
    QGroupBox,
    QHBoxLayout,
    QLabel,
    QMainWindow,
    QPushButton,
    QScrollArea,
    QSlider,
    QTableWidget,
    QTableWidgetItem,
    QTextEdit,
    QVBoxLayout,
    QWidget,
)

from .errors import E_BUSY, E_TF_INVALID, VolRenderError
from .render import (
    LightDriftReport,
    ModeSwitchReport,
    PrecisionComparison,
    RenderResult,
    VolumeRenderer,
)
from .session import RenderSession
from .snapshot import load_snapshot
from .transfer import TransferFunction
from .volume import MappingChange, VolumeData


ROOT = Path(__file__).resolve().parents[1]
DEFAULT_VOLUME = ROOT / "samples" / "density_64.vol"
DEFAULT_MASK = ROOT / "samples" / "segmentation_64.vol"

DEFAULT_POINTS = [
    (0.00, 0.00, 0.00, 0.00, 0.00),
    (0.12, 0.01, 0.18, 0.42, 0.95),
    (0.42, 0.16, 0.95, 0.62, 0.18),
    (0.72, 0.58, 1.00, 0.88, 0.74),
    (1.00, 0.94, 1.00, 1.00, 1.00),
]


class TaskThread(QThread):
    succeeded = pyqtSignal(object)
    failed = pyqtSignal(object)

    def __init__(self, action: Callable[[], Any]) -> None:
        super().__init__()
        self.action = action

    def run(self) -> None:
        try:
            self.succeeded.emit(self.action())
        except Exception as exc:  # noqa: BLE001 - propagated to the UI status
            self.failed.emit(exc)


def rgba_to_qimage(rgba8: np.ndarray) -> QImage:
    array = np.ascontiguousarray(rgba8, dtype=np.uint8)
    height, width = array.shape[:2]
    image = QImage(
        array.data,
        width,
        height,
        int(array.strides[0]),
        QImage.Format_RGBA8888_Premultiplied,
    )
    return image.copy()


def heatmap_to_qimage(error_map: np.ndarray, threshold: float) -> QImage:
    ratio = np.clip(np.asarray(error_map, dtype=np.float64) / threshold, 0.0, 1.0)
    red = np.clip(2.0 * ratio, 0.0, 1.0)
    green = np.clip(2.0 - 2.0 * ratio, 0.0, 1.0)
    blue = np.clip(1.0 - ratio, 0.0, 1.0)
    rgb = np.stack((red, green, blue), axis=-1)
    rgba = np.zeros((*ratio.shape, 4), dtype=np.uint8)
    rgba[..., :3] = np.floor(rgb * 255.0 + 0.5).astype(np.uint8)
    rgba[..., 3] = 255
    return rgba_to_qimage(rgba)


class VolumeWindow(QMainWindow):
    def __init__(self, volume_path: Path, mask_path: Path | None = None) -> None:
        super().__init__()
        self.volume = VolumeData.from_file(volume_path)
        self.mask = VolumeData.from_file(mask_path) if mask_path else None
        self.transfer = TransferFunction(DEFAULT_POINTS)
        self.renderer = VolumeRenderer(
            volume=self.volume,
            transfer=self.transfer,
            mode="float",
            step=1.0,
            image_size=(96, 96),
            error_threshold=1.0e-3,
        )
        self.session = RenderSession(self.renderer)
        self.last_result: RenderResult | None = None
        self.task: TaskThread | None = None
        self._success_callback: Callable[[Any], None] | None = None
        self._set_window_title(volume_path.name)
        self._build_ui()
        self._sync_controls()
        self.request_render()

    def _set_window_title(self, volume_name: str) -> None:
        mask_text = " + mask" if self.mask is not None else ""
        self.setWindowTitle(f"NumPy Volume Renderer - {volume_name}{mask_text}")
        self.resize(1180, 760)

    def _build_ui(self) -> None:
        root = QWidget()
        root_layout = QHBoxLayout(root)
        self.display = QLabel("Rendering...")
        self.display.setAlignment(Qt.AlignCenter)
        self.display.setMinimumSize(640, 640)
        self.display.setStyleSheet("background:#101318;color:#d7dde8;")

        side = QWidget()
        side_layout = QVBoxLayout(side)
        side_layout.setContentsMargins(8, 8, 8, 8)

        window_box = QGroupBox("Window / Level")
        window_form = QFormLayout(window_box)
        self.width_slider = QSlider(Qt.Horizontal)
        self.width_slider.setRange(1, 600)
        self.width_value = QLabel()
        width_row = QHBoxLayout()
        width_row.addWidget(self.width_slider)
        width_row.addWidget(self.width_value)
        self.level_slider = QSlider(Qt.Horizontal)
        self.level_slider.setRange(-200, 500)
        self.level_value = QLabel()
        level_row = QHBoxLayout()
        level_row.addWidget(self.level_slider)
        level_row.addWidget(self.level_value)
        window_form.addRow("Width", width_row)
        window_form.addRow("Level", level_row)
        self.width_slider.valueChanged.connect(
            lambda value: self.width_value.setText(str(value))
        )
        self.level_slider.valueChanged.connect(
            lambda value: self.level_value.setText(str(value))
        )
        self.width_slider.sliderReleased.connect(self.request_window_render)
        self.level_slider.sliderReleased.connect(self.request_window_render)
        self.apply_window_button = QPushButton("Apply Window")
        self.apply_window_button.clicked.connect(self.request_window_render)
        window_form.addRow(self.apply_window_button)

        tf_box = QGroupBox("Transfer Function")
        tf_layout = QVBoxLayout(tf_box)
        self.tf_table = QTableWidget(0, 5)
        self.tf_table.setHorizontalHeaderLabels(["x", "opacity", "R", "G", "B"])
        self.tf_table.horizontalHeader().setStretchLastSection(True)
        self.tf_table.verticalHeader().setVisible(False)
        tf_layout.addWidget(self.tf_table)
        tf_buttons = QHBoxLayout()
        self.add_point_button = QPushButton("Add Point")
        self.remove_point_button = QPushButton("Remove Point")
        self.apply_tf_button = QPushButton("Apply TF")
        tf_buttons.addWidget(self.add_point_button)
        tf_buttons.addWidget(self.remove_point_button)
        tf_buttons.addWidget(self.apply_tf_button)
        tf_layout.addLayout(tf_buttons)
        self.add_point_button.clicked.connect(self._add_tf_point)
        self.remove_point_button.clicked.connect(self._remove_tf_point)
        self.apply_tf_button.clicked.connect(self.request_tf_render)

        render_box = QGroupBox("Precision / Sampling")
        render_form = QFormLayout(render_box)
        self.precision_combo = QComboBox()
        self.precision_combo.addItems(["float", "fixed"])
        self.precision_combo.currentTextChanged.connect(self.request_precision_switch)
        self.step_spin = QDoubleSpinBox()
        self.step_spin.setRange(0.1, 4.0)
        self.step_spin.setSingleStep(0.25)
        self.step_spin.setDecimals(2)
        self.threshold_spin = QDoubleSpinBox()
        self.threshold_spin.setRange(1.0e-6, 1.0)
        self.threshold_spin.setDecimals(6)
        self.threshold_spin.setSingleStep(1.0e-4)
        render_form.addRow("Mode", self.precision_combo)
        render_form.addRow("Step", self.step_spin)
        render_form.addRow("Error limit", self.threshold_spin)

        actions_box = QGroupBox("Actions")
        actions_layout = QVBoxLayout(actions_box)
        self.render_button = QPushButton("Render")
        self.compare_button = QPushButton("Compare Precision")
        self.drift_button = QPushButton("Audit Lighting Drift")
        self.heatmap_check = QCheckBox("Show error heat map")
        self.save_button = QPushButton("Save Snapshot...")
        self.load_button = QPushButton("Load Snapshot...")
        actions_layout.addWidget(self.render_button)
        actions_layout.addWidget(self.compare_button)
        actions_layout.addWidget(self.drift_button)
        actions_layout.addWidget(self.heatmap_check)
        actions_layout.addWidget(self.save_button)
        actions_layout.addWidget(self.load_button)
        self.render_button.clicked.connect(self.request_render)
        self.compare_button.clicked.connect(self.request_compare)
        self.drift_button.clicked.connect(self.request_drift_audit)
        self.heatmap_check.stateChanged.connect(self._redisplay)
        self.save_button.clicked.connect(self.request_snapshot_save)
        self.load_button.clicked.connect(self.request_snapshot_load)

        self.status = QTextEdit()
        self.status.setReadOnly(True)
        self.status.setMinimumHeight(150)

        side_layout.addWidget(window_box)
        side_layout.addWidget(tf_box)
        side_layout.addWidget(render_box)
        side_layout.addWidget(actions_box)
        side_layout.addWidget(self.status)
        scroll = QScrollArea()
        scroll.setWidgetResizable(True)
        scroll.setWidget(side)
        scroll.setMinimumWidth(440)
        root_layout.addWidget(self.display, 1)
        root_layout.addWidget(scroll)
        self.setCentralWidget(root)

    def _sync_controls(self) -> None:
        widgets = [
            self.width_slider,
            self.level_slider,
            self.precision_combo,
            self.step_spin,
            self.threshold_spin,
        ]
        previous = [widget.blockSignals(True) for widget in widgets]
        try:
            self.width_slider.setValue(int(round(self.volume.window_width)))
            self.level_slider.setValue(int(round(self.volume.window_level)))
            self.width_value.setText(str(self.width_slider.value()))
            self.level_value.setText(str(self.level_slider.value()))
            self.precision_combo.setCurrentText(self.renderer.mode)
            self.step_spin.setValue(self.renderer.step)
            self.threshold_spin.setValue(self.renderer.error_threshold)
            self._load_tf_table()
        finally:
            for widget, state in zip(widgets, previous):
                widget.blockSignals(state)

    def _load_tf_table(self) -> None:
        points = self.renderer.transfer.points
        self.tf_table.setRowCount(len(points))
        for row, point in enumerate(points):
            values = [point.x, point.opacity, point.r, point.g, point.b]
            for column, value in enumerate(values):
                self.tf_table.setItem(
                    row,
                    column,
                    QTableWidgetItem(f"{value:.6f}"),
                )

    def _add_tf_point(self) -> None:
        row = self.tf_table.rowCount()
        self.tf_table.insertRow(row)
        defaults = [0.5, 0.2, 0.8, 0.8, 0.8]
        for column, value in enumerate(defaults):
            self.tf_table.setItem(row, column, QTableWidgetItem(str(value)))

    def _remove_tf_point(self) -> None:
        row = self.tf_table.currentRow()
        if row >= 0:
            self.tf_table.removeRow(row)

    def _parse_tf_table(self) -> list[tuple[float, float, float, float, float]]:
        points: list[tuple[float, float, float, float, float]] = []
        for row in range(self.tf_table.rowCount()):
            values: list[float] = []
            for column in range(5):
                item = self.tf_table.item(row, column)
                if item is None:
                    raise VolRenderError(
                        E_TF_INVALID,
                        f"transfer function cell ({row + 1}, {column + 1}) is empty",
                    )
                try:
                    values.append(float(item.text()))
                except ValueError as exc:
                    raise VolRenderError(
                        E_TF_INVALID,
                        f"transfer function cell ({row + 1}, {column + 1}) is not numeric",
                    ) from exc
            points.append(tuple(values))  # type: ignore[arg-type]
        return points

    def _apply_sampling_controls(self) -> None:
        self.renderer.set_step(self.step_spin.value())
        self.renderer.set_error_threshold(self.threshold_spin.value())

    def _set_busy(self, busy: bool) -> None:
        widgets = [
            self.render_button,
            self.compare_button,
            self.drift_button,
            self.apply_window_button,
            self.apply_tf_button,
            self.add_point_button,
            self.remove_point_button,
            self.precision_combo,
            self.save_button,
            self.load_button,
            self.width_slider,
            self.level_slider,
        ]
        for widget in widgets:
            widget.setEnabled(not busy)

    def _run_task(
        self,
        action: Callable[[], Any],
        on_success: Callable[[Any], None],
    ) -> None:
        if self.task is not None and self.task.isRunning():
            self._show_error(VolRenderError(E_BUSY, "a render task is already in progress"))
            return
        self._set_busy(True)
        self._success_callback = on_success
        self.task = TaskThread(action)
        self.task.succeeded.connect(self._on_task_succeeded)
        self.task.failed.connect(self._on_task_failed)
        self.task.finished.connect(self._on_task_finished)
        self.task.start()

    def _on_task_succeeded(self, value: Any) -> None:
        if self._success_callback is not None:
            self._success_callback(value)

    def _on_task_failed(self, error: object) -> None:
        self._show_error(error)
        if isinstance(error, VolRenderError) and error.code == E_TF_INVALID:
            self._load_tf_table()
        self.precision_combo.blockSignals(True)
        self.precision_combo.setCurrentText(self.renderer.mode)
        self.precision_combo.blockSignals(False)

    def _on_task_finished(self) -> None:
        self._set_busy(False)
        self._success_callback = None
        if self.task is not None:
            self.task.deleteLater()
            self.task = None

    def _show_error(self, error: object) -> None:
        if isinstance(error, VolRenderError):
            text = f"{error.code}\n{error.message}"
            if error.details:
                text += f"\n\n{error.details}"
        else:
            text = f"{type(error).__name__}: {error}"
        self.status.setPlainText(
            "Render failed; last good framebuffer retained.\n\n" + text
        )

    def _set_result(self, result: RenderResult, prefix: str = "") -> None:
        self.last_result = result
        self._redisplay()
        summary = result.error_summary
        text = (
            f"{prefix}"
            f"mode={result.mode}  hash={result.content_hash[:16]}...\n"
            f"status={summary.status}  converged={summary.converged}\n"
            f"error mean={summary.mean_error:.6g} max={summary.max_error:.6g} "
            f"p95={summary.p95_error:.6g}\n"
            f"untrusted={summary.untrusted_pixels}/{summary.total_pixels}  "
            f"threshold={summary.threshold:.6g}\n"
            f"render={result.elapsed_ms:.1f} ms"
        )
        self.status.setPlainText(text)

    def _redisplay(self) -> None:
        if self.last_result is None:
            return
        if self.heatmap_check.isChecked():
            image = heatmap_to_qimage(
                self.last_result.error_map,
                self.last_result.error_summary.threshold,
            )
        else:
            image = rgba_to_qimage(self.last_result.rgba8)
        pixmap = QPixmap.fromImage(image)
        self.display.setPixmap(
            pixmap.scaled(
                self.display.size() * 0.96,
                Qt.KeepAspectRatio,
                Qt.SmoothTransformation,
            )
        )

    def request_render(self) -> None:
        if self.task is not None and self.task.isRunning():
            self._show_error(VolRenderError(E_BUSY, "a render task is already in progress"))
            return
        self._apply_sampling_controls()
        self._run_task(self.session.submit_render, self._on_render_result)

    def _on_render_result(self, result: RenderResult) -> None:
        self._set_result(result)

    def request_window_render(self) -> None:
        if self.task is not None and self.task.isRunning():
            self._show_error(VolRenderError(E_BUSY, "a render task is already in progress"))
            return
        self._apply_sampling_controls()
        width = float(self.width_slider.value())
        level = float(self.level_slider.value())

        def callback(value: tuple[MappingChange, RenderResult]) -> None:
            change, result = value
            self._set_result(
                result,
                prefix=(
                    f"mapping {change.reason}: revision "
                    f"{change.old_revision}->{change.new_revision}, "
                    f"max|delta|={change.max_abs_diff:.6g}\n"
                ),
            )

        self._run_task(
            lambda: self.session.submit_window_and_render(width, level),
            callback,
        )

    def request_tf_render(self) -> None:
        if self.task is not None and self.task.isRunning():
            self._show_error(VolRenderError(E_BUSY, "a render task is already in progress"))
            return
        self._apply_sampling_controls()
        try:
            points = self._parse_tf_table()
        except VolRenderError as exc:
            self._show_error(exc)
            self._load_tf_table()
            return

        def callback(result: RenderResult) -> None:
            self._set_result(result, prefix="transfer function updated\n")

        self._run_task(
            lambda: self.session.submit_transfer_and_render(points),
            callback,
        )

    def request_precision_switch(self, mode: str) -> None:
        if self.task is not None and self.task.isRunning():
            return
        self._apply_sampling_controls()

        def callback(report: ModeSwitchReport) -> None:
            self.precision_combo.blockSignals(True)
            self.precision_combo.setCurrentText(report.mode)
            self.precision_combo.blockSignals(False)
            comparison = report.comparison
            self._set_result(
                report.result,
                prefix=(
                    f"precision switched\n"
                    f"float/fixed mean diff={comparison.mean_abs_pixel_diff:.4g}, "
                    f"max diff={comparison.max_abs_pixel_diff:.4g}, "
                    f"changed={comparison.changed_pixel_fraction:.2%}\n"
                    f"max propagated error float={comparison.float_max_error:.6g}, "
                    f"fixed={comparison.fixed_max_error:.6g}\n"
                ),
            )

        self._run_task(
            lambda: self.session.submit_precision_switch(mode),
            callback,
        )

    def request_compare(self) -> None:
        if self.task is not None and self.task.isRunning():
            self._show_error(VolRenderError(E_BUSY, "a render task is already in progress"))
            return
        self._apply_sampling_controls()

        def callback(comparison: PrecisionComparison) -> None:
            self.status.setPlainText(
                "float vs fixed\n"
                f"mean pixel diff={comparison.mean_abs_pixel_diff:.6g}\n"
                f"RMS={comparison.rms_pixel_diff:.6g}  "
                f"max={comparison.max_abs_pixel_diff:.6g}\n"
                f"changed fraction={comparison.changed_pixel_fraction:.2%}\n"
                f"max error: float={comparison.float_max_error:.6g}, "
                f"fixed={comparison.fixed_max_error:.6g}\n"
                f"untrusted: float={comparison.float_untrusted_pixels}, "
                f"fixed={comparison.fixed_untrusted_pixels}\n"
                f"transfer semantics unchanged={comparison.semantics_unchanged}"
            )

        self._run_task(self.session.submit_compare_precision, callback)

    def request_drift_audit(self) -> None:
        if self.task is not None and self.task.isRunning():
            self._show_error(VolRenderError(E_BUSY, "a render task is already in progress"))
            return
        self._apply_sampling_controls()

        def callback(report: LightDriftReport) -> None:
            self.status.setPlainText(
                "lighting drift audit\n"
                f"step {report.reference_step} vs {report.fine_step}\n"
                f"max={report.max_abs_diff:.6g}  p95={report.p95_abs_diff:.6g}\n"
                f"mean={report.mean_abs_diff:.6g}  "
                f"tolerance={report.tolerance:.6g}\n"
                f"passed={report.passed}  compared_pixels={report.compared_pixels}"
            )

        self._run_task(
            self.session.submit_lighting_audit,
            callback,
        )

    def request_snapshot_save(self) -> None:
        if self.task is not None and self.task.isRunning():
            self._show_error(VolRenderError(E_BUSY, "a render task is already in progress"))
            return
        path, _ = QFileDialog.getSaveFileName(
            self,
            "Save snapshot",
            str(ROOT / "artifacts" / "render.volsnap.json"),
            "Volume snapshot (*.volsnap.json *.json)",
        )
        if not path:
            return
        self._apply_sampling_controls()
        expected = self.renderer.state_hash()

        def callback(saved: Path) -> None:
            self.status.setPlainText(f"snapshot saved:\n{saved}")

        self._run_task(
            lambda: self.session.submit_snapshot_save(
                path,
                expected_state_hash=expected,
            ),
            callback,
        )

    def request_snapshot_load(self) -> None:
        if self.task is not None and self.task.isRunning():
            self._show_error(VolRenderError(E_BUSY, "a render task is already in progress"))
            return
        path, _ = QFileDialog.getOpenFileName(
            self,
            "Load snapshot",
            str(ROOT / "artifacts"),
            "Volume snapshot (*.volsnap.json *.json)",
        )
        if not path:
            return
        try:
            snapshot = load_snapshot(path)
        except VolRenderError as exc:
            self._show_error(exc)
            return

        def callback(result: RenderResult) -> None:
            self._sync_controls()
            self._set_result(result, prefix=f"snapshot loaded: {path}\n")

        self._run_task(
            lambda: self.session.submit_restore_snapshot(snapshot, rerender=True),
            callback,
        )

    def closeEvent(self, event: Any) -> None:
        if self.task is not None and self.task.isRunning():
            self.task.wait(2000)
        super().closeEvent(event)


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description="NumPy volume renderer desktop demo")
    parser.add_argument("--volume", type=Path, default=DEFAULT_VOLUME)
    parser.add_argument("--mask", type=Path, default=DEFAULT_MASK)
    args, qt_args = parser.parse_known_args(argv)

    app = QApplication.instance() or QApplication([sys.argv[0], *qt_args])
    try:
        window = VolumeWindow(args.volume, args.mask if args.mask.exists() else None)
    except VolRenderError as exc:
        print(str(exc), file=sys.stderr)
        return 2
    window.show()
    return int(app.exec_())


if __name__ == "__main__":
    raise SystemExit(main())
