from __future__ import annotations

import os

import pytest

os.environ.setdefault("QT_QPA_PLATFORM", "offscreen")

pytest.importorskip("PyQt5")

from PyQt5.QtCore import QEventLoop, QTimer
from PyQt5.QtWidgets import QApplication

from mocap_workbench.gui.app import WorkbenchWindow
from mocap_workbench.samples import SampleSpec, generate_sample


def test_gui_background_solve_and_residual_panel(tmp_path):
    sample = generate_sample(
        tmp_path / "gui",
        SampleSpec(
            name="gui",
            seed=8100,
            frames=24,
            joints=16,
            missing_rate=0.1,
        ),
    )
    app = QApplication.instance() or QApplication([])
    window = WorkbenchWindow(
        bvh_path=sample["bvh"],
        constraints_path=sample["constraints"],
        snapshot_dir=str(tmp_path / "snapshots"),
    )
    window.chunk_spin.setValue(12)
    window.workers_spin.setValue(1)
    loop = QEventLoop()
    window.start_solve()
    window.worker.completed.connect(lambda _report: QTimer.singleShot(50, loop.quit))
    QTimer.singleShot(15000, loop.quit)
    loop.exec_()
    assert window.report is not None
    assert window.report.status == "completed"
    assert window.range_label.text() == "committed: 0-23"
    assert window.residual_table.rowCount() > 0
    first_stats = window.report.cache_stats
    tolerance_widget = window.constraint_table.cellWidget(0, 4)
    tolerance_widget.setValue(tolerance_widget.value() + 0.001)
    window.apply_constraints()
    second_loop = QEventLoop()
    window.start_solve()
    window.worker.completed.connect(
        lambda _report: QTimer.singleShot(50, second_loop.quit)
    )
    QTimer.singleShot(15000, second_loop.quit)
    second_loop.exec_()
    assert window.report is not None
    assert window.report.status == "completed"
    assert window.report.cache_stats["hits"] == 24
    assert first_stats["hits"] == 0
    app.processEvents()
    window.close()

