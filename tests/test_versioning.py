from __future__ import annotations

import pytest

from mcsync.errors import E_VERSION_INVALID
from mcsync.versioning import (
    downgrade_report,
    load_report,
    normalize_version,
    parse_report,
    save_report,
)


def _v1_document() -> dict:
    return {
        "schema_version": "v1",
        "sample_rate": 48000,
        "reference_channel": 0,
        "markers": [
            {
                "channel": channel,
                "marker_index": index,
                "arrival_sample": 12000
                + channel * 3
                + index * (24000 + channel * 5),
            }
            for channel in range(2)
            for index in range(5)
        ],
    }


def test_v1_upgrade_derives_v3_fields_with_provenance():
    report = parse_report(_v1_document())
    assert report.schema_version == "v3"
    assert report.source_version == "v1"
    assert report.segment_delays
    assert report.drift_fits
    assert report.diagnoses
    assert report.provenance["segment_delays"].startswith("v1:derived")
    assert report.provenance["compensation"] == "unavailable_in_v1"


def test_downgrade_lists_every_lost_field():
    report = parse_report(_v1_document())
    v2 = downgrade_report(report, "v2")
    assert v2.document["compatibility"]["code"] == "E_VERSION_LOSS"
    assert "compensation" in v2.document["compatibility"]["lost_fields"]
    assert v2.document["schema_version"] == "v2"
    v1 = downgrade_report(report, "v1")
    assert "segment_delays" in v1.lost_fields
    assert "markers[*].confidence" in v1.lost_fields
    assert all(
        set(marker) == {"channel", "marker_index", "arrival_sample"}
        for marker in v1.document["markers"]
    )


def test_v3_roundtrip_preserves_content_hash(tmp_path):
    report = parse_report(_v1_document())
    path = tmp_path / "report_v3.json"
    save_report(report, path, "v3")
    loaded = load_report(path)
    assert loaded.content_hash() == loaded.integrity["content_sha256"]
    assert loaded.source_version == "v1"
    assert len(loaded.markers) == len(report.markers)


def test_v2_load_upgrades_and_marks_direct_fields(tmp_path):
    report = parse_report(_v1_document())
    path = tmp_path / "report_v2.json"
    save_report(report, path, "v2")
    loaded = load_report(path)
    assert loaded.schema_version == "v3"
    assert loaded.source_version == "v1"
    assert loaded.provenance["segment_delays"] == "v2:direct_segment_delays"
    assert loaded.provenance["drift_fits"] == "v2:direct_drift_fits"


def test_unknown_version_is_rejected():
    with pytest.raises(Exception) as raised:
        normalize_version("v9")
    assert raised.value.code == E_VERSION_INVALID

