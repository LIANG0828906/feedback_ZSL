"""Interactive PySide6 editor.

Material list shows sRGB / Display-P3 swatches and numeric values side by
side; the timeline shows the version evolution per material with
dependency-factor highlighting.  Recompute runs on a background thread so
the main thread never blocks; only one recompute task may run at a time
(duplicate submissions surface E_BUSY) and cancellation keeps the last
published release.  Importable without a display server -- use
QT_QPA_PLATFORM=offscreen for headless runs.
"""

from __future__ import annotations

import sys

from . import samplegen
from .engine import MaterialLibrary
from .errors import ErrorCode, SpectralStudioError
from .versioning import diff_versions


def _qt():
    from PySide6 import QtCore, QtGui, QtWidgets
    return QtCore, QtGui, QtWidgets


class RecomputeWorker:
    """QThread wrapper around a single library submit."""

    def __init__(self, library, ops, ui_state):
        QtCore, _, _ = _qt()

        class _Worker(QtCore.QThread):
            finished_ok = QtCore.Signal(object)
            failed = QtCore.Signal(str)

            def run(self_inner):
                try:
                    release = library.submit(ops, ui_state=ui_state)
                except SpectralStudioError as exc:
                    self_inner.failed.emit(f"{exc.code}: {exc.message}")
                    return
                except Exception as exc:  # pragma: no cover - defensive
                    self_inner.failed.emit(str(exc))
                    return
                self_inner.finished_ok.emit(release)

        self.thread = _Worker()


