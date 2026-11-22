import shutil
import sys
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from ocr_pipeline.generator import generate_dataset


@pytest.fixture()
def dataset(tmp_path):
    data_dir = tmp_path / "data"
    generate_dataset(data_dir, seed=42, num_images=6)
    return data_dir


@pytest.fixture()
def fresh_copy(dataset, tmp_path):
    """A byte-identical copy of the dataset for parallel-run comparison."""
    clone = tmp_path / "data_clone"
    shutil.copytree(dataset, clone, ignore=shutil.ignore_patterns(
        "checkpoints", "exports"))
    return clone
