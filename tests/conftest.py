from __future__ import annotations

import json
from pathlib import Path

import pytest

from formation_workbench.generator import generate_scene
from formation_workbench.validation import load_scene, validate_scene


ROOT = Path(__file__).resolve().parents[1]
SAMPLE_PATH = ROOT / "samples" / "formation_scene.json"


@pytest.fixture(scope="session")
def sample_payload() -> dict:
    if SAMPLE_PATH.exists():
        return json.loads(SAMPLE_PATH.read_text(encoding="utf-8"))
    return generate_scene()


@pytest.fixture()
def sample_scene(sample_payload):
    return validate_scene(sample_payload)


@pytest.fixture()
def sample_engine(sample_scene, tmp_path):
    from formation_workbench.engine import FormationEngine

    engine = FormationEngine(
        sample_scene,
        checkpoint_path=tmp_path / "formation_checkpoint.json",
    )
    result = engine.build_all(parallel=True, save_checkpoint=True)
    assert result["complete"]
    return engine
