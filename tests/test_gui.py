import os

import pytest

pytest.importorskip("PySide6")
os.environ.setdefault("QT_QPA_PLATFORM", "offscreen")

from PySide6.QtWidgets import QApplication  # noqa: E402

from gui import MainWindow  # noqa: E402


@pytest.fixture(scope="module")
def qapp():
    app = QApplication.instance() or QApplication([])
    yield app


def test_gui_offscreen_full_run(qapp, samples_dir, tmp_path):
    win = MainWindow(samples_root=samples_dir,
                     persist_root=str(tmp_path / "ckpt_gui"))
    assert win.project_list.count() == 3
    assert "台词 8 句" in win.summary_label.text()
    win.start_run()
    assert win._worker is not None
    assert win._worker.wait(30000)
    qapp.processEvents()
    assert win.progress.value() == win.progress.maximum() == 8
    win.close()
