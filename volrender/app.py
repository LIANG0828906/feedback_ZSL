"""PyQt5 desktop example for hash-bound interactive preview."""

from __future__ import annotations

import argparse
import sys
from pathlib import Path
from typing import Any

import numpy as np
from PyQt5.QtCore import Qt, QTimer
from PyQt5.QtGui import QColor, QImage, QPixmap
from PyQt5.QtWidgets import (
    QApplication,
    QCheckBox,
    QDoubleSpinBox,
    QFileDialog,
    QFormLayout,
    QGroupBox,
    QHBoxLayout,
    QHeaderView,
    QLabel,
    QMainWindow,
    QMessageBox,
    QPushButton,
    QScrollArea,
    QSlider,
    QTableWidget,
    QTableWidgetItem,
    QVBoxLayout,
    QWidget,
)

from .buffers import BufferStore
from .errors import VolumeRenderError
from .gl_probe import probe_opengl
from .params import RenderParameters
from .pipeline import generate_project_assets, project_directories
from .preview import PreviewController
from .renderer import RenderCancelled, RenderResult
from .snapshot import create_snapshot, load_snapshot, save_snapshot
from .synthetic import baseline_parameters
from .tasks import OffscreenRenderService, RenderJob
from .trace import DeterministicTrace
from .transfer import ControlPoint, TransferFunction, validate_control_points
from .volume import VolumeData, load_volume


def _hash_short(value: str | None) -> str:
    return value[:12] if value else "-"


