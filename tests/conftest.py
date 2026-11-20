from __future__ import annotations

import json
from pathlib import Path

import pytest

from studio_calib.samplegen import generate_dataset


@pytest.fixture(scope="session")
def sample_dataset(tmp_path_factory: pytest.TempPathFactory) -> dict:
    root = tmp_path_factory.mktemp("deterministic-samples")
    manifest = generate_dataset(root, seed=20261120)
    return {
        "root": root,
        "surround": root / "surround51.wav",
        "mono": root / "mono.wav",
        "manifest": manifest,
        "truth": manifest["truth"],
    }


@pytest.fixture()
def cache_dir(tmp_path: Path) -> Path:
    return tmp_path / "cache"


@pytest.fixture()
def report_dir(tmp_path: Path) -> Path:
    path = tmp_path / "reports"
    path.mkdir()
    return path

