from __future__ import annotations

from pathlib import Path

import pytest

from tactical_review.model import InitialState, load_revocations, load_shots
from tactical_review.samplegen import generate_sample


@pytest.fixture(scope="session")
def sample_dir(tmp_path_factory: pytest.TempPathFactory) -> Path:
    directory = tmp_path_factory.mktemp("sample")
    generate_sample(
        directory,
        seed=20261122,
        cover_count=8,
        shot_count=42,
        end_step=40,
        step_ms=50,
        compensation_window=3,
        include_anomalies=True,
    )
    return directory


@pytest.fixture
def loaded_sample(sample_dir: Path):
    initial = InitialState.load(str(sample_dir / "initial_state.json"))
    _, shots = load_shots(str(sample_dir / "shots.json"))
    revocations = load_revocations(str(sample_dir / "revocations.json"))
    return initial, shots, revocations

