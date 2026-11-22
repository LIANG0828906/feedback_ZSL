from __future__ import annotations

import os
from pathlib import Path

os.environ.setdefault("QT_QPA_PLATFORM", "offscreen")

from PySide6.QtWidgets import QApplication

from sciseq.gui.app import MainWindow


def test_desktop_window_offscreen_smoke(tiny_sequence, tmp_path: Path) -> None:
    app = QApplication.instance() or QApplication([])
    window = MainWindow(tiny_sequence.root, tmp_path / "gui_checkpoint")
    assert window.timeline.maximum() == tiny_sequence.frame_count - 1
    assert window.table.rowCount() == tiny_sequence.frame_count
    window.timeline.setValue(2)
    app.processEvents()
    assert window._current_frame == 2
    window.show()
    app.processEvents()
    window.close()
    app.processEvents()

