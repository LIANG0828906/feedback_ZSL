"""Configuration loading, schema validation and parser-stage injection."""

from __future__ import annotations

import json
from pathlib import Path
from typing import Any

from .errors import (
    E_CONFIG_INVALID,
    E_CONSTRAINT_INVALID,
    E_INJECT_CONSTRAINT_LIMIT,
    E_INJECT_DANGLING_DOOR,
    E_INJECT_KEY_CYCLE,
    E_INJECT_ROOM_BOUNDS,
    E_INJECT_TRUNCATED_INPUT,
    WorkbenchError,
)
from .models import (
    CONSTRAINT_TYPES,
    Constraint,
    Door,
    Level,
    Room,
    Tag,
    WorkbenchConfig,
    cycle_in_key_dependencies,
    serialize_config,
    validate_level_structure,
)
from .util import content_hash


SUPPORTED_INJECTIONS = {
    "room_bounds",
    "dangling_door",
    "key_cycle",
    "constraint_limit",
    "truncated_input",
}

ROUNDING_MODES = {
    "half_up",
    "half_even",
    "floor",
    "ceiling",
    "down",
    "up",
}


def _config_error(message: str, location: str, *, code: str = E_CONFIG_INVALID) -> WorkbenchError:
    return WorkbenchError(code, message, details={"location": location})


def _require_int(value: Any, location: str, minimum: int | None = None) -> int:
    if isinstance(value, bool) or not isinstance(value, int):
        raise _config_error("expected integer", location)
    if minimum is not None and value < minimum:
        raise _config_error(f"expected integer >= {minimum}", location)
    return value


def _require_number(value: Any, location: str, minimum: float | None = None) -> float:
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        raise _config_error("expected number", location)
    result = float(value)
    if minimum is not None and result < minimum:
        raise _config_error(f"expected number >= {minimum}", location)
    if result != result or result in (float("inf"), float("-inf")):
        raise _config_error("expected finite number", location)
    return result


def _inject_document(document: dict[str, Any], injection: str) -> dict[str, Any]:
    """Corrupt the input document at parser entry, before schema validation.

    This function intentionally does not call the generator or reachability
    implementation. Its malformed fixtures are caught by the entry parser.
    """

    result = json.loads(json.dumps(document))
    if injection == "room_bounds":
        fixture = result.setdefault("input_layout", {})
        rooms = fixture.setdefault("rooms", [])
        rooms.append(
            {
                "id": "injected_room",
                "x": -7,
                "y": 1,
                "width": 3,
                "height": 3,
            }
        )
        fixture.setdefault("doors", [])
        fixture.setdefault("tags", [])
        return result
    if injection == "dangling_door":
        fixture = result.setdefault("input_layout", {})
        rooms = fixture.setdefault("rooms", [])
        if not rooms:
            rooms.append({"id": "injected_anchor", "x": 0, "y": 0, "width": 3, "height": 3})
        fixture.setdefault("tags", [])
        fixture.setdefault("doors", []).append(
            {
                "id": "injected_door",
                "room_a": rooms[0].get("id", "injected_anchor"),
                "room_b": "injected_missing_room",
                "direction": "both",
            }
        )
        return result
    if injection == "key_cycle":
        fixture = result.setdefault("input_layout", {})
        fixture.setdefault("rooms", []).extend(
            [
                {"id": "cycle_room_a", "x": 0, "y": 0, "width": 3, "height": 3},
                {"id": "cycle_room_b", "x": 4, "y": 0, "width": 3, "height": 3},
            ]
        )
        fixture.setdefault("doors", []).append(
            {
                "id": "cycle_door",
                "room_a": "cycle_room_a",
                "room_b": "cycle_room_b",
                "direction": "both",
                "locked_by": "cycle_key_a",
            }
        )
        fixture.setdefault("tags", []).extend(
            [
                {"id": "cycle_tag_a", "kind": "key", "room_id": "cycle_room_a", "key_id": "cycle_key_a"},
                {"id": "cycle_tag_b", "kind": "key", "room_id": "cycle_room_b", "key_id": "cycle_key_b"},
            ]
        )
        fixture["key_dependencies"] = {
            "cycle_key_a": "cycle_key_b",
            "cycle_key_b": "cycle_key_a",
        }
        return result
    if injection == "constraint_limit":
        limit = int(result.get("limits", {}).get("max_constraints", 64))
        constraints = result.setdefault("constraints", [])
        while len(constraints) <= limit:
            index = len(constraints)
            constraints.append(
                {
                    "id": f"injected_limit_{index}",
                    "type": "connectivity",
                    "priority": 0,
                    "hard": True,
                }
            )
        return result
    raise ValueError(f"unknown document injection: {injection}")


