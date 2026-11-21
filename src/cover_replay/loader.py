"""Strict local JSON loader with record/field-level diagnostics."""

from __future__ import annotations

import json
from decimal import Decimal
from pathlib import Path
from typing import Any

from .errors import E_REPLAY_INVALID, ReplayError
from .fixed import as_decimal
from .model import (
    Bounds,
    CoverSpec,
    Neighbor,
    ReplayConfig,
    Revoke,
    Scenario,
    Shot,
)


def _load_json(path: Path, field_path: str) -> Any:
    try:
        with path.open("r", encoding="utf-8") as handle:
            return json.load(handle, parse_float=Decimal)
    except FileNotFoundError:
        raise ReplayError(E_REPLAY_INVALID, f"file not found: {path.name}", None, field_path) from None
    except json.JSONDecodeError as error:
        raise ReplayError(
            E_REPLAY_INVALID,
            f"invalid JSON: {error.msg}",
            None,
            field_path,
            {"line": error.lineno, "column": error.colno},
        ) from None


def _require_object(value: Any, field_path: str, record_index: int | None = None) -> dict[str, Any]:
    if not isinstance(value, dict):
        raise ReplayError(E_REPLAY_INVALID, "expected an object", record_index, field_path)
    return value


def _require_list(value: Any, field_path: str, record_index: int | None = None) -> list[Any]:
    if not isinstance(value, list):
        raise ReplayError(E_REPLAY_INVALID, "expected an array", record_index, field_path)
    return value


def _require_field(
    record: dict[str, Any],
    key: str,
    field_path: str,
    record_index: int | None = None,
) -> Any:
    if key not in record:
        raise ReplayError(
            E_REPLAY_INVALID,
            "required field is missing",
            record_index,
            f"{field_path}.{key}",
        )
    return record[key]


def _require_string(
    record: dict[str, Any],
    key: str,
    field_path: str,
    record_index: int | None = None,
) -> str:
    value = _require_field(record, key, field_path, record_index)
    if not isinstance(value, str) or not value.strip():
        raise ReplayError(
            E_REPLAY_INVALID,
            "expected a non-empty string",
            record_index,
            f"{field_path}.{key}",
        )
    return value


def _require_int(
    record: dict[str, Any],
    key: str,
    field_path: str,
    record_index: int | None = None,
    *,
    minimum: int | None = None,
) -> int:
    value = _require_field(record, key, field_path, record_index)
    if isinstance(value, bool) or not isinstance(value, int):
        raise ReplayError(
            E_REPLAY_INVALID,
            "expected an integer",
            record_index,
            f"{field_path}.{key}",
        )
    if minimum is not None and value < minimum:
        raise ReplayError(
            E_REPLAY_INVALID,
            f"value must be >= {minimum}",
            record_index,
            f"{field_path}.{key}",
            {"actual": value},
        )
    return value


def _require_decimal(
    record: dict[str, Any],
    key: str,
    field_path: str,
    record_index: int | None = None,
) -> Decimal:
    value = _require_field(record, key, field_path, record_index)
    return as_decimal(value, field_path=f"{field_path}.{key}", record_index=record_index)


