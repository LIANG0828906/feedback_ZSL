from __future__ import annotations

from pathlib import Path

import pytest

from spatial_workbench.analysis import analyze_sample
from spatial_workbench.io import load_sample
from spatial_workbench.models import AnalysisConfig
from spatial_workbench.samplegen import generate_sample_bundle


@pytest.fixture(scope="session")
def sample_bundle(tmp_path_factory: pytest.TempPathFactory):
    directory = tmp_path_factory.mktemp("samples")
    return generate_sample_bundle(
        directory,
        seed=12_345,
        sample_rate=16_000,
        length=8_192,
        layout_name="quad",
    )


@pytest.fixture(scope="session")
def normal_sample(sample_bundle):
    return load_sample(sample_bundle.normal)


@pytest.fixture()
def config() -> AnalysisConfig:
    return AnalysisConfig(
        frame_length=512,
        hop_length=256,
        energy_threshold=0.005,
        max_workers=4,
        max_buffer_bytes=4 * 1024 * 1024,
    )


@pytest.fixture(scope="session")
def analyzed(normal_sample):
    config = AnalysisConfig(
        frame_length=512,
        hop_length=256,
        energy_threshold=0.005,
        max_workers=2,
        max_buffer_bytes=4 * 1024 * 1024,
    )
    context, frames, truth = analyze_sample(normal_sample, config)
    return context, frames, truth

