from __future__ import annotations

from dataclasses import replace

import pytest

from spectral_editor.color import (
    ColorPipeline,
    WHITE_POINTS,
    max_channel_delta,
)
from spectral_editor.errors import BusinessError, E_PIPELINE_UNINIT
from spectral_editor.models import IlluminantSpec


def test_uninitialized_pipeline_returns_business_code(definitions, pipeline_config):
    pipeline = ColorPipeline()
    with pytest.raises(BusinessError) as caught:
        pipeline.compute(definitions[0].params, pipeline_config)
    assert caught.value.code == E_PIPELINE_UNINIT


def test_pipeline_outputs_both_targets_and_intermediates(definitions, pipeline_config):
    pipeline = ColorPipeline.from_config(pipeline_config)
    result = pipeline.compute(definitions[0].params, pipeline_config)
    assert set(result.colors) == {"srgb", "display-p3"}
    assert all(0.0 <= channel <= 1.0 for target in result.colors.values() for channel in target)
    assert len(result.intermediates["source_xyz"]) == 3
    assert len(result.intermediates["adaptation"]["matrix"]) == 9
    assert (
        result.intermediates["spectral_normalization"]["original_sample_count"]
        == definitions[0].params["spectrum"]["original_sample_count"]
    )


def test_display_srgb_round_trip_for_same_profile(definitions, pipeline_config):
    pipeline = ColorPipeline.from_config(pipeline_config)
    display = next(item for item in definitions if item.material_id == "display-srgb-blueprint")
    result = pipeline.compute(display.params, pipeline_config)
    assert result.colors["srgb"] == pytest.approx(display.params["encoded"], abs=1e-10)


def test_adaptation_and_white_point_change_creates_nonzero_delta(
    definitions, pipeline_config
):
    pipeline = ColorPipeline.from_config(pipeline_config)
    spectral = next(item for item in definitions if item.kind == "spectral")
    baseline = pipeline.compute(spectral.params, pipeline_config)
    changed_config = replace(
        pipeline_config,
        white_point_name="D50",
        white_point_xyz=WHITE_POINTS["D50"],
        adaptation="none",
    )
    changed = pipeline.compute(spectral.params, changed_config)
    delta = max_channel_delta(baseline.colors, changed.colors)
    assert delta["srgb"] > 0.001
    assert delta["display-p3"] > 0.001


def test_observer_and_illuminant_affect_spectral_but_not_display(
    definitions, pipeline_config
):
    pipeline = ColorPipeline.from_config(pipeline_config)
    spectral = next(item for item in definitions if item.kind == "spectral")
    display = next(item for item in definitions if item.kind == "display")
    spectral_base = pipeline.compute(spectral.params, pipeline_config)
    display_base = pipeline.compute(display.params, pipeline_config)
    changed = replace(
        pipeline_config,
        observer="10deg",
        illuminant=IlluminantSpec("Blackbody-5000K", 5000.0),
    )
    spectral_after = pipeline.compute(spectral.params, changed)
    display_after = pipeline.compute(display.params, changed)
    assert spectral_after.dependencies["observer"] != spectral_base.dependencies["observer"]
    assert spectral_after.dependencies["illuminant"] != spectral_base.dependencies["illuminant"]
    assert display_after.dependencies == display_base.dependencies
    assert display_after.colors == display_base.colors
