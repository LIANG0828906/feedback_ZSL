import pytest

from syncdemo import generator
from syncdemo.schema import load_sample


@pytest.fixture(scope="session")
def sample_dir(tmp_path_factory):
    path = tmp_path_factory.mktemp("sample") / "sample"
    return generator.generate(outdir=path, ticks=60)


@pytest.fixture()
def sample(sample_dir):
    return load_sample(sample_dir)
