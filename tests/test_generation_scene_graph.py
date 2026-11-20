from __future__ import annotations

import json
from pathlib import Path

import numpy as np
import pytest

from inverse_render.errors import ErrorCode, ToolError
from inverse_render.engine import IncrementalEngine
from inverse_render.cli import main as cli_main
from inverse_render.generate import PRESETS, generate_samples
from inverse_render.graph import DependencyGraph, Edge, build_dependency_graph
from inverse_render.params import ParameterStore, resolve_group_specs


def _all_files(root: Path) -> dict[str, bytes]:
    return {
        str(path.relative_to(root)): path.read_bytes()
        for path in sorted(root.rglob("*"))
        if path.is_file()
    }


def test_cli_env_check_emits_json(capsys) -> None:
    assert cli_main(["env-check"]) == 0
    payload = json.loads(capsys.readouterr().out)
    assert payload["torch_cpu_only"] is True
    assert payload["writable_cwd"] is True


def test_generation_is_byte_deterministic(tmp_path: Path) -> None:
    first = generate_samples(tmp_path / "a", seed=991)
    second = generate_samples(tmp_path / "b", seed=991)
    assert first == second
    assert _all_files(tmp_path / "a") == _all_files(tmp_path / "b")
    assert [entry["resolution"] for entry in first["samples"]] == [
        [preset["width"], preset["height"]] for preset in PRESETS
    ]
    assert [entry["group_count"] for entry in first["samples"]] == [4, 6, 8]
    for entry in first["samples"]:
        target = np.load(tmp_path / "a" / entry["sample"] / "target.npy")
        height, width = entry["resolution"][1], entry["resolution"][0]
        assert target.shape == (height, width, 3)
        assert np.all(np.isfinite(target))
        assert float(np.max(target)) > 0.0


def test_scene_groups_and_impact_sets(load_sample) -> None:
    _, scene, groups, _, graph = load_sample("coupled_small")
    assert [group.id for group in groups] == ["camera", "geometry", "material", "light"]
    assert [group.kind for group in groups] == [
        "camera",
        "geometry",
        "material",
        "light",
    ]
    assert graph.impacted_nodes(["material"]) == ("lighting", "loss")
    assert graph.impacted_nodes(["light"]) == (
        "shadow_factor",
        "lighting",
        "loss",
    )
    assert graph.impacted_nodes(["camera"]) == (
        "intersections",
        "visibility",
        "shadow_factor",
        "lighting",
        "loss",
    )
    assert graph.upstream_groups("shadow_factor") == (
        "camera",
        "geometry",
        "light",
    )
    assert graph.version
    assert graph.structure_hash == scene.structure_hash()


def test_graph_diff_reports_added_and_deleted_edges(load_sample) -> None:
    _, scene, category_groups, _, category_graph = load_sample("coupled_small")
    object_groups = resolve_group_specs(
        scene,
        {
            "schema_version": 1,
            "groups": [
                {"id": "camera", "kind": "camera", "parameters": ["camera.*"]},
                {"id": "geometry.a", "kind": "geometry", "parameters": ["geometry.primary.*"]},
                {"id": "geometry.b", "kind": "geometry", "parameters": ["geometry.accent.*"]},
                {"id": "geometry.floor", "kind": "geometry", "parameters": ["geometry.floor.*"]},
                {"id": "material.a", "kind": "material", "parameters": ["material.primary.*"]},
                {"id": "material.b", "kind": "material", "parameters": ["material.accent.*"]},
                {"id": "material.floor", "kind": "material", "parameters": ["material.floor.*"]},
                {"id": "light", "kind": "light", "parameters": ["light.*"]},
            ],
        },
    )
    object_graph = build_dependency_graph(scene, object_groups)
    diff = object_graph.diff(category_graph)
    assert diff.added
    assert diff.deleted
    assert diff.graph_version_before == category_graph.version
    assert diff.graph_version_after == object_graph.version
    encoded = diff.to_ordered()
    assert list(encoded) == [
        "graph_version_before",
        "graph_version_after",
        "added",
        "deleted",
        "changed",
    ]

    engine = IncrementalEngine(
        scene, ParameterStore(scene, category_groups), category_graph
    )
    engine.evaluate()
    new_store = ParameterStore(scene, object_groups)
    reconfigure_diff = engine.reconfigure(scene, object_groups, new_store)
    assert reconfigure_diff.added
    assert engine.cache == {}
    assert engine.evaluate().changed_groups == ()


def test_cycle_is_rejected_as_graph_invalid(load_sample) -> None:
    _, scene, groups, _, graph = load_sample("coupled_small")
    cyclic_edges = list(graph.edges) + [
        Edge("loss", "group:camera", "data", "injected cycle")
    ]
    with pytest.raises(ToolError) as caught:
        DependencyGraph(
            nodes=graph.nodes,
            edges=cyclic_edges,
            group_specs=groups,
            structure_hash=scene.structure_hash(),
        )
    assert caught.value.code == ErrorCode.E_GRAPH_INVALID
    assert "cycle" in caught.value.message


def test_invalid_or_incomplete_group_configuration_is_rejected(load_sample) -> None:
    _, scene, _, _, _ = load_sample("coupled_small")
    with pytest.raises(ToolError) as caught:
        resolve_group_specs(
            scene,
            {
                "schema_version": 1,
                "groups": [
                    {
                        "id": "broken",
                        "kind": "camera",
                        "parameters": ["camera.does_not_exist"],
                    }
                ],
            },
        )
    assert caught.value.code == ErrorCode.E_GRAPH_INVALID

    with pytest.raises(ToolError) as caught:
        resolve_group_specs(
            scene,
            {
                "schema_version": 1,
                "groups": [
                    {
                        "id": "camera",
                        "parameters": ["camera.*"],
                    }
                ],
            },
        )
    assert caught.value.code == ErrorCode.E_GRAPH_INVALID
    assert "assigned" in caught.value.message

