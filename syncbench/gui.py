"""PySide6 diagnostic workbench for multichannel drift synchronization."""

from __future__ import annotations

from PySide6.QtCore import QObject, QThread, Signal
from PySide6.QtGui import QColor, QPainter, QPen
from PySide6.QtWidgets import (
    QComboBox,
    QDoubleSpinBox,
    QHBoxLayout,
    QLabel,
    QMainWindow,
    QMessageBox,
    QPushButton,
    QVBoxLayout,
    QWidget,
)

from .errors import E_BUSY, SyncError
from .pipeline import SyncConfig, SyncSession


class PlotWidget(QWidget):
    """Minimal deterministic plot canvas (no external plotting deps)."""

    def __init__(self, parent=None):
        super().__init__(parent)
        self.setMinimumHeight(160)
        self._series = []
        self._heatmap = None

    def set_series(self, series):
        self._series = series
        self._heatmap = None
        self.update()

    def set_heatmap(self, matrix):
        self._heatmap = matrix
        self._series = []
        self.update()

    def paintEvent(self, event):
        painter = QPainter(self)
        painter.fillRect(self.rect(), QColor("white"))
        if self._heatmap is not None:
            self._paint_heatmap(painter)
        elif self._series:
            self._paint_series(painter)
        painter.end()

    def _paint_series(self, painter):
        xs = [x for pts, _, _ in self._series for x, _ in pts]
        ys = [y for pts, _, _ in self._series for _, y in pts]
        if not xs:
            return
        x0, x1 = min(xs), max(xs)
        y0, y1 = min(ys), max(ys)
        if x1 == x0:
            x1 = x0 + 1
        if y1 == y0:
            y0, y1 = y0 - 1, y1 + 1
        w, h = self.width() - 20, self.height() - 20

        def map_pt(x, y):
            return (10 + (x - x0) / (x1 - x0) * w, 10 + (1 - (y - y0) / (y1 - y0)) * h)

        for pts, color, as_dots in self._series:
            painter.setPen(QPen(QColor(color), 2))
            mapped = [map_pt(x, y) for x, y in pts]
            if as_dots:
                for px, py in mapped:
                    painter.drawEllipse(int(px) - 2, int(py) - 2, 4, 4)
            else:
                for i in range(1, len(mapped)):
                    painter.drawLine(
                        int(mapped[i - 1][0]), int(mapped[i - 1][1]),
                        int(mapped[i][0]), int(mapped[i][1]),
                    )

    def _paint_heatmap(self, painter):
        matrix = self._heatmap
        rows = len(matrix)
        cols = max(len(r) for r in matrix) if rows else 0
        if not rows or not cols:
            return
        vmax = max(abs(v) for row in matrix for v in row) or 1.0
        cw = max(self.width() / cols, 1)
        ch = max(self.height() / rows, 1)
        for r, row in enumerate(matrix):
            for c, val in enumerate(row):
                ratio = min(abs(val) / vmax, 1.0)
                color = QColor(int(255 * ratio), 40, int(255 * (1 - ratio)))
                painter.fillRect(
                    int(c * cw), int(r * ch), int(cw) + 1, int(ch) + 1, color
                )


class SyncWorker(QObject):
    finished = Signal(dict)
    failed = Signal(str, str)

    def __init__(self, session, data_dir, config, out_dir, trace_path):
        super().__init__()
        self._args = (session, data_dir, config, out_dir, trace_path)

    def run(self):
        session, data_dir, config, out_dir, trace_path = self._args
        try:
            result = session.run(data_dir, config, out_dir, trace_path)
        except SyncError as exc:
            self.failed.emit(exc.code, exc.message)
        except Exception as exc:  # noqa: BLE001 - surfaced to the UI
            self.failed.emit("E_INTERNAL", str(exc))
        else:
            self.finished.emit(result)


