from __future__ import annotations

from dataclasses import replace

from npr_pipeline.geometry import segment_segment_distance
from npr_pipeline.strokes import (
    generate_outline,
    generate_shadow,
    generate_smear,
)


def test_three_layers_have_density_modulated_smear(balanced_scene, balanced_params) -> None:
    outline = generate_outline(balanced_scene, balanced_params)
    shadow = generate_shadow(
        balanced_scene,
        balanced_params,
        outline.strokes_by_zone,
    )
    smear = generate_smear(
        balanced_scene,
        balanced_params,
        shadow.strokes_by_zone,
    )
    assert outline.stroke_count > 0
    assert shadow.stroke_count > 0
    assert smear.stroke_count > 0
    assert shadow.excluded_count > 0
    opacities = {
        stroke.opacity
        for strokes in smear.strokes_by_zone.values()
        for stroke in strokes
    }
    assert len(opacities) > 5
    assert min(opacities) < max(opacities)


def test_shadow_strokes_avoid_outline_capsules(balanced_scene, balanced_params) -> None:
    outline = generate_outline(balanced_scene, balanced_params)
    shadow = generate_shadow(
        balanced_scene,
        balanced_params,
        outline.strokes_by_zone,
    )
    outline_strokes = [
        stroke for values in outline.strokes_by_zone.values() for stroke in values
    ]
    for shadow_strokes in shadow.strokes_by_zone.values():
        for shadow_stroke in shadow_strokes:
            for outline_stroke in outline_strokes:
                clearance = segment_segment_distance(
                    shadow_stroke.points[0],
                    shadow_stroke.points[1],
                    outline_stroke.points[0],
                    outline_stroke.points[1],
                )
                threshold = (
                    shadow_stroke.width * 0.5
                    + outline_stroke.width * 0.5
                    + balanced_params.shadow.reject_margin
                )
                assert clearance >= threshold - 0.01


def test_same_parameters_produce_identical_stroke_bytes(
    balanced_scene,
    balanced_params,
) -> None:
    first = generate_outline(balanced_scene, balanced_params)
    second = generate_outline(balanced_scene, balanced_params)
    assert [batch.strokes for batch in first.batches] == [
        batch.strokes for batch in second.batches
    ]
    changed = replace(
        balanced_params,
        outline=replace(balanced_params.outline, width=balanced_params.outline.width + 0.5),
    )
    third = generate_outline(balanced_scene, changed)
    assert first.batches[0].strokes[0].to_dict(0) != third.batches[0].strokes[0].to_dict(0)


def test_batches_follow_uniform_grid(balanced_scene, balanced_params) -> None:
    outline = generate_outline(balanced_scene, balanced_params)
    for batch in outline.batches:
        assert batch.batch_id == f"outline:{batch.zone_id}:{batch.chunk_id}"
        for stroke in batch.strokes:
            midpoint = stroke.midpoint
            assert int(midpoint[0] // balanced_params.grid_size) == batch.column
            assert int(midpoint[1] // balanced_params.grid_size) == batch.row

