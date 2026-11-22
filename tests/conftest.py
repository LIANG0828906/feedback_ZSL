from __future__ import annotations

import sys
from pathlib import Path

import pytest

ROOT = Path(__file__).resolve().parents[1]
SRC = ROOT / "src"
if str(SRC) not in sys.path:
    sys.path.insert(0, str(SRC))

from edgepipe.samples import generate_samples, load_manifest  # noqa: E402


@pytest.fixture(scope="session")
def repo_root() -> Path:
    return ROOT


@pytest.fixture(scope="session")
def samples_dir(repo_root: Path) -> Path:
    target = repo_root / "samples"
    if not (target / "index.json").exists():
        generate_samples(target, clean=False)
    return target


@pytest.fixture(scope="session")
def manifest(samples_dir: Path):
    return load_manifest(samples_dir / "index.json")


@pytest.fixture(scope="session")
def first_sample(manifest):
    return dict(manifest["samples"][0])

