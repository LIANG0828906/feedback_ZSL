"""桌面工作台启动入口。"""

from __future__ import annotations

import sys
from pathlib import Path

from PySide6.QtWidgets import QApplication

from .main_window import MainWindow


def main() -> int:
    app = QApplication(sys.argv)
    workdir = Path(sys.argv[1]) if len(sys.argv) > 1 else Path("workdir")
    window = MainWindow(workdir)
    window.show()
    return app.exec()


if __name__ == "__main__":
    raise SystemExit(main())
