from __future__ import annotations

import pytest
import soundfile as sf

from offline_master.codes import BusinessError, ErrorCode
from offline_master.manifest import validate_manifest


def test_valid_manifest_accepts(project_dir):
    project, manifest = project_dir
    validated = validate_manifest(manifest, str(project))
    assert validated["sample_rate"] == 48000
    assert validated["angles"] == list(range(0, 360, 10))


def test_every_invalid_rule_collected_without_short_circuit(project_dir):
    project, manifest = project_dir
    manifest["sample_rate"] = 22050
    manifest["turntable_angles_deg"] = [0, 0, 10]
    manifest["lip_sync_points_s"] = [0.5, 0.4]
    manifest["tracks"][0]["anchor"] = [9.0, 9.0, 9.0]
    manifest["tracks"][1]["file"] = manifest["tracks"][0]["file"]
    manifest["tracks"][2]["material_type"] = "dialogue"
    manifest["tracks"].append(
        {
            "file": "stems/ghost.wav",
            "material_type": "mystery",
            "seed": 1,
            "anchor": [0, 1, 0],
            "start_offset_s": 0.0,
        }
    )
    with pytest.raises(BusinessError) as exc:
        validate_manifest(manifest, str(project))
    assert exc.value.code == ErrorCode.E_MANIFEST_INVALID
    reasons = {warning["reason"] for warning in exc.value.warnings}
    # Multiple distinct categories must coexist in one structured report.
    assert {
        "angle_sequence_invalid",
        "lip_points_not_ascending",
        "anchor_out_of_bounds",
        "duplicate_file_path",
        "duplicate_material_type",
        "unknown_material_type",
        "file_not_found",
        "sample_rate_mismatch",
    }.issubset(reasons)
    assert all("file" in warning for warning in exc.value.warnings)


def test_bit_depth_and_channel_mismatch_detected(project_dir):
    project, manifest = project_dir
    target = project / manifest["tracks"][0]["file"]
    data, sr = sf.read(target)
    sf.write(target, data, sr, subtype="PCM_24")
    with pytest.raises(BusinessError) as exc:
        validate_manifest(manifest, str(project))
    reasons = {w["reason"] for w in exc.value.warnings}
    assert "bit_depth_mismatch" in reasons
