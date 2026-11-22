from __future__ import annotations

import time
from pathlib import Path

from glyph_workbench.params import RenderParams


def test_gui_offscreen_smoke_and_commit(document, tmp_path: Path) -> None:
    from PySide6.QtGui import QFontDatabase
    from PySide6.QtWidgets import QApplication

    from glyph_workbench.gui import MainWindow

    app = QApplication.instance() or QApplication([])
    window = MainWindow(
        model_path=Path(__file__).resolve().parents[1] / "samples" / "glyph_samples.json",
        persist_root=tmp_path / "persist",
        state_model=tmp_path / "model.json",
        glyph_id="A",
        params=RenderParams(0.25, 0.35, 8, 2),
    )
    window.show()

    deadline = time.time() + 12
    while time.time() < deadline:
        app.processEvents()
        time.sleep(0.02)
        if (
            window.last_result is not None
            and window.last_result.complete
            and not window._busy()
        ):
            break
    assert window.last_result is not None
    assert window.last_result.complete
    assert window.state_table.rowCount() == 2
    assert window.progress.value() == 2
    assert any("WenQuanYi" in family for family in QFontDatabase.families())

    segment = window.session.working_glyph.contours[1].segments[0]
    window.session.set_control_point(
        1,
        0,
        "p1",
        (segment.p1[0] + 4, segment.p1[1] + 2),
    )
    assert window.session.dirty_contours == {1}
    window.commit_batch()
    deadline = time.time() + 12
    while time.time() < deadline:
        app.processEvents()
        time.sleep(0.02)
        if window.session.last_commit is not None and not window._busy():
            break
    assert window.session.last_commit is not None
    assert window.session.last_commit.success
    screenshot = tmp_path / "workbench.png"
    assert window.grab().save(str(screenshot), "PNG")
    assert screenshot.stat().st_size > 10_000
    window.close()

