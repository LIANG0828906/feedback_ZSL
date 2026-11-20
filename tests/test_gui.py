from __future__ import annotations

import os
from pathlib import Path

os.environ.setdefault("QT_QPA_PLATFORM", "offscreen")

from PySide6.QtWidgets import QApplication

from hdr_studio.demo import run_demo
from hdr_studio.gui import WorkbenchWindow


def test_workbench_constructs_and_previews_published_version(tmp_path: Path) -> None:
    run_demo(tmp_path, seed=9, force=True)
    app = QApplication.instance() or QApplication([])
    window = WorkbenchWindow(tmp_path)
    try:
        assert window.engine.latest_record is not None
        assert window.version_list.count() == 4
        window._set_frame(2)
        app.processEvents()
        assert window.frame_slider.value() == 2
        assert window.preview_label.pixmap() is not None
    finally:
        window.close()

