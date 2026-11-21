from __future__ import annotations

import os
from pathlib import Path

import pytest

os.environ.setdefault("QT_QPA_PLATFORM", "offscreen")
pytest.importorskip("PySide6")

from PySide6 import QtWidgets

from subtitle_localizer.session import LayoutSession
from subtitle_localizer.ui import MainWindow

from .helpers import make_case


def test_qt_workbench_creates_window_and_writes_project_temp(tmp_path: Path) -> None:
    case_dir = make_case(tmp_path)
    with LayoutSession.open(case_dir) as session:
        session.commit()
    app = QtWidgets.QApplication.instance() or QtWidgets.QApplication([])
    window = MainWindow(case_dir, refresh_ms=50)
    window.show()
    app.processEvents()
    marker = tmp_path / "window-write.txt"
    marker.write_text("ok\n", encoding="utf-8")
    app.processEvents()
    window.close()
    app.processEvents()
    window.deleteLater()
    app.processEvents()
    app.quit()
    app.shutdown()
    assert marker.read_text(encoding="utf-8") == "ok\n"

