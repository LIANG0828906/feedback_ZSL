"""Record parsing and field-level validation."""

from __future__ import annotations

from dataclasses import dataclass
from pathlib import Path
from typing import Any

from .config import ReplayConfig
from .errors import RecordInvalid
from .util import as_decimal, load_json, to_units


@dataclass(frozen=True, slots=True)
class CoverSpec:
    block_id: int
    x: int
    y: int
    width: int
    height: int
    hardness: int
    durability_max: int
    segment_threshold: int
    spread_radius: int
    memory_units: int

    def contains(self, x: int, y: int) -> bool:
        return self.x <= x <= self.x + self.width and self.y <= y <= self.y + self.height

    def rectangle_distance_squared(self, other: "CoverSpec") -> int:
        dx = max(
            0,
            max(other.x - (self.x + self.width), self.x - (other.x + other.width)),
        )
        dy = max(
            0,
            max(other.y - (self.y + self.height), self.y - (other.y + other.height)),
        )
        return dx * dx + dy * dy


@dataclass(frozen=True, slots=True)
class ShotSpec:
    shot_index: int
    player_id: str
    client_seq: int
    local_timestamp: str
    arrival_seq: int
    block_id: int
    hit_x: int
    hit_y: int
    incidence_angle: int
    momentum: int
    declared_step: int
    predicted_damage: int | None


@dataclass(frozen=True, slots=True)
class ReplayRecord:
    match_id: str
    model_version: str | None
    baseline_version: str | None
    covers: tuple[CoverSpec, ...]
    shots: tuple[ShotSpec, ...]
    raw: dict[str, Any]

    @property
    def covers_by_id(self) -> dict[int, CoverSpec]:
        return {cover.block_id: cover for cover in self.covers}

    @property
    def shots_by_step(self) -> dict[int, tuple[ShotSpec, ...]]:
        grouped: dict[int, list[ShotSpec]] = {}
        for shot in self.shots:
            grouped.setdefault(shot.declared_step, []).append(shot)
        return {
            step: tuple(
                sorted(
                    shots,
                    key=lambda shot: (shot.player_id, shot.client_seq, shot.shot_index),
                )
            )
            for step, shots in grouped.items()
        }


def _required(payload: dict[str, Any], name: str, field: str, shot_index: int | None = None) -> Any:
    if name not in payload:
        raise RecordInvalid(
            f"missing field {field}.{name}", shot_index=shot_index, field=f"{field}.{name}"
        )
    return payload[name]


def _integer(
    value: Any,
    field: str,
    *,
    shot_index: int | None = None,
    minimum: int | None = None,
) -> int:
    if isinstance(value, bool) or not isinstance(value, int):
        raise RecordInvalid(
            f"{field} must be an integer", shot_index=shot_index, field=field
        )
    if minimum is not None and value < minimum:
        raise RecordInvalid(
            f"{field} must be at least {minimum}", shot_index=shot_index, field=field
        )
    return value


def _number_units(
    value: Any,
    field: str,
    config: ReplayConfig,
    *,
    shot_index: int | None = None,
    nonnegative: bool = False,
) -> int:
    if isinstance(value, bool) or value is None:
        raise RecordInvalid(
            f"{field} must be a finite number", shot_index=shot_index, field=field
        )
    try:
        decimal = as_decimal(value, field)
    except Exception:
        raise RecordInvalid(
            f"{field} must be a finite number", shot_index=shot_index, field=field
        ) from None
    if nonnegative and decimal < 0:
        raise RecordInvalid(
            f"{field} must be non-negative", shot_index=shot_index, field=field
        )
    return to_units(decimal, config.fractional_digits, field)