def _load_config(data: Any, field_path: str = "config") -> ReplayConfig:
    config = _require_object(data, field_path)
    if "schema_version" in config:
        schema_version = _require_int(config, "schema_version", field_path, minimum=1)
        if schema_version != 1:
            raise ReplayError(
                E_REPLAY_INVALID,
                "unsupported config schema_version",
                None,
                f"{field_path}.schema_version",
                {"expected": 1, "actual": schema_version},
            )
    physical_steps = _require_int(config, "physical_steps", field_path, minimum=1)
    decimals = _require_int(config, "fixed_point_decimals", field_path, minimum=0)
    if decimals > 9:
        raise ReplayError(
            E_REPLAY_INVALID,
            "fixed_point_decimals must be <= 9",
            None,
            f"{field_path}.fixed_point_decimals",
            {"actual": decimals},
        )
    parallelism = _require_int(config, "parallelism", field_path, minimum=1)
    memory_budget = _require_int(config, "memory_budget_bytes", field_path, minimum=1)
    raw_sequence = _require_list(
        _require_field(config, "degradation_sequence", field_path),
        f"{field_path}.degradation_sequence",
    )
    if not raw_sequence:
        raise ReplayError(
            E_REPLAY_INVALID,
            "degradation_sequence must not be empty",
            None,
            f"{field_path}.degradation_sequence",
        )
    sequence: list[Decimal] = []
    previous = Decimal(1)
    for index, raw_scale in enumerate(raw_sequence):
        scale = as_decimal(
            raw_scale,
            field_path=f"{field_path}.degradation_sequence[{index}]",
        )
        if not (Decimal(0) < scale <= Decimal(1)):
            raise ReplayError(
                E_REPLAY_INVALID,
                "degradation scale must be in (0, 1]",
                None,
                f"{field_path}.degradation_sequence[{index}]",
                {"actual": str(scale)},
            )
        if scale > previous:
            raise ReplayError(
                E_REPLAY_INVALID,
                "degradation_sequence must be non-increasing",
                None,
                f"{field_path}.degradation_sequence[{index}]",
                {"actual": str(scale), "previous": str(previous)},
            )
        sequence.append(scale)
        previous = scale
    crack_iterations = _require_int(config, "crack_max_iterations", field_path, minimum=1)
    propagation_ratio = _require_decimal(config, "crack_propagation_ratio", field_path)
    if not (Decimal(0) <= propagation_ratio <= Decimal(1)):
        raise ReplayError(
            E_REPLAY_INVALID,
            "crack_propagation_ratio must be in [0, 1]",
            None,
            f"{field_path}.crack_propagation_ratio",
            {"actual": str(propagation_ratio)},
        )
    revoke_window = _require_int(config, "revocation_window_steps", field_path, minimum=0)
    damage_model_version = _require_string(config, "damage_model_version", field_path)
    baseline_version = _require_string(config, "baseline_version", field_path)
    quantization_mode = config.get("quantization_mode", "half_even")
    if quantization_mode not in {"half_even", "half_up"}:
        raise ReplayError(
            E_REPLAY_INVALID,
            "quantization_mode must be half_even or half_up",
            None,
            f"{field_path}.quantization_mode",
            {"actual": quantization_mode},
        )
    return ReplayConfig(
        physical_steps=physical_steps,
        fixed_point_decimals=decimals,
        parallelism=parallelism,
        memory_budget_bytes=memory_budget,
        degradation_sequence=tuple(sequence),
        crack_max_iterations=crack_iterations,
        crack_propagation_ratio=propagation_ratio,
        revocation_window_steps=revoke_window,
        damage_model_version=damage_model_version,
        baseline_version=baseline_version,
        quantization_mode=quantization_mode,
    )


