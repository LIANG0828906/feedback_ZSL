from __future__ import annotations

import pytest

from inverse_imaging.samples import generate_sample
from inverse_imaging.scene import load_scene


@pytest.fixture(scope="session")
def sample_path(tmp_path_factory: pytest.TempPathFactory):
    root = tmp_path_factory.mktemp("sample")
    result = generate_sample(
        root / "scene",
        seed=19,
        width=12,
        height=9,
        sphere_count=2,
        softness=0.075,
        noise=0.002,
    )
    return result


@pytest.fixture()
def scene(sample_path):
    return load_scene(sample_path["scene"])

