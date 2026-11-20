from __future__ import annotations

from pathlib import Path

import pytest

from mesh_repair.samples import generate_sample_bundle


@pytest.fixture(scope="session")
def sample_bundle(tmp_path_factory: pytest.TempPathFactory) -> dict:
    target = tmp_path_factory.mktemp("samples")
    return generate_sample_bundle(target)
