from __future__ import annotations

import json
from pathlib import Path

import pytest

from score_engine.sample_generator import generate_samples


@pytest.fixture(scope="session")
def generated(tmp_path_factory: pytest.TempPathFactory) -> Path:
    root = tmp_path_factory.mktemp("generated")
    generate_samples(root, seed=20261122)
    return root


def load_json(path: Path) -> dict:
    with path.open("r", encoding="utf-8") as handle:
        return json.load(handle)

