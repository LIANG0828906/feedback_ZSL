from __future__ import annotations

from pathlib import Path

import pytest

from inverse_render.generate import generate_samples
from inverse_render.graph import build_dependency_graph
from inverse_render.params import ParameterStore, load_group_specs
from inverse_render.scene import load_scene


@pytest.fixture(scope="session")
def sample_root(tmp_path_factory: pytest.TempPathFactory) -> Path:
    root = tmp_path_factory.mktemp("samples")
    generate_samples(root, seed=24680)
    return root


@pytest.fixture
def load_sample(sample_root: Path):
    def loader(name: str = "coupled_small"):
        sample = sample_root / name
        scene = load_scene(sample / "scene.json")
        groups = load_group_specs(scene, sample / "groups.json")
        store = ParameterStore(scene, groups)
        graph = build_dependency_graph(scene, groups)
        return sample, scene, groups, store, graph

    return loader

