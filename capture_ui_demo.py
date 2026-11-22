#!/usr/bin/env python3
"""Reproducibly process the first sample and capture the heads-up UI evidence."""

from __future__ import annotations

import json
import os
import shutil
import time
from pathlib import Path

os.environ.setdefault("QT_QPA_PLATFORM", "offscreen")

from PySide6.QtWidgets import QApplication

from spectraforge.ui.app import MainWindow
from spectraforge.ui.theme import APP_QSS


def _wait(app: QApplication, predicate, timeout: float = 30.0) -> None:
    deadline = time.time() + timeout
    while time.time() < deadline:
        app.processEvents()
        if predicate():
            return
        time.sleep(0.01)
    raise TimeoutError("UI operation timed out")


def main() -> int:
    root = Path(__file__).resolve().parent
    artifacts = root / "artifacts"
    artifacts.mkdir(exist_ok=True)
    runtime = artifacts / "ui_runtime"
    if runtime.exists():
        shutil.rmtree(runtime)
    app = QApplication([])
    app.setStyle("Fusion")
    app.setStyleSheet(APP_QSS)
    window = MainWindow(root / "samples", runtime)
    window.resize(1600, 1000)
    window.show()
    app.processEvents()
    window.start_processing()
    _wait(app, lambda: not window._job_running)
    window.table.selectRow(3)
    app.processEvents()
    window.grab().save(str(artifacts / "ui_workbench.png"))
    window.preview_tabs.setCurrentIndex(1)
    app.processEvents()
    window.grab().save(str(artifacts / "ui_band_array.png"))
    window.compare_full()
    _wait(app, lambda: not window._compare_running)
    for tab in window.findChildren(type(window.preview_tabs)):
        for index in range(tab.count()):
            if tab.tabText(index) == "Difference":
                tab.setCurrentIndex(index)
    app.processEvents()
    window.grab().save(str(artifacts / "ui_difference.png"))
    result = {
        "scene": window.scene.metadata.scene_id,
        "progress": window.manager.progress(),
        "complete": bool(window.output and window.output.complete),
        "peak_radiance": float(window.output.peak_radiance) if window.output else None,
        "gamut_pixels": int(window.output.gamut_count) if window.output else None,
        "comparison_exact": "Pixel-exact" in window.compare_report.text(),
        "screenshots": [
            str(artifacts / "ui_workbench.png"),
            str(artifacts / "ui_band_array.png"),
            str(artifacts / "ui_difference.png"),
        ],
    }
    (artifacts / "ui_demo_summary.json").write_text(
        json.dumps(result, indent=2, ensure_ascii=False) + "\n",
        encoding="utf-8",
    )
    print(json.dumps(result, indent=2, ensure_ascii=False))
    window.close()
    app.processEvents()
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
