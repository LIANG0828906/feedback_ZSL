"""PySide6 example application plus a deterministic headless entry point."""

from __future__ import annotations

import argparse
import json
import os
from pathlib import Path
from typing import Any

import numpy as np

from .budget import BudgetConfig
from .demo import render_budget_trajectory
from .errors import BusinessError
from .pngio import colorize_error
from .renderer import RayCaster
from .transfer import ControlPoint, DEFAULT_CONTROL_POINTS, TransferFunction
from .view import CameraView
from .volume import load_volume


def probe_offscreen() -> dict[str, Any]:
    """Probe Qt's offscreen surface; callers always retain the NumPy fallback."""

    if not os.environ.get("DISPLAY") and not os.environ.get("WAYLAND_DISPLAY"):
        os.environ.setdefault("QT_QPA_PLATFORM", "offscreen")
    try:
        from PySide6.QtGui import (
            QGuiApplication,
            QOffscreenSurface,
            QOpenGLContext,
            QSurfaceFormat,
        )
    except Exception as exc:  # pragma: no cover - environment dependent
        return {"available": False, "backend": "numpy-raster", "reason": f"PySide6: {exc}"}
    app = QGuiApplication.instance()
    owns_app = app is None
    if owns_app:
        app = QGuiApplication([])
    surface = None
    context = None
    try:
        surface = QOffscreenSurface()
        surface.setFormat(QSurfaceFormat.defaultFormat())
        surface.create()
        if not surface.isValid():
            return {
                "available": False,
                "backend": "numpy-raster",
                "reason": "QOffscreenSurface invalid",
            }
        context = QOpenGLContext()
        context.setFormat(surface.format())
        context_created = bool(context.create())
        current = bool(context_created and context.makeCurrent(surface))
        version = ""
        renderer = "unknown"
        if current:
            try:
                from OpenGL import GL

                version_value = GL.glGetString(GL.GL_VERSION)
                renderer_value = GL.glGetString(GL.GL_RENDERER)
                version = version_value.decode("utf-8", errors="replace") if version_value else ""
                renderer = renderer_value.decode("utf-8", errors="replace") if renderer_value else ""
            except Exception:
                version = "context-current"
        return {
            "available": current,
            "backend": "qt-opengl-offscreen+numpy-raster" if current else "numpy-raster",
            "renderer": renderer,
            "reason": "OpenGL context current" if current else "OpenGL context unavailable",
            "version": version,
        }
    except Exception as exc:  # pragma: no cover - environment dependent
        return {"available": False, "backend": "numpy-raster", "reason": str(exc)}
    finally:
        if context is not None:
            try:
                if context.isValid():
                    context.doneCurrent()
            except Exception:
                pass
        if surface is not None:
            surface.destroy()
        if owns_app:
            app.quit()


