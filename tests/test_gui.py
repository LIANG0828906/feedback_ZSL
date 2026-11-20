from __future__ import annotations

import os
from pathlib import Path

os.environ.setdefault("QT_QPA_PLATFORM", "offscreen")

from PyQt5.QtWidgets import QApplication

from conftest import write_test_wav
from game_audio_restorer.gui import RestorationWindow
from game_audio_restorer.wav_io import read_wav


def test_gui_loads_waveform_grid_and_versions(tmp_path: Path) -> None:
    app = QApplication.instance() or QApplication([])
    source = tmp_path / "gui.wav"
    write_test_wav(source, duration=2.1, channels=2)
    window = RestorationWindow()
    audio = read_wav(source)
    window.audio = audio
    window.waveform.set_audio(audio)
    window.file_label.setText(source.name)
    window._populate_segment_rows(audio)
    assert window.table.rowCount() == 3
    assert window.waveform.grab().size().width() > 0

    window.version_combo.setCurrentIndex(2)
    app.processEvents()
    assert window.version_combo.currentData() == 3
    assert window.channel_combo.isEnabled()
    assert window.loudness_spin.isEnabled()
    window.version_combo.setCurrentIndex(0)
    app.processEvents()
    assert not window.channel_combo.isEnabled()
    assert not window.loudness_spin.isEnabled()
    recipe = window._current_recipe()
    assert recipe.version == 1
    window.close()
