"""工作台应用入口。

有显示环境直接开窗；无显示环境可设 QT_QPA_PLATFORM=offscreen 做自检。
"""

from __future__ import annotations

import os
import sys

from PySide6.QtWidgets import QApplication

from .engine import Workbench
from .model import INTENT_RELATIVE
from .ui.main_window import WorkbenchWindow


def main() -> int:
    base = os.path.join(os.path.dirname(__file__), "..", "samples")
    app = QApplication(sys.argv)
    wb = Workbench(
        os.path.join(base, "profiles", "src_v3.json"),
        os.path.join(base, "profiles", "dst_v3.json"),
        INTENT_RELATIVE, output_depth=16, batch_rows=8, max_workers=4,
        snapshot_dir=os.path.join(base, "..", ".snapshot"))
    window = WorkbenchWindow(wb)
    window.load_and_run(os.path.join(base, "images",
                                     "tile_32x24_16_rgb.cimg"), 16)
    window.show()
    return app.exec()


if __name__ == "__main__":
    raise SystemExit(main())
