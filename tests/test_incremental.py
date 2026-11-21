from __future__ import annotations

from dataclasses import replace

import pytest

from npr_pipeline.errors import E_COMMIT_FAILED, PipelineError
from npr_pipeline.pipeline import RenderPipeline
from npr_pipeline.storage import Store


def _layer_rewritten(result, layer: int) -> int:
    return sum(
        1
        for item in result.batch_traces
        if item.layer == layer and not item.reused and not item.removed
    )


def _layer_reused(result, layer: int) -> int:
    return sum(1 for item in result.batch_traces if item.layer == layer and item.reused)


def test_incremental_layer_factors(balanced_scene, balanced_params, tmp_path) -> None:
    store = Store(tmp_path / "work")
    pipeline = RenderPipeline(store)
    first = pipeline.render(balanced_scene, balanced_params)
    assert first.committed and first.version == 1
    unchanged = pipeline.render(balanced_scene, balanced_params)
    assert not unchanged.committed
    assert unchanged.affected_batches == 0
    assert unchanged.reused_batches == len(unchanged.manifest["batches"])

    shadow_changed = replace(
        balanced_params,
        shadow=replace(balanced_params.shadow, color="#101010"),
    )
    second = pipeline.render(balanced_scene, shadow_changed)
    assert second.version == 2
    assert _layer_rewritten(second, 0) == 0
    assert _layer_rewritten(second, 1) > 0
    assert _layer_rewritten(second, 2) == 0
    assert _layer_reused(second, 0) > 0
    assert _layer_reused(second, 2) > 0

    smear_changed = replace(
        shadow_changed,
        smear=replace(shadow_changed.smear, width=shadow_changed.smear.width + 0.4),
    )
    third = pipeline.render(balanced_scene, smear_changed)
    assert third.version == 3
    assert _layer_rewritten(third, 0) == 0
    assert _layer_rewritten(third, 1) == 0
    assert _layer_rewritten(third, 2) > 0

    outline_color_changed = replace(
        smear_changed,
        outline=replace(smear_changed.outline, color="#000000"),
    )
    fourth = pipeline.render(balanced_scene, outline_color_changed)
    assert fourth.version == 4
    assert _layer_rewritten(fourth, 0) > 0
    assert _layer_rewritten(fourth, 1) == 0
    assert _layer_rewritten(fourth, 2) == 0

    outline_geometry_changed = replace(
        outline_color_changed,
        outline=replace(
            outline_color_changed.outline,
            width=outline_color_changed.outline.width + 0.2,
        ),
    )
    fifth = pipeline.render(balanced_scene, outline_geometry_changed)
    assert fifth.version == 5
    assert _layer_rewritten(fifth, 0) > 0
    assert _layer_rewritten(fifth, 1) > 0
    assert _layer_rewritten(fifth, 2) > 0
    assert any(
        "outline_culling_closure" in item.reasons
        for item in fifth.batch_traces
        if item.layer == 1
    )
    assert any(
        "shadow_density_closure" in item.reasons
        for item in fifth.batch_traces
        if item.layer == 2
    )


def test_commit_failure_keeps_previous_version(
    balanced_scene,
    balanced_params,
    tmp_path,
) -> None:
    store = Store(tmp_path / "work")
    pipeline = RenderPipeline(store)
    first = pipeline.render(balanced_scene, balanced_params)
    changed = replace(
        balanced_params,
        shadow=replace(balanced_params.shadow, spacing=balanced_params.shadow.spacing + 0.7),
    )
    with pytest.raises(PipelineError) as caught:
        pipeline.render(
            balanced_scene,
            changed,
            fail_after_batches=1,
        )
    assert caught.value.code == E_COMMIT_FAILED
    current = store.current_manifest()
    assert current is not None
    assert current["version"] == first.version
    assert current["content_hash"] == first.version_hash
    assert not list(store.root.rglob(".*.tmp.*"))
    assert not (store.staging_dir / "state.json").exists()

