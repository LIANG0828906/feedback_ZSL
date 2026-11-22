from __future__ import annotations

import copy
from pathlib import Path

import pytest

from transparent_pipeline.sample_generator import generate_sample
from transparent_pipeline.scene import Scene, load_truth


@pytest.fixture(scope="session")
def sample_bundle(tmp_path_factory: pytest.TempPathFactory) -> dict:
    directory = tmp_path_factory.mktemp("sample")
    manifest = generate_sample(directory)
    scene = Scene.load(directory / "scene.json")
    truth = load_truth(directory / "truth.json", scene)
    return {
        "directory": directory,
        "manifest": manifest,
        "scene": scene,
        "truth": truth,
        "scene_dict": copy.deepcopy(scene.data),
    }


@pytest.fixture()
def scene(sample_bundle: dict) -> Scene:
    return Scene.from_dict(copy.deepcopy(sample_bundle["scene_dict"]))


@pytest.fixture()
def truth(sample_bundle: dict) -> dict:
    return copy.deepcopy(sample_bundle["truth"])

