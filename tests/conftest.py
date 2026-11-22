import os
import sys

import pytest

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

from a11y_media.generator import generate_all
from a11y_media.loader import load_project


@pytest.fixture(scope="session")
def samples_dir(tmp_path_factory):
    root = tmp_path_factory.mktemp("samples")
    generate_all(str(root), count=4, base_seed=42)
    return str(root)


@pytest.fixture(scope="session")
def projects(samples_dir):
    return [
        load_project(os.path.join(samples_dir, d))
        for d in sorted(os.listdir(samples_dir))
    ]


@pytest.fixture(scope="session")
def project(projects):
    return projects[0]
