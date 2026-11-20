"""一键验收脚本：样本生成、并行回放、预算强制、恢复、差异诊断与逐字节复现。"""

from __future__ import annotations

import filecmp
import json
import shutil
import subprocess
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT))

from replay.config import DEFAULT_CONFIG, load_config  # noqa: E402
from replay.hashing import read_json  # noqa: E402
from replay.model import MatchDescription  # noqa: E402
from replay.scheduler import ReplayScheduler  # noqa: E402
from replay.verifier import verify  # noqa: E402


def reset(path: Path) -> None:
    shutil.rmtree(path, ignore_errors=True)
    path.mkdir(parents=True, exist_ok=True)


def run_batch(output: Path, config, only_clean: bool) -> None:
    manifest = read_json(ROOT / "samples/matches/manifest.json")
    scheduler = ReplayScheduler(
        config, output, turn_pause_seconds=0.0
    )
    try:
        for entry in manifest["matches"]:
            if only_clean and entry.get("expect_error"):
                continue
            code, _ = scheduler.submit(MatchDescription.load(entry["sample"]))
            assert code == "OK", (entry["match_id"], code)
        scheduler.wait_all()
    finally:
        scheduler.shutdown()


def compare_trees(a: Path, b: Path) -> list[str]:
    files = sorted(
        str(p.relative_to(a)) for p in a.rglob("*") if p.is_file()
    )
    return [
        rel
        for rel in files
        if not filecmp.cmp(a / rel, b / rel, shallow=False)
    ]


def main() -> int:
    print("== 1. 生成确定性样本与基线 ==")
    subprocess.run(
        [sys.executable, "-m", "replay.cli", "generate"],
        cwd=ROOT,
        check=True,
    )

    print("== 2. 并行回放（全局并发上限 %d）==" % DEFAULT_CONFIG.max_concurrency)
    run_a = ROOT / "output/acceptance/a"
    run_b = ROOT / "output/acceptance/b"
    reset(run_a)
    reset(run_b)
    run_batch(run_a, DEFAULT_CONFIG, only_clean=True)
    run_batch(run_b, DEFAULT_CONFIG, only_clean=True)

    print("== 3. 逐字节一致性 ==")
    diffs = compare_trees(run_a, run_b)
    print("差异文件:", diffs or "无（两次运行逐字节一致）")
    assert not diffs

    print("== 4. 基线比对 ==")
    for match_dir in sorted(p for p in run_a.iterdir() if p.is_dir()):
        baseline = (
            ROOT / "samples/baselines" / f"{match_dir.name}.baseline.json"
        )
        report = verify(match_dir.name, match_dir, baseline)
        print(f"  {match_dir.name}: {report.code} {report.message}")
        assert report.passed

    print("== 5. 预算强制 E_BUDGET_EXCEEDED ==")
    tight = load_config(ROOT / "config/replay.tight.json")
    tight_out = ROOT / "output/acceptance/tight"
    reset(tight_out)
    scheduler = ReplayScheduler(tight, tight_out, turn_pause_seconds=0.0)
    try:
        desc = MatchDescription.load(
            ROOT / "samples/matches/tight_budget_s05.json"
        )
        scheduler.submit(desc)
        scheduler.wait_all()
    finally:
        scheduler.shutdown()
    failed = scheduler.get_task("tight_budget_s05").snapshot_status()
    print("  ", json.dumps(failed, ensure_ascii=False))
    assert failed["error_code"] == "E_BUDGET_EXCEEDED"

    print("== 6. 从已提交快照恢复并与全量结果一致 ==")
    recover_scheduler = ReplayScheduler(
        DEFAULT_CONFIG, tight_out, turn_pause_seconds=0.0
    )
    try:
        code, _ = recover_scheduler.submit(desc, recover=True)
        assert code == "OK"
        recover_scheduler.wait_all()
    finally:
        recover_scheduler.shutdown()
    recovered = recover_scheduler.get_task("tight_budget_s05")
    assert recovered.status == "COMPLETED"
    full_dir = run_a / "tight_budget_s05"
    # tight 样本没有基线，直接与一次完整跑完的目录比较快照字节。
    full_scheduler = ReplayScheduler(
        DEFAULT_CONFIG,
        ROOT / "output/acceptance/full_tight",
        turn_pause_seconds=0.0,
    )
    reset(ROOT / "output/acceptance/full_tight")
    full_scheduler = ReplayScheduler(
        DEFAULT_CONFIG,
        ROOT / "output/acceptance/full_tight",
        turn_pause_seconds=0.0,
    )
    try:
        full_scheduler.submit(desc)
        full_scheduler.wait_all()
    finally:
        full_scheduler.shutdown()
    for turn in range(1, desc.turn_count + 1):
        name = f"snapshot_{turn:06d}.json"
        assert (
            tight_out / "tight_budget_s05/snapshots" / name
        ).read_bytes() == (
            ROOT
            / "output/acceptance/full_tight/tight_budget_s05/snapshots"
            / name
        ).read_bytes(), f"恢复后回合 {turn} 快照不一致"
    print("   恢复后全部", desc.turn_count, "个快照与全量运行逐字节一致")

    print("== 7. 差异诊断（篡改基线 state_hash）==")
    baseline_path = ROOT / "samples/baselines/small_s01.baseline.json"
    raw = read_json(baseline_path)
    original = raw["turns"][1]["state_hash"]
    raw["turns"][1]["state_hash"] = "a" * len(original)
    tampered = ROOT / "output/acceptance/tampered.baseline.json"
    from replay.hashing import write_json_atomic

    write_json_atomic(tampered, raw)
    report = verify(
        "small_s01", run_a / "small_s01", tampered
    )
    print(
        "  ",
        report.code,
        "turn=",
        report.first_mismatch_turn,
        "path=",
        report.field_path,
    )
    assert report.first_mismatch_turn == 2
    assert report.field_path == "turns[2].state_hash"

    print("\n全部验收项通过 ✔")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
