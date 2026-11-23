"""GUI 应用启动入口。"""
from __future__ import annotations

import argparse
import os
import sys

from PySide6.QtWidgets import QApplication

from .main_window import MainWindow


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description="LoudBench GUI")
    parser.add_argument("--manifest", default="samples/manifest.json")
    parser.add_argument("--checkpoints", default="checkpoints")
    parser.add_argument("--exports", default="exports")
    args = parser.parse_args(argv)

    app = QApplication.instance() or QApplication(sys.argv)
    window = MainWindow(
        manifest_path=args.manifest,
        checkpoint_dir=os.path.join(args.checkpoints, "gui"),
        export_dir=args.exports)
    window.show()
    return app.exec()


if __name__ == "__main__":
    raise SystemExit(main())
