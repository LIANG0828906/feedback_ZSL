import os
import sys

import pytest

sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.abspath(__file__))))

from jitter_workbench.sample_gen import generate_sample  # noqa: E402
from jitter_workbench.loader import load_sample  # noqa: E402
from jitter_workbench.controller import ControllerParams  # noqa: E402


@pytest.fixture(scope="session")
def sample_dir(tmp_path_factory):
    d = tmp_path_factory.mktemp("samples") / "voice_seed42"
    generate_sample(str(d))
    return str(d)


@pytest.fixture(scope="session")
def sample(sample_dir):
    return load_sample(sample_dir)


@pytest.fixture()
def params():
    return ControllerParams()


@pytest.fixture()
def ckpt_dir(tmp_path):
    d = tmp_path / "ckpt"
    os.makedirs(str(d), exist_ok=True)
    return str(d)
