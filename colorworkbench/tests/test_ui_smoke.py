"""业务要求 7：界面可在离屏环境构建并触发一次增量重算。"""

import os
import unittest

os.environ.setdefault("QT_QPA_PLATFORM", "offscreen")

from PySide6.QtWidgets import QApplication  # noqa: E402

from cwb.engine import Workbench  # noqa: E402
from cwb.model import INTENT_ABSOLUTE, INTENT_RELATIVE  # noqa: E402
from cwb.ui.main_window import WorkbenchWindow  # noqa: E402

SAMPLES = os.path.join(os.path.dirname(__file__), "..", "samples")


class UiSmokeTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.app = QApplication.instance() or QApplication([])

    def test_window_loads_and_recomputes(self):
        wb = Workbench(
            os.path.join(SAMPLES, "profiles", "src_v3.json"),
            os.path.join(SAMPLES, "profiles", "dst_v3.json"),
            INTENT_RELATIVE, output_depth=16, batch_rows=8, max_workers=2)
        window = WorkbenchWindow(wb)
        window.load_and_run(
            os.path.join(SAMPLES, "images", "tile_32x24_16_rgb.cimg"), 16)
        self.assertEqual(window.batch_table.rowCount(),
                         len(wb.pipeline.batches))
        window.intent_box.setCurrentText(INTENT_ABSOLUTE)
        window._on_recompute()
        self.assertEqual(wb.config.intent, INTENT_ABSOLUTE)


if __name__ == "__main__":
    unittest.main()
