import os

import pytest

os.environ.setdefault("QT_QPA_PLATFORM", "offscreen")

pytest.importorskip("PySide6")


def test_mainwindow_headless_smoke(tmp_path):
    from PySide6 import QtWidgets
    from spectral_studio.ui import MainWindow, build_library

    app = QtWidgets.QApplication.instance() or QtWidgets.QApplication([])
    library = build_library(snapshot_path=str(tmp_path / "snap.json"))
    window = MainWindow(library)
    assert window.table.rowCount() == 7
    assert window.timeline.count() == 7
    window.whitepoint.setCurrentText("D50")
    window.on_apply()
    window.worker.thread.wait(5000)
    assert library.current_release.release_id == 2
    # Second submit while a worker reference exists but finished: allowed.
    window.on_apply()
    window.worker.thread.wait(5000)
    assert library.current_release.release_id == 3
    window.on_cancel()  # must not raise
