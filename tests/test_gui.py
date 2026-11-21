from __future__ import annotations

import os
from pathlib import Path

os.environ.setdefault("QT_QPA_PLATFORM", "offscreen")

pytest = __import__("pytest")
QtCore = pytest.importorskip("PySide6.QtCore")
pytest.importorskip("PySide6.QtWidgets")

from PySide6.QtWidgets import QApplication

from haptic_studio.gui import MainWindow
from haptic_studio.samples import generate_samples


def test_gui_offscreen_recompute(tmp_path: Path) -> None:
    app = QApplication.instance() or QApplication([])
    sample_dir = tmp_path / "samples"
    generate_samples(sample_dir)
    window = MainWindow(sample_dir, tmp_path / "runtime", 20261122)
    window.show()
    app.processEvents()
    assert window.session is not None
    assert window.segment_table.rowCount() == 4
    assert window._current_query is not None
    assert window._current_query.hit_version == 1

    window.segment_table.item(0, 5).setText("sine")
    window._start_recompute()
    QtCore.QThread.msleep(10)
    for _ in range(300):
        app.processEvents()
        if window.worker is None:
            break
        QtCore.QThread.msleep(10)
    app.processEvents()
    assert window.worker is None
    assert window.session is not None
    assert window.session.latest_publication.publication_version == 2
    assert window._current_query is not None
    assert window._current_query.hit_version == 2
    assert window.version_list.count() == 2
    image = window.grab()
    assert image.width() > 500
    assert image.height() > 400
    window.close()
