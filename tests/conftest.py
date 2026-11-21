from __future__ import annotations

import pytest

from mcsync.models import (
    CompensationConfig,
    MarkerConfig,
    PipelineConfig,
    SegmentationConfig,
)
from mcsync.pipeline import SyncPipeline
from mcsync.synth import SyntheticConfig, generate_synthetic_audio


@pytest.fixture(scope="session")
def synthetic():
    return generate_synthetic_audio(SyntheticConfig(duration_seconds=4.0))


@pytest.fixture(scope="session")
def pipeline_config() -> PipelineConfig:
    return PipelineConfig(
        marker=MarkerConfig(
            period_seconds=0.5,
            pulse_duration_samples=96,
        ),
        segmentation=SegmentationConfig(segment_length_samples=48000),
        compensation=CompensationConfig(
            mode="linear",
            tolerance_samples=2.0,
        ),
    )


@pytest.fixture(scope="session")
def processed(synthetic, pipeline_config):
    return SyncPipeline(pipeline_config).analyze(synthetic.audio)

