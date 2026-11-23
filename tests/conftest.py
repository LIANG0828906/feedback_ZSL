from __future__ import annotations

from pathlib import Path

import pytest

from acoustic_workbench.io import SampleRepository
from acoustic_workbench.samplegen import generate_samples

ROOT = Path(__file__).resolve().parents[1]
SAMPLES = ROOT / "samples"


@pytest.fixture(scope="session")
def repository() -> SampleRepository:
    if not (SAMPLES / "manifest.json").exists():
        generate_samples(SAMPLES, force=True)
    return SampleRepository(SAMPLES)


@pytest.fixture(scope="session")
def short_sample(repository: SampleRepository):
    return repository.load("short_single_000")
