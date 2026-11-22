"""GUI smoke test (offscreen): build the window, load, run三路径."""
import os

import pytest

pytest.importorskip("PyQt5")
os.environ.setdefault("QT_QPA_PLATFORM", "offscreen")

from PyQt5.QtWidgets import QApplication  # noqa: E402

from jitter_workbench.gui import MainWindow  # noqa: E402
from jitter_workbench.sample_gen import generate_sample  # noqa: E402


@pytest.fixture(scope="module")
def app():
    return QApplication.instance() or QApplication([])


def test_mainwindow_load_and_render(app, tmp_path):
    root = tmp_path
    sdir = root / "samples" / "voice_seed42"
    sdir.mkdir(parents=True)
    generate_sample(str(sdir))
    (root / "checkpoints").mkdir()
    win = MainWindow(str(root))
    win.sample_combo.setCurrentText("voice_seed42")
    win.on_load()
    assert win.sample is not None
    from jitter_workbench.paths import (run_offline, run_realtime,
                                        run_resume)
    from jitter_workbench.compare import compare_three
    params = win._params()
    ck = str(root / "checkpoints")
    r_off = run_offline(win.sample, params)
    r_rt = run_realtime(win.sample, params, ck)
    r_re = run_resume(win.sample, params, ck, stop_after_windows=137)
    out = {"off": r_off, "rt": r_rt, "re": r_re,
           "report": compare_three(r_rt, r_re, r_off), "params": params}
    win._show_results(out)
    assert win.decision_table.rowCount() == len(r_off.decisions)
    win.wave_view.repaint()
    win.delay_view.repaint()
    win.energy_view.repaint()