def _load_covers(data: Any) -> tuple[CoverSpec, ...]:
    records = _require_list(data, "covers")
    covers: list[CoverSpec] = []
    seen: set[str] = set()
    for index, raw_cover in enumerate(records):
        field_path = f"covers[{index}]"
        cover = _require_object(raw_cover, field_path, index)
        cover_id = _require_string(cover, "id", field_path, index)
        if cover_id in seen:
            raise ReplayError(
                E_REPLAY_INVALID,
                "duplicate cover id",
                index,
                f"{field_path}.id",
                {"actual": cover_id},
            )
        seen.add(cover_id)
        raw_bounds = _require_object(
            _require_field(cover, "bounds", field_path, index),
            f"{field_path}.bounds",
            index,
        )
        bounds = Bounds(
            min_x=_require_decimal(raw_bounds, "min_x", f"{field_path}.bounds", index),
            min_y=_require_decimal(raw_bounds, "min_y", f"{field_path}.bounds", index),
            max_x=_require_decimal(raw_bounds, "max_x", f"{field_path}.bounds", index),
            max_y=_require_decimal(raw_bounds, "max_y", f"{field_path}.bounds", index),
        )
        if bounds.min_x >= bounds.max_x:
            raise ReplayError(
                E_REPLAY_INVALID,
                "bounds.min_x must be < bounds.max_x",
                index,
                f"{field_path}.bounds.min_x",
            )
        if bounds.min_y >= bounds.max_y:
            raise ReplayError(
                E_REPLAY_INVALID,
                "bounds.min_y must be < bounds.max_y",
                index,
                f"{field_path}.bounds.min_y",
            )
        hardness = _require_decimal(cover, "material_hardness", field_path, index)
        if hardness <= 0:
            raise ReplayError(
                E_REPLAY_INVALID,
                "material_hardness must be > 0",
                index,
                f"{field_path}.material_hardness",
            )
        durability = _require_decimal(cover, "max_durability", field_path, index)
        if durability <= 0:
            raise ReplayError(
                E_REPLAY_INVALID,
                "max_durability must be > 0",
                index,
                f"{field_path}.max_durability",
            )
        threshold = _require_decimal(cover, "segment_damage_threshold", field_path, index)
        if threshold <= 0 or threshold > durability:
            raise ReplayError(
                E_REPLAY_INVALID,
                "segment_damage_threshold must be in (0, max_durability]",
                index,
                f"{field_path}.segment_damage_threshold",
                {"actual": str(threshold), "max_durability": str(durability)},
            )
        radius = _require_decimal(cover, "attenuation_radius", field_path, index)
        if radius <= 0:
            raise ReplayError(
                E_REPLAY_INVALID,
                "attenuation_radius must be > 0",
                index,
                f"{field_path}.attenuation_radius",
            )
        raw_neighbors = _require_list(
            _require_field(cover, "neighbors", field_path, index),
            f"{field_path}.neighbors",
            index,
        )
        neighbors: list[Neighbor] = []
        neighbor_ids: set[str] = set()
        for neighbor_index, raw_neighbor in enumerate(raw_neighbors):
            neighbor_path = f"{field_path}.neighbors[{neighbor_index}]"
            neighbor = _require_object(raw_neighbor, neighbor_path, index)
            neighbor_id = _require_string(neighbor, "cover_id", neighbor_path, index)
            if neighbor_id == cover_id:
                raise ReplayError(
                    E_REPLAY_INVALID,
                    "cover cannot be its own neighbor",
                    index,
                    f"{neighbor_path}.cover_id",
                )
            if neighbor_id in neighbor_ids:
                raise ReplayError(
                    E_REPLAY_INVALID,
                    "duplicate neighbor id",
                    index,
                    f"{neighbor_path}.cover_id",
                    {"actual": neighbor_id},
                )
            distance = _require_decimal(neighbor, "distance", neighbor_path, index)
            if distance < 0:
                raise ReplayError(
                    E_REPLAY_INVALID,
                    "neighbor distance must be >= 0",
                    index,
                    f"{neighbor_path}.distance",
                )
            neighbor_ids.add(neighbor_id)
            neighbors.append(Neighbor(cover_id=neighbor_id, distance=distance))
        buffer_bytes = _require_int(
            cover, "temporary_buffer_bytes", field_path, index, minimum=1
        )
        covers.append(
            CoverSpec(
                cover_id=cover_id,
                record_index=index,
                bounds=bounds,
                material_hardness=hardness,
                max_durability=durability,
                segment_damage_threshold=threshold,
                attenuation_radius=radius,
                neighbors=tuple(neighbors),
                temporary_buffer_bytes=buffer_bytes,
            )
        )
    if not covers:
        raise ReplayError(E_REPLAY_INVALID, "at least one cover is required", None, "covers")
    cover_ids = {cover.cover_id for cover in covers}
    for cover in covers:
        for neighbor_index, neighbor in enumerate(cover.neighbors):
            if neighbor.cover_id not in cover_ids:
                raise ReplayError(
                    E_REPLAY_INVALID,
                    "neighbor cover does not exist",
                    cover.record_index,
                    f"covers[{cover.record_index}].neighbors[{neighbor_index}].cover_id",
                    {"actual": neighbor.cover_id},
                )
    return tuple(covers)


def _resolve_cover(
    raw_position: dict[str, Any],
    covers: tuple[CoverSpec, ...],
    field_path: str,
    record_index: int,
    configured_cover_id: Any = None,
) -> str:
    hit_x = _require_decimal(raw_position, "x", field_path, record_index)
    hit_y = _require_decimal(raw_position, "y", field_path, record_index)
    containing = [
        cover.cover_id for cover in covers if cover.bounds.contains(hit_x, hit_y)
    ]
    if configured_cover_id is not None:
        if not isinstance(configured_cover_id, str) or not configured_cover_id:
            raise ReplayError(
                E_REPLAY_INVALID,
                "cover_id must be a non-empty string",
                record_index,
                f"shots[{record_index}].cover_id",
            )
        if configured_cover_id not in containing:
            if configured_cover_id not in {cover.cover_id for cover in covers}:
                message = "cover_id does not exist"
            else:
                message = "hit position is outside the declared cover"
            raise ReplayError(
                E_REPLAY_INVALID,
                message,
                record_index,
                f"shots[{record_index}].hit_position",
                {"cover_id": configured_cover_id},
            )
        return configured_cover_id
    if not containing:
        raise ReplayError(
            E_REPLAY_INVALID,
            "hit position is outside every cover",
            record_index,
            f"shots[{record_index}].hit_position",
            {"x": str(hit_x), "y": str(hit_y)},
        )
    return containing[0]


