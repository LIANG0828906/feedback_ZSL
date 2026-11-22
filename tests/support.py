from __future__ import annotations

import contextlib
import io
from pathlib import Path
from typing import Iterator

from mshdr.dataset import DatasetIndex
from mshdr.samplegen import generate_dataset


def generate_single_scene(root: Path) -> DatasetIndex:
    dataset_path = root / "samples" / "dataset.json"
    with contextlib.redirect_stdout(io.StringIO()):
        generate_dataset(dataset_path.parent, scene_count=1)
    return DatasetIndex.load(dataset_path)


@contextlib.contextmanager
def quiet() -> Iterator[None]:
    with contextlib.redirect_stdout(io.StringIO()):
        yield

