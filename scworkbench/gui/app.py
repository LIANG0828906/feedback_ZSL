from __future__ import annotations

import argparse
import sys
from pathlib import Path

from PySide6.QtCore import Qt
from PySide6.QtWidgets import QApplication

from .fonts import apply_application_font
from .main_window import MainWindow


def run_gui(
    sample_path: str | Path,
    data_dir: str | Path,
    *,
    use_loopback: bool = True,
    auto_start: bool = True,
) -> int:
    QApplication.setHighDpiScaleFactorRoundingPolicy(
        Qt.HighDpiScaleFactorRoundingPolicy.PassThrough
    )
    app = QApplication.instance() or QApplication([sys.argv[0]])
    app.setApplicationName("Cast Workbench")
    app.setOrganizationName("Cast Workbench")
    apply_application_font(app)
    window = MainWindow(
        sample_path,
        data_dir,
        use_loopback=use_loopback,
        auto_start=auto_start,
    )
    window.show()
    return app.exec()


if __name__ == "__main__":
    parser = argparse.ArgumentParser()
    parser.add_argument("--sample", default="samples/sample_session.json")
    parser.add_argument("--data-dir", default="artifacts")
    parser.add_argument("--no-loopback", action="store_true")
    parser.add_argument("--no-auto-start", action="store_true")
    args = parser.parse_args()
    raise SystemExit(
        run_gui(
            args.sample,
            args.data_dir,
            use_loopback=not args.no_loopback,
            auto_start=not args.no_auto_start,
        )
    )
