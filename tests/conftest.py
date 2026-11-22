import json
import os
import shutil
import sys

import pytest

sys.path.insert(0, os.path.abspath(os.path.join(os.path.dirname(__file__), "..")))

from volview.generator import generate_dataset  # noqa: E402
from volview.model import VolumeModel, VolumeRegistry  # noqa: E402
from volview.snapshot import SnapshotStore  # noqa: E402


@pytest.fixture(scope="session")
def samples_dir(tmp_path_factory):
    directory = tmp_path_factory.mktemp("samples")
    generate_dataset(str(directory))
    return str(directory)


@pytest.fixture()
def volume_dir(samples_dir):
    return os.path.join(samples_dir, "vol_a")


@pytest.fixture()
def model(volume_dir):
    return VolumeModel.load(volume_dir)


@pytest.fixture()
def registry():
    return VolumeRegistry()


@pytest.fixture()
def store(tmp_path):
    return SnapshotStore(str(tmp_path / "snapshots"))


@pytest.fixture()
def truth(samples_dir):
    def load(volume_id):
        with open(os.path.join(samples_dir, volume_id, "truth.json"),
                  encoding="utf-8") as fh:
            return json.load(fh)
    return load


def copy_volume(samples_dir, dst_dir, volume_id="vol_a"):
    dst = os.path.join(dst_dir, volume_id)
    if os.path.exists(dst):
        shutil.rmtree(dst)
    shutil.copytree(os.path.join(samples_dir, volume_id), dst)
    return dst
