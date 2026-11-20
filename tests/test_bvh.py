from __future__ import annotations

from pathlib import Path

import pytest

from mocap_workbench.bvh import parse_bvh, parse_bvh_text
from mocap_workbench.errors import E_BVH_INVALID, BusinessError


def test_bvh_hash_matches_generated_baseline(samples):
    record = samples["small"]
    document = parse_bvh(record["bvh"])
    baseline = record["baseline_data"]
    assert document.frame_count == baseline["frame_count"]
    for frame in (0, 1, document.frame_count // 2, document.frame_count - 1):
        assert document.frame_hash(frame) == baseline["pose_hashes"][frame]
    assert document.pose_hash(document.evaluate(0)) == document.frame_hash(0)


def test_bvh_rejects_frame_count_mismatch(tmp_path: Path, samples):
    text = Path(samples["small"]["bvh"]).read_text(encoding="utf-8")
    broken = text.replace("Frames: 90", "Frames: 91", 1)
    with pytest.raises(BusinessError) as caught:
        parse_bvh_text(broken)
    assert caught.value.code == E_BVH_INVALID
    assert "motion data length" in caught.value.message


def test_bvh_rejects_channel_order_mismatch(tmp_path: Path, samples):
    text = Path(samples["small"]["bvh"]).read_text(encoding="utf-8")
    broken = text.replace(
        "CHANNELS 3 Zrotation Xrotation Yrotation",
        "CHANNELS 3 Zrotation Xrotation Xrotation",
        1,
    )
    with pytest.raises(BusinessError) as caught:
        parse_bvh_text(broken)
    assert caught.value.code == E_BVH_INVALID
    assert "duplicate rotation channel" in caught.value.message


def test_bvh_rejects_missing_motion_value(tmp_path: Path, samples):
    lines = Path(samples["small"]["bvh"]).read_text(encoding="utf-8").splitlines()
    last = lines[-1].split()
    lines[-1] = " ".join(last[:-1])
    with pytest.raises(BusinessError) as caught:
        parse_bvh_text("\n".join(lines) + "\n")
    assert caught.value.code == E_BVH_INVALID


def test_bvh_rejects_duplicate_joint_names(tmp_path: Path, samples):
    text = Path(samples["small"]["bvh"]).read_text(encoding="utf-8")
    broken = text.replace("JOINT Spine", "JOINT Hips", 1)
    with pytest.raises(BusinessError) as caught:
        parse_bvh_text(broken)
    assert caught.value.code == E_BVH_INVALID
    assert "duplicate joint name" in caught.value.message


def test_fk_transform_hash_is_stable(samples):
    document = parse_bvh(samples["small"]["bvh"])
    first = [document.frame_hash(frame) for frame in range(10)]
    second = [document.frame_hash(frame) for frame in range(10)]
    assert first == second

