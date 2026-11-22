import os

import pytest

pytest.importorskip("PySide6")

os.environ.setdefault("QT_QPA_PLATFORM", "offscreen")

from PySide6.QtWidgets import QApplication, QMessageBox

from scoreforge.generator import write_samples
from scoreforge.gui.app import MainWindow


@pytest.fixture(scope="module")
def qapp():
    app = QApplication.instance() or QApplication([])
    yield app


@pytest.fixture(autouse=True)
def _no_modal_dialogs(monkeypatch):
    monkeypatch.setattr(QMessageBox, "information", lambda *a, **k: None)
    monkeypatch.setattr(QMessageBox, "warning", lambda *a, **k: None)


def test_main_window_loads_and_commits(qapp, tmp_path):
    sample_dir = tmp_path / "samples"
    write_samples(sample_dir, seed=42, count=1)
    window = MainWindow(str(sample_dir), str(tmp_path / "ck"))
    assert window.project_combo.count() == 1
    window.on_load()
    assert window.session is not None
    assert window.measure_table.rowCount() > 0

    window.measure_table.selectRow(0)
    window.refresh_edit_combos()
    note_id = window.note_combo.currentData()
    assert note_id is not None
    pid, mi = window.current_key()
    note = window.session.working.part(pid).measures[mi].note(note_id)
    window.duration_spin.setValue(note.duration_ticks - 120)
    window.on_edit_duration()
    window.on_commit_selected()
    assert (pid, mi) in window.session.committed

    window.on_verify_precision()
    window.on_restore()
    assert window.session is not None
    window.on_play_pause()
    assert window.playback.playing
    window.on_tick()
    window.on_play_pause()
    assert not window.playback.playing
    window.close()
