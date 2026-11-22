"""Initial state parsing, validation, and normalized immutable match data."""

from __future__ import annotations

from dataclasses import dataclass
from typing import Any

from .codes import E_ARG, E_NUM, E_STATE, TickArenaError
from .numeric import BPS, NumericError, assert_no_float, fingerprint, require_int, require_str

INITIAL_FORMAT_VERSION = 1

DEFAULT_RULES: dict[str, int] = {
    "gather_base": 20,
    "gather_variance": 5,
    "gather_drop_bps": 1_200,
    "potion_drop": 1,
    "potion_heal_bps": 3_500,
    "revive_hp_bps": 5_000,
    "revive_resource_bps": 5_000,
    "passive_resource": 1,
    "score_per_kill": 1,
    "win_score": 5,
    "crit_multiplier_bps": 20_000,
}


@dataclass(frozen=True)
class Rules:
    gather_base: int
    gather_variance: int
    gather_drop_bps: int
    potion_drop: int
    potion_heal_bps: int
    revive_hp_bps: int
    revive_resource_bps: int
    passive_resource: int
    score_per_kill: int
    win_score: int
    crit_multiplier_bps: int


@dataclass(frozen=True)
class SkillDef:
    skill_id: str
    cost: int
    cooldown: int
    range: int
    damage: int
    variance_bps: int
    crit_bps: int
    hit_bps: int
    crit_multiplier_bps: int


@dataclass(frozen=True)
class UnitDef:
    unit_id: str
    player_id: str
    team: str
    x: int
    y: int
    spawn_x: int
    spawn_y: int
    hp: int
    max_hp: int
    resource: int
    max_resource: int
    attack: int
    defense: int
    move_speed: int
    revive_ticks: int
    skills: dict[str, SkillDef]
    items: dict[str, int]


@dataclass(frozen=True)
class MatchDef:
    normalized_initial: dict[str, Any]
    initial_fingerprint: str
    seed: int
    tick_count: int
    width: int
    height: int
    rules: Rules
    units: tuple[UnitDef, ...]
    team_ids: tuple[str, ...]

    @property
    def unit_map(self) -> dict[str, UnitDef]:
        return {unit.unit_id: unit for unit in self.units}

    @property
    def max_ticks(self) -> int:
        return self.tick_count


def _map_int(raw: dict[str, Any], field: str, default: int | None = None) -> int:
    if field not in raw:
        if default is None:
            raise TickArenaError(E_ARG, f"missing required field {field}")
        return default
    return require_int(raw[field], field)


def _normalize_skills(raw: Any, unit_path: str, default_crit_bps: int) -> dict[str, Any]:
    if not isinstance(raw, dict) or not raw:
        raise TickArenaError(E_ARG, f"{unit_path}.skills must be a non-empty object")
    result: dict[str, Any] = {}
    for skill_key, skill_raw in raw.items():
        skill_id = require_str(skill_key, f"{unit_path}.skills key")
        if not isinstance(skill_raw, dict):
            raise TickArenaError(E_ARG, f"{unit_path}.skills.{skill_id} must be an object")
        result[skill_id] = {
            "cost": _map_int(skill_raw, "cost"),
            "cooldown": _map_int(skill_raw, "cooldown"),
            "range": _map_int(skill_raw, "range"),
            "damage": _map_int(skill_raw, "damage"),
            "variance_bps": _map_int(skill_raw, "variance_bps", 0),
            "crit_bps": _map_int(skill_raw, "crit_bps", 0),
            "hit_bps": _map_int(skill_raw, "hit_bps", BPS),
            "crit_multiplier_bps": _map_int(
                skill_raw, "crit_multiplier_bps", default_crit_bps
            ),
        }
        skill = result[skill_id]
        if skill["cost"] < 0 or skill["cooldown"] < 0 or skill["range"] < 0:
            raise NumericError(f"{unit_path}.skills.{skill_id} cost/cooldown/range < 0")
        if skill["damage"] < 0:
            raise NumericError(f"{unit_path}.skills.{skill_id}.damage < 0")
        if not 0 <= skill["variance_bps"] <= BPS:
            raise NumericError(
                f"{unit_path}.skills.{skill_id}.variance_bps must be in [0, 10000]"
            )
        if not 0 <= skill["crit_bps"] <= BPS:
            raise NumericError(
                f"{unit_path}.skills.{skill_id}.crit_bps must be in [0, 10000]"
            )
        if not 0 <= skill["hit_bps"] <= BPS:
            raise NumericError(
                f"{unit_path}.skills.{skill_id}.hit_bps must be in [0, 10000]"
            )
        if skill["crit_multiplier_bps"] < BPS:
            raise NumericError(
                f"{unit_path}.skills.{skill_id}.crit_multiplier_bps must be >= 10000"
            )
    return result


