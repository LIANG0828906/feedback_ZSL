from __future__ import annotations

import os
from pathlib import Path

import pytest

os.environ.setdefault("QT_QPA_PLATFORM", "offscreen")

from tactical_review.simulation import Simulation
from tactical_review.snapshot import SnapshotStore


def test_workbench_renders_only_published_snapshots(
    tmp_path: Path, sample_dir: Path, loaded_sample
) -> None:
    QtWidgets = pytest.importorskip("PySide6.QtWidgets")
    pytest.importorskip("PySide6.QtCore")
    initial, shots, revocations = loaded_sample
    store = SnapshotStore(tmp_path / "gui-run")
    simulation = Simulation(initial, shots, revocations)
    for _ in range(3):
        simulation.advance()
        store.write(simulation)
    app = QtWidgets.QApplication.instance() or QtWidgets.QApplication([])
    from tactical_review.gui import WorkbenchWindow

    window = WorkbenchWindow(
        str(sample_dir),
        str(store.directory),
        step_delay=0.0,
    )
    try:
        window._refresh(force=True)
        app.processEvents()
        assert window.cover_table.rowCount() == 8
        assert window.step_card.value.text().startswith("03 /")
        assert window.revoke_table.rowCount() == 0
        assert window.fork_table.rowCount() == 3
        assert window.status_chip.text() == "IDLE"
    finally:
        window.close()

