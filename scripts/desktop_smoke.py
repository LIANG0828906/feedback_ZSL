from __future__ import annotations

import os
import sys
import tempfile
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "src"))

if not os.environ.get("DISPLAY") and not os.environ.get("WAYLAND_DISPLAY"):
    os.environ.setdefault("QT_QPA_PLATFORM", "offscreen")

from PySide6 import QtWidgets  # noqa: E402

from subtitle_localizer.generator import DEFAULT_SPECS, generate_case  # noqa: E402
from subtitle_localizer.session import LayoutSession  # noqa: E402
from subtitle_localizer.ui import MainWindow  # noqa: E402


def main() -> int:
    app = QtWidgets.QApplication.instance() or QtWidgets.QApplication(sys.argv[:1])
    with tempfile.TemporaryDirectory(dir=ROOT / "artifacts") as directory:
        case_dir = Path(directory) / DEFAULT_SPECS[0].directory
        generate_case(Path(directory), DEFAULT_SPECS[0], 7)
        with LayoutSession.open(case_dir, truncate_trace=True) as session:
            session.commit()
        window = MainWindow(case_dir, refresh_ms=50)
        window.show()
        app.processEvents()
        marker = Path(directory) / "desktop_smoke.txt"
        marker.write_text("window-created\n", encoding="utf-8")
        app.processEvents()
        window.close()
        app.processEvents()
        window.deleteLater()
        app.processEvents()
        app.quit()
        app.shutdown()
        if not marker.exists():
            raise RuntimeError("desktop smoke file was not written")
    print("desktop smoke ok")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