class VolumeRenderWindow(QMainWindow):
    def __init__(self, root: Path, size: int = 64, *, smoke_test: bool = False) -> None:
        super().__init__()
        self.root = root
        self.size = int(size)
        self.directories = project_directories(root)
        self.volume = self._ensure_volume()
        self.viewport = (256, 256)
        self.buffer_store = BufferStore(self.directories["buffers"])
        self.service = OffscreenRenderService(buffer_store=self.buffer_store, max_workers=2, batch_size=8)
        self.volume_key = self.volume.content_sha256
        self.preview = PreviewController()
        self.active_job: RenderJob | None = None
        self.active_params: RenderParameters | None = None
        self._populating = False
        self._smoke_test = smoke_test
        self.snapshot_path = self.directories["snapshots"] / f"gui_latest_{self.size}.json"
        self.trace = DeterministicTrace(self.directories["traces"] / f"gui_{self.size}.jsonl", reset=True)
        self.initial_params = baseline_parameters(self.volume, viewport=self.viewport)
        self.transfer = TransferFunction(self.initial_params.control_points)
        self.setWindowTitle(f"VSRender Hash-Bound Preview - {self.size}^3")
        self.resize(1120, 720)
        self._build_ui()
        self._set_ui_from_params(self.initial_params)
        self._show_store_frontend()
        self._load_initial_frame()
        if smoke_test:
            QTimer.singleShot(8000, self.close)

    def _ensure_volume(self) -> VolumeData:
        path = self.directories["samples"] / f"synthetic_{self.size}x{self.size}x{self.size}.vol"
        if not path.is_file():
            generate_project_assets(self.root, sizes=(self.size,))
        return load_volume(path)

    def _build_ui(self) -> None:
        central = QWidget()
        root_layout = QHBoxLayout(central)
        image_column = QVBoxLayout()
        self.image_label = QLabel("Waiting for a committed offscreen frame...")
        self.image_label.setAlignment(Qt.AlignCenter)
        self.image_label.setMinimumSize(540, 540)
        self.image_label.setStyleSheet("background:#080b10;color:#9fb0c6;border:1px solid #283548;")
        self.commit_label = QLabel("DISPLAYED COMMITTED: -")
        self.commit_label.setWordWrap(True)
        self.commit_label.setTextInteractionFlags(Qt.TextSelectableByMouse)
        self.pending_label = QLabel("PENDING (NOT DISPLAYED): none")
        self.pending_label.setWordWrap(True)
        self.pending_label.setStyleSheet("color:#e0a33b;")
        self.status_label = QLabel("")
        self.status_label.setWordWrap(True)
        image_column.addWidget(self.image_label, 1)
        image_column.addWidget(self.commit_label)
        image_column.addWidget(self.pending_label)
        image_column.addWidget(self.status_label)
        root_layout.addLayout(image_column, 3)

        scroll = QScrollArea()
        scroll.setWidgetResizable(True)
        scroll.setFixedWidth(430)
        panel = QWidget()
        panel_layout = QVBoxLayout(panel)

        window_group = QGroupBox("Window (drag updates pending only)")
        window_layout = QFormLayout(window_group)
        data_min = int(np.floor(float(self.volume.density.min()))) - 500
        data_max = int(np.ceil(float(self.volume.density.max()))) + 500
        self.wl_slider = QSlider(Qt.Horizontal)
        self.wl_slider.setRange(data_min, data_max)
        self.wl_slider.setValue(int(round(self.initial_params.window_center)))
        self.wl_value = QLabel("")
        self.ww_slider = QSlider(Qt.Horizontal)
        self.ww_slider.setRange(1, max(2, (data_max - data_min) * 2))
        self.ww_slider.setValue(int(round(self.initial_params.window_width)))
        self.ww_value = QLabel("")
        window_layout.addRow("WL", self.wl_slider)
        window_layout.addRow("", self.wl_value)
        window_layout.addRow("WW", self.ww_slider)
        window_layout.addRow("", self.ww_value)
        self.wl_slider.valueChanged.connect(self._window_edited)
        self.ww_slider.valueChanged.connect(self._window_edited)
        panel_layout.addWidget(window_group)

        sampling_group = QGroupBox("Sampling / viewport")
        sampling_layout = QFormLayout(sampling_group)
        self.step_spin = QDoubleSpinBox()
        self.step_spin.setDecimals(4)
        self.step_spin.setRange(0.002, 0.5)
        self.step_spin.setSingleStep(0.002)
        self.step_spin.setValue(self.initial_params.step_size)
        self.mask_checkbox = QCheckBox("Overlay segmentation mask")
        self.mask_checkbox.setChecked(self.initial_params.mask_enabled)
        self.step_spin.valueChanged.connect(self._window_edited)
        self.mask_checkbox.toggled.connect(self._window_edited)
        sampling_layout.addRow("Sample step", self.step_spin)
        sampling_layout.addRow("", self.mask_checkbox)
        sampling_layout.addRow("Viewport", QLabel(f"{self.viewport[0]} x {self.viewport[1]} (fixed)"))
        panel_layout.addWidget(sampling_group)

        tf_group = QGroupBox("Transfer function control points")
        tf_layout = QVBoxLayout(tf_group)
        self.tf_table = QTableWidget(0, 5)
        self.tf_table.setHorizontalHeaderLabels(["pos", "alpha", "R", "G", "B"])
        self.tf_table.horizontalHeader().setSectionResizeMode(QHeaderView.Stretch)
        self.tf_table.verticalHeader().setVisible(False)
        self.tf_table.itemChanged.connect(self._window_edited)
        tf_layout.addWidget(self.tf_table)
        tf_buttons = QHBoxLayout()
        add_point = QPushButton("Add point")
        remove_point = QPushButton("Remove point")
        add_point.clicked.connect(self._add_control_point)
        remove_point.clicked.connect(self._remove_control_point)
        tf_buttons.addWidget(add_point)
        tf_buttons.addWidget(remove_point)
        tf_layout.addLayout(tf_buttons)
        panel_layout.addWidget(tf_group)

        action_group = QGroupBox("Commit / snapshots")
        action_layout = QVBoxLayout(action_group)
        action_row = QHBoxLayout()
        self.commit_button = QPushButton("Commit && render")
        self.cancel_button = QPushButton("Cancel render")
        self.commit_button.clicked.connect(self._commit)
        self.cancel_button.clicked.connect(self._cancel_active)
        action_row.addWidget(self.commit_button)
        action_row.addWidget(self.cancel_button)
        action_layout.addLayout(action_row)
        snapshot_row = QHBoxLayout()
        save_button = QPushButton("Save snapshot")
        load_button = QPushButton("Load snapshot")
        save_button.clicked.connect(self._save_snapshot_dialog)
        load_button.clicked.connect(self._load_snapshot_dialog)
        snapshot_row.addWidget(save_button)
        snapshot_row.addWidget(load_button)
        action_layout.addLayout(snapshot_row)
        panel_layout.addWidget(action_group)

        gl_status = probe_opengl()
        if gl_status.get("available"):
            gl_text = (
                f"PyOpenGL {gl_status['pyopengl_version']} / context OK\n"
                f"{gl_status['renderer']}"
            )
        else:
            gl_text = (
                f"PyOpenGL {gl_status.get('pyopengl_version', '-')}; context unavailable\n"
                "Deterministic CPU offscreen path remains active."
            )
        gl_label = QLabel(gl_text)
        gl_label.setWordWrap(True)
        gl_label.setStyleSheet("color:#8094aa;")
        panel_layout.addWidget(gl_label)
        panel_layout.addStretch(1)
        scroll.setWidget(panel)
        root_layout.addWidget(scroll, 2)
        self.setCentralWidget(central)

    def _show_store_frontend(self) -> None:
        opengl = probe_opengl()
        self.trace.record(
            "app_started",
            volume_hash=self.volume.content_sha256,
            viewport=list(self.viewport),
            pyopengl_available=bool(opengl.get("available")),
            renderer_backend="numpy-cpu-deterministic",
        )

    def _load_initial_frame(self) -> None:
        try:
            stored = self.buffer_store.load_verified(
                self.initial_params.parameter_hash,
                expected_parameter_hash=self.initial_params.parameter_hash,
            )
        except VolumeRenderError:
            self._start_render(self.initial_params, reason="initial_render")
            return
        self.preview.commit(self.initial_params, stored.result)
        self.trace.record_render("committed_preview_loaded", self.initial_params, stored.result)
        self._paint_committed()

    def _window_edited(self, *_args: Any) -> None:
        if self._populating:
            return
        self.wl_value.setText(f"{self.wl_slider.value()}")
        self.ww_value.setText(f"{self.ww_slider.value()}")
        self._refresh_pending()

    def _control_points_from_table(self) -> tuple[ControlPoint, ...]:
        points: list[ControlPoint] = []
        for row in range(self.tf_table.rowCount()):
            values: list[float] = []
            for column in range(5):
                item = self.tf_table.item(row, column)
                if item is None:
                    raise VolumeRenderError(
                        "E_TF_INVALID",
                        "control point cell is empty",
                        field=f"control_points[{row}]",
                    )
                values.append(float(item.text()))
            points.append(ControlPoint(values[0], values[1], (values[2], values[3], values[4])))
        return validate_control_points(points)

    def _build_params_from_ui(self) -> RenderParameters | None:
        try:
            points = self._control_points_from_table()
            mask_enabled = self.mask_checkbox.isChecked()
            return RenderParameters(
                volume_hash=self.volume.content_sha256,
                window_center=float(self.wl_slider.value()),
                window_width=float(self.ww_slider.value()),
                control_points=points,
                step_size=float(self.step_spin.value()),
                view_matrix=self.initial_params.view_matrix,
                viewport=self.viewport,
                mask_enabled=mask_enabled,
                mask_hash=self.volume.mask_sha256 if mask_enabled else "",
            )
        except (ValueError, VolumeRenderError) as exc:
            if isinstance(exc, VolumeRenderError):
                self.status_label.setText(f"Invalid pending parameters: {exc}")
            else:
                self.status_label.setText(f"Invalid pending parameters: {exc}")
            self.pending_label.setText("PENDING (NOT DISPLAYED): invalid parameters")
            return None

    def _refresh_pending(self) -> None:
        pending = self._build_params_from_ui()
        if pending is None:
            self.preview.set_pending(None)
            return
        self.preview.set_pending(pending)
        self.pending_label.setText(
            "PENDING (NOT DISPLAYED): "
            f"{pending.parameter_hash}  WL={pending.window_center:g} WW={pending.window_width:g} "
            f"step={pending.step_size:g} mask={pending.mask_enabled}"
        )
        displayed = self.preview.displayed_parameter_hash
        if displayed == pending.parameter_hash:
            self.pending_label.setText("PENDING: matches the displayed committed parameter group")

    def _paint_committed(self) -> None:
        frame = self.preview.frame
        if frame is None:
            return
        self.preview.validate()
        height, width = frame.result.color.shape[:2]
        payload = frame.result.color.tobytes(order="C")
        image = QImage(payload, width, height, width * 4, QImage.Format_RGBA8888).copy()
        pixmap = QPixmap.fromImage(image).scaled(
            self.image_label.width(),
            self.image_label.height(),
            Qt.KeepAspectRatio,
            Qt.SmoothTransformation,
        )
        self.image_label.setPixmap(pixmap)
        params = frame.params
        self.commit_label.setText(
            "DISPLAYED COMMITTED: "
            f"{params.parameter_hash} | buffer {frame.result.buffer_hash} | "
            f"WL={params.window_center:g} WW={params.window_width:g} "
            f"step={params.step_size:g} mask={params.mask_enabled} "
            f"viewport={params.viewport[0]}x{params.viewport[1]}"
        )
        self.pending_label.setText("PENDING (NOT DISPLAYED): none")
        self.status_label.setText(
            "The image, numeric readout and hashes above all come from one committed parameter group."
        )

    def _start_render(self, params: RenderParameters, *, reason: str) -> None:
        if self.active_job is not None:
            self.status_label.setText("E_BUSY: a render is already active for this volume.")
            return
        try:
            job = self.service.submit(
                self.volume_key,
                self.volume,
                params,
                snapshot_path=self.snapshot_path,
            )
        except VolumeRenderError as exc:
            self.status_label.setText(str(exc))
            return
        self.active_job = job
        self.active_params = params
        self.preview.set_pending(params)
        self.status_label.setText(
            f"Rendering {reason} for pending hash {_hash_short(params.parameter_hash)}. "
            "Displayed frame remains the last committed one."
        )
        self.trace.record(
            "render_submitted",
            reason=reason,
            parameter_hash=params.parameter_hash,
            displayed_parameter_hash=self.preview.displayed_parameter_hash,
            displayed_buffer_hash=self.preview.displayed_buffer_hash,
        )
        QTimer.singleShot(80, self._poll_render)

    def _poll_render(self) -> None:
        job = self.active_job
        if job is None:
            return
        if not job.future.done():
            QTimer.singleShot(80, self._poll_render)
            return
        params = self.active_params
        self.active_job = None
        self.active_params = None
        try:
            result = job.future.result()
            assert params is not None
            self.preview.commit(params, result)
            self.trace.record_render("preview_committed", params, result)
            self._paint_committed()
            self._refresh_pending()
        except RenderCancelled:
            self.status_label.setText(
                "Render cancelled at a checkpoint. The last committed frame remains displayed."
            )
            self.trace.record(
                "render_cancelled",
                parameter_hash=params.parameter_hash if params is not None else None,
            )
        except VolumeRenderError as exc:
            self.status_label.setText(str(exc))
            self.trace.record(
                "render_rejected",
                code=exc.code.value,
                field=exc.field,
                parameter_hash=params.parameter_hash if params is not None else None,
            )
        except Exception as exc:
            self.status_label.setText(f"Render failed: {exc}")
            self.trace.record(
                "render_failed",
                error_type=type(exc).__name__,
                message=str(exc),
            )

    def _commit(self) -> None:
        params = self._build_params_from_ui()
        if params is None:
            return
        try:
            self.transfer.update(params.control_points)
        except VolumeRenderError as exc:
            self.status_label.setText(f"{exc} Previous LUT retained.")
            return
        self.preview.set_pending(params)
        self._start_render(params, reason="operator_commit")

    def _cancel_active(self) -> None:
        if self.service.cancel(self.volume_key):
            self.status_label.setText("Cancellation requested; waiting for the next ray-batch checkpoint.")

    def _set_ui_from_params(self, params: RenderParameters) -> None:
        self._populating = True
        try:
            self.wl_slider.setValue(int(round(params.window_center)))
            self.ww_slider.setValue(int(round(params.window_width)))
            self.step_spin.setValue(params.step_size)
            self.mask_checkbox.setChecked(params.mask_enabled)
            self.viewport = params.viewport
            self.tf_table.setRowCount(len(params.control_points))
            for row, point in enumerate(params.control_points):
                values = (
                    point.position,
                    point.opacity,
                    point.color[0],
                    point.color[1],
                    point.color[2],
                )
                for column, value in enumerate(values):
                    item = QTableWidgetItem(f"{value:.6g}")
                    item.setTextAlignment(Qt.AlignRight | Qt.AlignVCenter)
                    self.tf_table.setItem(row, column, item)
            self.wl_value.setText(f"{self.wl_slider.value()}")
            self.ww_value.setText(f"{self.ww_slider.value()}")
        finally:
            self._populating = False

    def _add_control_point(self) -> None:
        row = self.tf_table.rowCount()
        self.tf_table.insertRow(row)
        previous = 0.0
        if row:
            previous_item = self.tf_table.item(row - 1, 0)
            if previous_item is not None:
                previous = float(previous_item.text())
        position = min(1.0, previous + 0.05) if previous < 1.0 else 1.0
        values = (position, 0.3, 0.8, 0.8, 0.8)
        for column, value in enumerate(values):
            self.tf_table.setItem(row, column, QTableWidgetItem(f"{value:.6g}"))
        self._refresh_pending()

    def _remove_control_point(self) -> None:
        row = self.tf_table.currentRow()
        if row < 0:
            row = self.tf_table.rowCount() - 1
        if row >= 0:
            self.tf_table.removeRow(row)
            self._refresh_pending()

    def _save_snapshot_dialog(self) -> None:
        frame = self.preview.frame
        if frame is None:
            self.status_label.setText("No committed frame is available to snapshot.")
            return
        default = str(self.directories["snapshots"] / f"gui_manual_{self.size}.json")
        path, _ = QFileDialog.getSaveFileName(self, "Save snapshot", default, "JSON (*.json)")
        if not path:
            return
        try:
            snapshot = create_snapshot(
                frame.params,
                frame.result,
                color_buffer_name=frame.params.parameter_hash,
            )
            save_snapshot(snapshot, path)
            self.status_label.setText(f"Snapshot saved atomically: {path}")
            self.trace.record(
                "snapshot_saved",
                parameter_hash=snapshot.parameter_hash,
                color_buffer_hash=snapshot.color_buffer_hash,
                snapshot_id=snapshot.snapshot_id,
            )
        except VolumeRenderError as exc:
            self.status_label.setText(str(exc))

    def _load_snapshot_dialog(self) -> None:
        default = str(self.directories["snapshots"])
        path, _ = QFileDialog.getOpenFileName(self, "Load snapshot", default, "JSON (*.json)")
        if path:
            self.load_snapshot_file(Path(path))

    def load_snapshot_file(self, path: Path) -> None:
        try:
            snapshot = load_snapshot(path)
            if snapshot.params.volume_hash != self.volume.content_sha256:
                raise VolumeRenderError(
                    "E_SNAPSHOT_INVALID",
                    "snapshot belongs to a different volume",
                    field="volume_hash",
                )
        except VolumeRenderError as exc:
            self.status_label.setText(str(exc))
            return
        self._set_ui_from_params(snapshot.params)
        self.preview.set_pending(snapshot.params)
        self.trace.record(
            "snapshot_loaded_requested_rerender",
            parameter_hash=snapshot.parameter_hash,
            color_buffer_hash=snapshot.color_buffer_hash,
            displayed_parameter_hash=self.preview.displayed_parameter_hash,
        )
        self._start_render(snapshot.params, reason="snapshot_load")

    def closeEvent(self, event: Any) -> None:
        self.service.cancel(self.volume_key)
        self.trace.close()
        self.service.shutdown(wait=False)
        super().closeEvent(event)


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description="Hash-bound volume rendering desktop preview")
    parser.add_argument("--root", default="output", help="project artifact directory")
    parser.add_argument("--size", type=int, default=64, choices=(64, 128))
    parser.add_argument("--smoke-test", action="store_true", help="close automatically after startup")
    return parser


def main(argv: list[str] | None = None) -> int:
    args = build_parser().parse_args(argv)
    root = Path(args.root).resolve()
    if not (root / "samples").exists():
        generate_project_assets(root, sizes=(args.size,))
    application = QApplication.instance() or QApplication(sys.argv[:1])
    window = VolumeRenderWindow(root, args.size, smoke_test=args.smoke_test)
    window.show()
    if args.smoke_test:
        QTimer.singleShot(9000, application.quit)
    return int(application.exec_())


if __name__ == "__main__":
    raise SystemExit(main())
