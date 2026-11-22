from __future__ import annotations

import os

os.environ.setdefault("QT_QPA_PLATFORM", "offscreen")

from PySide6.QtWidgets import QApplication

from formation_workbench.gui.app import MainWindow


def test_gui_offscreen_smoke(sample_engine, tmp_path):
    app = QApplication.instance() or QApplication([])
    window = MainWindow(
        sample_engine,
        checkpoint_path=tmp_path / "gui_checkpoint.json",
    )
    window.show()
    app.processEvents()
    assert window.windowTitle() == "Formation Coupling Lab"
    assert window._member_table.rowCount() == 4
    assert window._member_table.item(0, 1).text() == "ok"
    window.close()
    app.processEvents()
