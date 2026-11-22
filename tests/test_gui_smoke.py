from __future__ import annotations

import os
from pathlib import Path

import pytest

from terrain_pipeline import BakeParams, Pipeline
from terrain_pipeline.gui import run_gui


def test_gui_offscreen_smoke(project_dir: Path, params: BakeParams, tmp_path: Path) -> None:
    pytest.importorskip("PyQt5")
    cache = tmp_path / "cache"
    assert Pipeline(project_dir, cache, params=params).run(mode="full").success
    os.environ.setdefault("QT_QPA_PLATFORM", "offscreen")
    run_gui(project_dir, cache, params=params, smoke_test=True)
    assert True
