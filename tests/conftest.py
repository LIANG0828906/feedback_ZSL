import copy

import pytest

from scoreforge.generator import generate_project
from scoreforge.model import load_project
from scoreforge.session import EditSession


@pytest.fixture(scope="session")
def project_data():
    return generate_project(42, 0)


@pytest.fixture()
def loaded(project_data):
    score, truth = load_project(copy.deepcopy(project_data))
    return score, truth


@pytest.fixture()
def session(loaded):
    score, _ = loaded
    return EditSession(score)
