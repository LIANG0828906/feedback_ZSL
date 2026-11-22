from __future__ import annotations

import os
from pathlib import Path

os.environ.setdefault("QT_QPA_PLATFORM", "offscreen")


def test_gui_builds_without_display(repo_root: Path):
    from PyQt5.QtWidgets import QApplication

    from edgepipe.gui import MainWindow

    app = QApplication.instance() or QApplication(["edgepipe-test"])
    window = MainWindow(
        manifest_path=repo_root / "samples" / "index.json",
        checkpoint_root=repo_root / "checkpoints" / "gui-test",
    )
    assert window.sample_combo.count() >= 4
    assert window.current_image is not None
    assert {"original", "gradient", "open", "close", "thin", "prune", "diff"} <= set(
        window.mask_panels
    )
    window._render_state_matrix(None)
    assert window.state_table.rowCount() > 0
    assert window.state_table.columnCount() == 5
    window.close()
    app.processEvents()