class MainWindow:
    def __init__(self, library):
        QtCore, QtGui, QtWidgets = _qt()
        self.QtCore = QtCore
        self.QtGui = QtGui
        self.QtWidgets = QtWidgets
        self.library = library
        self.worker = None

        self.window = QtWidgets.QMainWindow()
        self.window.setWindowTitle("Spectral Material Library")
        central = QtWidgets.QWidget()
        self.window.setCentralWidget(central)
        layout = QtWidgets.QHBoxLayout(central)

        # Left: material table with side-by-side target color spaces.
        self.table = QtWidgets.QTableWidget(0, 6)
        self.table.setHorizontalHeaderLabels(
            ["material", "version", "sRGB", "sRGB values",
             "Display-P3", "Display-P3 values"])
        layout.addWidget(self.table, stretch=3)

        # Right: environment controls + timeline.
        right = QtWidgets.QVBoxLayout()
        layout.addLayout(right, stretch=2)

        form = QtWidgets.QFormLayout()
        self.whitepoint = QtWidgets.QComboBox()
        self.whitepoint.addItems(["D65", "D50", "A", "E"])
        self.observer = QtWidgets.QComboBox()
        self.observer.addItems(["1931_2", "1964_10"])
        self.illuminant = QtWidgets.QComboBox()
        self.illuminant.addItems(["D65", "D50", "A", "E"])
        self.adaptation = QtWidgets.QComboBox()
        self.adaptation.addItems(["bradford", "von_kries", "none"])
        form.addRow("white point", self.whitepoint)
        form.addRow("observer", self.observer)
        form.addRow("illuminant", self.illuminant)
        form.addRow("adaptation", self.adaptation)
        right.addLayout(form)

        buttons = QtWidgets.QHBoxLayout()
        self.apply_button = QtWidgets.QPushButton("Apply (recompute)")
        self.cancel_button = QtWidgets.QPushButton("Cancel")
        self.rollback_button = QtWidgets.QPushButton("Rollback selected")
        self.diff_button = QtWidgets.QPushButton("Diff versions")
        buttons.addWidget(self.apply_button)
        buttons.addWidget(self.cancel_button)
        buttons.addWidget(self.rollback_button)
        buttons.addWidget(self.diff_button)
        right.addLayout(buttons)

        self.status = QtWidgets.QLabel("ready")
        right.addWidget(self.status)

        self.timeline = QtWidgets.QListWidget()
        right.addWidget(self.timeline)

        self.apply_button.clicked.connect(self.on_apply)
        self.cancel_button.clicked.connect(self.on_cancel)
        self.rollback_button.clicked.connect(self.on_rollback)
        self.diff_button.clicked.connect(self.on_diff)
        self.refresh()

    def refresh(self):
        release = self.library.current_release
        self.table.setRowCount(0)
        if release is None:
            return
        for row, mid in enumerate(sorted(release.versions)):
            version = release.versions[mid]
            self.table.insertRow(row)
            self.table.setItem(row, 0, self.QtWidgets.QTableWidgetItem(mid))
            self.table.setItem(row, 1, self.QtWidgets.QTableWidgetItem(
                str(version.version)))
            for col, space in ((2, "srgb"), (4, "display-p3")):
                rgb = version.encoded.get(space)
                if rgb is None:
                    continue
                swatch = self.QtWidgets.QTableWidgetItem("")
                color = self.QtGui.QColor.fromRgbF(
                    max(0.0, min(1.0, rgb[0])),
                    max(0.0, min(1.0, rgb[1])),
                    max(0.0, min(1.0, rgb[2])))
                swatch.setBackground(self.QtGui.QBrush(color))
                self.table.setItem(row, col, swatch)
                self.table.setItem(row, col + 1,
                                   self.QtWidgets.QTableWidgetItem(
                                       ", ".join(f"{v:.4f}" for v in rgb)))

        self.timeline.clear()
        for mid in sorted(self.library._history):
            for version in self.library._history[mid]:
                reason = version.recompute_reason or ""
                item = self.QtWidgets.QListWidgetItem(
                    f"{mid} v{version.version} [{reason}] "
                    f"{version.content_hash[:12]}")
                if reason.startswith("env-factors-changed"):
                    item.setForeground(self.QtGui.QBrush(
                        self.QtGui.QColor("darkorange")))
                elif reason.startswith("material-params"):
                    item.setForeground(self.QtGui.QBrush(
                        self.QtGui.QColor("darkgreen")))
                self.timeline.addItem(item)

    def on_apply(self):
        if self.worker is not None and self.worker.thread.isRunning():
            self.status.setText(ErrorCode.E_BUSY
                                + ": recompute already running")
            return
        ops = [{"op": "set_env",
                "white_point": self.whitepoint.currentText(),
                "observer": self.observer.currentText(),
                "illuminant": self.illuminant.currentText(),
                "adaptation": self.adaptation.currentText()}]
        self.status.setText("recomputing...")
        self.worker = RecomputeWorker(self.library, ops,
                                      ui_state={"panel": "env"})
        self.worker.thread.finished_ok.connect(self._on_done)
        self.worker.thread.failed.connect(self._on_failed)
        self.worker.thread.start()

    def _on_done(self, release):
        self.status.setText(f"published release {release.release_id}")
        self.refresh()

    def _on_failed(self, message):
        self.status.setText(message)

    def on_cancel(self):
        self.library.cancel()
        self.status.setText("cancel requested; last published release kept")

    def _selected_material(self):
        row = self.table.currentRow()
        if row < 0:
            return None
        item = self.table.item(row, 0)
        return item.text() if item else None

    def on_rollback(self):
        mid = self._selected_material()
        if mid is None:
            self.status.setText("select a material row first")
            return
        history = self.library.history_of(mid)
        if len(history) < 2:
            self.status.setText("no earlier version to roll back to")
            return
        target = history[-2].version
        try:
            self.library.submit([{"op": "rollback", "id": mid,
                                  "version": target}])
        except SpectralStudioError as exc:
            self.status.setText(f"{exc.code}: {exc.message}")
            return
        self.status.setText(f"rolled back {mid} to v{target} (appended)")
        self.refresh()

    def on_diff(self):
        mid = self._selected_material()
        if mid is None:
            self.status.setText("select a material row first")
            return
        history = self.library.history_of(mid)
        if len(history) < 2:
            self.status.setText("need at least two versions to diff")
            return
        diff = diff_versions(history[-2], history[-1])
        self.QtWidgets.QMessageBox.information(
            self.window, f"diff {mid}", repr(diff))

    def show(self):
        self.window.show()


def build_library(snapshot_path=None):
    library = MaterialLibrary(snapshot_path=snapshot_path)
    for doc in samplegen.generate_samples()[0]:
        library.add_material(doc)
    library.submit([], ui_state={"panel": "materials"})
    return library


def main(argv=None):
    QtCore, _, QtWidgets = _qt()
    app = QtWidgets.QApplication(argv if argv is not None else sys.argv)
    window = MainWindow(build_library())
    window.show()
    return app.exec()


if __name__ == "__main__":
    raise SystemExit(main())
