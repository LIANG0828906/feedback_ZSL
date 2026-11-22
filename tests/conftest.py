import os
import sys

import pytest

sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "src"))

from subpipeline.loader import load_project
from subpipeline.samples import generate_all
from subpipeline.truth import build_ground_truth
from subpipeline.samples import write_truth


@pytest.fixture(scope="session")
def samples_root(tmp_path_factory):
    root = tmp_path_factory.mktemp("samples")
    generate_all(str(root), seed=20261123)
    for entry in _project_dirs(root):
        project, _m, _w, _h = load_project(entry)
        try:
            write_truth(entry, build_ground_truth(project))
        except Exception:
            pass
    return str(root)


def _project_dirs(root):
    return [os.path.join(str(root), name) for name in sorted(os.listdir(root))
            if name.startswith("proj_")]


@pytest.fixture
def main_project(samples_root):
    return load_project(os.path.join(samples_root, "proj_main_20261123"))[0]


@pytest.fixture
def narrow_project(samples_root):
    return load_project(os.path.join(samples_root, "proj_narrow_20261124"))[0]


@pytest.fixture
def runs_root(tmp_path):
    return str(tmp_path / "runs")
