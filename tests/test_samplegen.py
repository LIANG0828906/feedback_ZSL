"""要求 8：确定性样本入口与基线哈希复现。"""

from __future__ import annotations

import json
from pathlib import Path

from mediaaudit.cli import run
from mediaaudit.samplegen import SAMPLE_SPECS, generate_samples
from mediaaudit.util import canonical_json


def test_sample_generation_deterministic(tmp_path: Path) -> None:
    first = generate_samples(tmp_path / "a")
    second = generate_samples(tmp_path / "b")
    def strip_paths(baseline: dict) -> dict:
        return {
            name: {k: v for k, v in item.items() if not k.endswith("path") and k not in ("wav", "frames")}
            for name, item in baseline["samples"].items()
        }

    assert canonical_json(strip_paths(first)) == canonical_json(strip_paths(second))
    for name in first["samples"]:
        assert len(first["samples"][name]["pcm_sha256"]) == 64


def test_all_sample_rates_and_layouts_present(tmp_path: Path) -> None:
    baseline = generate_samples(tmp_path)
    rates = {item["spec"]["sample_rate"] for item in baseline["samples"].values()}
    channels = {item["spec"]["channels"] for item in baseline["samples"].values()}
    assert rates == {spec.sample_rate for spec in SAMPLE_SPECS}
    assert channels == {1, 2}


def test_cli_full_flow_and_reproducible_trajectory(tmp_path: Path) -> None:
    work_a = tmp_path / "wa"
    work_b = tmp_path / "wb"
    summary_a = run(work_a, "mono_16k")
    summary_b = run(work_b, "mono_16k")
    assert summary_a["rollback"]["hash_match_v1"] is True
    assert summary_a["span_check"]["consistent"] is False
    assert summary_a["recovery"]["manifest_rows"] > 0

    traj_a = (work_a / "audit" / "trajectory.jsonl").read_text()
    traj_b = (work_b / "audit" / "trajectory.jsonl").read_text()
    # 轨迹中的版本、序号、哈希在两次独立运行间逐字节复现。
    assert traj_a == traj_b


def test_baseline_file_written(tmp_path: Path) -> None:
    generate_samples(tmp_path)
    baseline = json.loads((tmp_path / "baseline.json").read_text())
    assert "tier_sequences" in baseline
