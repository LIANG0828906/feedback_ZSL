from __future__ import annotations

import json
from pathlib import Path

import pytest

from npr_pipeline.params import RenderParams
from npr_pipeline.samples import generate_samples
from npr_pipeline.scene import Scene, load_scene


@pytest.fixture(scope="session")
def sample_root(tmp_path_factory: pytest.TempPathFactory) -> Path:
    root = tmp_path_factory.mktemp("npr-samples")
    generate_samples(root, seed=20261122)
    return root


@pytest.fixture(scope="session")
def balanced_scene(sample_root: Path) -> Scene:
    return load_scene(sample_root / "balanced" / "scene.json")


@pytest.fixture(scope="session")
def balanced_params(sample_root: Path) -> RenderParams:
    raw = json.loads(
        (sample_root / "balanced" / "params.json").read_text(encoding="utf-8")
    )
    return RenderParams.from_dict(raw)