def _load_shots(
    data: Any,
    covers: tuple[CoverSpec, ...],
    physical_steps: int,
) -> tuple[Shot, ...]:
    records = _require_list(data, "shots")
    shots: list[Shot] = []
    seen_keys: set[str] = set()
    for index, raw_shot in enumerate(records):
        field_path = f"shots[{index}]"
        shot = _require_object(raw_shot, field_path, index)
        player_id = _require_string(shot, "player_id", field_path, index)
        client_seq = _require_int(shot, "client_seq", field_path, index, minimum=0)
        key = f"{player_id}:{client_seq}"
        if key in seen_keys:
            raise ReplayError(
                E_REPLAY_INVALID,
                "duplicate player_id/client_seq key",
                index,
                f"{field_path}.client_seq",
                {"key": key},
            )
        seen_keys.add(key)
        local_timestamp = _require_decimal(shot, "local_timestamp", field_path, index)
        if local_timestamp < 0:
            raise ReplayError(
                E_REPLAY_INVALID,
                "local_timestamp must be >= 0",
                index,
                f"{field_path}.local_timestamp",
            )
        arrival_seq = _require_int(shot, "arrival_seq", field_path, index, minimum=0)
        raw_position = _require_object(
            _require_field(shot, "hit_position", field_path, index),
            f"{field_path}.hit_position",
            index,
        )
        hit_x = _require_decimal(raw_position, "x", f"{field_path}.hit_position", index)
        hit_y = _require_decimal(raw_position, "y", f"{field_path}.hit_position", index)
        cover_id = _resolve_cover(
            raw_position,
            covers,
            f"{field_path}.hit_position",
            index,
            shot.get("cover_id"),
        )
        incidence_angle = _require_decimal(shot, "incidence_angle", field_path, index)
        if not (Decimal(0) <= incidence_angle <= Decimal(90)):
            raise ReplayError(
                E_REPLAY_INVALID,
                "incidence_angle must be in [0, 90]",
                index,
                f"{field_path}.incidence_angle",
                {"actual": str(incidence_angle)},
            )
        momentum = _require_decimal(shot, "momentum", field_path, index)
        if momentum < 0:
            raise ReplayError(
                E_REPLAY_INVALID,
                "momentum must be >= 0",
                index,
                f"{field_path}.momentum",
            )
        effective_step = _require_int(
            shot, "effective_step", field_path, index, minimum=0
        )
        if effective_step >= physical_steps:
            raise ReplayError(
                E_REPLAY_INVALID,
                "effective_step is outside physical_steps",
                index,
                f"{field_path}.effective_step",
                {"effective_step": effective_step, "physical_steps": physical_steps},
            )
        shots.append(
            Shot(
                index=index,
                player_id=player_id,
                client_seq=client_seq,
                local_timestamp=local_timestamp,
                arrival_seq=arrival_seq,
                hit_x=hit_x,
                hit_y=hit_y,
                incidence_angle=incidence_angle,
                momentum=momentum,
                effective_step=effective_step,
                cover_id=cover_id,
            )
        )
    return tuple(shots)


def _load_revokes(data: Any, physical_steps: int) -> tuple[Revoke, ...]:
    records = _require_list(data, "revokes")
    revokes: list[Revoke] = []
    seen: set[tuple[str, int, int]] = set()
    for index, raw_revoke in enumerate(records):
        field_path = f"revokes[{index}]"
        revoke = _require_object(raw_revoke, field_path, index)
        player_id = _require_string(revoke, "player_id", field_path, index)
        client_seq = _require_int(revoke, "client_seq", field_path, index, minimum=0)
        arrival_seq = _require_int(revoke, "arrival_seq", field_path, index, minimum=0)
        effective_step = _require_int(
            revoke, "effective_step", field_path, index, minimum=0
        )
        if effective_step >= physical_steps:
            raise ReplayError(
                E_REPLAY_INVALID,
                "effective_step is outside physical_steps",
                index,
                f"{field_path}.effective_step",
                {"effective_step": effective_step, "physical_steps": physical_steps},
            )
        duplicate_key = (player_id, client_seq, effective_step)
        if duplicate_key in seen:
            raise ReplayError(
                E_REPLAY_INVALID,
                "duplicate revoke for the same player/client/effective_step",
                index,
                f"{field_path}.client_seq",
                {"key": f"{player_id}:{client_seq}"},
            )
        seen.add(duplicate_key)
        revokes.append(
            Revoke(
                index=index,
                player_id=player_id,
                client_seq=client_seq,
                arrival_seq=arrival_seq,
                effective_step=effective_step,
            )
        )
    return tuple(revokes)


