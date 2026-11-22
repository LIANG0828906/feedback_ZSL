from __future__ import annotations

import os
import sys
import tempfile
from pathlib import Path

os.environ.setdefault("QT_QPA_PLATFORM", "offscreen")

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT))

from PySide6.QtWidgets import QApplication  # noqa: E402

from scworkbench.gui.fonts import apply_application_font  # noqa: E402
from scworkbench.gui.main_window import MainWindow  # noqa: E402


def main() -> int:
    app = QApplication.instance() or QApplication(sys.argv)
    apply_application_font(app)
    output = ROOT / "artifacts" / "generated"
    output.mkdir(parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory() as temp:
        window = MainWindow(
            ROOT / "samples" / "sample_session.json",
            temp,
            use_loopback=True,
            auto_start=False,
        )
        window.show()
        app.processEvents()
        window.advance_demo(48)
        app.processEvents()
        clean = output / "workbench_clean.png"
        window.grab().save(str(clean))

        mixed_index = window.anomaly_combo.findData("mixed")
        window.anomaly_combo.setCurrentIndex(mixed_index)
        window._prepare_engine(restore=False)
        window.advance_demo(62)
        app.processEvents()
        faults = output / "workbench_faults.png"
        window.grab().save(str(faults))
        window.close()
        app.processEvents()
    print(clean)
    print(faults)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
