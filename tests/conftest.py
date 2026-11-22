from __future__ import annotations

import shutil
from pathlib import Path

import pytest
import torch

from diffrender.generator import generate_dataset
from diffrender.schema import Dataset, load_dataset


@pytest.fixture(scope="session", autouse=True)
def deterministic_torch() -> None:
    torch.set_num_threads(1)


@pytest.fixture(scope="session")
def dataset(tmp_path_factory: pytest.TempPathFactory) -> Dataset:
    root = tmp_path_factory.mktemp("dataset") / "data"
    generate_dataset(root, seed=20261123, force=False)
    return load_dataset(root)


@pytest.fixture()
def dataset_copy(dataset: Dataset, tmp_path: Path) -> Path:
    target = tmp_path / "copy"
    shutil.copytree(dataset.root, target)
    return target

