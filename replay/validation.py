"""Recording JSON parsing, protocol validation, and v1 -> v2 upgrades."""

from __future__ import annotations

import json
import os
from decimal import Decimal
from typing import Any

from .errors import (
    E_BASELINE_CORRUPT,
    E_COORD_RANGE,
    E_DUP_CLIENT_SEQ,
    E_INPUT_INVALID,
    E_LATE_OP,
    E_NONFINITE,
    ReplayError,
    fail,
)
from .model import (
    Config,
    EntityState,
    Operation,
    Recording,
    RoomState,
    STATE_MODEL_VERSION,
    dec,
    require_finite,
)


DEFAULT_TOLERANCES = {
    "position": Decimal("0.000001"),
    "velocity": Decimal("0.000001"),
    "heading": Decimal("0.000001"),
    "resource": Decimal("0.000001"),
}


def _as_dict(value: Any, path: str) -> dict[str, Any]:
    if not isinstance(value, dict):
        fail(E_INPUT_INVALID, "expected JSON object", path)
    return value


def _as_list(value: Any, path: str) -> list[Any]:
    if not isinstance(value, list):
        fail(E_INPUT_INVALID, "expected JSON array", path)
    return value


def _as_int(value: Any, path: str) -> int:
    if isinstance(value, bool) or not isinstance(value, int):
        fail(E_INPUT_INVALID, "expected integer", path)
    return value


def _as_bool(value: Any, path: str) -> bool:
    if not isinstance(value, bool):
        fail(E_INPUT_INVALID, "expected boolean", path)
    return value


def _as_str(value: Any, path: str) -> str:
    if not isinstance(value, str) or not value:
        fail(E_INPUT_INVALID, "expected non-empty string", path)
    return value


def _json_load(path: str) -> Any:
    try:
        with open(path, "r", encoding="utf-8") as handle:
            return json.load(
                handle,
                parse_float=Decimal,
                parse_int=int,
                parse_constant=lambda text: float(text),
            )
    except FileNotFoundError:
        fail(E_INPUT_INVALID, "recording file does not exist", path)
    except json.JSONDecodeError as exc:
        fail(
            E_INPUT_INVALID,
            f"invalid JSON: {exc.msg}",
            f"{path}:{exc.lineno}:{exc.colno}",
        )
    except OSError as exc:
        fail(E_INPUT_INVALID, f"cannot read recording: {exc}", path)
    raise AssertionError("unreachable")


def load_recording(path: str, *, inject: list[str] | None = None) -> Recording:
    raw = _json_load(path)
    injection_context = None
    if inject:
        from .injection import apply_recording_injections

        raw, injection_context = apply_recording_injections(raw, inject)
    try:
        return parse_recording(raw)
    except ReplayError as exc:
        raise exc.with_injection(injection_context) from None


