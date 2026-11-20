"""确定性样本生成器。

生成多种对局规模与随机种子的本地样本及权威基线。
生成过程只使用种子化 PCG64，不含时间戳或外部熵，重复运行逐字节一致。
"""

from __future__ import annotations

from pathlib import Path

import numpy as np

from .config import REPLAYER_VERSION
from .hashing import write_json_atomic
from .model import MatchDescription, TurnBudget
from .verifier import generate_baseline

ACTION_CYCLE = ("attack", "defend", "charge", "skill")

# (样本名, 回合数, 种子, 初始 HP)
SAMPLE_SPECS = [
    ("small_s01", 6, 101, 120),
    ("small_s02", 6, 202, 120),
    ("medium_s03", 12, 303, 200),
    ("large_s04", 20, 404, 300),
]


def build_description(
    name: str, turn_count: int, seed: int, hp: int, budget: TurnBudget
) -> MatchDescription:
    rng = np.random.Generator(np.random.PCG64(seed))
    actions = []
    for turn in range(1, turn_count + 1):
        turn_actions = []
        for player_id in (1, 2):
            index = int(
                rng.integers(0, len(ACTION_CYCLE))
            )  # 固定序列：每回合两名玩家各抽一次
            turn_actions.append(
                {"player_id": player_id, "type": ACTION_CYCLE[index]}
            )
        actions.append(turn_actions)
    initial_state = {
        "players": [
            {"id": 1, "hp": hp, "energy": 40, "shield": 0},
            {"id": 2, "hp": hp, "energy": 40, "shield": 0},
        ]
    }
    return MatchDescription(
        match_id=name,
        rng_seed=seed,
        turn_count=turn_count,
        initial_state=initial_state,
        actions=actions,
        turn_budget=budget,
    )


def generate_samples(
    sample_root: str | Path,
    baseline_root: str | Path,
    compute_budget: int,
    memory_budget: int,
) -> list[Path]:
    sample_root = Path(sample_root)
    baseline_root = Path(baseline_root)
    sample_root.mkdir(parents=True, exist_ok=True)
    baseline_root.mkdir(parents=True, exist_ok=True)
    written: list[Path] = []
    per_turn_budget = TurnBudget(compute=500, memory=500)

    manifest = {
        "version": REPLAYER_VERSION,
        "compute_budget": compute_budget,
        "memory_budget": memory_budget,
        "matches": [],
    }
    for name, turn_count, seed, hp in SAMPLE_SPECS:
        desc = build_description(name, turn_count, seed, hp, per_turn_budget)
        sample_path = sample_root / f"{name}.json"
        desc.save(sample_path)
        baseline_path = baseline_root / f"{name}.baseline.json"
        generate_baseline(
            desc, compute_budget, memory_budget, baseline_path
        )
        manifest["matches"].append(
            {
                "match_id": name,
                "sample": str(sample_path),
                "baseline": str(baseline_path),
                "turn_count": turn_count,
                "seed": seed,
            }
        )
        written.append(sample_path)

    # 刻意构造一个预算过紧的样本，用于验证 E_BUDGET_EXCEEDED 链路。
    tight = build_description(
        "tight_budget_s05", 8, 505, 160, per_turn_budget
    )
    tight_path = sample_root / "tight_budget_s05.json"
    tight.save(tight_path)
    manifest["matches"].append(
        {
            "match_id": "tight_budget_s05",
            "sample": str(tight_path),
            "baseline": None,
            "expect_error": "E_BUDGET_EXCEEDED",
            "turn_count": 8,
            "seed": 505,
        }
    )
    written.append(tight_path)
    write_json_atomic(sample_root / "manifest.json", manifest)
    return written
