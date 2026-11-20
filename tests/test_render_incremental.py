from __future__ import annotations

from collections import OrderedDict

import pytest
import torch

from inverse_render.engine import GradientResult, IncrementalEngine
from inverse_render.errors import ErrorCode, ToolError
from inverse_render.hashing import tensor_map_hash
from inverse_render.rendering import full_render
from inverse_render.serialization import gradients_to_ordered, render_to_ordered


def test_forward_gradients_and_fixed_order(load_sample) -> None:
    _, scene, _, store, _ = load_sample("coupled_small")
    nodes, loss = full_render(store, scene)
    assert float(loss.detach()) > 0.0
    assert torch.isfinite(loss)
    shadow = nodes["shadow_factor"]["shadow_factor"]
    assert float(shadow.min()) >= 0.0
    assert float(shadow.max()) <= 1.0

    store.zero_grad()
    loss.backward()
    for path, parameter in store.parameters.items():
        assert parameter.grad is not None, path
        assert torch.isfinite(parameter.grad).all(), path

    forward = render_to_ordered(
        scene_name=scene.name,
        resolution=scene.resolution,
        nodes=nodes,
        loss=loss,
    )
    assert list(forward) == [
        "schema_version",
        "scene",
        "resolution",
        "node_order",
        "nodes",
        "loss",
        "loss_hash",
        "content_hash",
    ]
    assert list(forward["nodes"]) == [
        "intersections",
        "visibility",
        "shadow_factor",
        "lighting",
        "loss",
    ]
    gradients = gradients_to_ordered(
        group_order=store.group_order,
        parameter_order=store.parameter_order,
        gradients=store.group_gradients(),
        loss=float(loss.detach()),
    )
    assert list(gradients) == [
        "schema_version",
        "group_order",
        "groups",
        "loss",
        "content_hash",
    ]
    assert gradients["group_order"] == ["camera", "geometry", "material", "light"]
    assert forward["content_hash"] == render_to_ordered(
        scene_name=scene.name,
        resolution=scene.resolution,
        nodes=nodes,
        loss=loss,
    )["content_hash"]


def test_incremental_reuse_matches_full_for_every_group(load_sample) -> None:
    _, scene, _, store, graph = load_sample("coupled_small")
    engine = IncrementalEngine(scene, store, graph)
    initial = engine.evaluate()
    engine.backward(initial)
    assert set(initial.recomputed_nodes) == set(engine.graph.nodes) - set(
        node for node in engine.graph.nodes if node.startswith("group:")
    )

    updates = [
        ("camera", {"camera.position": [0.21, 0.56, 4.05]}, {
            "intersections", "visibility", "shadow_factor", "lighting", "loss"
        }),
        ("geometry", {"geometry.primary.center": [-0.70, -0.03, 0.02]}, {
            "intersections", "visibility", "shadow_factor", "lighting", "loss"
        }),
        ("light", {"light.direction": [-0.36, -0.74, -0.31]}, {
            "shadow_factor", "lighting", "loss"
        }),
        ("material", {"material.primary.albedo": [0.61, 0.27, 0.15]}, {
            "lighting", "loss"
        }),
    ]
    for group_id, values, expected_affected in updates:
        store.update_group(group_id, values)
        evaluation = engine.evaluate()
        assert set(evaluation.affected_nodes) == expected_affected
        assert set(evaluation.recomputed_nodes) == expected_affected
        assert set(evaluation.reused_nodes) == (
            {"intersections", "visibility", "shadow_factor", "lighting", "loss"}
            - expected_affected
        )
        gradient_result = engine.backward(evaluation)
        comparison = engine.assert_full_equivalence(gradient_result)
        assert comparison.matched
        assert comparison.loss_abs_error == 0.0
        assert comparison.max_group_grad_error <= 1e-12


def test_unversioned_change_rejects_cache_and_falls_back(load_sample) -> None:
    _, scene, _, store, graph = load_sample("coupled_small")
    engine = IncrementalEngine(scene, store, graph)
    engine.backward(engine.evaluate())
    with torch.no_grad():
        store.parameters["material.accent.albedo"].add_(0.15)
    evaluation = engine.evaluate()
    assert evaluation.degraded
    assert evaluation.degraded_reason == "upstream_content_changed_without_invalidation"
    assert "lighting" in evaluation.stale_nodes
    assert set(evaluation.recomputed_nodes) == {
        "intersections",
        "visibility",
        "shadow_factor",
        "lighting",
        "loss",
    }
    assert evaluation.reused_nodes == ()
    gradient_result = engine.backward(evaluation)
    assert engine.assert_full_equivalence(gradient_result).matched


def test_reuse_mismatch_reports_node_and_group(load_sample) -> None:
    _, scene, _, store, graph = load_sample("coupled_small")
    engine = IncrementalEngine(scene, store, graph)
    engine.backward(engine.evaluate())
    store.update_group(
        "material", {"material.primary.albedo": [0.5, 0.3, 0.2]}
    )
    evaluation = engine.evaluate()
    gradient_result = engine.backward(evaluation)
    corrupted = OrderedDict(
        (group, OrderedDict((path, value.clone()) for path, value in values.items()))
        for group, values in gradient_result.gradients.items()
    )
    corrupted["material"]["material.primary.albedo"][0] += 0.75
    bad = GradientResult(
        loss=gradient_result.loss,
        gradients=corrupted,
        gradient_norm=gradient_result.gradient_norm,
        evaluation=evaluation,
    )
    with pytest.raises(ToolError) as caught:
        engine.assert_full_equivalence(bad)
    assert caught.value.code == ErrorCode.E_REUSE_MISMATCH
    assert caught.value.details["group"] == "material"
    assert caught.value.details["node"] == "lighting"