def _resolve_auxiliary_file(
    match: dict[str, Any],
    key: str,
    base_dir: Path,
    field_path: str,
) -> Path | None:
    value = match.get(key)
    if value is None:
        return None
    if not isinstance(value, str) or not value:
        raise ReplayError(E_REPLAY_INVALID, "expected a non-empty path", None, field_path)
    path = Path(value)
    return path if path.is_absolute() else base_dir / path


def _locate_input(input_path: Path) -> tuple[Path, Path | None]:
    path = input_path.expanduser()
    if path.is_dir():
        candidates = ("match.json", "replay.json")
        match_path = next((path / name for name in candidates if (path / name).is_file()), None)
        if match_path is None:
            raise ReplayError(
                E_REPLAY_INVALID,
                "input directory must contain match.json or replay.json",
                None,
                "match",
                {"directory": path.name},
            )
        covers_path = path / "covers.json"
        return match_path, covers_path if covers_path.is_file() else None
    if not path.is_file():
        raise ReplayError(E_REPLAY_INVALID, f"input path not found: {path}", None, "input")
    return path, None


def load_scenario(input_path: str | Path) -> Scenario:
    match_path, sibling_covers = _locate_input(Path(input_path))
    match_data = _require_object(_load_json(match_path, "match"), "match")
    if "schema_version" in match_data:
        schema_version = _require_int(match_data, "schema_version", "match", minimum=1)
        if schema_version != 1:
            raise ReplayError(
                E_REPLAY_INVALID,
                "unsupported match schema_version",
                None,
                "match.schema_version",
                {"expected": 1, "actual": schema_version},
            )
    base_dir = match_path.parent

    config_path = _resolve_auxiliary_file(match_data, "config_file", base_dir, "match.config_file")
    if config_path is not None:
        config = _load_config(_load_json(config_path, "config"), "config")
    elif "config" in match_data:
        config = _load_config(match_data["config"], "config")
    else:
        default_config_path = base_dir / "config.json"
        if default_config_path.is_file():
            config = _load_config(_load_json(default_config_path, "config"), "config")
        else:
            raise ReplayError(
                E_REPLAY_INVALID,
                "configuration is missing",
                None,
                "match.config",
            )

    covers_path = _resolve_auxiliary_file(match_data, "covers_file", base_dir, "match.covers_file")
    if covers_path is None:
        covers_path = sibling_covers
    if covers_path is not None:
        covers_data = _load_json(covers_path, "covers")
    elif "covers" in match_data:
        covers_data = match_data["covers"]
    else:
        raise ReplayError(
            E_REPLAY_INVALID,
            "cover descriptions are missing",
            None,
            "match.covers",
        )
    covers = _load_covers(covers_data)

    shots = _load_shots(
        _require_field(match_data, "shots", "match"),
        covers,
        config.physical_steps,
    )
    revokes = _load_revokes(
        match_data.get("revokes", []),
        config.physical_steps,
    )

    baseline_path = _resolve_auxiliary_file(
        match_data, "baseline_file", base_dir, "match.baseline_file"
    )
    if baseline_path is None:
        candidate = base_dir / "baseline.json"
        baseline_path = candidate if candidate.is_file() else None

    source_files: dict[str, Path] = {"match": match_path}
    if config_path is not None:
        source_files["config"] = config_path
    if covers_path is not None:
        source_files["covers"] = covers_path
    if baseline_path is not None:
        source_files["baseline"] = baseline_path
    return Scenario(
        config=config,
        covers=covers,
        shots=shots,
        revokes=revokes,
        source_dir=base_dir,
        source_files=source_files,
        baseline_path=baseline_path,
    )