def parse_cover(
    raw: Any, index: int, config: ReplayConfig
) -> CoverSpec:
    field_root = f"covers[{index}]"
    if not isinstance(raw, dict):
        raise RecordInvalid(f"{field_root} must be an object", field=field_root)
    block_id = _integer(
        _required(raw, "block_id", field_root), f"{field_root}.block_id", minimum=0
    )
    x = _number_units(_required(raw, "x", field_root), f"{field_root}.x", config)
    y = _number_units(_required(raw, "y", field_root), f"{field_root}.y", config)
    width = _number_units(
        _required(raw, "width", field_root),
        f"{field_root}.width",
        config,
        nonnegative=True,
    )
    height = _number_units(
        _required(raw, "height", field_root),
        f"{field_root}.height",
        config,
        nonnegative=True,
    )
    hardness = _number_units(
        _required(raw, "material_hardness", field_root),
        f"{field_root}.material_hardness",
        config,
        nonnegative=True,
    )
    durability = _number_units(
        _required(raw, "durability_max", field_root),
        f"{field_root}.durability_max",
        config,
        nonnegative=True,
    )
    threshold = _number_units(
        _required(raw, "segment_damage_threshold", field_root),
        f"{field_root}.segment_damage_threshold",
        config,
        nonnegative=True,
    )
    spread_radius = _number_units(
        _required(raw, "spread_radius", field_root),
        f"{field_root}.spread_radius",
        config,
        nonnegative=True,
    )
    memory_units = raw.get("memory_units", config.memory_units_per_block)
    memory_units = _integer(
        memory_units,
        f"{field_root}.memory_units",
        minimum=1,
    )
    if width <= 0 or height <= 0:
        raise RecordInvalid(
            f"{field_root}.width and height must be positive", field=f"{field_root}.width"
        )
    if hardness <= 0:
        raise RecordInvalid(
            f"{field_root}.material_hardness must be positive",
            field=f"{field_root}.material_hardness",
            block_id=block_id,
        )
    if durability <= 0:
        raise RecordInvalid(
            f"{field_root}.durability_max must be positive",
            field=f"{field_root}.durability_max",
            block_id=block_id,
        )
    if threshold <= 0:
        raise RecordInvalid(
            f"{field_root}.segment_damage_threshold must be positive",
            field=f"{field_root}.segment_damage_threshold",
            block_id=block_id,
        )
    return CoverSpec(
        block_id=block_id,
        x=x,
        y=y,
        width=width,
        height=height,
        hardness=hardness,
        durability_max=durability,
        segment_threshold=threshold,
        spread_radius=spread_radius,
        memory_units=memory_units,
    )


def parse_shot(
    raw: Any,
    index: int,
    config: ReplayConfig,
    covers_by_id: dict[int, CoverSpec],
) -> ShotSpec:
    field_root = f"shots[{index}]"
    if not isinstance(raw, dict):
        raise RecordInvalid(
            f"{field_root} must be an object", shot_index=index, field=field_root
        )
    player_id = _required(raw, "player_id", field_root, index)
    if not isinstance(player_id, str) or not player_id:
        raise RecordInvalid(
            f"{field_root}.player_id must be a non-empty string",
            shot_index=index,
            field=f"{field_root}.player_id",
        )
    client_seq = _integer(
        _required(raw, "client_seq", field_root, index),
        f"{field_root}.client_seq",
        shot_index=index,
        minimum=0,
    )
    timestamp = _required(raw, "local_timestamp", field_root, index)
    if isinstance(timestamp, bool) or not isinstance(timestamp, (str, int, float)):
        raise RecordInvalid(
            f"{field_root}.local_timestamp must be a scalar",
            shot_index=index,
            field=f"{field_root}.local_timestamp",
        )
    arrival_seq = _integer(
        _required(raw, "arrival_seq", field_root, index),
        f"{field_root}.arrival_seq",
        shot_index=index,
        minimum=0,
    )
    block_id = _integer(
        _required(raw, "block_id", field_root, index),
        f"{field_root}.block_id",
        shot_index=index,
        minimum=0,
    )
    cover = covers_by_id.get(block_id)
    if cover is None:
        raise RecordInvalid(
            f"{field_root}.block_id refers to an unknown cover",
            shot_index=index,
            field=f"{field_root}.block_id",
            block_id=block_id,
        )
    hit = _required(raw, "hit_position", field_root, index)
    if not isinstance(hit, dict):
        raise RecordInvalid(
            f"{field_root}.hit_position must be an object",
            shot_index=index,
            field=f"{field_root}.hit_position",
        )
    hit_x = _number_units(
        _required(hit, "x", f"{field_root}.hit_position", index),
        f"{field_root}.hit_position.x",
        config,
        shot_index=index,
    )
    hit_y = _number_units(
        _required(hit, "y", f"{field_root}.hit_position", index),
        f"{field_root}.hit_position.y",
        config,
        shot_index=index,
    )
    if not cover.contains(hit_x, hit_y):
        raise RecordInvalid(
            f"{field_root}.hit_position lies outside cover {block_id}",
            shot_index=index,
            field=f"{field_root}.hit_position",
            block_id=block_id,
        )
    angle = _number_units(
        _required(raw, "incidence_angle", field_root, index),
        f"{field_root}.incidence_angle",
        config,
        shot_index=index,
        nonnegative=True,
    )
    momentum = _number_units(
        _required(raw, "momentum", field_root, index),
        f"{field_root}.momentum",
        config,
        shot_index=index,
        nonnegative=True,
    )
    declared_step = _integer(
        _required(raw, "declared_step", field_root, index),
        f"{field_root}.declared_step",
        shot_index=index,
        minimum=1,
    )
    if declared_step > config.physics_steps:
        raise RecordInvalid(
            f"{field_root}.declared_step exceeds physics_steps",
            shot_index=index,
            field=f"{field_root}.declared_step",
        )
    predicted: int | None = None
    if "predicted_damage" in raw:
        predicted = _number_units(
            raw["predicted_damage"],
            f"{field_root}.predicted_damage",
            config,
            shot_index=index,
            nonnegative=True,
        )
    return ShotSpec(
        shot_index=index,
        player_id=player_id,
        client_seq=client_seq,
        local_timestamp=str(timestamp),
        arrival_seq=arrival_seq,
        block_id=block_id,
        hit_x=hit_x,
        hit_y=hit_y,
        incidence_angle=angle,
        momentum=momentum,
        declared_step=declared_step,
        predicted_damage=predicted,
    )


