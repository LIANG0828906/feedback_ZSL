import json
import os
import shutil
import sys

import pytest

sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.abspath(__file__))))

import samplegen  # noqa: E402

SEED = 20261123


@pytest.fixture(scope="session")
def samples_dir(tmp_path_factory):
    out = tmp_path_factory.mktemp("samples")
    samplegen.generate(str(out), seed=SEED, n_projects=3)
    return str(out)


@pytest.fixture()
def manifest_path(samples_dir):
    return os.path.join(samples_dir, "proj_0", "manifest.json")


@pytest.fixture()
def persist_dir(tmp_path):
    return str(tmp_path / "ckpt")


def load_manifest(path):
    with open(path, encoding="utf-8") as fh:
        return json.load(fh)


def write_manifest(directory, data, audio_from=None):
    os.makedirs(directory, exist_ok=True)
    if audio_from:
        shutil.copy(audio_from, os.path.join(directory, data["audio_file"]))
    path = os.path.join(directory, "manifest.json")
    with open(path, "w", encoding="utf-8") as fh:
        json.dump(data, fh, ensure_ascii=False)
    return path
