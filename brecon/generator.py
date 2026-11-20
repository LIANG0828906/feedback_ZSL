from __future__ import annotations

import random
from dataclasses import dataclass
from pathlib import Path
from typing import Any

from .canonical import MODEL_VERSION, compact_json
from .engine import ReplayEngine
from .errors import BreconError
from .io_utils import write_json_atomic, write_text_atomic
from .lock import RoomBusyLock, room_lock_path

ITEMS = ("ammo", "armor", "fuel", "med", "scope")


@dataclass(frozen=True)
class Profile:
    players: int
    steps: int
    disorder: str
    seed: int
    name: str | None = None

    @property
    def sample_name(self) -> str:
        if self.name:
            return self.name
        return f"p{self.players}_s{self.steps}_{self.disorder}"


def default_profiles(base_seed: int = 20261121) -> list[Profile]:
    definitions = (
        (2, 12, "low"),
        (4, 20, "medium"),
        (8, 30, "high"),
    )
    return [
        Profile(
            players=players,
            steps=steps,
            disorder=disorder,
            seed=base_seed + index * 100003,
        )
        for index, (players, steps, disorder) in enumerate(definitions)
    ]


def _boxes(profile: Profile) -> list[dict[str, Any]]:
    if profile.disorder == "low":
        box_count, slots_per_box = 2, 3
    elif profile.disorder == "high":
        box_count, slots_per_box = max(2, (profile.players + 2) // 3), 8
    else:
        box_count, slots_per_box = 2, 4
    boxes: list[dict[str, Any]] = []
    for box_index in range(box_count):
        slots = []
        for slot_index in range(slots_per_box):
            slots.append(
                {
                    "slot_id": f"s{box_index:02d}{slot_index:02d}",
                    "item": ITEMS[(box_index * 3 + slot_index) % len(ITEMS)],
                    "quantity": 1,
                }
            )
        boxes.append({"box_id": f"box-{box_index + 1:02d}", "slots": slots})
    return boxes


def _sync_steps(steps: int) -> list[int]:
    values = {
        1,
        max(1, steps // 4),
        max(1, steps // 2),
        max(1, (steps * 3) // 4),
        steps,
    }
    return sorted(step for step in values if step <= steps)


def _config(profile: Profile, room_id: str) -> dict[str, Any]:
    players = [f"p{index + 1:02d}" for index in range(profile.players)]
    return {
        "model_version": MODEL_VERSION,
        "room_id": room_id,
        "players": players,
        "step_ms": 100,
        "origin_ts_ms": 0,
        "compensation_window_ms": 300,
        "observation": {
            "start_step": 1,
            "end_step": profile.steps,
            "sync_steps": _sync_steps(profile.steps),
            "tolerance": 0,
        },
        "boxes": _boxes(profile),
        "initial_inventory": {player: {} for player in players},
        "operations_file": "operations.json",
        "baseline_file": "baseline.json",
        "disorder": profile.disorder,
        "seed": profile.seed,
        "trace_file": "trace.jsonl",
        "latency_file": "latency.json",
        "snapshot_dir": "snapshots",
        "strict_latency": True,
    }


def _operation(
    *,
    op_type: str,
    player: str,
    client_seq: int,
    local_ts_ms: int,
    arrival_ts_ms: int,
    sample: str,
    box_id: str | None = None,
    slot_id: str | None = None,
    item: str | None = None,
    quantity: int = 1,
    target_client_seq: int | None = None,
) -> dict[str, Any]:
    operation: dict[str, Any] = {
        "op_type": op_type,
        "player_id": player,
        "client_seq": client_seq,
        "local_ts_ms": int(local_ts_ms),
        "arrival_ts_ms": int(arrival_ts_ms),
    }
    if op_type == "claim":
        operation.update(
            {
                "box_id": box_id,
                "slot_id": slot_id,
                "item": item,
                "quantity": quantity,
                "voucher_id": f"v-{sample}-{player}-{client_seq:04d}",
            }
        )
    else:
        operation["target_client_seq"] = int(target_client_seq or 0)
    return operation


def generate_operations(
    profile: Profile,
    config: dict[str, Any],
) -> list[dict[str, Any]]:
    rng = random.Random(profile.seed)
    sample = profile.sample_name
    players = list(config["players"])
    slots = [
        (box["box_id"], slot["slot_id"], slot["item"])
        for box in config["boxes"]
        for slot in box["slots"]
    ]
    claim_target = max(8, profile.players * max(4, (profile.steps + 1) // 2))
    delay_ranges = {
        "low": (2, 45),
        "medium": (4, 120),
        "high": (8, 220),
    }
    low_delay, high_delay = delay_ranges.get(profile.disorder, (5, 180))
    claims: list[dict[str, Any]] = []
    next_sequence = {player: 0 for player in players}
    for index in range(claim_target):
        player = players[index % len(players)]
        next_sequence[player] += 1
        box_id, slot_id, item = slots[rng.randrange(len(slots))]
        local_ts = (
            (index * config["step_ms"]) // max(1, profile.players)
            + rng.randint(5, 85)
        )
        delay = rng.randint(low_delay, high_delay)
        claim = _operation(
            op_type="claim",
            player=player,
            client_seq=next_sequence[player],
            local_ts_ms=local_ts,
            arrival_ts_ms=local_ts + delay,
            sample=sample,
            box_id=box_id,
            slot_id=slot_id,
            item=item,
        )
        claims.append(claim)

    operations = list(claims)

    same_step_count = max(1, profile.players // 2)
    for index in range(same_step_count):
        claim = claims[(index * max(1, len(claims) // same_step_count)) % len(claims)]
        scenario_step = min(profile.steps, 2 + index * 3)
        claim["local_ts_ms"] = scenario_step * config["step_ms"] + 5
        claim["arrival_ts_ms"] = scenario_step * config["step_ms"] + 8
        claim["_case"] = "same_step_revoke"
        player = claim["player_id"]
        next_sequence[player] += 1
        operations.append(
            _operation(
                op_type="revoke",
                player=player,
                client_seq=next_sequence[player],
                local_ts_ms=scenario_step * config["step_ms"] + 25,
                arrival_ts_ms=scenario_step * config["step_ms"] + 29,
                sample=sample,
                target_client_seq=claim["client_seq"],
            )
        )
        operations[-1]["_case"] = "same_step_revoke"

    late_count = 1 if profile.disorder == "low" else max(3, profile.players // 2)
    late_candidates = [claim for claim in claims if not claim.get("_case")] or claims
    for index in range(late_count):
        claim = late_candidates[(index * 7 + 3) % len(late_candidates)]
        arrival_step = min(profile.steps, 4 + index * 2)
        arrival_ts = arrival_step * config["step_ms"] - 10
        claim["arrival_ts_ms"] = arrival_ts
        claim["local_ts_ms"] = (
            arrival_step * config["step_ms"]
            - config["compensation_window_ms"]
            - 80
        )
        claim["_case"] = "late"

    duplicate_count = 1 + (1 if profile.disorder != "low" else 0)
    for index in range(duplicate_count):
        source = claims[(index * 11 + 5) % len(claims)]
        duplicate = dict(source)
        duplicate["arrival_ts_ms"] = int(source["arrival_ts_ms"]) + 35
        duplicate["_delivery"] = "duplicate"
        operations.append(duplicate)

    if profile.disorder == "high":
        conflict_candidates = [
            claim for claim in claims if claim.get("_case") not in {"same_step_revoke", "late"}
        ] or claims
        conflict_source = conflict_candidates[
            (profile.players + 2) % len(conflict_candidates)
        ]
        safe_arrival = min(
            int(conflict_source["arrival_ts_ms"]),
            profile.steps * config["step_ms"] - 20,
        )
        conflict_source["arrival_ts_ms"] = max(10, safe_arrival)
        conflict_source["local_ts_ms"] = max(
            5, int(conflict_source["arrival_ts_ms"]) - 20
        )
        conflict_source["item"] = "wrong-item"
        conflict_source["_case"] = "credit_conflict"

    if profile.steps >= 6:
        early = next(
            (
                claim
                for claim in claims
                if claim.get("_case") not in {"same_step_revoke", "late"}
                and int(claim["local_ts_ms"]) > 0
            ),
            claims[0],
        )
        early["local_ts_ms"] = max(5, early["local_ts_ms"])
        early["arrival_ts_ms"] = max(
            8, min(int(early["arrival_ts_ms"]), profile.steps * config["step_ms"] - 20)
        )
        player = early["player_id"]
        next_sequence[player] += 1
        closed_step = min(profile.steps, 6)
        operations.append(
            _operation(
                op_type="revoke",
                player=player,
                client_seq=next_sequence[player],
                local_ts_ms=closed_step * config["step_ms"] + 25,
                arrival_ts_ms=closed_step * config["step_ms"] + 29,
                sample=sample,
                target_client_seq=early["client_seq"],
            )
        )
        operations[-1]["_case"] = "revoke_closed"

    if profile.disorder != "low" or profile.steps >= 12:
        future_count = 2 if profile.disorder != "low" else 1
        for index in range(future_count):
            player = players[index % len(players)]
            next_sequence[player] += 1
            box_id, slot_id, item = slots[rng.randrange(len(slots))]
            local_ts = (profile.steps + 1) * config["step_ms"] + index * 31
            operations.append(
                _operation(
                    op_type="claim",
                    player=player,
                    client_seq=next_sequence[player],
                    local_ts_ms=local_ts,
                    arrival_ts_ms=local_ts + rng.randint(10, 80),
                    sample=sample,
                    box_id=box_id,
                    slot_id=slot_id,
                    item=item,
                )
            )

    ordered = sorted(
        enumerate(operations),
        key=lambda pair: (
            int(pair[1]["arrival_ts_ms"]),
            pair[0],
        ),
    )
    for arrival_sequence, (_, operation) in enumerate(ordered):
        operation["arrival_seq"] = arrival_sequence

    rng.shuffle(operations)
    return operations


def write_sample(
    workspace: str | Path,
    profile: Profile,
    *,
    force: bool = False,
) -> dict[str, Any]:
    workspace_path = Path(workspace).resolve()
    sample_dir = workspace_path / "samples" / profile.sample_name
    if sample_dir.exists() and any(sample_dir.iterdir()) and not force:
        raise BreconError(
            "E_EXISTS",
            f"sample already exists: {sample_dir}",
            details={"sample": profile.sample_name},
        )
    if force and sample_dir.exists():
        import shutil

        shutil.rmtree(sample_dir)
    sample_dir.mkdir(parents=True, exist_ok=True)

    room_id = f"room-{profile.sample_name}"
    config = _config(profile, room_id)
    operations = generate_operations(profile, config)
    with RoomBusyLock(room_id, room_lock_path(sample_dir)):
        write_json_atomic(sample_dir / "config.json", config)
        write_text_atomic(
            sample_dir / "operations.json",
            compact_json({"operations": operations}) + "\n",
        )
        engine = ReplayEngine.from_sample_dir(sample_dir, write_snapshots=True).run()
        engine.write_outputs(include_baseline=True)
    return {
        "name": profile.sample_name,
        "directory": f"samples/{profile.sample_name}",
        "players": profile.players,
        "steps": profile.steps,
        "disorder": profile.disorder,
        "seed": profile.seed,
        "room_id": room_id,
        "operations": len(engine.operations),
        "head_state_hash": engine.trace_entries[-1]["state_hash"],
        "head_chain_hash": engine.trace_entries[-1]["chain_hash"],
    }


def generate_workspace(
    workspace: str | Path,
    profiles: list[Profile] | None = None,
    *,
    force: bool = False,
) -> dict[str, Any]:
    workspace_path = Path(workspace).resolve()
    workspace_path.mkdir(parents=True, exist_ok=True)
    selected = profiles or default_profiles()
    samples = [
        write_sample(workspace_path, profile, force=force) for profile in selected
    ]
    active = {
        "model_version": MODEL_VERSION,
        "active_sample": samples[0]["name"] if samples else None,
        "samples": samples,
    }
    write_json_atomic(workspace_path / "active.json", active)
    return active

