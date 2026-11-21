from __future__ import annotations

import os

os.environ.setdefault("QT_QPA_PLATFORM", "offscreen")

from PySide6.QtWidgets import QApplication  # noqa: E402

from audio_sync.ui import MainWindow  # noqa: E402


def test_workbench_constructs_and_exposes_controls(tmp_path) -> None:
    app = QApplication.instance() or QApplication([])
    window = MainWindow()
    window.output_edit.setText(str(tmp_path))
    window.show()
    app.processEvents()
    assert window.segment_spin.value() == 2.0
    assert window.interpolation_combo.currentText() == "linear"
    assert window.strategy_combo.currentText() == "auto"
    assert window.cancel_button.isEnabled() is False
    window.close()
