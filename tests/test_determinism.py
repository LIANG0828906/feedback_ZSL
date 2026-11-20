from __future__ import annotations

from atlas_pipeline.export import export_published
from atlas_pipeline.pipeline import AtlasPipeline
from atlas_pipeline.sample import generate_atlas


def _run_sequence(root):
    atlas = generate_atlas(
        root / "seed.atlas",
        width=40,
        height=24,
        bit_depth=12,
        color_space="display-p3",
        transfer="linear",
        grid_cols=2,
        grid_rows=3,
        source_peak_nits=1000.0,
        seed=991,
    )
    pipeline = AtlasPipeline(atlas)
    pipeline.submit(
        config={"target_color_space": "rec2020", "target_transfer": "pq"},
        tile_updates={1: {"exposure_ev": -0.25, "tone_curve": "hable"}},
    )
    pipeline.submit(tile_updates={4: {"exposure_ev": 0.4, "tone_curve": "reinhard"}})
    pipeline.rollback(2)
    pipeline.submit(
        tile_updates={5: {"exposure_ev": 0.1, "tone_curve": "reinhard"}},
        expected_version=4,
    )
    result = export_published(
        pipeline,
        root / "out.atlas",
        root / "report.json",
    )
    pipeline.write_trace(root / "trace.json")
    snapshot = pipeline.save_snapshot(root / "snapshot.json")
    return result, snapshot


def test_same_seed_and_operation_sequence_is_byte_identical(tmp_path):
    first_result, first_snapshot = _run_sequence(tmp_path / "first")
    second_result, second_snapshot = _run_sequence(tmp_path / "second")
    assert first_result.atlas_sha256 == second_result.atlas_sha256
    assert first_result.report_sha256 == second_result.report_sha256
    assert first_snapshot == second_snapshot
    assert (tmp_path / "first" / "out.atlas").read_bytes() == (
        tmp_path / "second" / "out.atlas"
    ).read_bytes()
    assert (tmp_path / "first" / "report.json").read_bytes() == (
        tmp_path / "second" / "report.json"
    ).read_bytes()
    assert (tmp_path / "first" / "trace.json").read_bytes() == (
        tmp_path / "second" / "trace.json"
    ).read_bytes()
    assert (tmp_path / "first" / "snapshot.json").read_bytes() == (
        tmp_path / "second" / "snapshot.json"
    ).read_bytes()