def _run_gui(volume_path: Path) -> int:
    from PySide6.QtCore import Qt
    from PySide6.QtGui import QImage, QPixmap
    from PySide6.QtWidgets import (
        QApplication,
        QDoubleSpinBox,
        QFormLayout,
        QHBoxLayout,
        QLabel,
        QMainWindow,
        QPushButton,
        QSlider,
        QSpinBox,
        QTableWidget,
        QTableWidgetItem,
        QTabWidget,
        QVBoxLayout,
        QWidget,
    )

    app = QApplication.instance() or QApplication([])
    probe = probe_offscreen()

    class MainWindow(QMainWindow):
        def __init__(self) -> None:
            super().__init__()
            self.setWindowTitle("SVOL Budgeted Volume Preview")
            self.resize(1180, 760)
            self.volume_path = volume_path
            self.volume = load_volume(volume_path)
            self.tf = TransferFunction(DEFAULT_CONTROL_POINTS)
            self.last_result = None
            self.last_baseline = None

            controls = QWidget()
            form = QFormLayout(controls)
            self.window_width = QDoubleSpinBox()
            self.window_width.setRange(1.0, 200000.0)
            self.window_width.setValue(self.volume.header.window_width)
            self.window_level = QDoubleSpinBox()
            self.window_level.setRange(-200000.0, 200000.0)
            self.window_level.setValue(self.volume.header.window_level)
            self.window_width_slider = QSlider(Qt.Orientation.Horizontal)
            self.window_width_slider.setRange(1, 100000)
            self.window_width_slider.setValue(int(self.volume.header.window_width))
            self.window_level_slider = QSlider(Qt.Orientation.Horizontal)
            self.window_level_slider.setRange(-50000, 100000)
            self.window_level_slider.setValue(int(self.volume.header.window_level))
            self.window_width_slider.valueChanged.connect(
                lambda value: self.window_width.setValue(float(value))
            )
            self.window_level_slider.valueChanged.connect(
                lambda value: self.window_level.setValue(float(value))
            )
            self.window_width.valueChanged.connect(
                lambda value: self.window_width_slider.setValue(int(value))
            )
            self.window_level.valueChanged.connect(
                lambda value: self.window_level_slider.setValue(int(value))
            )
            self.step_size = QDoubleSpinBox()
            self.step_size.setRange(0.05, 128.0)
            self.step_size.setValue(1.0 if max(self.volume.dims) <= 128 else 0.7)
            self.step_size.setSingleStep(0.25)
            self.max_samples = QSpinBox()
            self.max_samples.setRange(1, 2_000_000_000)
            self.max_samples.setValue(200_000_000)
            self.max_memory = QSpinBox()
            self.max_memory.setRange(1, 2_000_000_000)
            self.max_memory.setValue(600_000_000)
            self.yaw = QDoubleSpinBox()
            self.yaw.setRange(-180.0, 180.0)
            self.yaw.setValue(35.0)
            self.pitch = QDoubleSpinBox()
            self.pitch.setRange(-85.0, 85.0)
            self.pitch.setValue(24.0)
            for label, widget in (
                ("Window width", self.window_width),
                ("Width slider", self.window_width_slider),
                ("Window level", self.window_level),
                ("Level slider", self.window_level_slider),
                ("Sample step", self.step_size),
                ("Max samples", self.max_samples),
                ("Max memory B", self.max_memory),
                ("Yaw", self.yaw),
                ("Pitch", self.pitch),
            ):
                form.addRow(label, widget)

            self.tf_table = QTableWidget(len(DEFAULT_CONTROL_POINTS), 5)
            self.tf_table.setHorizontalHeaderLabels(["x", "R", "G", "B", "Opacity"])
            for row, point in enumerate(DEFAULT_CONTROL_POINTS):
                for column, value in enumerate(point.as_list()):
                    self.tf_table.setItem(row, column, QTableWidgetItem(f"{value:g}"))
            form.addRow(QLabel("Transfer function"), self.tf_table)

            apply_tf = QPushButton("Apply transfer function")
            apply_tf.clicked.connect(self.apply_tf)
            render_button = QPushButton("Render preview")
            render_button.clicked.connect(self.render)
            form.addRow(apply_tf, render_button)
            self.status = QLabel(
                f"backend={probe['backend']} ({probe['reason']}); "
                "NumPy raster remains authoritative when offscreen GL is unavailable."
            )
            self.status.setWordWrap(True)
            form.addRow(self.status)

            self.render_label = QLabel("No render yet")
            self.render_label.setAlignment(Qt.AlignmentFlag.AlignCenter)
            self.heatmap_label = QLabel("No heatmap yet")
            self.heatmap_label.setAlignment(Qt.AlignmentFlag.AlignCenter)
            tabs = QTabWidget()
            tabs.addTab(self.render_label, "Render")
            tabs.addTab(self.heatmap_label, "Error heatmap")
            image_panel = QWidget()
            image_layout = QVBoxLayout(image_panel)
            image_layout.addWidget(tabs)
            body = QWidget()
            body_layout = QHBoxLayout(body)
            body_layout.addWidget(controls, 0)
            body_layout.addWidget(image_panel, 1)
            self.setCentralWidget(body)

        def apply_tf(self) -> None:
            points: list[ControlPoint] = []
            try:
                for row in range(self.tf_table.rowCount()):
                    values = [
                        float(self.tf_table.item(row, column).text())
                        for column in range(5)
                    ]
                    points.append(ControlPoint(*values))
                self.tf.replace(points)
                bounds = self.tf.interval_error_bounds
                self.status.setText(f"TF applied; interval error bounds={bounds}")
            except (BusinessError, TypeError, ValueError, AttributeError) as exc:
                self.status.setText(
                    f"TF rejected; previous LUT retained: "
                    f"{exc.to_dict() if isinstance(exc, BusinessError) else exc}"
                )

        def _pixmap(self, pixels: np.ndarray) -> QPixmap:
            height, width, _ = pixels.shape
            image = QImage(
                pixels.data,
                width,
                height,
                width * 3,
                QImage.Format.Format_RGB888,
            )
            return QPixmap.fromImage(image.copy())

        def render(self) -> None:
            try:
                self.apply_tf()
                try:
                    self.volume = load_volume(
                        self.volume_path,
                        window_override=(self.window_width.value(), self.window_level.value()),
                    )
                except BusinessError:
                    self.volume = load_volume(self.volume_path)
                ray = RayCaster(self.volume, self.tf)
                image_size = 256 if max(self.volume.dims) <= 128 else 192
                view = CameraView(yaw_deg=self.yaw.value(), pitch_deg=self.pitch.value())
                ample = BudgetConfig(
                    max_samples=10**18,
                    max_memory_bytes=10**18,
                    ray_batch=8192,
                )
                self.last_baseline = ray.render(
                    view,
                    width=image_size,
                    height=image_size,
                    step_size=self.step_size.value(),
                    budget=ample,
                )
                budget = BudgetConfig(
                    max_samples=int(self.max_samples.value()),
                    max_memory_bytes=int(self.max_memory.value()),
                )
                self.last_result = ray.render(
                    view,
                    width=image_size,
                    height=image_size,
                    step_size=self.step_size.value(),
                    budget=budget,
                )
                self.render_label.setPixmap(self._pixmap(self.last_result.pixels))
                difference = np.max(
                    np.abs(
                        self.last_result.pixels.astype(np.int16)
                        - self.last_baseline.pixels.astype(np.int16)
                    ),
                    axis=2,
                )
                maximum = max(1, int(np.max(difference)))
                heatmap = colorize_error(difference, max_error=maximum)
                self.heatmap_label.setPixmap(self._pixmap(heatmap))
                self.status.setText(
                    "stages="
                    + json.dumps(self.last_result.degradation_stages)
                    + "; step="
                    + f"{self.last_result.step_size:g}"
                    + "; samples="
                    + f"{self.last_result.usage['samples_used']}"
                    + "; memory="
                    + f"{self.last_result.usage['memory_used_bytes']}"
                    + "; error="
                    + json.dumps(self.last_result.error_summary, sort_keys=True)
                )
            except BusinessError as exc:
                self.status.setText(json.dumps(exc.to_dict(), sort_keys=True, ensure_ascii=False))

    window = MainWindow()
    window.show()
    return int(app.exec())


def parse_args(argv: list[str] | None = None) -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--volume",
        type=Path,
        default=Path("artifacts/samples/small/volume.svol"),
    )
    parser.add_argument("--headless", action="store_true")
    parser.add_argument("--probe-only", action="store_true")
    parser.add_argument("--output", type=Path, default=Path("artifacts/demo_small"))
    parser.add_argument("--tier", default="small")
    return parser.parse_args(argv)


def main(argv: list[str] | None = None) -> int:
    args = parse_args(argv)
    if args.probe_only:
        print(json.dumps(probe_offscreen(), sort_keys=True, indent=2))
        return 0
    if args.headless:
        row = render_budget_trajectory(
            volume_path=args.volume,
            output_dir=args.output,
            tier=args.tier,
        )
        print(
            json.dumps(
                {
                    "output": str(args.output),
                    "repeat_identical": row["repeat_identical"],
                    "stages": row["results"][-1].degradation_stages,
                },
                sort_keys=True,
                indent=2,
            )
        )
        return 0
    return _run_gui(args.volume)


if __name__ == "__main__":
    raise SystemExit(main())

