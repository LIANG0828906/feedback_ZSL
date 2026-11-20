"""确定性样本与基线生成器。

使用固定种子的 NumPy Generator 生成多组（玩家数 × 步数 × 透支密度）
样本。相同种子与参数下，输入 JSON 与轨迹日志逐字节一致。
client_ts / arrival_ts 只用于延迟观测，不进入任何结算与哈希。
"""

from __future__ import annotations

import os

import numpy as np

from .baseline import build_baseline, write_baseline
from .engine import QuotaReplayEngine
from .models import RULE_VERSION, canonical_dumps
from .parser import parse_input

SCENARIO_MATRIX = (
    # (场景名, 玩家数, 步数, 每步配额, 透支密度, 策略)
    ("small_reject", 2, 8, 10.0, 0.35, "reject"),
    ("medium_reject", 4, 20, 12.0, 0.5, "reject"),
    ("large_degrade", 6, 40, 15.0, 0.65, "degrade"),
)


def _rng(seed: int, scenario: str) -> np.random.Generator:
    digest = int(
        __import__("hashlib").sha256(f"{seed}:{scenario}".encode("utf-8")).hexdigest(),
        16,
    )
    return np.random.default_rng(digest)


def generate_scenario(
    scenario: str,
    players: int,
    steps: int,
    quota: float,
    overdraft_density: float,
    policy: str,
    seed: int,
    out_dir: str,
) -> dict:
    rng = _rng(seed, scenario)
    player_ids = tuple(f"P{idx:02d}" for idx in range(players))

    penalty_rule = {
        "policy": policy,
        "reject_penalty": 2.0,
        "degrade_rate": 1.5,
        "degrade_fixed": 1.0,
    }
    config = {
        "exercise_id": f"ex-{scenario}-s{seed}",
        "total_steps": steps,
        "per_step_quota": quota,
        "player_ids": list(player_ids),
        "penalty_rule": penalty_rule,
    }

    operations = []
    seq_counters = {pid: 0 for pid in player_ids}
    op_index = 0
    for step in range(steps):
        for pid in player_ids:
            # 密度越高，该玩家该步操作条数与单条用量越大
            count = 1 + int(rng.random() < overdraft_density) + int(
                rng.random() < overdraft_density * 0.5
            )
            for _ in range(count):
                if rng.random() < overdraft_density:
                    required = float(round(rng.uniform(quota * 0.6, quota * 1.2), 4))
                else:
                    required = float(round(rng.uniform(0.1, quota * 0.5), 4))
                base_ts = step * 100.0 + float(rng.integers(0, 50))
                latency = float(rng.integers(1, 20))
                operations.append(
                    {
                        "op_id": f"op-{op_index:06d}",
                        "player_id": pid,
                        "client_seq": seq_counters[pid],
                        "effective_step": step,
                        "required_compute": required,
                        "client_ts": base_ts,
                        "arrival_ts": base_ts + latency,
                    }
                )
                seq_counters[pid] += 1
                op_index += 1

    # 故意打乱到达顺序，证明结算只依赖 (player_id, client_seq) 排序。
    shuffled = list(operations)
    rng.shuffle(shuffled)
    payload = {
        "schema_version": "quota-replay-input/v1",
        "rule_version": RULE_VERSION,
        "config": config,
        "operations": shuffled,
    }

    scenario_dir = os.path.join(out_dir, scenario)
    os.makedirs(scenario_dir, exist_ok=True)
    input_path = os.path.join(scenario_dir, "input.json")
    baseline_path = os.path.join(scenario_dir, "baseline.json")
    trajectory_path = os.path.join(scenario_dir, "trajectory.jsonl")

    encoded = canonical_dumps(payload) + "\n"
    with open(input_path, "w", encoding="utf-8", newline="\n") as handle:
        handle.write(encoded)

    parsed_config, parsed_ops = parse_input(payload)
    engine = QuotaReplayEngine(parsed_config, parsed_ops)
    engine.run()
    baseline = build_baseline(parsed_config, engine.results)
    write_baseline(baseline_path, baseline)

    from .runner import _write_trajectory_lines

    with open(trajectory_path, "w", encoding="utf-8", newline="\n") as handle:
        for result in engine.results:
            _write_trajectory_lines(handle, result)

    return {
        "scenario": scenario,
        "input_path": input_path,
        "baseline_path": baseline_path,
        "trajectory_path": trajectory_path,
        "operation_count": len(operations),
        "step_count": steps,
    }


def generate_all(out_dir: str, seed: int = 20261121) -> list[dict]:
    os.makedirs(out_dir, exist_ok=True)
    summaries = []
    for scenario, players, steps, quota, density, policy in SCENARIO_MATRIX:
        summaries.append(
            generate_scenario(
                scenario, players, steps, quota, density, policy, seed, out_dir
            )
        )
    manifest = {
        "seed": seed,
        "rule_version": RULE_VERSION,
        "scenarios": summaries,
    }
    with open(
        os.path.join(out_dir, "manifest.json"), "w", encoding="utf-8", newline="\n"
    ) as handle:
        handle.write(canonical_dumps(manifest) + "\n")
    return summaries
