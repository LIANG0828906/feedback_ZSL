from __future__ import annotations

import os

os.environ.setdefault("QT_QPA_PLATFORM", "offscreen")

from PySide6.QtWidgets import QApplication

from broadcast_loudness.synth import synthesize_signal
from broadcast_loudness.ui import MeterWindow
from broadcast_loudness.wavio import write_wav


def test_ui_snapshot_switch_smoke(tmp_path) -> None:
    path = tmp_path / "ui.wav"
    write_wav(path, synthesize_signal(5, 6, 2.5), 24)
    app = QApplication.instance() or QApplication([])
    window = MeterWindow(refresh_ms=40)
    window.load_audio(path)
    assert window.session is not None
    window.session.advance(2 * 48_000)
    window._refresh_view()
    first_version = window.plot._version
    window.session.change_parameters(relative_gate_lu=-9.0)
    window._refresh_view()
    assert window.plot._version > first_version
    image = window.grab()
    assert not image.isNull()
    window.close()
    app.processEvents()