class MainWindow(QMainWindow):
    def __init__(self, data_dir, out_dir, trace_path, n_channels=4):
        super().__init__()
        self.setWindowTitle("SyncBench Drift Workbench")
        self.data_dir = data_dir
        self.out_dir = out_dir
        self.trace_path = trace_path
        self.session = SyncSession()
        self._thread = None
        self._worker = None
        self.last_result = None

        central = QWidget()
        self.setCentralWidget(central)
        layout = QVBoxLayout(central)

        controls = QHBoxLayout()
        controls.addWidget(QLabel("Segment (s):"))
        self.seg_spin = QDoubleSpinBox()
        self.seg_spin.setRange(0.25, 60.0)
        self.seg_spin.setValue(2.0)
        controls.addWidget(self.seg_spin)
        controls.addWidget(QLabel("Ref ch:"))
        self.ref_combo = QComboBox()
        self.ref_combo.addItems([str(i) for i in range(n_channels)])
        controls.addWidget(self.ref_combo)
        controls.addWidget(QLabel("Interp:"))
        self.mode_combo = QComboBox()
        self.mode_combo.addItems(["linear", "sinc"])
        controls.addWidget(self.mode_combo)
        self.start_btn = QPushButton("Start sync")
        self.cancel_btn = QPushButton("Cancel")
        self.cancel_btn.setEnabled(False)
        controls.addWidget(self.start_btn)
        controls.addWidget(self.cancel_btn)
        self.status = QLabel("idle")
        controls.addWidget(self.status)
        layout.addLayout(controls)

        self.marker_plot = PlotWidget()
        self.delay_plot = PlotWidget()
        self.heatmap_plot = PlotWidget()
        layout.addWidget(QLabel("Marker arrivals"))
        layout.addWidget(self.marker_plot)
        layout.addWidget(QLabel("Segment delays + drift fit"))
        layout.addWidget(self.delay_plot)
        layout.addWidget(QLabel("Post-compensation residual heatmap"))
        layout.addWidget(self.heatmap_plot)

        self.start_btn.clicked.connect(self.start_sync)
        self.cancel_btn.clicked.connect(self.session.cancel)

    def current_config(self):
        return SyncConfig(
            seg_len_s=self.seg_spin.value(),
            ref_channel=int(self.ref_combo.currentText()),
            mode=self.mode_combo.currentText(),
        )

    def start_sync(self):
        if self._thread is not None:
            self._emit_busy()
            return
        config = self.current_config()
        self._thread = QThread(self)
        self._worker = SyncWorker(
            self.session, self.data_dir, config, self.out_dir, self.trace_path
        )
        self._worker.moveToThread(self._thread)
        self._thread.started.connect(self._worker.run)
        self._worker.finished.connect(self._on_finished)
        self._worker.failed.connect(self._on_failed)
        self._worker.finished.connect(self._thread.quit)
        self._worker.failed.connect(self._thread.quit)
        self._thread.finished.connect(self._on_thread_done)
        self.start_btn.setEnabled(False)
        self.cancel_btn.setEnabled(True)
        self.status.setText("running")
        self._thread.start()

    def _emit_busy(self):
        self.status.setText(E_BUSY)
        QMessageBox.warning(self, E_BUSY, "A sync task is already running.")

    def _on_finished(self, result):
        self.last_result = result
        self.status.setText(f"done: {result['strategy']}")
        self._render(result)

    def _on_failed(self, code, message):
        if code == E_BUSY:
            self._emit_busy()
        else:
            self.status.setText(f"{code}: {message}")
        if self.session.last_result is not None:
            self.last_result = self.session.last_result

    def _on_thread_done(self):
        self._thread = None
        self._worker = None
        self.start_btn.setEnabled(True)
        self.cancel_btn.setEnabled(False)

    def _render(self, result):
        channels = result["channels"]
        series = []
        palette = ["#d62728", "#1f77b4", "#2ca02c", "#9467bd",
                   "#ff7f0e", "#8c564b", "#e377c2", "#7f7f7f"]
        for ch in channels:
            pts = [
                (m["index"], ch["channel"] + 0.5)
                for m in ch["markers"]["markers"]
            ]
            series.append((pts, palette[ch["channel"] % 8], True))
        self.marker_plot.set_series(series)
        for ch in channels:
            if ch["segments"]:
                pts = [(s["center"], s["delay"]) for s in ch["segments"]]
                fit = ch["fit"]
                line = [(x, fit["offset"] + fit["slope"] * x) for x, _ in pts]
                self.delay_plot.set_series(
                    [(pts, "#1f77b4", True), (line, "#d62728", False)]
                )
                break
        matrix = []
        for ch in channels:
            if ch["fit"]["residuals"]:
                matrix.append([abs(r) for r in ch["fit"]["residuals"]])
        if matrix:
            self.heatmap_plot.set_heatmap(matrix)


def run_gui(data_dir, out_dir, trace_path, n_channels=4):
    import sys

    from PySide6.QtWidgets import QApplication

    app = QApplication(sys.argv)
    window = MainWindow(data_dir, out_dir, trace_path, n_channels)
    window.resize(900, 700)
    window.show()
    return app.exec()
