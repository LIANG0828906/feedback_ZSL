from __future__ import annotations

import json
from pathlib import Path

import pytest

from audio_sync.synthesizer import synthesize_dataset


@pytest.fixture(scope="session")
def drift_dataset(tmp_path_factory) -> dict:
    root = tmp_path_factory.mktemp("drift_sample")
    return synthesize_dataset(
        root,
        duration_seconds=16.0,
        channels=8,
        seed=20261122,
    )


@pytest.fixture(scope="session")
def marker_ground_truth(drift_dataset) -> dict:
    return json.loads(Path(drift_dataset["markers"]).read_text(encoding="utf-8"))[
        "ground_truth"
    ]

