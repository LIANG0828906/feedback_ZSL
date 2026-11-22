"""Deterministic local event-stream generator."""

from __future__ import annotations

import argparse
import hashlib
import json
from decimal import Decimal, InvalidOperation
from pathlib import Path
from typing import Any, Iterator

from .codes import E_ARG, TickArenaError
from .events import inspect_event_file, write_event_stream
from .model import parse_initial_state
from .numeric import BPS, require_int


def _gen_u64(*parts: Any) -> int:
    message = ":".join(str(part) for part in parts).encode("utf-8")
    return int.from_bytes(
        hashlib.blake2b(message, digest_size=8, person=b"genarena").digest(),
        "big",
    )


def density_to_bps(event_density: Any) -> int:
    try:
        value = Decimal(str(event_density))
    except (InvalidOperation, ValueError) as exc:
        raise TickArenaError(E_ARG, "event_density must be a decimal number") from exc
    if value < 0:
        raise TickArenaError(E_ARG, "event_density must be >= 0")
    scaled = value * BPS
    if scaled != scaled.to_integral_value():
        raise TickArenaError(E_ARG, "event_density supports at most four decimals")
    return int(scaled)


def make_initial_state(
    *,
    seed: int,
    players: int = 4,
    tick_count: int = 2_000,
    win_score: int = 5,
    map_size: int = 32,
) -> dict[str, Any]:
    require_int(seed, "seed", minimum=0, maximum=(1 << 64) - 1)
    players = require_int(players, "players", minimum=2, maximum=16)
    tick_count = require_int(tick_count, "tick_count", minimum=1)
    win_score = require_int(win_score, "win_score", minimum=1)
    map_size = require_int(map_size, "map_size", minimum=8)
    player_rows: list[dict[str, Any]] = []
    for index in range(players):
        team = "red" if index % 2 == 0 else "blue"
        lane = index // 2
        x = 2 if team == "red" else map_size - 3
        y = min(map_size - 2, 2 + lane * 3)
        player_rows.append(
            {
                "player_id": f"p{index + 1}",
                "team": team,
                "unit": {
                    "unit_id": f"u{index + 1}",
                    "x": x,
                    "y": y,
                    "spawn_x": x,
                    "spawn_y": y,
                    "hp": 260,
                    "max_hp": 260,
                    "resource": 100,
                    "max_resource": 200,
                    "attack": 10,
                    "defense": 2,
                    "move_speed": 1,
                    "revive_ticks": 25,
                    "skills": {
                        "bolt": {
                            "cost": 18,
                            "cooldown": 4,
                            "range": 64,
                            "damage": 45,
                            "variance_bps": 1_200,
                            "crit_bps": 1_500,
                            "hit_bps": 9_200,
                            "crit_multiplier_bps": 20_000,
                        }
                    },
                    "items": {"potion": 3},
                },
            }
        )
    initial = {
        "format_version": 1,
        "seed": seed,
        "tick_count": tick_count,
        "map": {"width": map_size, "height": map_size},
        "rules": {
            "gather_base": 20,
            "gather_variance": 5,
            "gather_drop_bps": 1_200,
            "potion_drop": 1,
            "potion_heal_bps": 3_500,
            "revive_hp_bps": 5_000,
            "revive_resource_bps": 5_000,
            "passive_resource": 1,
            "score_per_kill": 1,
            "win_score": win_score,
            "crit_multiplier_bps": 20_000,
        },
        "players": player_rows,
    }
    parse_initial_state(initial)
    return initial


def generate_events(
    initial_state: dict[str, Any],
    *,
    event_density: Any = 1.0,
) -> Iterator[dict[str, Any]]:
    match = parse_initial_state(initial_state)
    density_bps = density_to_bps(event_density)
    count_per_tick = density_bps // BPS
    remainder = density_bps % BPS
    units = [unit.unit_id for unit in match.units]
    teams = {unit.unit_id: unit.team for unit in match.units}
    for tick in range(match.tick_count):
        count = count_per_tick
        if remainder and _gen_u64(match.seed, tick, "density") % BPS < remainder:
            count += 1
        for ordinal in range(count):
            marker = _gen_u64(match.seed, tick, ordinal, "event")
            actor_index = marker % len(units)
            actor = units[actor_index]
            selector = (marker >> 16) % BPS
            if selector < 6_000:
                directions = ((1, 0), (-1, 0), (0, 1), (0, -1))
                dx, dy = directions[(marker >> 32) % 4]
                event = {
                    "tick": tick,
                    "type": "move",
                    "actor": actor,
                    "dx": dx,
                    "dy": dy,
                }
            elif selector < 7_600:
                enemies = [unit for unit in units if teams[unit] != teams[actor]]
                target = enemies[(marker >> 32) % len(enemies)]
                event = {
                    "tick": tick,
                    "type": "skill",
                    "actor": actor,
                    "skill": "bolt",
                    "target": target,
                }
            elif selector < 9_000:
                event = {
                    "tick": tick,
                    "type": "gather",
                    "actor": actor,
                }
            else:
                event = {
                    "tick": tick,
                    "type": "item",
                    "actor": actor,
                    "item": "potion",
                }
            yield event


def write_generated_match(
    path: str | Path,
    initial_state: dict[str, Any],
    *,
    event_density: Any,
) -> str:
    return write_event_stream(
        path,
        initial_state,
        generate_events(initial_state, event_density=event_density),
    )


def write_event_file(
    path: str | Path,
    initial_state: dict[str, Any],
    *,
    event_density: Any,
) -> str:
    return write_generated_match(path, initial_state, event_density=event_density)


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--seed", type=int, required=True)
    parser.add_argument("--players", type=int, default=4)
    parser.add_argument("--ticks", type=int, required=True)
    parser.add_argument(
        "--event-density",
        default="1.0",
        help="average events per tick; up to four decimal places",
    )
    parser.add_argument(
        "--win-score",
        type=int,
        default=5,
        help="team kill score that locks the match; use a large value to reach the tick limit",
    )
    parser.add_argument("--map-size", type=int, default=32)
    parser.add_argument("--output", required=True)
    args = parser.parse_args(argv)
    initial = make_initial_state(
        seed=args.seed,
        players=args.players,
        tick_count=args.ticks,
        win_score=args.win_score,
        map_size=args.map_size,
    )
    try:
        expected_hash = write_generated_match(
            args.output,
            initial,
            event_density=args.event_density,
        )
        info = inspect_event_file(args.output)
    except TickArenaError as exc:
        print(json.dumps({"code": exc.code, "message": exc.message}, ensure_ascii=False))
        return 2
    info["write_fingerprint"] = expected_hash
    print(json.dumps(info, ensure_ascii=False, sort_keys=True))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

