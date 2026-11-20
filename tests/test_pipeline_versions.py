from __future__ import annotations

import numpy as np
import pytest

from atlas_pipeline.errors import AtlasError, ErrorCode
from atlas_pipeline.pipeline import AtlasPipeline


def test_incremental_submission_only_recomputes_changed_tile(pipeline):
    initial = pipeline.current_version_record
    updated = pipeline.submit(
        tile_updates={1: {"exposure_ev": 0.5, "tone_curve": "reinhard"}},
        expected_version=1,
    )
    assert updated.number == 2
    assert updated.parent_version == 1
    assert updated.parent_chain_hash == initial.chain_hash
    assert updated.affected_tiles == (1,)
    assert updated.reused_tiles == (0, 2, 3)
    assert updated.output_tile_hashes[0] == initial.output_tile_hashes[0]
    assert updated.output_tile_hashes[2] == initial.output_tile_hashes[2]
    assert updated.output_tile_hashes[3] == initial.output_tile_hashes[3]
    assert updated.output_tile_hashes[1] != initial.output_tile_hashes[1]
    check = pipeline.recompute_full_check(2)
    assert check.exact_hash_match
    assert check.maximum_summary_delta == 0.0
    assert check.reused_tiles == (0, 2, 3)
    preview = pipeline.get_preview(2)
    assert preview.version.number == 2
    assert preview.srgb.shape[-1] == 4
    assert np.isfinite(preview.srgb).all()


def test_shared_chain_change_invalidates_every_tile(pipeline):
    initial = pipeline.current_version_record
    updated = pipeline.submit(
        config={
            "target_color_space": "display-p3",
            "target_transfer": "linear",
        }
    )
    assert updated.affected_tiles == tuple(range(4))
    assert updated.reused_tiles == ()
    assert all(
        old != new
        for old, new in zip(initial.output_tile_hashes, updated.output_tile_hashes)
    )
    assert "target_color_space" in pipeline.diff(1, 2).config_changes


def test_lower_target_peak_is_allowed_with_compression_curves(pipeline):
    version = pipeline.submit(
        config={"target_peak_nits": 500.0},
        tile_updates={
            index: {"tone_curve": "reinhard", "tone_strength": 0.8}
            for index in range(pipeline.atlas.tile_count)
        },
    )
    assert version.affected_tiles == tuple(range(pipeline.atlas.tile_count))
    assert pipeline.recompute_full_check(version.number).exact_hash_match


def test_diff_and_rollback_append_new_monotonic_version(pipeline):
    pipeline.submit(tile_updates={1: {"exposure_ev": 0.25, "tone_curve": "hable"}})
    pipeline.submit(tile_updates={2: {"exposure_ev": -0.5, "tone_curve": "reinhard"}})
    rolled = pipeline.rollback(1)
    assert rolled.number == 4
    assert rolled.rollback_of == 1
    assert rolled.parent_version == 3
    assert rolled.tile_params == pipeline.get_version(1).tile_params
    assert rolled.config == pipeline.get_version(1).config
    assert pipeline.current_version_number == 4
    assert rolled.affected_tiles == ()
    assert rolled.reused_tiles == (0, 1, 2, 3)

    followup = pipeline.submit(
        tile_updates={3: {"exposure_ev": 0.75, "tone_curve": "reinhard"}}
    )
    assert followup.number == 5
    assert followup.parent_version == 4
    assert pipeline.diff(4, 5).tile_changes == {
        3: {
            "exposure_ev": {"from": 0.0, "to": 0.75},
            "tone_curve": {"from": "none", "to": "reinhard"},
        }
    }


def test_busy_and_cancel_leave_current_version_untouched(pipeline):
    initial = pipeline.current_version_record
    job = pipeline.begin_submission(
        config={
            "target_color_space": "rec2020",
            "target_transfer": "pq",
        }
    )
    with pytest.raises(AtlasError) as caught:
        pipeline.submit(tile_updates={0: {"tone_curve": "hable"}})
    assert caught.value.code == ErrorCode.E_BUSY
    assert pipeline.cancel_current()
    with pytest.raises(AtlasError) as caught:
        job.run_to_completion()
    assert caught.value.code == ErrorCode.E_CANCELLED
    assert pipeline.current_version_number == 1
    assert pipeline.current_version_record.chain_hash == initial.chain_hash
    assert pipeline.get_preview().version.chain_hash == initial.chain_hash


def test_rejection_preserves_complete_previous_version(pipeline):
    before = pipeline.current_version_record
    with pytest.raises(AtlasError) as caught:
        pipeline.submit(
            config={"target_peak_nits": 500.0},
            tile_updates={
                0: {"tone_curve": "none"},
                1: {"tone_curve": "none"},
                2: {"tone_curve": "none"},
                3: {"tone_curve": "none"},
            },
        )
    assert caught.value.code == ErrorCode.E_PARAM_CONFLICT
    assert caught.value.tile is not None
    assert caught.value.field == "tone_curve"
    assert pipeline.current_version_number == before.number
    assert pipeline.get_preview().version.chain_hash == before.chain_hash


def test_reopen_snapshot_preserves_versions(pipeline, tmp_path):
    pipeline.submit(tile_updates={2: {"exposure_ev": 0.3, "tone_curve": "reinhard"}})
    snapshot = tmp_path / "state.snapshot.json"
    pipeline.save_snapshot(snapshot)
    recovered = AtlasPipeline.from_snapshot(snapshot)
    assert recovered.current_version_number == 2
    assert recovered.current_version_record.chain_hash == pipeline.current_version_record.chain_hash
    assert recovered.recovery_report["resumed_pending"] is False
    assert recovered.recompute_full_check().exact_hash_match
