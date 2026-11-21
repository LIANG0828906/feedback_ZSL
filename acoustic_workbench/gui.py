"""Optional PyQt5 workbench front-end."""

from __future__ import annotations

import sys
from pathlib import Path
from typing import Optional

from .codes import AcousticError
from .engine import Workbench


def launch_gui(workspace: str, scene_path: Optional[str] = None) -> None:
    try:
        from matplotlib.backends.backend_qt5agg import FigureCanvasQTAgg
        from matplotlib.figure import Figure
        from PyQt5 import QtCore, QtWidgets
    except ImportError as exc:
        raise AcousticError(
            "E_CONFIG_INVALID",
            f"PyQt5/matplotlib GUI dependencies are unavailable: {exc}",
            injection_point="gui:import",
        ) from exc

    class Worker(QtCore.QThread):
        finished_result = QtCore.pyqtSignal(object)
        failed = QtCore.pyqtSignal(object)

        def __init__(self, bench: Workbench, operation: str, listener: str) -> None:
            super().__init__()
            self.bench = bench
            self.operation = operation
            self.listener = listener

        def run(self) -> None:
            try:
                if self.operation == "rebuild":
                    result = self.bench.rebuild(self.listener)
                else:
                    result = self.bench.render(self.listener)
                self.finished_result.emit(result)
            except AcousticError as exc:
                self.failed.emit(exc.to_dict())
            except Exception as exc:  # GUI boundary: show a structured failure
                self.failed.emit(
                    {
                        "ok": False,
                        "code": "E_SYNTHESIS_FAILED",
                        "message": str(exc),
                    }
                )

    app = QtWidgets.QApplication(sys.argv)
    window = QtWidgets.QMainWindow()
    window.setWindowTitle("Spatial Acoustic Field Workbench")
    central = QtWidgets.QWidget()
    window.setCentralWidget(central)
    layout = QtWidgets.QVBoxLayout(central)

    top = QtWidgets.QHBoxLayout()
    listener_box = QtWidgets.QComboBox()
    rebuild_button = QtWidgets.QPushButton("Rebuild")
    render_button = QtWidgets.QPushButton("Render 5.1")
    top.addWidget(QtWidgets.QLabel("Listener"))
    top.addWidget(listener_box)
    top.addWidget(rebuild_button)
    top.addWidget(render_button)
    layout.addLayout(top)

    figure = Figure(figsize=(8, 3), tight_layout=True)
    canvas = FigureCanvasQTAgg(figure)
    layout.addWidget(canvas, 2)
    log = QtWidgets.QPlainTextEdit()
    log.setReadOnly(True)
    layout.addWidget(log, 1)

    bench = Workbench(workspace, scene_path=scene_path)
    for listener_id in bench.listeners():
        listener_box.addItem(listener_id)
    workers = []

    def append_log(value: object) -> None:
        if isinstance(value, str):
            log.appendPlainText(value)
        else:
            import json

            log.appendPlainText(json.dumps(value, ensure_ascii=False, indent=2, sort_keys=True))

    def plot_field(field: dict) -> None:
        figure.clear()
        axis = figure.add_subplot(111)
        coefficients = field.get("coefficients", {})
        channels = list(coefficients)
        gains = [coefficients[channel].get("gain_rms_db", 0.0) for channel in channels]
        axis.bar(channels, gains)
        axis.set_ylabel("RMS gain (dB)")
        axis.set_title(f"Field {field.get('field_version', '')[:12]}")
        canvas.draw_idle()

    def start(operation: str) -> None:
        listener = listener_box.currentText()
        if not listener:
            return
        rebuild_button.setEnabled(False)
        render_button.setEnabled(False)
        worker = Worker(bench, operation, listener)
        workers.append(worker)

        def done(result: object) -> None:
            if operation == "rebuild" and isinstance(result, dict):
                plot_field(result["field"])
            append_log(result)
            rebuild_button.setEnabled(True)
            render_button.setEnabled(True)

        def failed(error: object) -> None:
            append_log(error)
            rebuild_button.setEnabled(True)
            render_button.setEnabled(True)

        worker.finished_result.connect(done)
        worker.failed.connect(failed)
        worker.finished.connect(lambda: workers.remove(worker) if worker in workers else None)
        worker.start()

    rebuild_button.clicked.connect(lambda: start("rebuild"))
    render_button.clicked.connect(lambda: start("render"))
    window.resize(980, 720)
    window.show()
    app.exec_()

