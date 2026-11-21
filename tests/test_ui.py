from __future__ import annotations

from pathlib import Path

from PySide6.QtWidgets import QApplication

from vectorbool.ui import MainWindow


def test_ui_constructs_offscreen_and_loads_samples() -> None:
    application = QApplication.instance() or QApplication([])
    window = MainWindow(str(Path("artifacts/samples/samples.json")))
    try:
        assert window.sample_combo.count() > 1
        assert window.preview_label is not None
        assert window.service.active_count == 0
    finally:
        window.close()
        application.processEvents()

