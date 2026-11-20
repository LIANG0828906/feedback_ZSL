from __future__ import annotations

import pytest

from atlas_pipeline.pipeline import AtlasPipeline
from atlas_pipeline.sample import generate_atlas


@pytest.fixture
def sample_atlas(tmp_path):
    return generate_atlas(
        tmp_path / "sample.atlas",
        width=32,
        height=24,
        bit_depth=10,
        color_space="rec709",
        transfer="srgb",
        grid_cols=2,
        grid_rows=2,
        source_peak_nits=1000.0,
        seed=23,
    )


@pytest.fixture
def pipeline(sample_atlas):
    return AtlasPipeline(sample_atlas)
