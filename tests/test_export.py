from __future__ import annotations

import json
from pathlib import Path

import pytest

from atlas_pipeline.errors import AtlasError, ErrorCode
from atlas_pipeline.export import export_published, verify_export


def test_export_reload_recomputes_hashes_and_version(pipeline, tmp_path):
    pipeline.submit(tile_updates={1: {"exposure_ev": 0.5, "tone_curve": "reinhard"}})
    version = pipeline.current_version_record
    result = export_published(
        pipeline,
        tmp_path / "out.atlas",
        tmp_path / "out.report.json",
        expected_version=version.number,
        expected_content_hash=version.content_hash,
        expected_manifest={
            index: digest
            for index, digest in enumerate(version.output_tile_hashes)
        },
    )
    verified = verify_export(tmp_path / "out.atlas", tmp_path / "out.report.json")
    assert verified["ok"]
    assert verified["version"] == version.number
    assert verified["content_hash"] == version.content_hash
    assert verified["atlas_sha256"] == result.atlas_sha256
    report = json.loads((tmp_path / "out.report.json").read_text(encoding="ascii"))
    assert report["atlas_version"] == version.number
    assert report["tile_params"][1]["tone_curve"] == "reinhard"
    assert report["tiles"][1]["max_code_deviation"] >= 0.0
    assert report["tiles"][1]["tile_hash"] == version.output_tile_hashes[1]


def test_export_is_byte_deterministic(pipeline, tmp_path):
    pipeline.submit(tile_updates={2: {"exposure_ev": -0.4, "tone_curve": "hable"}})
    first = export_published(pipeline, tmp_path / "a1.atlas", tmp_path / "r1.json")
    second = export_published(pipeline, tmp_path / "a2.atlas", tmp_path / "r2.json")
    assert first.atlas_sha256 == second.atlas_sha256
    assert first.report_sha256 == second.report_sha256
    assert (tmp_path / "a1.atlas").read_bytes() == (tmp_path / "a2.atlas").read_bytes()
    assert (tmp_path / "r1.json").read_bytes() == (tmp_path / "r2.json").read_bytes()


def test_tampered_export_report_is_stale(pipeline, tmp_path):
    export_published(pipeline, tmp_path / "out.atlas", tmp_path / "out.json")
    report = json.loads((tmp_path / "out.json").read_text(encoding="ascii"))
    report["tiles"][0]["tile_hash"] = "0" * 64
    (tmp_path / "out.json").write_text(
        json.dumps(report, sort_keys=True, indent=2) + "\n",
        encoding="ascii",
    )
    with pytest.raises(AtlasError) as caught:
        verify_export(tmp_path / "out.atlas", tmp_path / "out.json")
    assert caught.value.code == ErrorCode.E_VERSION_STALE


def test_export_records_all_block_parameters_and_errors(pipeline, tmp_path):
    pipeline.submit(tile_updates={3: {"exposure_ev": 1.0, "tone_curve": "reinhard"}})
    export_published(pipeline, tmp_path / "out.atlas", tmp_path / "out.json")
    report = json.loads((tmp_path / "out.json").read_text(encoding="ascii"))
    assert len(report["tiles"]) == pipeline.atlas.tile_count
    assert report["tile_params"] == [
        item.to_dict() for item in pipeline.current_version_record.tile_params
    ]
    for record in report["tiles"]:
        assert record["tile_hash"]
        assert 0.0 <= record["max_code_deviation"] <= 1024.0
        assert record["summary_hash"]
