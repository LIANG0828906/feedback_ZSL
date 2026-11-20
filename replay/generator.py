"""确定性样本生成器。

所有随机性来自 random.Random(seed)；相同种子与操作序列下生成的
指令流、状态哈希与轨迹日志逐字节一致。乱序程度 disorder∈[0,3]
控制接收乱序、重复投递、迟到与超额申请的比例。
"""

from __future__ import annotations

import json
import os
import random

from .canon import canonical_bytes


def generate_sample(seed: int, players: int, frames: int,
                    disorder: int = 1, match_id: str | None = None) -> dict:
    rng = random.Random(seed)
    match_id = match_id or f"m-{seed}-{players}-{frames}-{disorder}"
    player_ids = [f"p{i}" for i in range(players)]
    units = []
    for p in player_ids:
        for k in range(3):
            units.append({
                "id": f"{p}-u{k}",
                "owner": p,
                "x": rng.randrange(0, 64),
                "y": rng.randrange(0, 64),
                "vx": 0,
                "vy": 0,
                "alive": True,
            })
    quota = {p: [rng.randrange(3, 7) for _ in range(frames)]
             for p in player_ids}
    window = 2

    raw_cmds = []
    seqs = {p: 0 for p in player_ids}
    for t in range(frames):
        for p in player_ids:
            if rng.random() < 0.55:
                continue
            seq = seqs[p]
            seqs[p] += 1
            op = rng.choice(["move", "move", "stop", "strike"])
            if op == "strike":
                target = rng.choice(
                    [u["id"] for u in units if u["owner"] != p])
                dx = dy = 0
            else:
                target = f"{p}-u{rng.randrange(3)}"
                dx = rng.randrange(-1, 2) if op == "move" else 0
                dy = rng.randrange(-1, 2) if op == "move" else 0
            cost = rng.randrange(1, 4)
            if disorder >= 2 and rng.random() < 0.08:
                cost += 8  # 超额申请算力
            lag = 0
            r = rng.random()
            if disorder >= 1 and r < 0.15:
                lag = rng.randrange(1, window + 1)       # 窗口内迟到
            elif disorder >= 2 and r < 0.25:
                lag = rng.randrange(window + 1, window + 4)  # 超窗迟到
            raw_cmds.append({
                "player": p, "client_seq": seq,
                "submit_tick": t, "recv_tick": t + lag,
                "op": op, "unit": target, "dx": dx, "dy": dy,
                "cost": cost,
            })
            if disorder >= 3 and rng.random() < 0.10:
                dup = dict(raw_cmds[-1])
                dup["recv_tick"] = t + lag + rng.randrange(0, 2)
                raw_cmds.append(dup)  # 重复投递同一 client_seq

    # 接收序号：按接收帧分组，帧内按乱序程度洗牌后顺序编号
    by_tick: dict[int, list[dict]] = {}
    for c in raw_cmds:
        by_tick.setdefault(c["recv_tick"], []).append(c)
    recv_seq = 0
    ordered = []
    for tick in sorted(by_tick):
        group = by_tick[tick]
        if disorder >= 1:
            rng.shuffle(group)
        for c in group:
            c["recv_seq"] = recv_seq
            recv_seq += 1
            ordered.append(c)

    return {
        "version": 1,
        "match_id": match_id,
        "tick_ms": 100,
        "frames": frames,
        "compensation_window": window,
        "initial_state": {"players": player_ids, "units": units},
        "quota_per_frame": quota,
        "commands": ordered,
    }


def write_json(path: str, obj):
    os.makedirs(os.path.dirname(path), exist_ok=True)
    with open(path, "wb") as fh:
        fh.write(json.dumps(obj, sort_keys=True, indent=2,
                            ensure_ascii=False).encode("utf-8"))
        fh.write(b"\n")


def write_trace(path: str, trace_lines: list[dict]):
    os.makedirs(os.path.dirname(path), exist_ok=True)
    with open(path, "wb") as fh:
        for line in trace_lines:
            fh.write(canonical_bytes(line))
            fh.write(b"\n")


SUITE = [
    {"seed": 7, "players": 2, "frames": 24, "disorder": 0},
    {"seed": 11, "players": 3, "frames": 32, "disorder": 1},
    {"seed": 23, "players": 4, "frames": 40, "disorder": 2},
    {"seed": 42, "players": 5, "frames": 48, "disorder": 3},
]


def generate_suite(samples_dir: str, baseline_dir: str) -> list[str]:
    """生成整套样本与权威基线，返回样本名列表。"""
    from .engine import ReplayEngine  # 延迟导入避免环
    names = []
    for cfg in SUITE:
        name = (f"s{cfg['seed']}-p{cfg['players']}"
                f"-f{cfg['frames']}-d{cfg['disorder']}")
        sample = generate_sample(match_id=name, **cfg)
        write_json(os.path.join(samples_dir, f"{name}.json"), sample)
        engine = ReplayEngine(sample)
        engine.run()
        write_json(os.path.join(baseline_dir, f"{name}.baseline.json"),
                   engine.baseline())
        write_trace(os.path.join(baseline_dir, f"{name}.trace.jsonl"),
                    engine.trace_lines())
        names.append(name)
    return names
