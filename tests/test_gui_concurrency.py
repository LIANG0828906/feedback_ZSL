from __future__ import annotations

import json
import threading
import time
from pathlib import Path

import pytest
from PyQt5.QtWidgets import QApplication

from studio_calibrator.config import load_config, parse_config
from studio_calibrator.engine import AnalysisEngine, RecomputeManager
from studio_calibrator.errors import E_BUSY
from studio_calibrator.gui import WorkbenchWindow
from studio_calibrator.snapshot import SnapshotStore


@pytest.fixture(scope="module")
def app() -> QApplication:
    return QApplication.instance() or QApplication([])


def test_gui_renders_all_bands_and_recomputes_in_background(
    app: QApplication,
    sample: Path,
    baseline: dict,
    tmp_path: Path,
) -> None:
    window = WorkbenchWindow(
        sample / "manifest.json",
        sample / "geometry.json",
        sample / "config.json",
        tmp_path / "gui_snapshots",
    )
    window.state = baseline["state"]
    window.refresh_views()
    for index in range(window.band_selector.count()):
        window.band_selector.setCurrentIndex(index)
        app.processEvents()
        assert window.route_table.rowCount() == 6
        assert window.budget_table.rowCount() == 30
    window.start_recompute()
    assert window.worker is not None
    assert window.worker.wait(30000)
    for _ in range(10):
        app.processEvents()
        time.sleep(0.01)
    assert window.state is not None
    assert window.state.status == "ok"
    committed_hash = window.state.content_hash_value
    assert window.start_button.isEnabled()
    window.close()
    restored_window = WorkbenchWindow(
        sample / "manifest.json",
        sample / "geometry.json",
        sample / "config.json",
        tmp_path / "gui_snapshots",
    )
    assert restored_window.state is not None
    assert restored_window.state.content_hash_value == committed_hash
    restored_window.close()


def test_duplicate_submit_returns_busy_and_cancel_retains_committed(
    sample: Path, baseline: dict, tmp_path: Path
) -> None:
    manager = RecomputeManager(
        AnalysisEngine(sample / "manifest.json", sample / "geometry.json"),
        SnapshotStore(tmp_path / "snapshots_busy"),
    )
    base_config = load_config(sample / "config.json")
    first = manager.recompute(base_config, tmp_path / "trace_base.jsonl")
    raw = json.loads((sample / "config.json").read_text(encoding="utf-8"))
    raw["target_suppression_db"] = 13.0
    changed = parse_config(raw)
    failures = []

    def run_second() -> None:
        try:
            manager.recompute(changed, tmp_path / "trace_second.jsonl")
        except Exception as exc:
            failures.append(exc)

    thread = threading.Thread(target=run_second)
    thread.start()
    deadline = time.monotonic() + 5.0
    while not manager.busy and time.monotonic() < deadline:
        time.sleep(0.005)
    assert manager.busy
    with pytest.raises(Exception) as caught:
        manager.recompute(changed, tmp_path / "trace_duplicate.jsonl")
    assert getattr(caught.value, "code", None) == E_BUSY
    assert manager.cancel()
    thread.join(timeout=30)
    assert not thread.is_alive()
    assert failures
    assert getattr(failures[0], "code", None) == "E_CANCELLED"
    assert manager.query().content_hash_value == first.content_hash_value
