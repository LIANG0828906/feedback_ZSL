from __future__ import annotations

from pathlib import Path

import pytest

from panoworkbench.dataset import Dataset, load_dataset
from panoworkbench.sample_generator import DatasetSpec, generate_dataset


@pytest.fixture(scope="session")
def sample_root(tmp_path_factory: pytest.TempPathFactory) -> Path:
    root = tmp_path_factory.mktemp("samples") / "tiny"
    generate_dataset(
        root,
        DatasetSpec("tiny", 3, 560, 420, 6.0),
        seed=4242,
    )
    return root


@pytest.fixture(scope="session")
def sample_dataset(sample_root: Path) -> Dataset:
    return load_dataset(sample_root)
