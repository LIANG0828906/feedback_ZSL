from __future__ import annotations

import os
from pathlib import Path

import pytest

os.environ.setdefault("QT_QPA_PLATFORM", "offscreen")

from filter_studio.samples import generate_samples


@pytest.fixture(scope="session")
def sample_bundle(tmp_path_factory: pytest.TempPathFactory) -> tuple[Path, dict]:
    directory = tmp_path_factory.mktemp("samples")
    manifest = generate_samples(directory)
    return directory, manifest


@pytest.fixture(scope="session")
def sample_by_mode(sample_bundle: tuple[Path, dict]) -> dict[str, dict]:
    _, manifest = sample_bundle
    return {sample["mode"]: sample for sample in manifest["samples"]}


@pytest.fixture(scope="session")
def qt_app():
    from PySide6.QtWidgets import QApplication

    app = QApplication.instance() or QApplication([])
    yield app