def inject_raw_text(raw_text: str, injection: str) -> str:
    if injection == "truncated_input":
        offset = max(1, min(len(raw_text) - 1, len(raw_text) // 2))
        raise WorkbenchError(
            E_INJECT_TRUNCATED_INPUT,
            "input JSON was truncated before parsing",
            injection_point={"location": "$", "byte_offset": offset},
            details={"received_bytes": len(raw_text), "truncated_to": offset},
        )
    return raw_text


def load_config(
    path: str | Path | None = None,
    *,
    raw_text: str | None = None,
    injection: str | None = None,
) -> WorkbenchConfig:
    if raw_text is None:
        if path is None:
            raise _config_error("config path or raw text is required", "$")
        try:
            raw_text = Path(path).read_text(encoding="utf-8")
        except OSError as exc:
            raise WorkbenchError(E_CONFIG_INVALID, f"cannot read config: {exc}", details={"location": str(path)}) from exc

    if injection:
        if injection not in SUPPORTED_INJECTIONS:
            raise _config_error(
                f"unsupported injection mode: {injection}",
                "$injection",
                code=E_CONFIG_INVALID,
            )
        raw_text = inject_raw_text(raw_text, injection)

    try:
        document = json.loads(raw_text)
    except json.JSONDecodeError as exc:
        if injection == "truncated_input":
            raise WorkbenchError(
                E_INJECT_TRUNCATED_INPUT,
                "truncated JSON rejected at parser entry",
                injection_point={"location": "$", "byte_offset": exc.pos},
                details={"line": exc.lineno, "column": exc.colno},
            ) from exc
        raise _config_error(
            "invalid JSON",
            f"$@{exc.pos}",
            code=E_CONFIG_INVALID,
        ) from exc
    if not isinstance(document, dict):
        raise _config_error("root must be an object", "$")

    if injection and injection not in ("truncated_input",):
        document = _inject_document(document, injection)
        if injection == "constraint_limit":
            limits = document.get("limits", {}) if isinstance(document.get("limits", {}), dict) else {}
            limit = limits.get("max_constraints", 64)
            count = len(document.get("constraints", []))
            raise WorkbenchError(
                E_INJECT_CONSTRAINT_LIMIT,
                "injected constraint count exceeds the configured limit",
                injection_point={"location": "/constraints", "index": int(limit) if isinstance(limit, int) else 64},
                details={"count": count, "limit": limit},
            )

    config = parse_config_document(document)
    return config


def parse_config_document(document: dict[str, Any]) -> WorkbenchConfig:
    version = _require_int(document.get("version", 1), "/version", 1)
    seed = _require_int(document.get("seed"), "/seed", 0)
    grid = document.get("grid")
    if not isinstance(grid, dict):
        raise _config_error("grid object is required", "/grid")
    grid_width = _require_int(grid.get("width"), "/grid/width", 1)
    grid_height = _require_int(grid.get("height"), "/grid/height", 1)

    limits = document.get("limits", {})
    if not isinstance(limits, dict):
        raise _config_error("limits must be an object", "/limits")
    max_constraints = _require_int(limits.get("max_constraints", 64), "/limits/max_constraints", 1)

    raw_constraints = document.get("constraints")
    if not isinstance(raw_constraints, list):
        raise _config_error("constraints array is required", "/constraints")
    if len(raw_constraints) > max_constraints:
        raise _config_error(
            f"constraint count {len(raw_constraints)} exceeds limit {max_constraints}",
            "/constraints",
            code=E_CONSTRAINT_INVALID,
        )

    constraints = tuple(
        _parse_constraint(raw, index, grid_width, grid_height)
        for index, raw in enumerate(raw_constraints)
    )
    ids = [constraint.id for constraint in constraints]
    if len(ids) != len(set(ids)):
        duplicate = next(item for item in ids if ids.count(item) > 1)
        raise _config_error(
            f"duplicate constraint id: {duplicate}",
            "/constraints",
            code=E_CONSTRAINT_INVALID,
        )

    generation = document.get("generation", {})
    if not isinstance(generation, dict):
        raise _config_error("generation must be an object", "/generation")
    quantization = document.get("quantization", {})
    if not isinstance(quantization, dict):
        raise _config_error("quantization must be an object", "/quantization")
    evaluation = document.get("evaluation", {})
    if not isinstance(evaluation, dict):
        raise _config_error("evaluation must be an object", "/evaluation")

    decimals = _require_int(quantization.get("decimals", 2), "/quantization/decimals", 0)
    if decimals > 12:
        raise _config_error("decimals must be <= 12", "/quantization/decimals")
    rounding = str(quantization.get("rounding", "half_up"))
    if rounding not in ROUNDING_MODES:
        raise _config_error("unsupported rounding mode", "/quantization/rounding")
    evaluation_mode = str(evaluation.get("mode", "fixed"))
    if evaluation_mode not in ("fixed", "float"):
        raise _config_error("evaluation.mode must be fixed or float", "/evaluation/mode")
    uncertain_policy = str(evaluation.get("uncertain_policy", "allow"))
    if uncertain_policy not in ("allow", "fail"):
        raise _config_error("uncertain_policy must be allow or fail", "/evaluation/uncertain_policy")

    direction_mode = str(generation.get("direction_mode", "bidirectional"))
    if direction_mode not in ("bidirectional", "mixed"):
        raise _config_error("direction_mode must be bidirectional or mixed", "/generation/direction_mode")

    room_min_width = _require_int(generation.get("room_min_width", 3), "/generation/room_min_width", 1)
    room_min_height = _require_int(generation.get("room_min_height", 3), "/generation/room_min_height", 1)
    room_max_width = _require_int(generation.get("room_max_width", 8), "/generation/room_max_width", room_min_width)
    room_max_height = _require_int(generation.get("room_max_height", 6), "/generation/room_max_height", room_min_height)
    if room_max_width > grid_width or room_max_height > grid_height:
        raise _config_error("room maximum dimensions exceed grid", "/generation")

    raw_copy = json.loads(json.dumps(document))
    config = WorkbenchConfig(
        version=version,
        seed=seed,
        grid_width=grid_width,
        grid_height=grid_height,
        constraints=constraints,
        decimals=decimals,
        rounding=rounding,
        max_attempts=_require_int(generation.get("max_attempts", 64), "/generation/max_attempts", 1),
        max_phase_retries=_require_int(
            generation.get("max_phase_retries", 8),
            "/generation/max_phase_retries",
            1,
        ),
        max_constraints=max_constraints,
        room_min_width=room_min_width,
        room_min_height=room_min_height,
        room_max_width=room_max_width,
        room_max_height=room_max_height,
        room_margin=_require_int(generation.get("room_margin", 1), "/generation/room_margin", 0),
        rewards=_require_int(generation.get("rewards", 1), "/generation/rewards", 0),
        direction_mode=direction_mode,
        evaluation_mode=evaluation_mode,
        uncertain_policy=uncertain_policy,
        raw_document=raw_copy,
        source_hash=content_hash(document),
    )
    _validate_constraint_semantics(config)
    _parse_fixture(config, document)
    return config


def _parse_constraint(
    raw: Any,
    index: int,
    grid_width: int,
    grid_height: int,
) -> Constraint:
    location = f"/constraints/{index}"
    if not isinstance(raw, dict):
        raise _config_error("constraint must be an object", location, code=E_CONSTRAINT_INVALID)
    constraint_id = raw.get("id")
    if not isinstance(constraint_id, str) or not constraint_id:
        raise _config_error("constraint id must be a non-empty string", f"{location}/id", code=E_CONSTRAINT_INVALID)
    kind = raw.get("type")
    if kind not in CONSTRAINT_TYPES:
        raise _config_error(f"unsupported constraint type: {kind}", f"{location}/type", code=E_CONSTRAINT_INVALID)
    priority = _require_int(raw.get("priority", 0), f"{location}/priority")
    hard = raw.get("hard", True)
    if not isinstance(hard, bool):
        raise _config_error("hard must be boolean", f"{location}/hard", code=E_CONSTRAINT_INVALID)
    decimals = raw.get("decimals")
    if decimals is not None:
        decimals = _require_int(decimals, f"{location}/decimals", 0)
        if decimals > 12:
            raise _config_error("decimals must be <= 12", f"{location}/decimals", code=E_CONSTRAINT_INVALID)
    rounding = raw.get("rounding")
    if rounding is not None and rounding not in ROUNDING_MODES:
        raise _config_error("unsupported rounding mode", f"{location}/rounding", code=E_CONSTRAINT_INVALID)

    params: dict[str, Any] = {}
    if kind == "room_count":
        minimum = _require_int(raw.get("min"), f"{location}/min", 1)
        maximum = _require_int(raw.get("max"), f"{location}/max", 1)
        if minimum > maximum:
            raise _config_error("min must be <= max", location, code=E_CONSTRAINT_INVALID)
        params = {"min": minimum, "max": maximum}
    elif kind in ("area_target", "door_count_target"):
        target = _require_number(raw.get("target"), f"{location}/target", 0.0)
        tolerance = _require_number(raw.get("tolerance", 0.0), f"{location}/tolerance", 0.0)
        coefficient = _require_number(raw.get("coefficient", 1.0), f"{location}/coefficient")
        weight = _require_number(raw.get("weight", 1.0), f"{location}/weight", 0.0)
        params = {
            "target": target,
            "tolerance": tolerance,
            "coefficient": coefficient,
            "weight": weight,
        }
    elif kind == "key_chain":
        keys = raw.get("keys", [])
        if not isinstance(keys, list) or not all(isinstance(key, str) and key for key in keys):
            raise _config_error("keys must be non-empty strings", f"{location}/keys", code=E_CONSTRAINT_INVALID)
        if len(keys) != len(set(keys)):
            raise _config_error("keys must be unique", f"{location}/keys", code=E_CONSTRAINT_INVALID)
        coefficients_raw = raw.get("coefficients", {})
        if not isinstance(coefficients_raw, dict):
            raise _config_error("coefficients must be an object", f"{location}/coefficients", code=E_CONSTRAINT_INVALID)
        coefficients: dict[str, float] = {}
        for key in keys:
            value = coefficients_raw.get(key, 1.0)
            coefficients[key] = _require_number(value, f"{location}/coefficients/{key}", 0.0)
        unknown_coefficients = sorted(set(coefficients_raw) - set(keys))
        if unknown_coefficients:
            raise _config_error(
                f"coefficients for unknown keys: {unknown_coefficients}",
                f"{location}/coefficients",
                code=E_CONSTRAINT_INVALID,
            )
        threshold = _require_number(raw.get("threshold", 0.0), f"{location}/threshold")
        params = {
            "keys": keys,
            "coefficients": coefficients,
            "threshold": threshold,
        }
    elif kind == "no_lock_unreachable_end":
        params = {}
    elif kind == "connectivity":
        scope = raw.get("scope", "all_rooms")
        if scope not in ("all_rooms", "start_to_end"):
            raise _config_error(
                "scope must be all_rooms or start_to_end",
                f"{location}/scope",
                code=E_CONSTRAINT_INVALID,
            )
        params = {"scope": scope}

    return Constraint(
        id=constraint_id,
        type=kind,
        priority=priority,
        hard=hard,
        params=params,
        decimals=decimals,
        rounding=rounding,
    )


def _validate_constraint_semantics(config: WorkbenchConfig) -> None:
    keychains = config.constraints_of_type("key_chain")
    if len(keychains) > 1:
        raise _config_error(
            "at most one key_chain constraint is supported",
            "/constraints",
            code=E_CONSTRAINT_INVALID,
        )
    room_constraints = config.constraints_of_type("room_count")
    if len(room_constraints) > 1:
        raise _config_error(
            "at most one room_count constraint is supported",
            "/constraints",
            code=E_CONSTRAINT_INVALID,
        )
    if config.grid_width < config.room_min_width or config.grid_height < config.room_min_height:
        raise _config_error("grid is smaller than minimum room size", "/grid")
    minimum, maximum = config.room_count_bounds()
    if minimum > maximum:
        raise _config_error("room_count constraints have empty intersection", "/constraints")
    if minimum < 1:
        raise _config_error("room_count minimum must be positive", "/constraints")
    if not keychains and config.constraints_of_type("no_lock_unreachable_end"):
        constraint = config.constraints_of_type("no_lock_unreachable_end")[0]
        raise WorkbenchError(
            E_CONSTRAINT_CONFLICT,
            "no_lock_unreachable_end requires a key_chain constraint",
            constraint_id=constraint.id,
            details={
                "stage": "config_validation",
                "minimal_evidence": {
                    "constraints": [constraint.id],
                    "facts": ["no key_chain constraint is configured"],
                },
            },
        )


def _parse_fixture(config: WorkbenchConfig, document: dict[str, Any]) -> None:
    fixture = document.get("input_layout")
    if fixture is None:
        return
    if not isinstance(fixture, dict):
        raise _config_error("input_layout must be an object", "/input_layout")
    grid = fixture.get("grid", {})
    width = _require_int(grid.get("width", config.grid_width), "/input_layout/grid/width", 1)
    height = _require_int(grid.get("height", config.grid_height), "/input_layout/grid/height", 1)
    rooms = fixture.get("rooms", [])
    doors = fixture.get("doors", [])
    tags = fixture.get("tags", [])
    if not all(isinstance(items, list) for items in (rooms, doors, tags)):
        raise _config_error("input_layout rooms/doors/tags must be arrays", "/input_layout")

    try:
        parsed_rooms = tuple(
            Room(
                id=str(item["id"]),
                x=int(item["x"]),
                y=int(item["y"]),
                width=int(item["width"]),
                height=int(item["height"]),
            )
            for item in rooms
        )
        parsed_doors = tuple(
            Door(
                id=str(item["id"]),
                room_a=str(item["room_a"]),
                room_b=str(item["room_b"]),
                direction=item.get("direction", "both"),
                locked_by=item.get("locked_by"),
            )
            for item in doors
        )
        parsed_tags = tuple(
            Tag(
                id=str(item["id"]),
                kind=item["kind"],
                room_id=str(item["room_id"]),
                key_id=item.get("key_id"),
                door_id=item.get("door_id"),
            )
            for item in tags
        )
    except (KeyError, TypeError, ValueError) as exc:
        raise _config_error(f"invalid input_layout item: {exc}", "/input_layout") from exc

    level = Level(width, height, parsed_rooms, parsed_doors, parsed_tags)
    issues = validate_level_structure(level, config)
    if issues:
        if any(issue["code"] == "room_out_of_bounds" for issue in issues):
            raise WorkbenchError(
                E_INJECT_ROOM_BOUNDS,
                "input contains a room outside the integer grid",
                injection_point=next(
                    issue for issue in issues if issue["code"] == "room_out_of_bounds"
                ),
                details={"issues": issues},
            )
        if any(issue["code"] == "dangling_door_endpoint" for issue in issues):
            raise WorkbenchError(
                E_INJECT_DANGLING_DOOR,
                "input contains a door endpoint that does not name a room",
                injection_point=next(
                    issue for issue in issues if issue["code"] == "dangling_door_endpoint"
                ),
                details={"issues": issues},
            )
        raise _config_error("input_layout is structurally invalid", "/input_layout")

    dependencies = fixture.get("key_dependencies", {})
    if dependencies:
        if not isinstance(dependencies, dict):
            raise _config_error("key_dependencies must be an object", "/input_layout/key_dependencies")
        cycle = cycle_in_key_dependencies({str(k): str(v) for k, v in dependencies.items()})
        if cycle:
            raise WorkbenchError(
                E_INJECT_KEY_CYCLE,
                "input contains a cyclic key/lock dependency",
                injection_point={
                    "location": "/input_layout/key_dependencies",
                    "cycle": cycle,
                },
                details={"cycle": cycle},
            )


