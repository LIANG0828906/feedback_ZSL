from __future__ import annotations

import numpy as np

from inverse_imaging.graph import DependencyGraph, IncrementalEvaluator
from inverse_imaging.params import GROUPS
from inverse_imaging.reduction import compute_gradients, gradient_byte_identity


def test_dependency_graph_affected_sets() -> None:
    graph = DependencyGraph()
    assert graph.affected_nodes(("material",)) == ("shading", "loss")
    assert graph.affected_nodes(("light",)) == (
        "shadow_factor",
        "shading",
        "loss",
    )
    assert graph.affected_nodes(("geometry",)) == (
        "intersections",
        "visibility",
        "shadow_factor",
        "shading",
        "loss",
    )
    assert graph.affected_nodes(("camera",)) == (
        "camera_rays",
        "intersections",
        "visibility",
        "shadow_factor",
        "shading",
        "loss",
    )


def test_incremental_reuses_unaffected_stages(scene) -> None:
    evaluator = IncrementalEvaluator(scene)
    first = evaluator.evaluate(
        scene.params,
        modified_groups=GROUPS,
        chunk_size=4,
        parallel=2,
    )
    assert set(first.recomputed_nodes) == {
        "camera_rays",
        "intersections",
        "visibility",
        "shadow_factor",
        "shading",
        "loss",
    }
    scene.params.values["material"]["ambient"] += 0.002
    scene.params.bump(["material"])
    second = evaluator.evaluate(
        scene.params,
        modified_groups=("material",),
        chunk_size=4,
        parallel=2,
    )
    assert set(second.affected_nodes) == {"shading", "loss"}
    assert set(second.reused_nodes) == {
        "camera_rays",
        "intersections",
        "visibility",
        "shadow_factor",
    }
    assert set(second.recomputed_nodes) == {"shading", "loss"}
    assert second.verification["equivalent"] is True


def test_stale_upstream_cache_is_denied_and_full_recompute_matches(scene) -> None:
    evaluator = IncrementalEvaluator(scene)
    evaluator.evaluate(
        scene.params,
        modified_groups=GROUPS,
        chunk_size=4,
        parallel=1,
    )
    scene.params.values["material"]["ambient"] += 0.003
    scene.params.bump(["material"])
    denied = evaluator.evaluate(
        scene.params,
        modified_groups=(),
        chunk_size=4,
        parallel=1,
    )
    assert denied.degraded is True
    assert denied.downgrade_reason.startswith("stale_dependency:shading:")
    fresh = compute_gradients(scene.params, scene, chunk_size=4, parallel=1)
    assert gradient_byte_identity(denied, fresh)


def test_cache_content_hash_mismatch_is_denied(scene) -> None:
    evaluator = IncrementalEvaluator(scene)
    evaluator.evaluate(
        scene.params,
        modified_groups=GROUPS,
        chunk_size=4,
        parallel=1,
    )
    key = next(
        key for key, _ in sorted(evaluator._cache.items()) if key[1] == "camera_rays"
    )
    entry = evaluator._cache[key]
    origins, directions = entry.value
    corrupted = directions.copy()
    corrupted.flat[0] += 0.25
    entry.value = (origins.copy(), corrupted)
    scene.params.values["light"]["intensity"] += 0.01
    scene.params.bump(["light"])
    denied = evaluator.evaluate(
        scene.params,
        modified_groups=("light",),
        chunk_size=4,
        parallel=1,
    )
    assert denied.degraded is True
    assert denied.downgrade_reason.startswith("cache_hash_mismatch:camera_rays:")
    fresh = compute_gradients(scene.params, scene, chunk_size=4, parallel=1)
    assert gradient_byte_identity(denied, fresh)


def test_unchanged_gradient_chunks_are_reused(scene) -> None:
    evaluator = IncrementalEvaluator(scene)
    first = evaluator.evaluate(
        scene.params,
        modified_groups=GROUPS,
        chunk_size=4,
        parallel=2,
    )
    second = evaluator.evaluate(
        scene.params,
        modified_groups=(),
        chunk_size=4,
        parallel=2,
    )
    assert second.recomputed_nodes == ()
    assert set(second.reused_nodes) == {
        "camera_rays",
        "intersections",
        "visibility",
        "shadow_factor",
        "shading",
        "loss",
    }
    assert second.gradients_sha256 == first.gradients_sha256
    assert second.loss == first.loss
    assert second.verification["equivalent"] is True