def parse_record(payload: dict[str, Any], config: ReplayConfig) -> ReplayRecord:
    if not isinstance(payload, dict):
        raise RecordInvalid("record root must be an object", field="$")
    match_id = _required(payload, "match_id", "record")
    if not isinstance(match_id, str) or not match_id:
        raise RecordInvalid("record.match_id must be a non-empty string", field="match_id")
    raw_covers = _required(payload, "covers", "record")
    if not isinstance(raw_covers, list) or not raw_covers:
        raise RecordInvalid("record.covers must be a non-empty array", field="covers")
    covers: list[CoverSpec] = []
    seen_blocks: set[int] = set()
    for index, raw_cover in enumerate(raw_covers):
        cover = parse_cover(raw_cover, index, config)
        if cover.block_id in seen_blocks:
            raise RecordInvalid(
                f"duplicate cover block_id {cover.block_id}",
                field=f"covers[{index}].block_id",
                block_id=cover.block_id,
            )
        seen_blocks.add(cover.block_id)
        covers.append(cover)
    covers_by_id = {cover.block_id: cover for cover in covers}
    raw_shots = _required(payload, "shots", "record")
    if not isinstance(raw_shots, list):
        raise RecordInvalid("record.shots must be an array", field="shots")
    shots: list[ShotSpec] = []
    seen_sequences: dict[tuple[str, int], int] = {}
    for index, raw_shot in enumerate(raw_shots):
        shot = parse_shot(raw_shot, index, config, covers_by_id)
        key = (shot.player_id, shot.client_seq)
        if key in seen_sequences:
            raise RecordInvalid(
                f"duplicate client_seq for player {shot.player_id}",
                shot_index=index,
                field=f"shots[{index}].client_seq",
            )
        seen_sequences[key] = index
        shots.append(shot)
    model_version = payload.get("model_version")
    baseline_version = payload.get("baseline_version")
    if model_version is not None and not isinstance(model_version, str):
        raise RecordInvalid("record.model_version must be a string", field="model_version")
    if baseline_version is not None and not isinstance(baseline_version, str):
        raise RecordInvalid(
            "record.baseline_version must be a string", field="baseline_version"
        )
    return ReplayRecord(
        match_id=match_id,
        model_version=model_version,
        baseline_version=baseline_version,
        covers=tuple(covers),
        shots=tuple(shots),
        raw=payload,
    )


def load_record(path: str | Path, config: ReplayConfig) -> ReplayRecord:
    try:
        payload = load_json(path)
    except (OSError, ValueError) as error:
        raise RecordInvalid(f"cannot read record: {error}", field="$") from None
    return parse_record(payload, config)