def _parse_config(raw: Any) -> Config:
    payload = _as_dict(raw, "config")

    def get_decimal(name: str, default: str | None = None) -> Decimal:
        if name not in payload:
            if default is None:
                fail(E_INPUT_INVALID, "missing configuration value", f"config.{name}")
            value = Decimal(default)
        else:
            value = dec(payload[name], f"config.{name}")
        require_finite(value, f"config.{name}")
        return value

    def get_int(name: str, default: int | None = None, *, minimum: int | None = None) -> int:
        if name not in payload:
            if default is None:
                fail(E_INPUT_INVALID, "missing configuration value", f"config.{name}")
            value = default
        else:
            value = _as_int(payload[name], f"config.{name}")
        if minimum is not None and value < minimum:
            fail(E_INPUT_INVALID, f"must be >= {minimum}", f"config.{name}")
        return value

    total_steps = get_int("total_steps", minimum=1)
    if total_steps > 1_000_000:
        fail(E_INPUT_INVALID, "total_steps exceeds implementation limit", "config.total_steps")
    timestep = get_decimal("timestep", "0.05")
    if timestep <= 0:
        fail(E_INPUT_INVALID, "timestep must be > 0", "config.timestep")
    flip_threshold = get_decimal("flip_threshold", "1")
    if flip_threshold <= 0:
        fail(E_INPUT_INVALID, "flip_threshold must be > 0", "config.flip_threshold")
    resource_threshold = get_decimal("resource_diff_threshold", "0.000001")
    if resource_threshold < 0:
        fail(
            E_INPUT_INVALID,
            "resource_diff_threshold must be >= 0",
            "config.resource_diff_threshold",
        )
    coord_limit = get_decimal("coord_limit", "1000000")
    if coord_limit <= 0:
        fail(E_INPUT_INVALID, "coord_limit must be > 0", "config.coord_limit")
    fixed_param_decimals = get_int(
        "fixed_param_decimals", 4, minimum=0
    )
    fixed_accum_decimals = get_int(
        "fixed_accum_decimals", max(fixed_param_decimals, 6), minimum=0
    )
    if fixed_param_decimals > 18 or fixed_accum_decimals > 18:
        fail(E_INPUT_INVALID, "fixed decimals must be <= 18", "config.fixed_decimals")
    late_window_steps = get_int("late_window_steps", 0, minimum=0)
    if "alive_default" in payload:
        alive_default = _as_bool(payload["alive_default"], "config.alive_default")
    else:
        alive_default = True

    tolerances_raw = payload.get("tolerances", {})
    tolerances_obj = _as_dict(tolerances_raw, "config.tolerances")
    tolerances = dict(DEFAULT_TOLERANCES)
    for name in DEFAULT_TOLERANCES:
        if name in tolerances_obj:
            value = dec(tolerances_obj[name], f"config.tolerances.{name}")
            require_finite(value, f"config.tolerances.{name}")
            if value < 0:
                fail(E_INPUT_INVALID, "tolerance must be >= 0", f"config.tolerances.{name}")
            tolerances[name] = value

    return Config(
        total_steps=total_steps,
        timestep=timestep,
        flip_threshold=flip_threshold,
        resource_diff_threshold=resource_threshold,
        coord_limit=coord_limit,
        fixed_param_decimals=fixed_param_decimals,
        fixed_accum_decimals=fixed_accum_decimals,
        late_window_steps=late_window_steps,
        alive_default=alive_default,
        tolerances=tolerances,
    )


def _parse_initial_state(
    raw: Any,
    *,
    source_version: str,
    config: Config,
) -> tuple[RoomState, dict[str, EntityState], list[dict[str, Any]]]:
    payload = _as_dict(raw, "initial_state")
    room_payload = _as_dict(payload.get("room"), "initial_state.room")
    if "resource" not in room_payload:
        fail(E_INPUT_INVALID, "missing room resource", "initial_state.room.resource")
    resource = dec(room_payload["resource"], "initial_state.room.resource")
    require_finite(resource, "initial_state.room.resource")

    entities_raw = _as_list(payload.get("entities"), "initial_state.entities")
    if not entities_raw:
        fail(E_INPUT_INVALID, "at least one entity is required", "initial_state.entities")
    entities: dict[str, EntityState] = {}
    derived: list[dict[str, Any]] = []
    for index, item_raw in enumerate(entities_raw):
        base = f"initial_state.entities[{index}]"
        item = _as_dict(item_raw, base)
        entity_id = _as_str(item.get("id"), f"{base}.id")
        if entity_id in entities:
            fail(E_INPUT_INVALID, "duplicate entity id", f"{base}.id")
        position = _as_dict(item.get("position"), f"{base}.position")
        velocity = _as_dict(item.get("velocity", {"x": 0, "y": 0}), f"{base}.velocity")
        x = dec(position.get("x"), f"{base}.position.x")
        y = dec(position.get("y"), f"{base}.position.y")
        heading = dec(item.get("heading", 0), f"{base}.heading")
        vx = dec(velocity.get("x", 0), f"{base}.velocity.x")
        vy = dec(velocity.get("y", 0), f"{base}.velocity.y")
        for value, path in (
            (x, f"{base}.position.x"),
            (y, f"{base}.position.y"),
            (heading, f"{base}.heading"),
            (vx, f"{base}.velocity.x"),
            (vy, f"{base}.velocity.y"),
        ):
            require_finite(value, path, nonfinite_code=E_NONFINITE)
        if abs(x) > config.coord_limit or abs(y) > config.coord_limit:
            fail(
                E_COORD_RANGE,
                "initial entity position exceeds coord_limit",
                f"{base}.position",
            )
        if "alive" in item:
            alive = _as_bool(item["alive"], f"{base}.alive")
        elif source_version == "v1":
            alive = config.alive_default
            derived.append(
                {
                    "path": f"{base}.alive",
                    "value": alive,
                    "source": "v1_default",
                    "default": config.alive_default,
                }
            )
        else:
            fail(E_INPUT_INVALID, "v2 entity is missing alive", f"{base}.alive")
        entities[entity_id] = EntityState(
            entity_id=entity_id,
            x=x,
            y=y,
            vx=vx,
            vy=vy,
            heading=heading,
            alive=alive,
        )
    return RoomState(resource=resource), entities, derived


