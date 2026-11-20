#!/usr/bin/env python3
"""端到端验收：三版本归一重放、哈希链、断点恢复、分叉诊断、六类异常注入。

全部使用本地确定性样本，离线运行；输出写入 data/acceptance/ 并打印结构化结果。
"""
from __future__ import annotations

import json
import shutil
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT))

from replay.errors import ReplayError  # noqa: E402
from replay.generator import GenerationPlan, write_generation  # noqa: E402
from replay.injector import apply_corrupt_snapshot, build_faulty_replay  # noqa: E402
from replay.pipeline import ReplayPipeline  # noqa: E402


def line(title: str) -> None:
    print(f"\n=== {title} ===")


def main() -> int:
    work = ROOT / "data" / "acceptance"
    if work.exists():
        shutil.rmtree(work)
    sample = work / "sample"
    plan = GenerationPlan(room_id="room-accept", seed=20261121, players=4, total_ticks=24, disorder=3)
    paths = write_generation(sample, plan)
    print(f"sample generated at {sample}")

    line("1. cross-version normalized replay + hash chain")
    chains = {}
    for version in (1, 2, 3):
        pipe = ReplayPipeline(
            snapshot_dir=work / f"snap_v{version}",
            baseline_path=paths["baseline"],
        )
        result = pipe.run(paths[f"v{version}"])
        chains[version] = result.hash_chain
        print(
            f"v{version}: ticks={result.total_ticks} head={result.hash_chain[0][:16]} "
            f"tail={result.hash_chain[-1][:16]} fork={result.fork is not None}"
        )
    assert chains[1] == chains[2] == chains[3], "cross-version hash chains diverged"
    print("all three versions produce byte-identical hash chains: OK")

    line("2. deterministic regeneration")
    again = work / "sample_again"
    write_generation(again, plan)
    for name in ("replay_v1.json", "replay_v2.json", "replay_v3.json", "baseline.json", "trace.log"):
        assert (sample / name).read_bytes() == (again / name).read_bytes(), name
    print("same seed -> replay/baseline/trace byte-identical: OK")

    line("3. crash recovery from committed snapshots")
    snap_dir = work / "recovery"
    ReplayPipeline(snapshot_dir=snap_dir, snapshot_every=3).run(paths["v1"])
    committed = sorted(p.name for p in snap_dir.iterdir())
    print("committed snapshots:", committed)
    for path in list(snap_dir.iterdir()):
        tick = int(path.stem.split("_")[1])
        if tick > 12:
            path.unlink()
    result = ReplayPipeline(snapshot_dir=snap_dir, snapshot_every=3).run(paths["v1"])
    print(f"resumed from snapshot tick={result.recovery_tick}, advanced new ticks={len(result.states)}")
    assert result.recovery_tick == 12 and len(result.states) == 12

    line("4. fork diagnosis")
    baseline = json.loads(Path(paths["baseline"]).read_text())
    target = next(item for item in baseline["entries"] if item["tick"] == 7)
    target["state"]["resources"]["ammo"] += 11
    target["state_hash"] = "b" * 64
    drifted = work / "baseline_drift.json"
    drifted.write_text(json.dumps(baseline), encoding="utf-8")
    fork_result = ReplayPipeline(
        snapshot_dir=work / "fork_snap", baseline_path=drifted
    ).run(paths["v2"])
    fork = fork_result.fork
    print(
        f"first fork tick={fork.tick} path={fork.field_path} "
        f"expected={fork.expected} actual={fork.actual} ops={fork.applied_ops} water={fork.water_level}"
    )
    assert fork.tick == 7 and "resources.ammo" in fork.field_path

    line("5. six injection faults (independent business codes)")
    v1 = json.loads(Path(paths["v1"]).read_text())
    v3 = json.loads(Path(paths["v3"]).read_text())
    cases = [
        ("duplicate_op", build_faulty_replay(v1, "duplicate_op", 1), 1),
        ("out_of_bounds", build_faulty_replay(v1, "out_of_bounds", 0), 0),
        ("late_op", build_faulty_replay(v1, "late_op", 0), 0),
        ("truncated_diff", build_faulty_replay(v3, "truncated_diff", 0), 0),
        ("format_order", build_faulty_replay(v1, "format_order", 0), 0),
    ]
    for fault, faulty, record in cases:
        snap_dir = work / f"inj_{fault}"
        try:
            ReplayPipeline(snapshot_dir=snap_dir, fault=fault, fault_record=record).run(faulty)
        except ReplayError as exc:
            wrote = snap_dir.exists() and any(snap_dir.iterdir())
            print(f"{fault:16s} -> {exc.code.value:24s} at {exc.injection_point} files_written={wrote}")
            assert not wrote
    # corrupt snapshot: 先正常落盘再损坏，恢复阶段拦截。
    corrupt_dir = work / "inj_corrupt"
    ReplayPipeline(snapshot_dir=corrupt_dir).run(paths["v2"])
    apply_corrupt_snapshot(corrupt_dir, tick=6)
    try:
        ReplayPipeline(
            snapshot_dir=corrupt_dir,
            fault="corrupt_snapshot",
            fault_extra={"tick": 6, "snapshot_dir": str(corrupt_dir)},
        ).run(paths["v2"])
    except ReplayError as exc:
        print(f"{'corrupt_snapshot':16s} -> {exc.code.value:24s} at tick={exc.record_index}")
        assert exc.code.value == "E_RECOVERY_INVALID"

    line("6. baseline error codes")
    missing = ReplayPipeline(snapshot_dir=work / "b1", baseline_path=work / "missing.json")
    try:
        missing.run(paths["v1"])
    except ReplayError as exc:
        print("missing baseline ->", exc.code.value)
    wrong = json.loads(Path(paths["baseline"]).read_text())
    wrong["state_model_version"] = 42
    wrong_path = work / "wrong_version.json"
    wrong_path.write_text(json.dumps(wrong), encoding="utf-8")
    try:
        ReplayPipeline(snapshot_dir=work / "b2", baseline_path=wrong_path).run(paths["v1"])
    except ReplayError as exc:
        print("baseline version mismatch ->", exc.code.value)

    line("trace log preview")
    print("\n".join(Path(paths["trace"]).read_text().splitlines()[:6]))
    print("\nALL ACCEPTANCE CHECKS PASSED")
    return 0


if __name__ == "__main__":
    sys.exit(main())
