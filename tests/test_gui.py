from __future__ import annotations

from pathlib import Path

import pytest
from PySide6.QtWidgets import QApplication

from spectral_editor.demo import run_demo
from spectral_editor.gui import MainWindow, run_gui
from spectral_editor.library import load_library


@pytest.fixture(scope="module")
def qapp():
    app = QApplication.instance() or QApplication([])
    yield app


def test_gui_renders_published_material_and_timeline(tmp_path: Path, qapp):
    run_demo(77, tmp_path / "demo")
    library, pending = load_library(tmp_path / "demo" / "state" / "snapshot.json")
    assert pending is None
    try:
        window = MainWindow(library)
        assert window.material_table.rowCount() == 8
        assert window.material_table.columnCount() == 6
        assert window.timeline_table.rowCount() >= 3
        assert window.white_point_combo.count() >= 6
        window.close()
    finally:
        library.close()


def test_offscreen_screenshot_entrypoint(tmp_path: Path):
    run_demo(88, tmp_path / "demo")
    screenshot = tmp_path / "gui.png"
    code = run_gui(
        tmp_path / "demo" / "state" / "snapshot.json",
        screenshot=screenshot,
        exit_after_ms=600,
    )
    assert code == 0
    assert screenshot.exists()
    assert screenshot.stat().st_size > 1000