def _parse_operations(raw: Any, *, config: Config, entities: dict[str, EntityState]) -> list[Operation]:
    operations_raw = _as_list(raw, "operations")
    operations: list[Operation] = []
    seen_ids: set[str] = set()
    seen_client_seq: dict[tuple[str, int], str] = {}
    for index, item_raw in enumerate(operations_raw):
        base = f"operations[{index}]"
        item = _as_dict(item_raw, base)
        op_id = _as_str(item.get("id"), f"{base}.id")
        if op_id in seen_ids:
            fail(E_INPUT_INVALID, "duplicate operation id", f"{base}.id")
        seen_ids.add(op_id)
        entity_id = _as_str(item.get("entity_id"), f"{base}.entity_id")
        if entity_id not in entities:
            fail(E_INPUT_INVALID, "operation references unknown entity", f"{base}.entity_id")
        client_seq = _as_int(item.get("client_seq"), f"{base}.client_seq")
        if client_seq < 0:
            fail(E_INPUT_INVALID, "client_seq must be >= 0", f"{base}.client_seq")
        client_key = (entity_id, client_seq)
        if client_key in seen_client_seq:
            fail(
                E_DUP_CLIENT_SEQ,
                f"duplicate client_seq for entity; first seen at {seen_client_seq[client_key]}",
                f"{base}.client_seq",
                details={"entity_id": entity_id, "client_seq": client_seq},
            )
        seen_client_seq[client_key] = base
        arrival_seq = _as_int(item.get("arrival_seq"), f"{base}.arrival_seq")
        if arrival_seq < 0:
            fail(E_INPUT_INVALID, "arrival_seq must be >= 0", f"{base}.arrival_seq")
        effective_step = _as_int(item.get("effective_step"), f"{base}.effective_step")
        if effective_step < 1 or effective_step > config.total_steps:
            fail(
                E_INPUT_INVALID,
                f"effective_step must be in [1, {config.total_steps}]",
                f"{base}.effective_step",
            )
        arrival_step = _as_int(
            item.get("arrival_step", effective_step), f"{base}.arrival_step"
        )
        if arrival_step < 1:
            fail(E_INPUT_INVALID, "arrival_step must be >= 1", f"{base}.arrival_step")
        if arrival_step < effective_step:
            fail(
                E_INPUT_INVALID,
                "arrival_step cannot precede effective_step",
                f"{base}.arrival_step",
            )
        if arrival_step - effective_step > config.late_window_steps:
            fail(
                E_LATE_OP,
                "operation arrived outside the declared late window",
                f"{base}.arrival_step",
                details={
                    "arrival_step": arrival_step,
                    "effective_step": effective_step,
                    "late_window_steps": config.late_window_steps,
                },
            )
        speed = dec(item.get("speed"), f"{base}.speed")
        heading = dec(item.get("heading"), f"{base}.heading")
        resource_delta = dec(item.get("resource_delta", 0), f"{base}.resource_delta")
        require_finite(speed, f"{base}.speed")
        require_finite(heading, f"{base}.heading", nonfinite_code=E_NONFINITE)
        require_finite(resource_delta, f"{base}.resource_delta")
        if speed < 0:
            fail(E_INPUT_INVALID, "speed must be >= 0", f"{base}.speed")
        operations.append(
            Operation(
                op_id=op_id,
                entity_id=entity_id,
                client_seq=client_seq,
                arrival_seq=arrival_seq,
                arrival_step=arrival_step,
                effective_step=effective_step,
                speed=speed,
                heading=heading,
                resource_delta=resource_delta,
                source_index=index,
            )
        )
    operations.sort(key=Operation.sort_key)
    return operations


