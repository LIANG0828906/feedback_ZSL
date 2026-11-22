#!/usr/bin/env python3
"""Native Windows Qt startup smoke test.

Run from Windows Python:
    python scripts\\windows_smoke.py
"""

from __future__ import annotations

import os
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
sys.path.insert(0, str(ROOT))
os.environ.setdefault("QT_QPA_PLATFORM", "windows")

from PySide6.QtGui import QGuiApplication
from PySide6.QtWidgets import QApplication

from filter_studio.ui.main import MainWindow, _configure_application


def main() -> int:
    app = QApplication([])
    _configure_application(app)
    window = MainWindow(
        ROOT / "samples",
        ROOT / "artifacts" / "windows-cache",
        auto_start=False,
    )
    window.show()
    app.processEvents()
    run = window.session.run()
    window._last_run = run
    window._refresh_all()
    app.processEvents()
    screenshot = ROOT / "artifacts" / "windows_ui.png"
    screenshot.parent.mkdir(parents=True, exist_ok=True)
    if not window.grab().save(str(screenshot)):
        raise RuntimeError("无法保存 Windows UI 截图")
    print(f"platform={QGuiApplication.platformName()}")
    print(f"python={sys.version.split()[0]}")
    print(f"run_status={run.status}")
    print(f"cache_entries={len(window.session.cache)}")
    print(f"screenshot={screenshot}")
    window.close()
    app.processEvents()
    return 0 if run.status == "complete" else 1


if __name__ == "__main__":
    raise SystemExit(main())