def _normalize_items(raw: Any, unit_path: str) -> dict[str, int]:
    if raw is None:
        return {}
    if not isinstance(raw, dict):
        raise TickArenaError(E_ARG, f"{unit_path}.items must be an object")
    result: dict[str, int] = {}
    for item_key, count in raw.items():
        item_id = require_str(item_key, f"{unit_path}.items key")
        result[item_id] = require_int(count, f"{unit_path}.items.{item_id}", minimum=0)
    return result


def parse_initial_state(raw: Any) -> MatchDef:
    """Validate and normalize an initial match table without using floating point."""

    try:
        assert_no_float(raw)
    except NumericError:
        raise
    if not isinstance(raw, dict):
        raise TickArenaError(E_ARG, "initial state must be an object")

    version = _map_int(raw, "format_version", INITIAL_FORMAT_VERSION)
    if version != INITIAL_FORMAT_VERSION:
        raise TickArenaError(E_STATE, f"unknown initial state format_version {version}")

    seed = require_int(raw.get("seed"), "seed", minimum=0, maximum=(1 << 64) - 1)
    tick_count = require_int(raw.get("tick_count"), "tick_count", minimum=1)

    map_raw = raw.get("map")
    if not isinstance(map_raw, dict):
        raise TickArenaError(E_ARG, "map must be an object")
    width = require_int(map_raw.get("width"), "map.width", minimum=2)
    height = require_int(map_raw.get("height"), "map.height", minimum=2)

    rules_raw = raw.get("rules", {})
    if not isinstance(rules_raw, dict):
        raise TickArenaError(E_ARG, "rules must be an object")
    unknown_rules = sorted(set(rules_raw) - set(DEFAULT_RULES))
    if unknown_rules:
        raise TickArenaError(E_ARG, f"unknown rules: {', '.join(unknown_rules)}")
    merged_rules = dict(DEFAULT_RULES)
    merged_rules.update(rules_raw)
    for key, value in merged_rules.items():
        merged_rules[key] = require_int(value, f"rules.{key}", minimum=0)
    for key in (
        "gather_drop_bps",
        "potion_heal_bps",
        "revive_hp_bps",
        "revive_resource_bps",
    ):
        if merged_rules[key] > BPS:
            raise NumericError(f"rules.{key} must be <= 10000")
    if merged_rules["score_per_kill"] < 1:
        raise NumericError("rules.score_per_kill must be >= 1")
    if merged_rules["win_score"] < 1:
        raise NumericError("rules.win_score must be >= 1")
    if merged_rules["crit_multiplier_bps"] < BPS:
        raise NumericError("rules.crit_multiplier_bps must be >= 10000")

    players_raw = raw.get("players")
    if not isinstance(players_raw, list) or len(players_raw) < 2:
        raise TickArenaError(E_ARG, "players must contain at least two players")

    normalized_players: list[dict[str, Any]] = []
    player_ids: set[str] = set()
    unit_ids: set[str] = set()
    team_ids: set[str] = set()
    for index, player_raw in enumerate(players_raw):
        path = f"players[{index}]"
        if not isinstance(player_raw, dict):
            raise TickArenaError(E_ARG, f"{path} must be an object")
        player_id = require_str(player_raw.get("player_id"), f"{path}.player_id")
        team = require_str(player_raw.get("team"), f"{path}.team")
        if player_id in player_ids:
            raise TickArenaError(E_ARG, f"duplicate player_id {player_id}")
        player_ids.add(player_id)
        team_ids.add(team)
        unit_raw = player_raw.get("unit")
        if not isinstance(unit_raw, dict):
            raise TickArenaError(E_ARG, f"{path}.unit must be an object")
        unit_path = f"{path}.unit"
        unit_id = require_str(unit_raw.get("unit_id"), f"{unit_path}.unit_id")
        if unit_id in unit_ids:
            raise TickArenaError(E_ARG, f"duplicate unit_id {unit_id}")
        unit_ids.add(unit_id)
        x = require_int(unit_raw.get("x"), f"{unit_path}.x", minimum=0, maximum=width - 1)
        y = require_int(unit_raw.get("y"), f"{unit_path}.y", minimum=0, maximum=height - 1)
        spawn_x = _map_int(unit_raw, "spawn_x", x)
        spawn_y = _map_int(unit_raw, "spawn_y", y)
        if not 0 <= spawn_x < width or not 0 <= spawn_y < height:
            raise TickArenaError(E_ARG, f"{unit_path} spawn is outside the map")
        max_hp = require_int(unit_raw.get("max_hp"), f"{unit_path}.max_hp", minimum=1)
        hp = require_int(unit_raw.get("hp"), f"{unit_path}.hp", minimum=1, maximum=max_hp)
        max_resource = require_int(
            unit_raw.get("max_resource"), f"{unit_path}.max_resource", minimum=0
        )
        resource = require_int(
            unit_raw.get("resource"),
            f"{unit_path}.resource",
            minimum=0,
            maximum=max_resource,
        )
        normalized_players.append(
            {
                "player_id": player_id,
                "team": team,
                "unit": {
                    "unit_id": unit_id,
                    "x": x,
                    "y": y,
                    "spawn_x": spawn_x,
                    "spawn_y": spawn_y,
                    "hp": hp,
                    "max_hp": max_hp,
                    "resource": resource,
                    "max_resource": max_resource,
                    "attack": _map_int(unit_raw, "attack", 0),
                    "defense": _map_int(unit_raw, "defense", 0),
                    "move_speed": _map_int(unit_raw, "move_speed", 1),
                    "revive_ticks": _map_int(unit_raw, "revive_ticks", 20),
                    "skills": _normalize_skills(
                        unit_raw.get("skills"),
                        unit_path,
                        merged_rules["crit_multiplier_bps"],
                    ),
                    "items": _normalize_items(unit_raw.get("items"), unit_path),
                },
            }
        )
        unit = normalized_players[-1]["unit"]
        if unit["attack"] < 0 or unit["defense"] < 0:
            raise NumericError(f"{unit_path} attack/defense must be >= 0")
        if unit["move_speed"] < 0:
            raise NumericError(f"{unit_path}.move_speed must be >= 0")
        if unit["revive_ticks"] < 1:
            raise NumericError(f"{unit_path}.revive_ticks must be >= 1")

    if len(team_ids) < 2:
        raise TickArenaError(E_ARG, "at least two distinct teams are required")

    normalized: dict[str, Any] = {
        "format_version": INITIAL_FORMAT_VERSION,
        "seed": seed,
        "tick_count": tick_count,
        "map": {"width": width, "height": height},
        "rules": merged_rules,
        "players": normalized_players,
    }
    initial_hash = fingerprint(normalized, "tickarena.initial.v1")

    units: list[UnitDef] = []
    for player in normalized_players:
        unit_raw = player["unit"]
        skills = {
            skill_id: SkillDef(
                skill_id=skill_id,
                cost=skill_raw["cost"],
                cooldown=skill_raw["cooldown"],
                range=skill_raw["range"],
                damage=skill_raw["damage"],
                variance_bps=skill_raw["variance_bps"],
                crit_bps=skill_raw["crit_bps"],
                hit_bps=skill_raw["hit_bps"],
                crit_multiplier_bps=skill_raw["crit_multiplier_bps"],
            )
            for skill_id, skill_raw in unit_raw["skills"].items()
        }
        units.append(
            UnitDef(
                unit_id=unit_raw["unit_id"],
                player_id=player["player_id"],
                team=player["team"],
                x=unit_raw["x"],
                y=unit_raw["y"],
                spawn_x=unit_raw["spawn_x"],
                spawn_y=unit_raw["spawn_y"],
                hp=unit_raw["hp"],
                max_hp=unit_raw["max_hp"],
                resource=unit_raw["resource"],
                max_resource=unit_raw["max_resource"],
                attack=unit_raw["attack"],
                defense=unit_raw["defense"],
                move_speed=unit_raw["move_speed"],
                revive_ticks=unit_raw["revive_ticks"],
                skills=skills,
                items=dict(unit_raw["items"]),
            )
        )

    rules = Rules(**merged_rules)
    return MatchDef(
        normalized_initial=normalized,
        initial_fingerprint=initial_hash,
        seed=seed,
        tick_count=tick_count,
        width=width,
        height=height,
        rules=rules,
        units=tuple(units),
        team_ids=tuple(sorted(team_ids)),
    )

