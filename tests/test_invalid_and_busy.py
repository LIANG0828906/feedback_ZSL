from __future__ import annotations

import pytest

from atlas_pipeline.errors import AtlasError, ErrorCode


def test_exposure_overflow_is_rejected_with_tile_and_field(pipeline):
    initial = pipeline.current_version_record
    with pytest.raises(AtlasError) as caught:
        pipeline.submit(
            tile_updates={2: {"exposure_ev": 4.0, "tone_curve": "none"}}
        )
    error = caught.value
    assert error.code == ErrorCode.E_PARAM_CONFLICT
    assert error.tile == 2
    assert error.field == "exposure_ev"
    assert pipeline.current_version_number == initial.number
    assert pipeline.get_preview().version.content_hash == initial.content_hash


def test_parameter_table_must_match_grid(pipeline):
    with pytest.raises(AtlasError) as caught:
        pipeline.submit(tile_params=[{} for _ in range(3)])
    assert caught.value.code == ErrorCode.E_PARAM_CONFLICT
    assert caught.value.field == "tile_params"
    assert caught.value.tile == 3

    with pytest.raises(AtlasError) as caught:
        pipeline.submit(tile_params={0: {}, 1: {}, 2: {}})
    assert caught.value.code == ErrorCode.E_PARAM_CONFLICT
    assert caught.value.field == "tile_params"
    assert caught.value.tile == 3


def test_unknown_tile_and_parameter_are_rejected(pipeline):
    with pytest.raises(AtlasError) as caught:
        pipeline.submit(tile_updates={99: {"exposure_ev": 0.1}})
    assert caught.value.code == ErrorCode.E_PARAM_CONFLICT
    assert caught.value.field == "tile_updates"
    assert caught.value.tile == 99

    with pytest.raises(AtlasError) as caught:
        pipeline.submit(tile_updates={1: {"bogus": 1}})
    assert caught.value.code == ErrorCode.E_PARAM_CONFLICT
    assert caught.value.field == "bogus"
    assert caught.value.tile == 1


def test_expected_version_and_stale_export_guards(pipeline, tmp_path):
    pipeline.submit(tile_updates={0: {"exposure_ev": 0.2, "tone_curve": "reinhard"}})
    with pytest.raises(AtlasError) as caught:
        pipeline.submit(expected_version=1)
    assert caught.value.code == ErrorCode.E_VERSION_STALE
    assert caught.value.field == "expected_version"

    from atlas_pipeline.export import export_published

    with pytest.raises(AtlasError) as caught:
        export_published(
            pipeline,
            tmp_path / "stale.atlas",
            tmp_path / "stale.json",
            expected_version=1,
        )
    assert caught.value.code == ErrorCode.E_VERSION_STALE
    assert caught.value.field == "version"

    with pytest.raises(AtlasError) as caught:
        export_published(
            pipeline,
            tmp_path / "stale2.atlas",
            tmp_path / "stale2.json",
            expected_manifest={0: "bad", 1: "bad", 2: "bad", 3: "bad"},
        )
    assert caught.value.code == ErrorCode.E_VERSION_STALE
    assert caught.value.field == "tile_hashes"
