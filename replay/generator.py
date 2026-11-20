"""确定性样本生成器。

相同种子与玩家/步数/乱序参数下：操作序列、状态哈希、基线与轨迹日志逐字节一致。
一次权威引擎推进产出状态，再分别投影为 v1/v2/v3 三种录像格式，因此跨版本
重放结果天然对齐。
"""
from __future__ import annotations

import json
import random
from dataclasses import dataclass
from pathlib import Path

from .baseline import BASELINE_MODEL_VERSION
from .diff_codec import _collect_state_paths, encode_diff
from .engine import DeterministicEngine
from .models import (
    OP_KILL,
    OP_MOVE,
    OP_REVIVE,
    OP_SPEND,
    OP_SUPPLY,
    RoomState,
    canonical_dumps,
)

CAPABILITIES_V3 = ["delta-diff", "lexicographic-fields", "snapshot-chain", "capability-flags"]


@dataclass
class GenerationPlan:
    room_id: str
    seed: int
    players: int
    total_ticks: int
    late_window: int = 2
    disorder: int = 2


def _initial_state(players: int) -> RoomState:
    entities = {}
    for index in range(players):
        entities[f"e{index}"] = {
            "pos": [float(index * 10), 0.0],
            "vel": [0.0, 0.0],
            "alive": True,
            "owner": index,
        }
    return RoomState(tick=0, entities=entities, resources={"ammo": 100, "materials": 50})


def _plan_operations(plan: GenerationPlan) -> list[dict]:
    rng = random.Random(plan.seed)
    raw: list[dict] = []
    client_counter = {player: 0 for player in range(plan.players)}
    op_cycle = [OP_MOVE, OP_MOVE, OP_SUPPLY, OP_SPEND, OP_KILL, OP_REVIVE]
    for tick in range(1, plan.total_ticks + 1):
        for player in range(plan.players):
            if rng.random() < 0.25:
                continue
            op_type = op_cycle[(tick + player) % len(op_cycle)]
            payload = _payload_for(op_type, player, plan.players, rng)
            raw.append(
                {
                    "player_id": player,
                    "client_seq": client_counter[player],
                    "tick": tick,
                    "op_type": op_type,
                    "payload": payload,
                }
            )
            client_counter[player] += 1
    # 乱序：用确定性洗牌决定服务器到达顺序，到达序号全局唯一。
    order = list(range(len(raw)))
    for _ in range(plan.disorder):
        rng.shuffle(order)
    for arrival, original_index in enumerate(order):
        raw[original_index]["arrival_seq"] = arrival
    raw.sort(key=lambda item: item["arrival_seq"])
    return raw


def _payload_for(op_type: str, player: int, players: int, rng: random.Random) -> dict:
    target = f"e{(player + 1) % max(players, 1)}"
    if op_type == OP_MOVE:
        return {"entity_id": f"e{player}", "dx": round(rng.uniform(-5, 5), 4), "dy": round(rng.uniform(-5, 5), 4)}
    if op_type in (OP_KILL, OP_REVIVE):
        return {"target_id": target}
    amount = rng.randint(1, 5)
    resource = "ammo" if op_type == OP_SPEND else "materials"
    return {"resource": resource, "amount": amount}


def _run_authority(plan: GenerationPlan, ops: list[dict]) -> tuple[DeterministicEngine, list]:
    from .models import Operation

    state0 = _initial_state(plan.players)
    engine = DeterministicEngine(state0, plan.late_window)
    engine.ingest([Operation.from_dict(item) for item in ops])
    reports = []
    for _ in range(plan.total_ticks):
        reports.append(engine.step())
    return engine, reports


def _base_header(plan: GenerationPlan, version: int) -> dict:
    return {
        "format_version": version,
        "room_id": plan.room_id,
        "tick_rate": 10,
        "total_ticks": plan.total_ticks,
        "late_window": plan.late_window,
        "initial_state": _initial_state(plan.players).canonical_payload(),
    }


def build_replays(plan: GenerationPlan) -> dict:
    ops = _plan_operations(plan)
    engine, reports = _run_authority(plan, ops)
    states = {report.tick: engine.state for report in reports}
    # 重新收集每步状态（引擎原地更新，需按报告时刻快照；重放一次保存副本）。
    state0 = _initial_state(plan.players)
    probe = DeterministicEngine(state0, plan.late_window)
    from .models import Operation

    probe.ingest([Operation.from_dict(item) for item in ops])
    tick_states: dict[int, RoomState] = {}
    tick_reports = {}
    for _ in range(plan.total_ticks):
        report = probe.step()
        tick_states[report.tick] = probe.state.clone()
        tick_reports[report.tick] = report

    field_order = _collect_state_paths(state0.canonical_payload())
    outputs: dict[int, dict] = {}
    for version in (1, 2, 3):
        replay = _base_header(plan, version)
        if version == 3:
            replay["field_order"] = field_order
            replay["capabilities"] = CAPABILITIES_V3
        records = []
        ticks_with_payload: set[int] = set()
        for item in ops:
            tick = item["tick"]
            record = {"tick": tick, "op": dict(item)}
            first_for_tick = tick not in ticks_with_payload
            if version == 2:
                record["snapshot"] = tick_states[tick].canonical_payload()
                record["events"] = _events_for(tick_reports[tick])
            if version == 3 and first_for_tick:
                record["snapshot"] = tick_states[tick].canonical_payload()
                record["events"] = _events_for(tick_reports[tick])
                base = tick_states[tick - 1] if tick - 1 in tick_states else state0
                record["diff"] = encode_diff(base, tick_states[tick])
            records.append(record)
            ticks_with_payload.add(tick)
        replay["records"] = records
        outputs[version] = replay

    baseline = {
        "state_model_version": BASELINE_MODEL_VERSION,
        "room_id": plan.room_id,
        "entries": [
            {
                "tick": tick,
                "state_hash": tick_reports[tick].state_hash,
                "state": tick_states[tick].canonical_payload(),
            }
            for tick in sorted(tick_states)
        ],
    }
    trace = _render_trace(plan, tick_reports)
    return {"replays": outputs, "baseline": baseline, "trace": trace, "reports": tick_reports}


def _events_for(report) -> list[dict]:
    return [
        {"tick": report.tick, "type": "op.applied", "op": op.to_dict()}
        for op in report.applied
    ]


def _render_trace(plan: GenerationPlan, reports: dict) -> str:
    lines = [f"room={plan.room_id} seed={plan.seed} players={plan.players} ticks={plan.total_ticks}"]
    for tick in sorted(reports):
        report = reports[tick]
        applied = ",".join(report.applied_keys())
        rejected = ",".join(item.code for item in report.rejected)
        lines.append(
            f"tick={tick} applied=[{applied}] rejected=[{rejected}] "
            f"hash={report.state_hash} water={report.water_level}"
        )
    return "\n".join(lines) + "\n"


def write_generation(output_dir: str | Path, plan: GenerationPlan) -> dict:
    out = Path(output_dir)
    out.mkdir(parents=True, exist_ok=True)
    bundle = build_replays(plan)
    paths = {}
    for version, replay in bundle["replays"].items():
        target = out / f"replay_v{version}.json"
        target.write_bytes(canonical_dumps(replay))
        paths[f"v{version}"] = target
    baseline_path = out / "baseline.json"
    baseline_path.write_bytes(canonical_dumps(bundle["baseline"]))
    trace_path = out / "trace.log"
    trace_path.write_text(bundle["trace"], encoding="utf-8")
    paths["baseline"] = baseline_path
    paths["trace"] = trace_path
    return paths
