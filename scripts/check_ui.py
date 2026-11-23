"""Headless PyQt startup smoke test."""

from __future__ import annotations

import os
import sys
import tempfile
from pathlib import Path

os.environ.setdefault("QT_QPA_PLATFORM", "offscreen")
sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from PyQt5.QtWidgets import QApplication  # noqa: E402

from acoustic_workbench.config import AnalysisConfig  # noqa: E402
from acoustic_workbench.models import RenderConfig  # noqa: E402
from acoustic_workbench.pipeline import AcousticPipeline  # noqa: E402
from acoustic_workbench.ui.main_window import create_window  # noqa: E402


def main() -> int:
    app = QApplication.instance() or QApplication([])
    window = create_window()
    window.show()
    app.processEvents()
    sample = window.current_sample
    with tempfile.TemporaryDirectory() as checkpoint_dir:
        pipeline = AcousticPipeline(
            sample,
            AnalysisConfig(max_parallelism=2),
            RenderConfig(convolution_window=1024, render_block_size=512),
            Path(checkpoint_dir) / "ui-smoke.sqlite3",
        )
        result = pipeline.run()
        pipeline.close()
    window._display_run(result)
    app.processEvents()
    screenshot = Path(__file__).resolve().parents[1] / "artifacts" / "ui_smoke.png"
    screenshot.parent.mkdir(parents=True, exist_ok=True)
    window.grab().save(str(screenshot))
    print(
        f"UI_OK title={window.windowTitle()} samples={window.sample_combo.count()} "
        f"size={window.width()}x{window.height()} screenshot={screenshot}"
    )
    window.close()
    app.processEvents()
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
