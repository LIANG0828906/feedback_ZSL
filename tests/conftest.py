from __future__ import annotations

import os
from pathlib import Path
from typing import Dict

import pytest

os.environ.setdefault("QT_QPA_PLATFORM", "offscreen")

from studio_calibrator.config import load_config
from studio_calibrator.engine import AnalysisEngine, RecomputeManager
from studio_calibrator.samplegen import generate_sample
from studio_calibrator.snapshot import SnapshotStore


@pytest.fixture(scope="session")
def sample(tmp_path_factory: pytest.TempPathFactory) -> Path:
    root = tmp_path_factory.mktemp("samples") / "default"
    generate_sample(root, seed=42, duration=1.0)
    return root


@pytest.fixture(scope="session")
def baseline(
    sample: Path, tmp_path_factory: pytest.TempPathFactory
) -> Dict[str, object]:
    root = tmp_path_factory.mktemp("baseline")
    snapshot_dir = root / "snapshots"
    trace_path = root / "trace.jsonl"
    engine = AnalysisEngine(sample / "manifest.json", sample / "geometry.json")
    manager = RecomputeManager(engine, SnapshotStore(snapshot_dir))
    state = manager.recompute(load_config(sample / "config.json"), trace_path)
    return {
        "root": root,
        "snapshot_dir": snapshot_dir,
        "trace_path": trace_path,
        "snapshot": manager.last_snapshot,
        "state": state,
        "manager": manager,
    }
