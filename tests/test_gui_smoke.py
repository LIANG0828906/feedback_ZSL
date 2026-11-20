from __future__ import annotations

import os
from pathlib import Path

os.environ.setdefault("QT_QPA_PLATFORM", "offscreen")

from PySide6.QtWidgets import QApplication  # noqa: E402

from repair_app.engine import SessionEngine  # noqa: E402
from repair_app.generator import generate_sample_file  # noqa: E402
from repair_app.gui import MainWindow  # noqa: E402
from repair_app.models import Interval  # noqa: E402


def test_gui_loads_timeline_and_session(tmp_path: Path) -> None:
    input_path = tmp_path / "input.wav"
    session_path = tmp_path / "session.json"
    output_path = tmp_path / "output.wav"
    generate_sample_file(input_path, sample_rate=8_000, channels=2, seconds=0.5, seed=1)
    engine = SessionEngine(session_path)
    engine.create(
        input_path=input_path,
        output_path=output_path,
        block_size=256,
        intervals=[Interval(1, 1000, 3000, gain_db=1.0)],
    )
    engine.process(max_new_blocks=3)
    app = QApplication.instance() or QApplication([])
    window = MainWindow()
    window.open_session_path(session_path)
    assert window.snapshot is not None
    assert window.timeline.samples is not None
    assert len(window.timeline.intervals) == 1
    assert window.progress_label.text()
    window.close()
    app.processEvents()