def parse_recording(raw: Any) -> Recording:
    payload = _as_dict(raw, "recording")
    source_version = _as_str(payload.get("recording_version", "v2"), "recording_version")
    if source_version not in ("v1", "v2"):
        fail(
            E_INPUT_INVALID,
            "recording_version must be v1 or v2",
            "recording_version",
        )
    if "match_id" not in payload:
        fail(E_INPUT_INVALID, "missing match_id", "match_id")
    match_id = _as_str(payload["match_id"], "match_id")
    config = _parse_config(payload.get("config"))
    room, entities, derived = _parse_initial_state(
        payload.get("initial_state"),
        source_version=source_version,
        config=config,
    )
    operations = _parse_operations(payload.get("operations"), config=config, entities=entities)
    by_step: dict[int, list[Operation]] = {}
    for operation in operations:
        by_step.setdefault(operation.effective_step, []).append(operation)
    for step_ops in by_step.values():
        step_ops.sort(key=Operation.sort_key)
    return Recording(
        match_id=match_id,
        source_version=source_version,
        state_model_version=STATE_MODEL_VERSION,
        config=config,
        room=room,
        entities=entities,
        operations=operations,
        operations_by_step=by_step,
        derived_fields=derived,
    )


def load_json_object(path: str, *, code: str = E_BASELINE_CORRUPT) -> Any:
    """Load an auxiliary JSON document with a caller-selected business code."""

    try:
        with open(path, "r", encoding="utf-8") as handle:
            return json.load(
                handle,
                parse_float=Decimal,
                parse_int=int,
                parse_constant=lambda text: float(text),
            )
    except FileNotFoundError:
        fail(code, "file does not exist", path)
    except json.JSONDecodeError as exc:
        fail(code, f"invalid JSON: {exc.msg}", f"{path}:{exc.lineno}:{exc.colno}")
    except OSError as exc:
        fail(code, f"cannot read file: {exc}", path)
    raise AssertionError("unreachable")


def atomic_write_json(path: str, payload: Any) -> None:
    """Write deterministic JSON using a temporary file and os.replace."""

    import tempfile

    directory = os.path.dirname(os.path.abspath(path)) or "."
    os.makedirs(directory, exist_ok=True)
    fd, temp_path = tempfile.mkstemp(prefix=".tmp-json-", dir=directory)
    try:
        with os.fdopen(fd, "w", encoding="utf-8") as handle:
            json.dump(
                payload,
                handle,
                ensure_ascii=True,
                sort_keys=True,
                separators=(",", ":"),
                allow_nan=False,
            )
            handle.write("\n")
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temp_path, path)
    except Exception:
        try:
            os.unlink(temp_path)
        except OSError:
            pass
        raise
