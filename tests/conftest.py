from __future__ import annotations

import os
from pathlib import Path

import pytest

os.environ.setdefault("QT_QPA_PLATFORM", "offscreen")

from glyph_workbench.models import load_document
from glyph_workbench.samplegen import generate_samples


@pytest.fixture(scope="session")
def sample_paths(tmp_path_factory: pytest.TempPathFactory) -> dict[str, Path]:
    output = tmp_path_factory.mktemp("samples")
    return generate_samples(output)


@pytest.fixture(scope="session")
def document(sample_paths: dict[str, Path]):
    return load_document(sample_paths["samples"])

